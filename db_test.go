package durakv_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	durakv "github.com/jsav2003/durable-kv"
)

func abre(t *testing.T, ruta string) *durakv.DB {
	t.Helper()
	db, err := durakv.Open(ruta)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func clave(i int) []byte { return fmt.Appendf(nil, "clave-%06d", i) }
func valor(i int) []byte { return fmt.Appendf(nil, "valor-%06d", i) }

func TestPutGetSobreDiscoReal(t *testing.T) {
	db := abre(t, filepath.Join(t.TempDir(), "base"))

	for i := range 500 {
		if err := db.Put(clave(i), valor(i)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	for i := range 500 {
		got, err := db.Get(clave(i))
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if !bytes.Equal(got, valor(i)) {
			t.Fatalf("Get(%d) = %q, quiero %q", i, got, valor(i))
		}
	}
	if _, err := db.Get([]byte("no-esta")); !errors.Is(err, durakv.ErrNotFound) {
		t.Errorf("Get de una clave ausente = %v, quiero ErrNotFound", err)
	}
	if err := db.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Open crea el directorio si no existe: ahí una ruta que no está es "todavía no hay base",
// no una equivocación.
func TestOpenCreaElDirectorio(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "a", "b", "base")
	db := abre(t, ruta)

	if err := db.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ruta, "datos.db")); err != nil {
		t.Errorf("no se creo datos.db: %v", err)
	}
}

// El ciclo completo sobre archivos de verdad: escribir, cerrar, reabrir.
func TestCerrarYReabrir(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "base")

	db := abre(t, ruta)
	for i := range 800 {
		if err := db.Put(clave(i), valor(i)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db2 := abre(t, ruta)
	for i := range 800 {
		got, err := db2.Get(clave(i))
		if err != nil {
			t.Fatalf("tras reabrir, Get(%d): %v", i, err)
		}
		if !bytes.Equal(got, valor(i)) {
			t.Fatalf("tras reabrir, Get(%d) = %q, quiero %q", i, got, valor(i))
		}
	}
	if err := db2.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestScanEnOrdenYPorRango(t *testing.T) {
	db := abre(t, filepath.Join(t.TempDir(), "base"))
	for i := range 300 {
		if err := db.Put(clave(i), valor(i)); err != nil {
			t.Fatal(err)
		}
	}

	var todas [][]byte
	if err := db.Scan(nil, nil, func(k, _ []byte) bool {
		todas = append(todas, bytes.Clone(k))
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(todas) != 300 {
		t.Fatalf("Scan completo dio %d claves, quiero 300", len(todas))
	}
	if !slices.IsSortedFunc(todas, bytes.Compare) {
		t.Error("Scan no devolvio las claves en orden")
	}

	var rango []string
	if err := db.Scan(clave(10), clave(15), func(k, _ []byte) bool {
		rango = append(rango, string(k))
		return true
	}); err != nil {
		t.Fatalf("Scan de rango: %v", err)
	}
	quiero := []string{
		string(clave(10)), string(clave(11)), string(clave(12)),
		string(clave(13)), string(clave(14)),
	}
	if !slices.Equal(rango, quiero) {
		t.Errorf("Scan [10,15) = %q, quiero %q", rango, quiero)
	}

	// Parar no es un fallo.
	n := 0
	if err := db.Scan(nil, nil, func(_, _ []byte) bool { n++; return n < 3 }); err != nil {
		t.Errorf("Scan interrumpido devolvio %v, quiero nil", err)
	}
	if n != 3 {
		t.Errorf("fn se llamo %d veces, quiero 3", n)
	}
}

// D6 cerrado. El valor que Get devuelve es del llamador: nada de lo que el motor haga
// después puede cambiárselo bajo los pies. Si pudiera, el motor devolvería datos que nadie
// escribió, con la página coherente y el CRC correcto -- un error que ninguna comprobación
// de integridad puede ver, porque está en el pasado del llamador.
//
// La forma de provocarlo no es cualquier escritura: los accesores del nodo devuelven
// subsectores del cuerpo de la página, y una inserción en otra hoja, o incluso en esta si
// hay espacio contiguo, no toca los bytes de una celda ya escrita. Lo que sí los mueve es la
// **compactación**, que reordena todas las celdas hacia el final del cuerpo y pone a cero lo
// que queda libre. Se fuerza sustituyendo la misma clave una y otra vez: cada sustitución es
// un borrado más una inserción (internal/tree, insertaEnHoja), así que deja una celda muerta
// y consume espacio contiguo hasta que la siguiente inserción tiene que compactar.
func TestGetDevuelveUnaCopia(t *testing.T) {
	db := abre(t, filepath.Join(t.TempDir(), "base"))

	relleno := func(b byte) []byte { return bytes.Repeat([]byte{b}, 700) }

	// Dos claves con valores grandes: caben en una hoja y dejan poco espacio contiguo.
	if err := db.Put(clave(0), relleno('a')); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(clave(1), relleno('b')); err != nil {
		t.Fatal(err)
	}

	got, err := db.Get(clave(1))
	if err != nil {
		t.Fatal(err)
	}
	antes := bytes.Clone(got)

	// Sustituciones repetidas de la otra clave de la misma hoja, hasta forzar compactaciones.
	for r := range 40 {
		if err := db.Put(clave(0), relleno(byte('a'+r%20))); err != nil {
			t.Fatal(err)
		}
	}

	if !bytes.Equal(got, antes) {
		t.Errorf("el valor devuelto por Get cambio bajo los pies del llamador: %q..., era %q...", got[:16], antes[:16])
	}
	// Y el valor sigue siendo el correcto al releerlo.
	releido, err := db.Get(clave(1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(releido, antes) {
		t.Errorf("el valor almacenado cambio: %q..., quiero %q...", releido[:16], antes[:16])
	}
}

func TestLimitesDeTamano(t *testing.T) {
	db := abre(t, filepath.Join(t.TempDir(), "base"))

	if err := db.Put(bytes.Repeat([]byte{'k'}, 513), []byte("v")); !errors.Is(err, durakv.ErrKeyTooLarge) {
		t.Errorf("clave de 513 bytes = %v, quiero ErrKeyTooLarge", err)
	}
	if err := db.Put([]byte("k"), bytes.Repeat([]byte{'v'}, 1000)); !errors.Is(err, durakv.ErrEntryTooLarge) {
		t.Errorf("entrada de mas de 1000 bytes = %v, quiero ErrEntryTooLarge", err)
	}
	// Y que el rechazo no envenene la base: la siguiente operación funciona.
	if err := db.Put([]byte("k"), []byte("v")); err != nil {
		t.Errorf("Put tras un rechazo por tamano: %v", err)
	}
}

// Cerrar dos veces no es un error, y usar una base cerrada sí.
func TestOperarSobreUnaBaseCerrada(t *testing.T) {
	db, err := durakv.Open(filepath.Join(t.TempDir(), "base"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("segundo Close = %v, quiero nil", err)
	}

	if err := db.Put([]byte("k"), []byte("v")); !errors.Is(err, durakv.ErrCerrada) {
		t.Errorf("Put tras Close = %v, quiero ErrCerrada", err)
	}
	if _, err := db.Get([]byte("k")); !errors.Is(err, durakv.ErrCerrada) {
		t.Errorf("Get tras Close = %v, quiero ErrCerrada", err)
	}
	if err := db.Scan(nil, nil, func(_, _ []byte) bool { return true }); !errors.Is(err, durakv.ErrCerrada) {
		t.Errorf("Scan tras Close = %v, quiero ErrCerrada", err)
	}
}

// Abrir sin cerrar y volver a abrir: el camino de recuperación sobre archivos reales, sin
// disco falso de por medio.
func TestReaperturaSinCerrar(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "base")

	db, err := durakv.Open(ruta)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		if err := db.Put(clave(i), valor(i)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	// Sin Close: se abandona la base como la dejaría un kill -9.

	db2 := abre(t, ruta)
	for i := range 400 {
		got, err := db2.Get(clave(i))
		if err != nil {
			t.Fatalf("Get(%d) tras reabrir sin cerrar: %v", i, err)
		}
		if !bytes.Equal(got, valor(i)) {
			t.Fatalf("Get(%d) = %q, quiero %q", i, got, valor(i))
		}
	}
	if err := db2.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// El checkpoint automático por umbral de bytes de WAL: con suficientes escrituras tiene que
// dispararse solo, y el directorio no puede acumular generaciones.
func TestElCheckpointSeDisparaSoloYNoAcumulaLogs(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "base")
	db := abre(t, ruta)

	// El umbral por defecto son 4 MiB y cada imagen de página son ~4 KiB, así que unos
	// pocos miles de Put lo cruzan varias veces.
	for i := range 4000 {
		if err := db.Put(clave(i), valor(i)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}

	entradas, err := os.ReadDir(ruta)
	if err != nil {
		t.Fatal(err)
	}
	logs := 0
	for _, e := range entradas {
		if len(e.Name()) > 10 && e.Name()[:10] == "datos.wal." {
			logs++
		}
	}
	if logs != 1 {
		t.Errorf("archivos de log en el directorio = %d, quiero 1", logs)
	}
	if err := db.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
