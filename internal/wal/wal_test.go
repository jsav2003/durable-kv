package wal_test

import (
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
	"github.com/jsav2003/motor-almacenamiento/internal/record"
	"github.com/jsav2003/motor-almacenamiento/internal/wal"
)

// hoja devuelve una página con contenido reconocible, para que un round-trip que perdiera
// bytes no pase por casualidad con un cuerpo a ceros.
func hoja(id uint64) *page.Page {
	p := page.New(id, page.TypeLeaf)
	p.NCells = 2
	for i := range p.Body {
		p.Body[i] = byte(id) ^ byte(i)
	}
	return p
}

func nuevo(t *testing.T, epoca uint32, lsn uint64) (*wal.WAL, *fsxtest.Disco) {
	t.Helper()
	d := fsxtest.Nuevo()
	w, err := wal.Abrir(d, epoca, lsn, 0)
	if err != nil {
		t.Fatalf("Abrir: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w, d
}

// grupo anexa las imágenes de ids y las cierra con un commit.
func grupo(t *testing.T, w *wal.WAL, st pager.State, ids ...uint64) {
	t.Helper()
	for _, id := range ids {
		if _, err := w.LogPage(hoja(id)); err != nil {
			t.Fatalf("LogPage(%d): %v", id, err)
		}
	}
	if err := w.Commit(st); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// El LSN es estrictamente monótono y el commit consume uno propio. Tiene que consumirlo:
// la contigüidad se comprueba sobre todos los registros, y un commit sin LSN dejaría un
// hueco que la lectura leería como el punto de la caída.
func TestLSNContiguoIncluidoElCommit(t *testing.T) {
	w, _ := nuevo(t, 0, 0)

	var lsns []uint64
	for _, id := range []uint64{2, 3, 4} {
		lsn, err := w.LogPage(hoja(id))
		if err != nil {
			t.Fatalf("LogPage: %v", err)
		}
		lsns = append(lsns, lsn)
	}
	if !slices.Equal(lsns, []uint64{1, 2, 3}) {
		t.Fatalf("lsns = %v, quiero [1 2 3]", lsns)
	}
	if err := w.Commit(pager.State{RootID: 2, TotalPages: 5}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if w.LSN() != 4 {
		t.Errorf("tras el commit LSN = %d, quiero 4", w.LSN())
	}
}

// LogPage sella el page_lsn en la página **antes** de serializarla. Al revés, la imagen
// llevaría dentro un page_lsn viejo y la recuperación dejaría en disco una página que
// miente sobre qué registro la modificó por última vez.
func TestLogPageSellaElLSNEnLaImagen(t *testing.T) {
	w, d := nuevo(t, 0, 0)

	p := hoja(7)
	lsn, err := w.LogPage(p)
	if err != nil {
		t.Fatalf("LogPage: %v", err)
	}
	if p.LSN != lsn {
		t.Errorf("la pagina en memoria quedo con lsn=%d, quiero %d", p.LSN, lsn)
	}
	if err := w.Commit(pager.State{RootID: 7, TotalPages: 8}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	f, _ := d.Open(wal.Nombre(0))
	grupos, _, err := wal.Leer(f, 0, 0)
	if !wal.EsFinDeLog(err) {
		t.Fatalf("Leer: %v", err)
	}
	if n := len(grupos); n != 1 {
		t.Fatalf("grupos = %d, quiero 1", n)
	}
	if got := grupos[0].Imagenes[0].LSN; got != lsn {
		t.Errorf("la imagen en el log lleva lsn=%d, quiero %d", got, lsn)
	}
}

// LogPage no sincroniza y Commit sí: es la separación de los pasos 3 y 5 de la sec. 7.2,
// y es lo que evita que cada página tocada cueste un fsync. Que FlushedLSN se quede atrás
// entre medias es lo que la regla de desalojo de la sec. 7.5 consulta.
func TestFlushedLSNSeQuedaAtrasHastaElCommit(t *testing.T) {
	w, _ := nuevo(t, 0, 0)

	w.LogPage(hoja(2))
	w.LogPage(hoja(3))
	if w.FlushedLSN() != 0 {
		t.Fatalf("FlushedLSN = %d tras anexar sin commit, quiero 0", w.FlushedLSN())
	}

	if err := w.Commit(pager.State{RootID: 2, TotalPages: 4}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if w.FlushedLSN() != w.LSN() {
		t.Errorf("FlushedLSN = %d tras el commit, quiero %d", w.FlushedLSN(), w.LSN())
	}
}

// Sync pone al día lo sincronizado sin escribir un commit. Es el camino que pide el pager
// cuando una página sucia tiene que bajar a datos.db con su page_lsn por delante.
func TestSyncPoneAlDiaSinCommit(t *testing.T) {
	w, _ := nuevo(t, 0, 0)

	w.LogPage(hoja(2))
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if w.FlushedLSN() != w.LSN() {
		t.Errorf("FlushedLSN = %d, quiero %d", w.FlushedLSN(), w.LSN())
	}
}

func TestLeerDevuelveLosGruposEnOrden(t *testing.T) {
	w, d := nuevo(t, 3, 100)

	grupo(t, w, pager.State{RootID: 2, TotalPages: 4}, 2, 3)
	grupo(t, w, pager.State{RootID: 5, TotalPages: 9}, 4, 5, 6)

	f, _ := d.Open(wal.Nombre(3))
	grupos, fin, err := wal.Leer(f, 3, 100)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Leer termino con %v, quiero io.EOF (final limpio)", err)
	}
	if len(grupos) != 2 {
		t.Fatalf("grupos = %d, quiero 2", len(grupos))
	}
	if len(grupos[0].Imagenes) != 2 || len(grupos[1].Imagenes) != 3 {
		t.Errorf("imagenes por grupo = %d y %d, quiero 2 y 3",
			len(grupos[0].Imagenes), len(grupos[1].Imagenes))
	}
	if grupos[1].Estado.RootID != 5 || grupos[1].Estado.TotalPages != 9 {
		t.Errorf("estado del segundo grupo = %+v", grupos[1].Estado)
	}
	// El LSN arranca donde lo dejó la generación anterior: no se reinicia al rotar.
	if grupos[0].LSNCommit != 103 || grupos[1].LSNCommit != 107 {
		t.Errorf("lsn de los commits = %d y %d, quiero 103 y 107",
			grupos[0].LSNCommit, grupos[1].LSNCommit)
	}
	// Con todos los grupos completos, el final coincide con el del archivo.
	if fin != d.Tamano(wal.Nombre(3)) {
		t.Errorf("fin = %d, quiero %d", fin, d.Tamano(wal.Nombre(3)))
	}
}

// El paso 4 de la sec. 8: un grupo sin su registro de commit se descarta **entero**. Es
// lo que convierte la promesa "todas o ninguna" en un mecanismo.
func TestElUltimoGrupoSinCommitSeDescartaEntero(t *testing.T) {
	w, d := nuevo(t, 0, 0)

	grupo(t, w, pager.State{RootID: 2, TotalPages: 4}, 2, 3)
	// Segundo grupo: tres imágenes anexadas y la caída antes del commit. El Sync las deja
	// en disco, que es justo el caso interesante -- están ahí, íntegras, y aun así no
	// cuentan.
	w.LogPage(hoja(4))
	w.LogPage(hoja(5))
	w.LogPage(hoja(6))
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	f, _ := d.Open(wal.Nombre(0))
	grupos, fin, err := wal.Leer(f, 0, 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Leer termino con %v, quiero io.EOF", err)
	}
	if len(grupos) != 1 {
		t.Fatalf("grupos = %d, quiero 1: el segundo no tiene commit", len(grupos))
	}
	// El final es el del último grupo completo, no el del último registro válido: un
	// escritor que reanudara más allá daría por buenas las tres imágenes huérfanas.
	if fin >= d.Tamano(wal.Nombre(0)) {
		t.Errorf("fin = %d, quiero por debajo del tamano del log (%d)",
			fin, d.Tamano(wal.Nombre(0)))
	}
}

// La regla de la sec. 8 paso 3: la lectura se detiene en el primer registro que falle y
// descarta todo lo que siga, **aunque más adelante haya registros perfectamente válidos**.
func TestUnCRCMaloCortaAunqueDespuesHayaGruposBuenos(t *testing.T) {
	w, d := nuevo(t, 0, 0)

	grupo(t, w, pager.State{RootID: 2, TotalPages: 4}, 2)
	corte := d.Tamano(wal.Nombre(0))
	grupo(t, w, pager.State{RootID: 3, TotalPages: 5}, 3)
	grupo(t, w, pager.State{RootID: 4, TotalPages: 6}, 4)

	// Un bit en la carga del primer registro del segundo grupo.
	f, _ := d.Open(wal.Nombre(0))
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, corte+20); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	buf[0] ^= 0x01
	if _, err := f.WriteAt(buf, corte+20); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	grupos, _, err := wal.Leer(f, 0, 0)
	if !errors.Is(err, record.ErrCorrupt) {
		t.Fatalf("Leer termino con %v, quiero record.ErrCorrupt", err)
	}
	if len(grupos) != 1 {
		t.Fatalf("grupos = %d, quiero 1: el tercero es valido pero esta detras del corte",
			len(grupos))
	}
}

// Defensa 1 de la sec. 7.4. Se construye a mano porque el WAL nunca produce un salto: lo
// que se prueba es que la lectura lo rechaza si el disco se lo entrega.
func TestUnSaltoDeLSNCortaLaLectura(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("log")
	wr := record.NewWriter(f, 0)

	carga := make([]byte, wal.CargaImagen)
	if err := wal.CodificaImagen(carga, hoja(2)); err != nil {
		t.Fatal(err)
	}
	wr.Append(record.Record{LSN: 1, Type: wal.TipoImagen, Epoch: 0, Payload: carga})
	wr.Append(record.Record{LSN: 2, Type: wal.TipoCommit, Epoch: 0,
		Payload: wal.CodificaCommit(1, pager.State{RootID: 2, TotalPages: 3})})
	// El 3 se salta.
	wr.Append(record.Record{LSN: 4, Type: wal.TipoImagen, Epoch: 0, Payload: carga})

	grupos, _, err := wal.Leer(f, 0, 0)
	if !errors.Is(err, wal.ErrLSNNoContiguo) {
		t.Fatalf("Leer termino con %v, quiero ErrLSNNoContiguo", err)
	}
	if len(grupos) != 1 {
		t.Errorf("grupos = %d, quiero 1", len(grupos))
	}
}

// Defensa 2 de la sec. 7.4. Un registro de otra generación tiene CRC perfectamente
// válido -- es un registro real, de antes de una rotación -- y sin esta comprobación se
// reproduciría encima de datos nuevos.
func TestUnRegistroDeOtraEpocaCortaLaLectura(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("log")
	wr := record.NewWriter(f, 0)

	carga := make([]byte, wal.CargaImagen)
	if err := wal.CodificaImagen(carga, hoja(2)); err != nil {
		t.Fatal(err)
	}
	wr.Append(record.Record{LSN: 1, Type: wal.TipoImagen, Epoch: 7, Payload: carga})
	wr.Append(record.Record{LSN: 2, Type: wal.TipoCommit, Epoch: 7,
		Payload: wal.CodificaCommit(1, pager.State{RootID: 2, TotalPages: 3})})
	wr.Append(record.Record{LSN: 3, Type: wal.TipoImagen, Epoch: 6, Payload: carga})

	grupos, _, err := wal.Leer(f, 7, 0)
	if !errors.Is(err, wal.ErrEpocaAjena) {
		t.Fatalf("Leer termino con %v, quiero ErrEpocaAjena", err)
	}
	if len(grupos) != 1 {
		t.Errorf("grupos = %d, quiero 1", len(grupos))
	}
}

// n_registros es redundante con lo contado, y por eso sirve de comprobación cruzada: un
// commit entero que declara un grupo que no está entero no es una caída, es un log que no
// es lo que dice ser.
func TestCommitQueDeclaraMasImagenesDeLasQueHay(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("log")
	wr := record.NewWriter(f, 0)

	carga := make([]byte, wal.CargaImagen)
	if err := wal.CodificaImagen(carga, hoja(2)); err != nil {
		t.Fatal(err)
	}
	wr.Append(record.Record{LSN: 1, Type: wal.TipoImagen, Epoch: 0, Payload: carga})
	wr.Append(record.Record{LSN: 2, Type: wal.TipoCommit, Epoch: 0,
		Payload: wal.CodificaCommit(4, pager.State{RootID: 2, TotalPages: 3})})

	grupos, _, err := wal.Leer(f, 0, 0)
	if !errors.Is(err, wal.ErrGrupoIncompleto) {
		t.Fatalf("Leer termino con %v, quiero ErrGrupoIncompleto", err)
	}
	if len(grupos) != 0 {
		t.Errorf("grupos = %d, quiero 0", len(grupos))
	}
}

func TestTipoDeRegistroDesconocido(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("log")
	wr := record.NewWriter(f, 0)
	wr.Append(record.Record{LSN: 1, Type: 99, Epoch: 0, Payload: []byte("x")})

	if _, _, err := wal.Leer(f, 0, 0); !errors.Is(err, wal.ErrTipoDesconocido) {
		t.Fatalf("Leer termino con %v, quiero ErrTipoDesconocido", err)
	}
}

// El orden del paso 5 de la sec. 7.4: crear el archivo nuevo y hacer fsync del directorio
// **antes** de borrar el viejo. Al revés, una caída entre el borrado y la creación deja al
// motor sin ninguna de las dos generaciones, y el WAL es la única copia buena de lo
// confirmado que todavía no bajó a datos.db.
func TestRotarCreaAntesDeBorrar(t *testing.T) {
	w, d := nuevo(t, 0, 0)
	grupo(t, w, pager.State{RootID: 2, TotalPages: 4}, 2)
	d.Traza.Limpia()

	if err := w.Rotar(); err != nil {
		t.Fatalf("Rotar: %v", err)
	}

	creado := d.Traza.Indice(wal.Nombre(1) + ":create")
	sync := d.Traza.Indice("dir:sync")
	borrado := d.Traza.Indice(wal.Nombre(0) + ":remove")
	if creado < 0 || sync < 0 || borrado < 0 {
		t.Fatalf("faltan eventos en la traza: %q", d.Traza.Eventos())
	}
	if creado >= sync || sync >= borrado {
		t.Errorf("orden create=%d dir:sync=%d remove=%d, quiero create < dir:sync < remove",
			creado, sync, borrado)
	}
	if !d.Traza.Contiene(wal.Nombre(0) + ":close") {
		t.Error("el log viejo no se cerro antes de borrarlo")
	}

	if d.Existe(wal.Nombre(0)) {
		t.Error("el log viejo sigue en el directorio")
	}
	if !d.Existe(wal.Nombre(1)) {
		t.Error("el log nuevo no esta en el directorio")
	}
}

// El LSN no se reinicia al rotar (defensa 1 de la sec. 7.4), pero la época sí avanza y el
// archivo nuevo empieza vacío.
func TestRotarConservaElLSNYAvanzaLaEpoca(t *testing.T) {
	w, _ := nuevo(t, 0, 0)
	grupo(t, w, pager.State{RootID: 2, TotalPages: 4}, 2, 3)
	antes := w.LSN()

	if err := w.Rotar(); err != nil {
		t.Fatalf("Rotar: %v", err)
	}
	if w.LSN() != antes {
		t.Errorf("LSN = %d tras rotar, quiero %d: no se reinicia", w.LSN(), antes)
	}
	if w.Epoca() != 1 {
		t.Errorf("epoca = %d, quiero 1", w.Epoca())
	}
	if w.Bytes() != 0 {
		t.Errorf("bytes = %d en la generacion nueva, quiero 0", w.Bytes())
	}
}

// Tras rotar, los registros nuevos llevan la época nueva, así que un lector de la vieja
// los rechaza en el primero.
func TestTrasRotarLosRegistrosLlevanLaEpocaNueva(t *testing.T) {
	w, d := nuevo(t, 0, 0)
	grupo(t, w, pager.State{RootID: 2, TotalPages: 4}, 2)
	desde := w.LSN()
	if err := w.Rotar(); err != nil {
		t.Fatalf("Rotar: %v", err)
	}
	grupo(t, w, pager.State{RootID: 3, TotalPages: 5}, 3)

	f, _ := d.Open(wal.Nombre(1))
	grupos, _, err := wal.Leer(f, 1, desde)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("con la epoca 1: %v", err)
	}
	if len(grupos) != 1 {
		t.Fatalf("grupos = %d, quiero 1", len(grupos))
	}

	if _, _, err := wal.Leer(f, 0, desde); !errors.Is(err, wal.ErrEpocaAjena) {
		t.Errorf("leyendo la generacion 1 como si fuera la 0: %v, quiero ErrEpocaAjena", err)
	}
}

// Un log vacío no es un error: es un motor recién abierto que todavía no confirmó nada.
func TestLogVacio(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("log")

	grupos, fin, err := wal.Leer(f, 0, 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Leer = %v, quiero io.EOF", err)
	}
	if len(grupos) != 0 || fin != 0 {
		t.Errorf("grupos = %d, fin = %d, quiero 0 y 0", len(grupos), fin)
	}
}

// EsFinDeLog separa "aquí termina el log" de "el disco no responde". Los dos llegan por el
// mismo camino y significan cosas opuestas: tratar el segundo como el primero descartaría
// datos confirmados y llamaría a eso una recuperación correcta.
func TestEsFinDeLogNoTragaUnFalloDeES(t *testing.T) {
	fin := []error{
		io.EOF, record.ErrCorrupt, record.ErrTruncated, record.ErrTooLarge,
		wal.ErrLSNNoContiguo, wal.ErrEpocaAjena, wal.ErrGrupoIncompleto,
		wal.ErrTipoDesconocido, wal.ErrCargaInvalida, page.ErrCorrupt, page.ErrWrongPage,
	}
	for _, err := range fin {
		if !wal.EsFinDeLog(err) {
			t.Errorf("EsFinDeLog(%v) = false, quiero true", err)
		}
	}
	if wal.EsFinDeLog(errors.New("disco: el dispositivo no responde")) {
		t.Error("EsFinDeLog trago un fallo de E/S como si fuera el final del log")
	}
}
