// Package fsxtest es el sistema de archivos en memoria con el que se prueban los
// paquetes que escriben a disco.
//
// Tiene dos modos. Por omisión no simula ningún fallo: cada WriteAt se aplica al vuelo y
// lo único que aporta sobre un buffer es la traza compartida por todos los archivos de un
// mismo Disco -- una versión en miniatura del árbitro de orden global de la sec. 9.1,
// porque lo que hay que demostrar del write-ahead no es que las escrituras ocurran, sino
// en qué orden ocurren **entre datos.db y el WAL**, y con una traza por componente ese
// orden relativo no se puede observar.
//
// Con Volatil en true entra el buffer de escrituras no sincronizadas de la sec. 9.1: un
// WriteAt no toca el contenido duradero hasta que un Sync lo vuelca, igual que el caché
// del sistema operativo. Las dos instancias de File (datos y WAL) comparten **una sola**
// cola de pendientes, etiquetada por nombre de archivo; Sync de un archivo vacía solo sus
// entradas. Esta es la base sobre la que la F4 monta el descarte, el reordenamiento y la
// escritura desgarrada de la caída.
//
// DirVolatil hace lo mismo con la **entrada de directorio**: crear o borrar un archivo no es
// duradero hasta el fsync del directorio. Es lo que vuelve provocable el corte de la fila 5a
// del argumento de correctitud -- la ventana entre el Open de la generación nueva del WAL y
// el dir.Sync() de la rotación de la sec. 7.4 --, que hasta aquí ningún test podía producir
// porque un archivo nacía duradero. Viene apagado: encenderlo cambia lo que sobrevive a una
// caída, y el barrido de la sec. 9.2 está calibrado sin él.
package fsxtest

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/jsav2003/durable-kv/internal/fsx"
)

// Traza es la secuencia de eventos observados, compartida por todos los archivos de un
// Disco y por el propio directorio.
type Traza struct {
	mu      sync.Mutex
	eventos []string
}

// Anota registra un evento.
func (t *Traza) Anota(formato string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventos = append(t.eventos, fmt.Sprintf(formato, args...))
}

// Eventos devuelve una copia de lo observado hasta ahora.
func (t *Traza) Eventos() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.eventos)
}

// Limpia descarta lo observado. Sirve para acotar una aserción a la parte de la corrida
// que interesa -- por ejemplo, a un solo checkpoint.
func (t *Traza) Limpia() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventos = nil
}

// Indice devuelve la posición del primer evento igual a e, o -1. Comparar dos índices es
// como se afirma un orden entre dos escrituras de archivos distintos.
func (t *Traza) Indice(e string) int {
	return slices.Index(t.Eventos(), e)
}

// Contiene informa si la traza incluye el evento exacto e.
func (t *Traza) Contiene(e string) bool {
	return t.Indice(e) >= 0
}

// escritura es un WriteAt que aún no ha pasado por Sync. Vive en la cola global del Disco,
// no en el Archivo, porque el orden que importa modelar es el que hay entre las escrituras
// de datos.db y las del WAL.
type escritura struct {
	archivo string
	off     int64
	datos   []byte
}

// Disco es un fsx.Dir en memoria.
type Disco struct {
	Traza    *Traza
	archivos map[string]*Archivo
	// SyncFalla, si no es nil, es el error que devuelve el fsync del directorio. Modela
	// la plataforma donde la operación no está disponible, y el fallo real de E/S.
	SyncFalla error

	// Volatil activa el buffer de la sec. 9.1. Con Volatil en false (lo normal fuera de la
	// F4) cada WriteAt se aplica al vuelo y el Disco es un observador simple.
	Volatil bool

	// DirVolatil lleva lo mismo a las entradas de directorio: con él en true, la creación y
	// el borrado de un archivo no son duraderos hasta Disco.Sync(). Es independiente de
	// Volatil -- una cosa es el contenido y otra la entrada -- y viene apagado.
	DirVolatil bool

	// borrados son los archivos que Remove ya quitó de la vista del proceso y cuyo borrado
	// todavía no pasó por el fsync del directorio: si la caída llega antes, resucitan con
	// los bytes que eran duraderos. Solo se usa con DirVolatil en true.
	borrados map[string]*Archivo

	// pendientes es la cola global de escrituras sin sincronizar, en orden de emisión.
	// Solo se usa con Volatil en true.
	pendientes []escritura

	// nEscrituras cuenta los WriteAt sobre todos los archivos del Disco. Es el número N de
	// "caer en la escritura N" de la sec. 9.2.
	nEscrituras int

	// CaeEn, si es > 0, es el número de escritura durante la cual el disco cae: al
	// emitirse ese WriteAt se procesa la cola de pendientes (descarte, reordenamiento y
	// una escritura desgarrada) y toda operación posterior devuelve ErrCaido. Requiere
	// Volatil.
	CaeEn int

	// CaeEnEventos, si no está vacío, es una secuencia de eventos de la traza tras la cual el
	// disco cae: al anotarse el último, habiendo ocurrido los anteriores en ese orden, se
	// procesa la cola igual que con CaeEn.
	//
	// Existe porque CaeEn cuenta WriteAt y hay cortes que no caen en ninguno: el de la fila
	// 5a ocurre entre el create del log nuevo y el fsync del directorio, y ahí no se escribe
	// un solo byte. Es una secuencia y no un evento suelto porque los que interesan no son
	// únicos -- "dir:sync" ocurre dos veces por rotación --, así que hace falta decir cuál.
	//
	// El evento marca el **comienzo** del efecto de la operación: caer en él significa que
	// esa operación no llegó a surtir efecto, y quien la pidió recibe ErrCaido.
	CaeEnEventos []string

	// vistos es cuántos elementos de CaeEnEventos van casados.
	vistos int

	// Semilla fija el azar del descarte, el reordenamiento y el desgarro. La misma semilla
	// con la misma CaeEn y la misma carga reproduce la caída bit a bit (sec. 9.1).
	Semilla int64

	// Caido pasa a true cuando la caída ya ocurrió.
	Caido bool
}

// Nuevo devuelve un Disco vacío con su traza.
func Nuevo() *Disco {
	return &Disco{
		Traza:    &Traza{},
		archivos: make(map[string]*Archivo),
		borrados: make(map[string]*Archivo),
	}
}

// evento anota e en la traza y, si con él se completa la secuencia de CaeEnEventos, hace
// caer el disco justo ahí. El llamador comprueba d.Caido y devuelve ErrCaido: la operación
// que disparó la caída no surte efecto.
func (d *Disco) evento(formato string, args ...any) {
	e := fmt.Sprintf(formato, args...)
	d.Traza.Anota("%s", e)
	if d.Caido || d.vistos >= len(d.CaeEnEventos) || e != d.CaeEnEventos[d.vistos] {
		return
	}
	d.vistos++
	if d.vistos == len(d.CaeEnEventos) {
		d.cae()
	}
}

// Open abre el archivo nombre, creándolo vacío si no existe. Devuelve siempre el mismo
// objeto para el mismo nombre: dos handles con contenidos distintos para el mismo archivo
// no es lo que hace un sistema de archivos.
func (d *Disco) Open(nombre string) (fsx.File, error) {
	if d.Caido {
		return nil, ErrCaido
	}
	if a, ok := d.archivos[nombre]; ok {
		d.evento("%s:open", nombre)
		if d.Caido {
			return nil, ErrCaido
		}
		return a, nil
	}
	a := &Archivo{nombre: nombre, disco: d, creacionPendiente: d.DirVolatil}
	d.archivos[nombre] = a
	// Crear de nuevo un nombre cuyo borrado seguía pendiente cancela la resurrección: la
	// entrada pasa a nombrar a este archivo, que es lo último que se pidió sobre ella.
	delete(d.borrados, nombre)
	d.evento("%s:create", nombre)
	if d.Caido {
		return nil, ErrCaido
	}
	return a, nil
}

// Remove borra el archivo nombre.
func (d *Disco) Remove(nombre string) error {
	if d.Caido {
		return ErrCaido
	}
	a, ok := d.archivos[nombre]
	if !ok {
		return fmt.Errorf("fsxtest: %s no existe", nombre)
	}
	delete(d.archivos, nombre)
	d.pendientes = slices.DeleteFunc(d.pendientes, func(e escritura) bool {
		return e.archivo == nombre
	})
	// Un borrado sin fsync del directorio puede no llegar al plato. Se guarda para que la
	// caída decida, salvo que la creación tampoco fuera duradera: entonces la entrada nunca
	// existió en el disco y no hay nada que deshacer.
	if d.DirVolatil && !a.creacionPendiente {
		d.borrados[nombre] = a
	}
	d.evento("%s:remove", nombre)
	if d.Caido {
		return ErrCaido
	}
	return nil
}

// Sync es el fsync del directorio: lo que hace duraderas las **entradas**, no los
// contenidos. Las creaciones dejan de estar pendientes y los borrados dejan de poder
// deshacerse.
func (d *Disco) Sync() error {
	if d.Caido {
		return ErrCaido
	}
	d.evento("dir:sync")
	if d.Caido {
		return ErrCaido
	}
	if d.SyncFalla != nil {
		// Un fsync que falla no promete nada: las entradas siguen pendientes.
		return d.SyncFalla
	}
	for _, a := range d.archivos {
		a.creacionPendiente = false
	}
	clear(d.borrados)
	return nil
}

// EntradaDuradera informa si la entrada de directorio de nombre sobreviviría a una caída:
// el archivo existe y su creación ya pasó por un fsync del directorio. Con DirVolatil
// apagado, toda entrada existente es duradera.
func (d *Disco) EntradaDuradera(nombre string) bool {
	a, ok := d.archivos[nombre]
	return ok && !a.creacionPendiente
}

// BorradoPendiente informa si el borrado de nombre todavía no es duradero, de modo que una
// caída podría devolver el archivo al directorio.
func (d *Disco) BorradoPendiente(nombre string) bool {
	_, ok := d.borrados[nombre]
	return ok
}

// Existe informa si el archivo está en el directorio.
func (d *Disco) Existe(nombre string) bool {
	_, ok := d.archivos[nombre]
	return ok
}

// Nombres devuelve los archivos del directorio, ordenados. Es la vista del **proceso**: un
// archivo recién creado se ve aunque su entrada no sea duradera todavía, y uno recién
// borrado no se ve aunque el borrado pueda deshacerse en una caída.
func (d *Disco) Nombres() []string {
	return slices.Sorted(maps.Keys(d.archivos))
}

// NombresDuraderos es la vista del **disco**: los archivos cuya entrada de directorio
// sobreviviría a una caída ahora mismo. Con DirVolatil apagado coincide con Nombres.
func (d *Disco) NombresDuraderos() []string {
	out := make([]string, 0, len(d.archivos)+len(d.borrados))
	for nombre, a := range d.archivos {
		if !a.creacionPendiente {
			out = append(out, nombre)
		}
	}
	for nombre := range d.borrados {
		out = append(out, nombre)
	}
	slices.Sort(out)
	return out
}

// Listar es Nombres con la firma de fsx.Dir.
func (d *Disco) Listar() ([]string, error) {
	return d.Nombres(), nil
}

// Bytes devuelve el contenido **duradero** de un archivo -- lo que sobreviviría a una
// caída ahora mismo --, o nil si no existe. Con Volatil en true, las escrituras que aún
// no han pasado por Sync no están aquí; se ven solo desde ReadAt, igual que el caché del
// sistema es coherente para el proceso pero no para el disco.
func (d *Disco) Bytes(nombre string) []byte {
	a, ok := d.archivos[nombre]
	if !ok {
		return nil
	}
	return slices.Clone(a.datos)
}

// bytesDe es Bytes contando también los archivos con el borrado pendiente, que Reabrir
// puede tener que resucitar.
func (d *Disco) bytesDe(nombre string) []byte {
	if b := d.Bytes(nombre); b != nil {
		return b
	}
	if a, ok := d.borrados[nombre]; ok {
		return slices.Clone(a.datos)
	}
	return nil
}

// Tamano es el tamano duradero de un archivo, o -1 si no existe.
func (d *Disco) Tamano(nombre string) int64 {
	a, ok := d.archivos[nombre]
	if !ok {
		return -1
	}
	return int64(len(a.datos))
}

// NEscrituras es cuántos WriteAt lleva el Disco sobre todos sus archivos.
func (d *Disco) NEscrituras() int {
	return d.nEscrituras
}

// Pendientes es cuántas escrituras sin sincronizar hay en la cola global.
func (d *Disco) Pendientes() int {
	return len(d.pendientes)
}

// aplica vuelca una escritura al contenido duradero de su archivo. Si el archivo ya no
// existe -- lo borró un Remove -- la escritura se pierde, que es lo correcto.
func (d *Disco) aplica(e escritura) {
	a, ok := d.archivos[e.archivo]
	if !ok {
		return
	}
	if fin := e.off + int64(len(e.datos)); fin > int64(len(a.datos)) {
		a.datos = append(a.datos, make([]byte, fin-int64(len(a.datos)))...)
	}
	copy(a.datos[e.off:], e.datos)
}

// sincroniza vuelca las pendientes de un archivo y las saca de la cola global. Las de los
// demás archivos se quedan: un fsync a datos.db no hace duraderas las escrituras al WAL.
func (d *Disco) sincroniza(nombre string) {
	resto := d.pendientes[:0:0]
	for _, e := range d.pendientes {
		if e.archivo == nombre {
			d.aplica(e)
		} else {
			resto = append(resto, e)
		}
	}
	d.pendientes = resto
}

// Reabrir devuelve un Disco nuevo, no volátil y con su propia traza, que hereda solo el
// contenido **duradero** de este: los mismos archivos, con los bytes que sobrevivirían a
// una caída ahora mismo. Es lo que ve un proceso que arranca sobre el disco que dejó el
// anterior al morir. Un archivo que existe pero está vacío se recrea vacío: su existencia
// es un dato que la recuperación lee (paso 1 de la sec. 8).
//
// Las entradas de directorio siguen el mismo criterio que los bytes: una creación que no
// pasó por el fsync del directorio no se hereda, y un borrado que tampoco pasó se deshace.
// Tras una caída eso no decide nada -- cae() ya dejó el directorio en su estado duradero --,
// y solo se nota al reabrir sobre un Disco que no llegó a caer.
func (d *Disco) Reabrir() *Disco {
	n := Nuevo()
	for _, nombre := range d.NombresDuraderos() {
		f, _ := n.Open(nombre)
		if b := d.bytesDe(nombre); len(b) > 0 {
			f.WriteAt(b, 0)
		}
	}
	return n
}

// Archivo es un fsx.File en memoria que anota lo que hace en la traza del Disco.
type Archivo struct {
	nombre string
	disco  *Disco
	datos  []byte
	// Cerrado se pone a true en Close. Un archivo cerrado sigue legible desde el Disco:
	// lo que interesa comprobar es que el WAL cierra el que deja atrás, no impedirlo.
	Cerrado bool
	// creacionPendiente es true entre el Open que creó el archivo y el primer fsync del
	// directorio. Solo lo pone DirVolatil.
	creacionPendiente bool
}

// visible es el contenido que ve el proceso: el duradero con las pendientes de este
// archivo superpuestas en orden. Es lo que devuelve una lectura mientras el proceso sigue
// vivo, aunque nada de eso haya llegado al disco.
func (a *Archivo) visible() []byte {
	b := slices.Clone(a.datos)
	for _, e := range a.disco.pendientes {
		if e.archivo != a.nombre {
			continue
		}
		if fin := e.off + int64(len(e.datos)); fin > int64(len(b)) {
			b = append(b, make([]byte, fin-int64(len(b)))...)
		}
		copy(b[e.off:], e.datos)
	}
	return b
}

func (a *Archivo) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fsxtest: offset negativo %d", off)
	}
	if a.disco.Caido {
		return 0, ErrCaido
	}
	datos := a.visible()
	if off >= int64(len(datos)) {
		return 0, io.EOF
	}
	n := copy(p, datos[off:])
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (a *Archivo) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fsxtest: offset negativo %d", off)
	}
	if a.disco.Caido {
		return 0, ErrCaido
	}
	a.disco.nEscrituras++
	e := escritura{archivo: a.nombre, off: off, datos: slices.Clone(p)}
	a.disco.pendientes = append(a.disco.pendientes, e)
	a.disco.evento("%s:write %d+%d", a.nombre, off, len(p))
	if a.disco.Caido {
		return 0, ErrCaido
	}
	if a.disco.CaeEn > 0 && a.disco.nEscrituras >= a.disco.CaeEn {
		a.disco.cae()
		return 0, ErrCaido
	}
	if !a.disco.Volatil {
		a.disco.sincroniza(a.nombre)
	}
	return len(p), nil
}

func (a *Archivo) Sync() error {
	if a.disco.Caido {
		return ErrCaido
	}
	a.disco.evento("%s:sync", a.nombre)
	if a.disco.Caido {
		return ErrCaido
	}
	a.disco.sincroniza(a.nombre)
	return nil
}

func (a *Archivo) Truncate(size int64) error {
	if a.disco.Caido {
		return ErrCaido
	}
	// El truncado vacía primero lo pendiente de este archivo: un archivo real no reordena
	// un truncado con las escrituras que ya tenía en el caché.
	a.disco.sincroniza(a.nombre)
	if size < int64(len(a.datos)) {
		a.datos = a.datos[:size]
	} else {
		a.datos = append(a.datos, make([]byte, size-int64(len(a.datos)))...)
	}
	a.disco.Traza.Anota("%s:truncate %d", a.nombre, size)
	return nil
}

func (a *Archivo) Close() error {
	a.Cerrado = true
	a.disco.Traza.Anota("%s:close", a.nombre)
	return nil
}

// Size es el tamaño visible del archivo: el duradero más lo que este proceso ha escrito y
// aún no ha sincronizado.
func (a *Archivo) Size() (int64, error) { return int64(len(a.visible())), nil }

var (
	_ fsx.Dir  = (*Disco)(nil)
	_ fsx.File = (*Archivo)(nil)
)
