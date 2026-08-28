// Package pager es la capa que hay entre el B+tree y el archivo de datos: entrega
// páginas, asigna las nuevas, lleva la cuenta de las sucias y delimita los grupos de
// mutaciones que el WAL confirmará todas o ninguna.
//
// El contrato de este paquete se cierra en la F1 y no en la F3, aunque el WAL detrás de
// él todavía no exista (DESIGN.md sec. 10). La razón es que las secs. 7.3 y 7.5 imponen
// tres condiciones sobre cómo el árbol muta páginas -- cada mutación debe producir una
// imagen registrable, pertenecer a un grupo de commit delimitado, y respetar el page_lsn
// frente al desalojo -- y la F2 son las 30 horas más caras del plan. Un árbol escrito
// contra un pager que no las impone se reescribe entero al llegar la F3.
//
// # Qué no hace todavía
//
// El checkpoint completo de la sec. 7.4 es de la F3: aquí están sus pasos 1 y 2
// (FlushDirty), pero no la escritura de la meta ni la rotación del WAL. El conjunto de
// páginas libres se mantiene en memoria y no se persiste, que es la decisión de la sec.
// 6.1, pero quien lo reconstruye al abrir es el barrido del árbol -- este paquete lo
// recibe por AdoptFreeSet, porque saber qué páginas son alcanzables exige conocer el
// árbol y eso es de la F2.
//
// # Sobre el tamaño del caché
//
// El caché no está acotado: las páginas residentes viven hasta el checkpoint. La regla
// de desalojo de la sec. 7.5 se implementa igualmente, en writePage, que es el único
// camino por el que una página baja a datos.db. Así la regla está viva -- el checkpoint
// pasa por ella en cada corrida -- y el día que el caché se acote no hay que buscar
// dónde ponerla. Ver docs/DEUDA-DISENO.md, D4.
//
// # Sobre abortar un grupo
//
// No hay AbortGroup, y es deliberado. Un grupo que no llega a CommitGroup es exactamente
// el caso que la recuperación descarta entero (sec. 8, paso 4), así que en disco no deja
// rastro. Lo que sí queda inconsistente es el caché: las páginas ya mutadas en memoria
// no coinciden con lo que la recuperación reconstruiría. Un error dentro de un grupo es
// por tanto fatal para el proceso -- se propaga hasta arriba y el pager no se vuelve a
// usar.
package pager

import (
	"maps"
	"slices"

	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

// MetaPages son las páginas 0 y 1, las dos ranuras alternadas de la sec. 5.2. No las
// asigna ni las ensucia este paquete: se escriben solo en el paso 3 del checkpoint, por
// su propio camino.
const MetaPages = 2

// File es la interfaz mínima de E/S que necesita este paquete (DESIGN.md sec. 9.1).
//
// Está declarada aquí y no importada de internal/record a propósito: en Go la interfaz
// la declara quien la consume, no quien la implementa. Son estructuralmente idénticas,
// así que el disco falso de la F4 -- con su árbitro de orden global entre datos.db y el
// WAL -- satisfará las dos sin saber que existen.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
}

// Pager entrega páginas del archivo de datos y lleva la contabilidad de lo que hay que
// escribir.
type Pager struct {
	f   File
	log Log

	// frames es el caché: para cada número de página, el único objeto Page vivo. Que sea
	// único importa -- dos objetos para la misma página divergen y uno de los dos pierde
	// sus cambios en silencio. Lo vigila ErrStalePage.
	frames map[uint64]*page.Page

	// dirty son las páginas modificadas que aún no han bajado a datos.db. Las metas nunca
	// están aquí (sec. 5.2, regla 1).
	dirty map[uint64]bool

	// group son las páginas ensuciadas dentro del grupo abierto, pendientes de
	// registrarse como imágenes en CommitGroup. Es nil cuando no hay grupo abierto, y esa
	// distinción -- nil frente a mapa vacío -- es la que hace posible ErrNoGroup.
	group map[uint64]bool

	// free es el conjunto de páginas libres (sec. 6.1). En memoria y nunca en disco.
	free map[uint64]bool

	// total es total_pages: cuántas páginas cubre el archivo lógicamente. onDisk es hasta
	// dónde está materializado de verdad. Difieren entre un Alloc que extiende y el
	// CommitGroup que lo consolida, que es donde la sec. 7.6 pone el fsync de extensión.
	total  uint64
	onDisk uint64

	// buf es el marco de E/S reutilizado. Una asignación de 4 KiB por página escrita
	// sería una asignación por operación del árbol.
	buf []byte
}

// New abre un pager sobre un archivo que ya mide totalPages páginas. No escribe nada:
// para un archivo nuevo está Create.
//
// totalPages viene del último registro de commit tras la recuperación (sec. 8, paso 7), o
// de la meta si no hubo nada que recuperar. Nunca del tamaño del archivo en el sistema de
// archivos: ese tamaño puede ir por delante, porque la sec. 7.6 extiende antes de
// confirmar.
func New(f File, log Log, totalPages uint64) (*Pager, error) {
	if totalPages < MetaPages {
		return nil, ErrOutOfRange
	}
	return &Pager{
		f:      f,
		log:    log,
		frames: make(map[uint64]*page.Page),
		dirty:  make(map[uint64]bool),
		free:   make(map[uint64]bool),
		total:  totalPages,
		onDisk: totalPages,
		buf:    make([]byte, page.Size),
	}, nil
}

// Create prepara un archivo vacío: materializa las dos ranuras meta y hace fsync. El
// contenido de las metas es de la F3 -- aquí quedan a ceros, que es un CRC inválido, y es
// justo lo que la sec. 8 paso 1 espera encontrar en un archivo sin checkpoint todavía:
// ninguna meta válida, así que se reproduce el WAL desde el LSN 0.
func Create(f File, log Log) (*Pager, error) {
	pg, err := New(f, log, MetaPages)
	if err != nil {
		return nil, err
	}
	pg.onDisk = 0
	if err := pg.extend(); err != nil {
		return nil, err
	}
	return pg, nil
}

// TotalPages es el total_pages que viajará en el próximo registro de commit.
func (pg *Pager) TotalPages() uint64 { return pg.total }

// FreePages devuelve el conjunto de páginas libres, ordenado. Lo consume Validate() para
// comprobar el invariante 6: la unión con las alcanzables cubre el archivo y la
// intersección es vacía.
func (pg *Pager) FreePages() []uint64 {
	return slices.Sorted(maps.Keys(pg.free))
}

// AdoptFreeSet fija el conjunto de páginas libres reconstruido al abrir (sec. 6.1). Lo
// llama quien acaba de barrer el árbol, porque saber qué páginas son alcanzables exige
// conocer el árbol y el pager no lo conoce.
//
// Rechaza las metas y todo lo que caiga fuera del archivo: una entrada así en el conjunto
// de libres se asignaría más tarde y produciría una escritura fuera de rango o encima de
// una meta.
func (pg *Pager) AdoptFreeSet(ids []uint64) error {
	set := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if id < MetaPages {
			return ErrMetaPage
		}
		if id >= pg.total {
			return ErrOutOfRange
		}
		if set[id] {
			return ErrDoubleFree
		}
		set[id] = true
	}
	pg.free = set
	return nil
}

// Get devuelve la página id, del caché si está residente y del archivo si no.
//
// Al leerla del archivo verifica CRC y page_id (page.Decode): el CRC distingue una página
// válida de una escrita a medias, y el page_id de una página íntegra que acabó en la
// ranura equivocada. Los errores de page se propagan tal cual, porque el que sea dice qué
// pasó.
func (pg *Pager) Get(id uint64) (*page.Page, error) {
	if id >= pg.total {
		return nil, ErrOutOfRange
	}
	if p, ok := pg.frames[id]; ok {
		return p, nil
	}
	if _, err := pg.f.ReadAt(pg.buf, int64(id)*page.Size); err != nil {
		return nil, err
	}
	p, err := page.Decode(pg.buf, id)
	if err != nil {
		return nil, err
	}
	pg.frames[id] = p
	return p, nil
}

// Alloc entrega una página nueva del tipo t, del conjunto de libres si lo hay y
// extendiendo el archivo si no. La devuelve ya en el caché y ya ensuciada, así que exige
// un grupo abierto.
//
// El cuerpo viene a ceros aunque la página se recicle: la imagen que irá al log es la de
// una página vacía, no la de la página vieja con sus claves de otra época.
//
// Cuando extiende, sube total_pages pero no toca el archivo todavía. La materialización y
// su fsync ocurren en CommitGroup, que es donde la sec. 7.6 los pide: "el archivo se
// extiende y se hace fsync antes de que un commit confirme un Put que use la página
// nueva".
func (pg *Pager) Alloc(t page.Type) (*page.Page, error) {
	if pg.group == nil {
		return nil, ErrNoGroup
	}

	var id uint64
	if len(pg.free) > 0 {
		// El menor de los libres, no uno cualquiera: recorrer un mapa en Go da un orden
		// distinto en cada corrida, y la F4 necesita que una semilla reproduzca la misma
		// secuencia de asignaciones bit a bit (sec. 9.1).
		id = slices.Min(slices.Collect(maps.Keys(pg.free)))
		delete(pg.free, id)
	} else {
		id = pg.total
		pg.total++
	}

	p := page.New(id, t)
	pg.frames[id] = p
	pg.dirty[id] = true
	pg.group[id] = true
	return p, nil
}

// Free devuelve la página id al conjunto de libres. Exige un grupo abierto, como toda
// mutación de la estructura.
//
// Saca la página del caché y de los conjuntos de sucias: ya no es alcanzable, así que
// escribirla en el checkpoint sería trabajo por nada. Lo que quede en su ranura del
// archivo es contenido viejo con CRC válido, y eso no rompe el invariante 6 -- la
// partición se decide por alcanzabilidad, no por lo que la página contenga, y el
// invariante solo exige que la página sea legible.
func (pg *Pager) Free(id uint64) error {
	if pg.group == nil {
		return ErrNoGroup
	}
	if id < MetaPages {
		return ErrMetaPage
	}
	if id >= pg.total {
		return ErrOutOfRange
	}
	if pg.free[id] {
		return ErrDoubleFree
	}
	pg.free[id] = true
	delete(pg.frames, id)
	delete(pg.dirty, id)
	delete(pg.group, id)
	return nil
}

// MarkDirty declara que p fue modificada. Es el punto por el que la sec. 7.3 se vuelve
// cumplible: solo dentro de un grupo, y quedando anotada para que CommitGroup produzca su
// imagen.
//
// Rechaza una página que no sea la que el caché tiene para ese id (ErrStalePage). Sin esa
// comprobación, mutar una copia obsoleta -- por ejemplo una Page que se guardó antes de
// un Free y un Alloc que reciclaron el mismo número -- pisaría en silencio los cambios de
// la página viva, y el fallo aparecería miles de operaciones después de su causa.
func (pg *Pager) MarkDirty(p *page.Page) error {
	if pg.group == nil {
		return ErrNoGroup
	}
	if p.ID < MetaPages {
		return ErrMetaPage
	}
	if p.ID >= pg.total {
		return ErrOutOfRange
	}
	if vivo, ok := pg.frames[p.ID]; ok && vivo != p {
		return ErrStalePage
	}
	pg.frames[p.ID] = p
	pg.dirty[p.ID] = true
	pg.group[p.ID] = true
	return nil
}

// BeginGroup abre un grupo de mutaciones. Todo lo que se ensucie hasta CommitGroup se
// aplicará entero o no se aplicará en absoluto.
//
// Los grupos no se anidan. Un Put que divide una hoja toca cuatro páginas y es un solo
// grupo; no hay un caso en el que un grupo deba contener a otro, y permitirlo produciría
// un commit que confirma las mutaciones de ambos.
func (pg *Pager) BeginGroup() error {
	if pg.group != nil {
		return ErrGroupOpen
	}
	pg.group = make(map[uint64]bool)
	return nil
}

// CommitGroup cierra el grupo abierto: materializa la extensión pendiente del archivo,
// registra la imagen de cada página ensuciada y escribe el registro de commit con el
// estado del asignador.
//
// El orden es el de las secs. 7.6 y 7.2 y no es negociable:
//
//  1. Extender datos.db y hacer fsync, si algún Alloc creció el archivo. Antes del
//     commit, porque la extensión es una operación de metadatos que ninguna imagen de
//     página describe: si el commit confirmara un Put que usa la página 900 y el archivo
//     midiera 800, la recuperación tendría que escribir en un agujero.
//  2. Anexar las imágenes. Cada una sella su page_lsn en la página (LogPage).
//  3. Escribir el commit y hacer fsync sobre el WAL. Hasta aquí nada es duradero;
//     después de aquí, todo lo es, y es cuando Put puede devolver nil.
//
// Las imágenes van en orden creciente de número de página. No hace falta para la
// correctitud -- la recuperación aplica el grupo entero -- pero sí para que una semilla
// de la F4 produzca el mismo log byte a byte en cada corrida.
//
// Las páginas siguen sucias al volver: están a salvo en el log, no en su sitio
// definitivo. Bajan a datos.db en el checkpoint (sec. 7.2, paso 7).
func (pg *Pager) CommitGroup(rootID uint64) error {
	if pg.group == nil {
		return ErrNoGroup
	}
	if err := pg.extend(); err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(pg.group)) {
		if _, err := pg.log.LogPage(pg.frames[id]); err != nil {
			return err
		}
	}
	st := State{
		RootID: rootID,
		// free_head va a cero: la sec. 6.1 decide no persistir el conjunto de libres, y la
		// sec. 7.3 conserva el campo en el formato por si algún día se persistiera. Se
		// escribe, no se lee en la recuperación.
		FreeHead:   0,
		TotalPages: pg.total,
	}
	if err := pg.log.Commit(st); err != nil {
		return err
	}
	pg.group = nil
	return nil
}

// FlushDirty son los pasos 1 y 2 del checkpoint (sec. 7.4): bajar a datos.db todas las
// páginas sucias y hacer fsync. Los pasos 3 a 5 -- escribir la meta, su fsync, y la
// rotación del WAL -- son de la F3.
//
// No corre con un grupo abierto. Un grupo a medias tiene páginas mutadas en memoria cuyo
// commit todavía no existe; escribirlas a datos.db es precisamente lo que la sec. 7.5
// prohíbe, y aquí se detecta antes en vez de dejarlo a la comprobación del page_lsn.
func (pg *Pager) FlushDirty() error {
	if pg.group != nil {
		return ErrGroupOpen
	}
	for _, id := range slices.Sorted(maps.Keys(pg.dirty)) {
		if err := pg.writePage(pg.frames[id]); err != nil {
			return err
		}
	}
	if err := pg.f.Sync(); err != nil {
		return err
	}
	clear(pg.dirty)
	return nil
}

// writePage baja una página a datos.db. Es el único camino por el que una página llega al
// archivo de datos, y por eso es donde vive la regla de desalojo de la sec. 7.5.
//
// La regla: una página sucia no puede escribirse mientras wal_flushed_lsn < page_lsn. Sin
// ella, el write-ahead se cumple en el orden de las llamadas pero se viola aquí -- si la
// página baja antes del fsync de su registro, la escritura puede desgarrarse y el log no
// tiene con qué repararla, porque su registro se perdió. Página alcanzable rota, sin
// reparación posible.
func (pg *Pager) writePage(p *page.Page) error {
	if pg.log.FlushedLSN() < p.LSN {
		if err := pg.log.Sync(); err != nil {
			return err
		}
		if pg.log.FlushedLSN() < p.LSN {
			return ErrWriteAhead
		}
	}
	if err := p.EncodeTo(pg.buf); err != nil {
		return err
	}
	_, err := pg.f.WriteAt(pg.buf, int64(p.ID)*page.Size)
	return err
}

// extend materializa en el archivo las páginas que total_pages promete y onDisk todavía
// no, y hace fsync.
//
// Escribe páginas cero explícitas, no un Truncate. Un archivo disperso deja agujeros que
// se leen como ceros y fallan el CRC igual, pero solo se materializan al escribirlos: el
// espacio podría no existir y el fallo aparecería en el peor momento. Es la misma
// escritura explícita que hace la recuperación en la sec. 8, paso 5.
func (pg *Pager) extend() error {
	if pg.onDisk >= pg.total {
		return nil
	}
	clear(pg.buf)
	for id := pg.onDisk; id < pg.total; id++ {
		if _, err := pg.f.WriteAt(pg.buf, int64(id)*page.Size); err != nil {
			return err
		}
	}
	if err := pg.f.Sync(); err != nil {
		return err
	}
	pg.onDisk = pg.total
	return nil
}
