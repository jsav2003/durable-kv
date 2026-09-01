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

// El truncado hacia arriba rellena con ceros y hacia abajo recorta: es lo que hace un
// sistema de archivos real, y la recuperación se apoya en ello al extender datos.db.
func TestTruncateEnAmbasDirecciones(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("f")
	a := f.(*fsxtest.Archivo)

	f.WriteAt([]byte("abcdef"), 0)
	f.Truncate(3)
	if a.Tamano() != 3 {
		t.Fatalf("tamano = %d, quiero 3", a.Tamano())
	}
	f.Truncate(5)
	if got := d.Bytes("f"); string(got) != "abc\x00\x00" {
		t.Errorf("contenido = %q, quiero \"abc\x00\x00\"", got)
	}
}
