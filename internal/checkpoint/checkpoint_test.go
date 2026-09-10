package checkpoint_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jsav2003/durable-kv/internal/checkpoint"
	"github.com/jsav2003/durable-kv/internal/fsx"
	"github.com/jsav2003/durable-kv/internal/fsx/fsxtest"
	"github.com/jsav2003/durable-kv/internal/meta"
	"github.com/jsav2003/durable-kv/internal/page"
	"github.com/jsav2003/durable-kv/internal/pager"
	"github.com/jsav2003/durable-kv/internal/wal"
)

// montaje es el motor mínimo con el que se prueba el checkpoint: un disco en memoria con
// datos.db y el WAL compartiendo traza, el pager encima, y el checkpoint sobre los dos.
type montaje struct {
	d     *fsxtest.Disco
	datos fsx.File
	pg    *pager.Pager
	log   *wal.WAL
	cp    *checkpoint.Checkpoint
}

func monta(t *testing.T, umbral int64) *montaje {
	t.Helper()
	d := fsxtest.Nuevo()

	datos, err := d.Open("datos.db")
	if err != nil {
		t.Fatal(err)
	}
	log, err := wal.Abrir(d, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })

	pg, err := pager.Create(datos, log)
	if err != nil {
		t.Fatal(err)
	}
	// Sin meta válida todavía, así que el primer checkpoint escribe la ranura 0.
	return &montaje{d: d, datos: datos, pg: pg, log: log,
		cp: checkpoint.Nuevo(datos, pg, log, 0, umbral)}
}

// pon asigna n páginas dentro de un grupo y lo cierra, que es lo que hace un Put.
func (m *montaje) pon(t *testing.T, n int) uint64 {
	t.Helper()
	if err := m.pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	var raiz uint64
	for i := range n {
		p, err := m.pg.Alloc(page.TypeLeaf)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if i == 0 {
			raiz = p.ID
		}
	}
	if err := m.pg.CommitGroup(raiz); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	return raiz
}

// El orden de los cinco pasos de la sec. 7.4, afirmado sobre la traza compartida entre
// datos.db y el WAL. Con una traza por componente este test no podría existir, y un
// checkpoint que liberara el log antes de bajar las páginas pasaría en verde.
func TestElOrdenDeLosCincoPasos(t *testing.T) {
	m := monta(t, 0)
	raiz := m.pon(t, 3)
	m.d.Traza.Limpia()

	if err := m.cp.Correr(raiz); err != nil {
		t.Fatalf("Correr: %v", err)
	}

	tz := m.d.Traza
	// Paso 1: la primera página del árbol baja a datos.db (la 2 en adelante; las ranuras
	// 0 y 1 son las metas y no pertenecen al pager).
	pagina := tz.Indice(fmt.Sprintf("datos.db:write %d+%d", 2*page.Size, page.Size))
	// Paso 3: la meta se escribe en la ranura 0.
	metaEscrita := tz.Indice(fmt.Sprintf("datos.db:write %d+%d", 0, page.Size))
	// Pasos 2 y 4: los dos fsync de datos.db, uno a cada lado de la escritura de la meta.
	syncs := indices(tz.Eventos(), "datos.db:sync")
	// Paso 5.
	walNuevo := tz.Indice(wal.Nombre(1) + ":create")
	dirSync := tz.Indice("dir:sync")
	walViejo := tz.Indice(wal.Nombre(0) + ":remove")

	if pagina < 0 || metaEscrita < 0 || walNuevo < 0 || dirSync < 0 || walViejo < 0 {
		t.Fatalf("faltan eventos en la traza: %q", tz.Eventos())
	}
	if len(syncs) < 2 {
		t.Fatalf("fsync de datos.db = %d, quiero al menos 2 (pasos 2 y 4): %q",
			len(syncs), tz.Eventos())
	}

	comprueba := []struct {
		antes, despues int
		porque         string
	}{
		{pagina, syncs[0], "las paginas sucias bajan antes del fsync del paso 2"},
		{syncs[0], metaEscrita, "la meta se escribe despues del fsync de las paginas"},
		{metaEscrita, syncs[1], "la meta se sincroniza despues de escribirse (paso 4)"},
		{syncs[1], walNuevo, "el log no se rota antes del fsync de la meta"},
		{walNuevo, dirSync, "el fsync del directorio va tras crear el log nuevo"},
		{dirSync, walViejo, "el log viejo no se borra antes del fsync del directorio"},
	}
	for _, c := range comprueba {
		if c.antes >= c.despues {
			t.Errorf("%s: %d no precede a %d\ntraza: %q",
				c.porque, c.antes, c.despues, tz.Eventos())
		}
	}
}

// La meta se escribe siempre sobre la más antigua de las dos. Si el sistema se cae
// escribiéndola, la otra sigue intacta -- y esa garantía se evapora si dos checkpoints
// seguidos escriben la misma ranura.
func TestLaMetaAlternaDeRanura(t *testing.T) {
	m := monta(t, 0)

	quiero := []uint64{0, 1, 0, 1}
	for i, ranura := range quiero {
		raiz := m.pon(t, 1)
		if got := m.cp.Ranura(); got != ranura {
			t.Fatalf("checkpoint %d: escribe la ranura %d, quiero %d", i, got, ranura)
		}
		m.d.Traza.Limpia()
		if err := m.cp.Correr(raiz); err != nil {
			t.Fatalf("Correr: %v", err)
		}
		evento := fmt.Sprintf("datos.db:write %d+%d", int64(ranura)*page.Size, page.Size)
		if !m.d.Traza.Contiene(evento) {
			t.Errorf("checkpoint %d: no se escribio la ranura %d\ntraza: %q",
				i, ranura, m.d.Traza.Eventos())
		}
	}
}

// La meta escrita describe dónde buscar los registros **posteriores** a este checkpoint, y
// esos irán a la generación nueva: por eso la época que se guarda es la N+1 y no la actual.
func TestLaMetaGuardaLaEpocaSiguienteYElLSNDelMomento(t *testing.T) {
	m := monta(t, 0)
	raiz := m.pon(t, 2)
	lsn := m.log.LSN()

	if err := m.cp.Correr(raiz); err != nil {
		t.Fatalf("Correr: %v", err)
	}

	got, ranura, err := meta.Leer(m.datos)
	if err != nil {
		t.Fatalf("meta.Leer: %v", err)
	}
	if ranura != 0 {
		t.Errorf("ranura = %d, quiero 0", ranura)
	}
	if got.Epoca != 1 {
		t.Errorf("epoca = %d, quiero 1: la generacion que existe tras el paso 5", got.Epoca)
	}
	if got.LSN != lsn {
		t.Errorf("lsn = %d, quiero %d", got.LSN, lsn)
	}
	if got.RootID != raiz {
		t.Errorf("root_id = %d, quiero %d", got.RootID, raiz)
	}
	if got.TotalPages != m.pg.TotalPages() {
		t.Errorf("total_pages = %d, quiero %d", got.TotalPages, m.pg.TotalPages())
	}
	if m.log.Epoca() != 1 {
		t.Errorf("el log quedo en la epoca %d, quiero 1", m.log.Epoca())
	}
}

// Tras el checkpoint no queda nada sucio: lo que el log tenía que aportar ya está en su
// sitio definitivo, y por eso el log se puede soltar.
func TestTrasElCheckpointElLogEstaVacioYNadaSucio(t *testing.T) {
	m := monta(t, 0)
	raiz := m.pon(t, 4)
	if m.log.Bytes() == 0 {
		t.Fatal("el log esta vacio antes del checkpoint: el montaje no prueba nada")
	}

	if err := m.cp.Correr(raiz); err != nil {
		t.Fatalf("Correr: %v", err)
	}
	if m.log.Bytes() != 0 {
		t.Errorf("bytes de log = %d tras rotar, quiero 0", m.log.Bytes())
	}
	// Un segundo checkpoint sin nada que bajar no escribe ninguna página del árbol.
	m.d.Traza.Limpia()
	if err := m.cp.Correr(raiz); err != nil {
		t.Fatalf("Correr (segunda): %v", err)
	}
	for _, e := range m.d.Traza.Eventos() {
		if e == fmt.Sprintf("datos.db:write %d+%d", 2*page.Size, page.Size) {
			t.Errorf("se rebajo una pagina que ya no estaba sucia\ntraza: %q",
				m.d.Traza.Eventos())
		}
	}
}

// Un grupo a medias tiene páginas mutadas en memoria cuyo commit todavía no existe.
// Bajarlas a datos.db es lo que la sec. 7.5 prohíbe, y aquí se detecta antes en vez de
// dejarlo a la comprobación del page_lsn.
func TestNoCorreConUnGrupoAbierto(t *testing.T) {
	m := monta(t, 0)
	if err := m.pg.BeginGroup(); err != nil {
		t.Fatal(err)
	}
	p, err := m.pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.cp.Correr(p.ID); !errors.Is(err, pager.ErrGroupOpen) {
		t.Fatalf("Correr con un grupo abierto = %v, quiero ErrGroupOpen", err)
	}
	// Y no dejó la meta a medio escribir por el camino.
	if _, _, err := meta.Leer(m.datos); !errors.Is(err, meta.ErrSinMetaValida) {
		t.Errorf("meta.Leer = %v, quiero ErrSinMetaValida: el checkpoint aborto antes", err)
	}
}

// El disparo es por bytes de WAL acumulados. Como el checkpoint siempre rota, lo acumulado
// es el tamaño de la generación actual, así que Toca vuelve a false por sí solo.
func TestTocaPorBytesDeWAL(t *testing.T) {
	// Una imagen de página son algo más de 4 KiB de log: con el umbral en 12 KiB hacen
	// falta tres para dispararlo.
	m := monta(t, 12*1024)

	if m.cp.Toca() {
		t.Fatal("Toca con el log vacio")
	}
	var raiz uint64
	for range 3 {
		raiz = m.pon(t, 1)
	}
	if !m.cp.Toca() {
		t.Fatalf("no toca con %d bytes de log y umbral 12288", m.log.Bytes())
	}

	if err := m.cp.Correr(raiz); err != nil {
		t.Fatalf("Correr: %v", err)
	}
	if m.cp.Toca() {
		t.Error("sigue tocando tras el checkpoint: la rotacion no reinicio la cuenta")
	}
}

func TestUmbralNoPositivoUsaElPorDefecto(t *testing.T) {
	m := monta(t, -1)
	m.pon(t, 1)
	if m.cp.Toca() {
		t.Error("Toca con un umbral que deberia ser el de 4 MiB por defecto")
	}
}

// indices devuelve todas las posiciones del evento e en la traza.
func indices(eventos []string, e string) []int {
	var out []int
	for i, v := range eventos {
		if v == e {
			out = append(out, i)
		}
	}
	return out
}
