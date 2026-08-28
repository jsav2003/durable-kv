package pager

import "github.com/jsav2003/motor-almacenamiento/internal/page"

// State es el estado del asignador que viaja en el registro de commit (DESIGN.md sec.
// 7.3). No es una copia de la meta: es la única fuente de verdad sobre dónde está la
// raíz y cuánto mide el archivo tras una caída, porque la meta solo se escribe en el
// checkpoint.
//
// Sin estos campos en el log, la raíz 5 se divide, las páginas 6, 7 y 8 quedan escritas
// y perfectas en disco tras la recuperación, y el motor arranca con root=5, que ahora es
// solo la mitad izquierda -- con Validate() en verde, porque una hoja como raíz es un
// B+tree legal.
type State struct {
	RootID   uint64
	FreeHead uint64
	// TotalPages es cuántas páginas mide datos.db. Va en el commit por la sec. 7.6: la
	// extensión del archivo es una operación de metadatos y no la describe ninguna imagen
	// de página, así que la recuperación necesita saber hasta dónde llega el archivo.
	TotalPages uint64
}

// Log es el WAL visto desde el pager. La implementación real llega en la F3; el contrato
// se fija ahora porque las secs. 7.3 y 7.5 imponen tres condiciones sobre cómo el árbol
// muta páginas -- imagen registrable, grupo de commit delimitado, page_lsn respetado
// frente al desalojo -- y una API de pager diseñada sin ellas se reescribe entera en la
// F3 (DESIGN.md sec. 10).
type Log interface {
	// LogPage asigna el LSN del registro que va a describir a p, lo sella en p.LSN, y
	// anexa la imagen completa de la página al log. No sincroniza: eso es Commit.
	//
	// El orden importa y es el de la sec. 7.2, pasos 2 y 3: la página en memoria recibe
	// primero el page_lsn del registro que la va a describir, y solo después se serializa.
	// Al revés, la imagen que va al log llevaría dentro un page_lsn viejo, y al
	// reaplicarla la recuperación dejaría en disco una página cuyo page_lsn miente sobre
	// qué registro la modificó por última vez.
	LogPage(p *page.Page) (lsn uint64, err error)

	// Commit escribe el registro de commit del grupo abierto y hace fsync sobre el WAL
	// (sec. 7.2, pasos 4 y 5). Hasta que Commit devuelve nil, nada del grupo es duradero;
	// después, todo lo es.
	Commit(st State) error

	// FlushedLSN es el LSN más alto cuyo registro está ya sincronizado en el WAL. Es lo
	// que consulta la regla de desalojo de la sec. 7.5.
	FlushedLSN() uint64

	// Sync fuerza el fsync del WAL fuera de un commit. Lo llama el pager cuando una
	// página sucia tiene que bajar a datos.db y su page_lsn va por delante de lo
	// sincronizado.
	Sync() error
}

// NopLog es la implementación de Log que se usa mientras el WAL no existe: durante toda
// la F2, según el plan de la sec. 10 ("commitGroup sea un no-op durante toda la F2").
//
// No escribe nada en ninguna parte, pero sí asigna LSN crecientes y los declara
// sincronizados al momento. Eso mantiene vivo el camino del page_lsn y de la regla de
// desalojo en vez de dejarlo en código muerto que nadie ejercita hasta la F3.
//
// Lo que NopLog no da es durabilidad: con él, una caída pierde todo lo que no haya
// llegado a datos.db por un checkpoint. La F2 no lo necesita -- su criterio de
// terminación es Put/Get/Scan con Validate() en verde, no sobrevivir a un kill -9.
type NopLog struct {
	lsn uint64
}

// LogPage asigna el siguiente LSN y lo sella en la página. No hay dónde anexar la imagen.
func (l *NopLog) LogPage(p *page.Page) (uint64, error) {
	l.lsn++
	p.LSN = l.lsn
	return l.lsn, nil
}

// Commit no tiene registro que escribir ni archivo que sincronizar. El estado del
// asignador se descarta: sin WAL no hay recuperación que lo lea.
func (l *NopLog) Commit(State) error { return nil }

// FlushedLSN declara sincronizado todo lo asignado, que es lo que hace que la regla de
// desalojo nunca bloquee mientras no haya WAL.
func (l *NopLog) FlushedLSN() uint64 { return l.lsn }

// Sync no tiene nada que forzar.
func (l *NopLog) Sync() error { return nil }
