package wal_test

import (
	"testing"

	"github.com/jsav2003/durable-kv/internal/fsx/fsxtest"
	"github.com/jsav2003/durable-kv/internal/pager"
	"github.com/jsav2003/durable-kv/internal/wal"
)

// FuzzLeer alimenta el lector de grupos con bytes arbitrarios. La sec. 9.4 lo pide
// explícitamente para "el lector de registros del WAL", y el requisito es el mismo que
// para el deserializador de páginas: **nunca debe provocar un pánico**, solo devolver un
// error de corrupción.
//
// Vale la pena decir por qué no basta con FuzzRecord, que ya existe un nivel más abajo:
// aquel ejercita el marco -- longitud, CRC, delimitación -- y este ejercita lo que se
// hace con una carga que el marco ya dio por buena. Un registro con CRC correcto puede
// llevar dentro una página con un tipo imposible, un page_id que no es el suyo, o una
// longitud que no corresponde a su tipo, y esos caminos solo se alcanzan desde aquí.
//
// El corpus se siembra con logs bien formados además de con basura: un fuzzer que
// arrancara solo de bytes aleatorios casi nunca produciría una cabecera de registro
// válida, así que nunca llegaría a la parte que este fuzzer existe para probar.
func FuzzLeer(f *testing.F) {
	f.Add(logDeEjemplo(2))
	f.Add(logDeEjemplo(2)[:37])
	f.Add(sinCommit())
	f.Add([]byte{})
	f.Add(make([]byte, 17))

	f.Fuzz(func(t *testing.T, datos []byte) {
		d := fsxtest.Nuevo()
		archivo, err := d.Open("log")
		if err != nil {
			t.Fatal(err)
		}
		if len(datos) > 0 {
			if _, err := archivo.WriteAt(datos, 0); err != nil {
				t.Fatal(err)
			}
		}

		// El contrato: Leer siempre termina y siempre dice por qué. Nunca entra en pánico
		// y nunca devuelve nil como motivo.
		grupos, fin, motivo := wal.Leer(archivo, 0, 0)
		if motivo == nil {
			t.Fatal("Leer devolvio motivo nil: siempre hay una razon por la que termino")
		}
		if fin < 0 || fin > int64(len(datos)) {
			t.Fatalf("fin = %d fuera de [0, %d]", fin, len(datos))
		}
		// Todo grupo devuelto está completo por construcción: si Leer entrega uno a
		// medias, la recuperación aplicaría medio grupo y eso es el hallazgo H1.
		for i, g := range grupos {
			if g.LSNCommit == 0 {
				t.Fatalf("grupo %d sin lsn de commit", i)
			}
			for j, p := range g.Imagenes {
				if p == nil {
					t.Fatalf("grupo %d, imagen %d: pagina nil", i, j)
				}
			}
		}
	})
}

// logDeEjemplo devuelve los bytes de un log con un grupo completo de n imágenes.
func logDeEjemplo(n int) []byte {
	d := fsxtest.Nuevo()
	w, err := wal.Abrir(d, 0, 0, 0)
	if err != nil {
		panic(err)
	}
	for i := range n {
		if _, err := w.LogPage(hoja(uint64(i + 2))); err != nil {
			panic(err)
		}
	}
	if err := w.Commit(pager.State{RootID: 2, TotalPages: uint64(n + 2)}); err != nil {
		panic(err)
	}
	w.Close()
	return d.Bytes(wal.Nombre(0))
}

// sinCommit devuelve un log con imágenes anexadas y ningún commit: el grupo que la
// recuperación descarta entero.
func sinCommit() []byte {
	d := fsxtest.Nuevo()
	w, err := wal.Abrir(d, 0, 0, 0)
	if err != nil {
		panic(err)
	}
	if _, err := w.LogPage(hoja(2)); err != nil {
		panic(err)
	}
	if err := w.Sync(); err != nil {
		panic(err)
	}
	w.Close()
	return d.Bytes(wal.Nombre(0))
}
