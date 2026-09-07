package fsxtest_test

import (
	"slices"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
)

// Lo que hace útil al doble es la traza compartida: los eventos de dos archivos distintos
// caen en la misma secuencia, y por eso se puede afirmar que una escritura al WAL ocurrió
// antes que una a datos.db. Un doble con una traza por archivo no podría demostrarlo.
func TestLaTrazaEsCompartida(t *testing.T) {
	d := fsxtest.Nuevo()

	wal, _ := d.Open("datos.wal.0")
	datos, _ := d.Open("datos.db")

	wal.WriteAt([]byte("r"), 0)
	wal.Sync()
	datos.WriteAt([]byte("p"), 0)

	quiero := []string{
		"datos.wal.0:create",
		"datos.db:create",
		"datos.wal.0:write 0+1",
		"datos.wal.0:sync",
		"datos.db:write 0+1",
	}
	if got := d.Traza.Eventos(); !slices.Equal(got, quiero) {
		t.Fatalf("traza = %q,\nquiero  %q", got, quiero)
	}

	if i, j := d.Traza.Indice("datos.wal.0:sync"), d.Traza.Indice("datos.db:write 0+1"); i >= j {
		t.Errorf("el sync del wal (%d) no precede a la escritura de datos (%d)", i, j)
	}
}

func TestOpenDevuelveElMismoArchivo(t *testing.T) {
	d := fsxtest.Nuevo()

	a, _ := d.Open("f")
	a.WriteAt([]byte("hola"), 0)

	b, _ := d.Open("f")
	buf := make([]byte, 4)
	if _, err := b.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "hola" {
		t.Errorf("contenido = %q, quiero \"hola\"", buf)
	}
}

func TestRemoveYExiste(t *testing.T) {
	d := fsxtest.Nuevo()

	d.Open("datos.wal.0")
	d.Open("datos.wal.1")
	if got := d.Nombres(); !slices.Equal(got, []string{"datos.wal.0", "datos.wal.1"}) {
		t.Fatalf("nombres = %q", got)
	}

	if err := d.Remove("datos.wal.0"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if d.Existe("datos.wal.0") {
		t.Error("datos.wal.0 sigue existiendo tras Remove")
	}
	if err := d.Remove("datos.wal.0"); err == nil {
		t.Error("Remove de un archivo inexistente no dio error")
	}
}

// Con Volatil en true una escritura sin Sync no llega al contenido duradero -- lo que
// sobreviviría a una caída -- pero sí la ve una lectura del mismo proceso, igual que el
// caché del sistema operativo.
func TestVolatilLaEscrituraSinSyncNoEsDuradera(t *testing.T) {
	d := fsxtest.Nuevo()
	d.Volatil = true
	f, _ := d.Open("f")

	f.WriteAt([]byte("hola"), 0)

	if got := d.Bytes("f"); got != nil {
		t.Errorf("contenido duradero = %q, quiero nil antes del Sync", got)
	}
	if d.Pendientes() != 1 {
		t.Errorf("pendientes = %d, quiero 1", d.Pendientes())
	}

	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "hola" {
		t.Errorf("lectura del proceso = %q, quiero \"hola\"", buf)
	}

	f.Sync()
	if got := d.Bytes("f"); string(got) != "hola" {
		t.Errorf("contenido duradero tras Sync = %q, quiero \"hola\"", got)
	}
	if d.Pendientes() != 0 {
		t.Errorf("pendientes tras Sync = %d, quiero 0", d.Pendientes())
	}
}

// La cola de pendientes es una sola para todos los archivos, pero Sync de uno vacía solo
// las suyas: un fsync a datos.db no hace duraderas las escrituras al WAL. Es el árbitro de
// orden global de la sec. 9.1 -- sin él, el orden relativo entre los dos archivos, que es
// el write-ahead logging, no se modela.
func TestVolatilSyncDeUnArchivoNoVuelcaElOtro(t *testing.T) {
	d := fsxtest.Nuevo()
	d.Volatil = true
	wal, _ := d.Open("datos.wal.0")
	datos, _ := d.Open("datos.db")

	wal.WriteAt([]byte("registro"), 0)
	datos.WriteAt([]byte("pagina"), 0)
	if d.NEscrituras() != 2 {
		t.Errorf("NEscrituras = %d, quiero 2", d.NEscrituras())
	}

	datos.Sync()

	if got := d.Bytes("datos.db"); string(got) != "pagina" {
		t.Errorf("datos.db duradero = %q, quiero \"pagina\"", got)
	}
	if got := d.Bytes("datos.wal.0"); got != nil {
		t.Errorf("wal duradero = %q, quiero nil: su Sync no ha ocurrido", got)
	}
	if d.Pendientes() != 1 {
		t.Errorf("pendientes = %d, quiero 1 (la del wal)", d.Pendientes())
	}

	wal.Sync()
	if got := d.Bytes("datos.wal.0"); string(got) != "registro" {
		t.Errorf("wal duradero tras su Sync = %q, quiero \"registro\"", got)
	}
}

// El truncado hacia arriba rellena con ceros y hacia abajo recorta: es lo que hace un
// sistema de archivos real, y la recuperación se apoya en ello al extender datos.db.
func TestTruncateEnAmbasDirecciones(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("f")
	a := f.(*fsxtest.Archivo)

	f.WriteAt([]byte("abcdef"), 0)
	f.Truncate(3)
	if n, _ := a.Size(); n != 3 {
		t.Fatalf("tamano = %d, quiero 3", n)
	}
	f.Truncate(5)
	if got := d.Bytes("f"); string(got) != "abc\x00\x00" {
		t.Errorf("contenido = %q, quiero \"abc\x00\x00\"", got)
	}
}
