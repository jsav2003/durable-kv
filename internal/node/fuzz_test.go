package node

import (
	"bytes"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

// arma monta una página con el cuerpo, el tipo, el número de celdas y la frontera del área
// de celdas que le den, ajustando el cuerpo a BodySize.
//
// El campo libre no se fuzzea: se deriva de nceldas, que es lo que D5 dice que es siempre
// (docs/DEUDA-DISENO.md). Un libre arbitrario haría que casi toda entrada muriera en la
// primera comprobación de Check, y el fuzzer se pasaría la corrida sin llegar nunca a la
// parte que interesa -- los slots y las celdas. Esa rama la cubre por enumeración
// TestCheckDetectaCorrupcion, que es donde se puede comprobar de verdad.
func arma(body []byte, tipo uint8, nceldas, freeEnd uint16) Node {
	p := page.New(1, page.Type(tipo))
	if len(body) > page.BodySize {
		body = body[:page.BodySize]
	}
	copy(p.Body, body)
	p.NCells = nceldas
	p.FreeEnd = freeEnd
	p.Free = uint16(page.HeaderSize + SlotSize*int(nceldas))
	return Of(p)
}

// FuzzNode somete el intérprete del cuerpo de la página a bytes arbitrarios. Es la mitad
// del formato que FuzzDecode no cubre: la F1 dejó el cuerpo opaco a propósito, así que un
// CRC en verde no dice nada de si los slots apuntan a algún sitio razonable.
//
// La propiedad es la que la sec. 9.4 del DESIGN.md exige, en su forma útil para esta capa:
// Check nunca entra en pánico y, cuando devuelve nil, el nodo entero se puede recorrer con
// Key, Value, Child y Search sin desbordar nada. Eso es lo que permite que Validate() y el
// descenso del árbol se apoyen en Check y no vuelvan a comprobar cada longitud.
//
// Se comprueba además que compactar conserve el contenido. Sin esa segunda propiedad, un
// Check demasiado estricto satisfaría la primera devolviendo error casi siempre, y el
// verde no significaría nada -- el mismo argumento por el que FuzzDecode exige la ida y
// vuelta y no solo la ausencia de pánico.
//
// Cómo correrlo:
//
//	go test ./internal/node -run=XXX -fuzz=FuzzNode -fuzztime=60s -fuzzminimizetime=2s
//
// La cota de minimización, por lo mismo que en internal/page: en cuanto una semilla cruza
// Check, minimizarla es muy caro y casi nunca conserva la cobertura. Ver BUGS.md, F1.
func FuzzNode(f *testing.F) {
	// Semillas: nodos válidos de los dos tipos, para que el fuzzer parta de estructura
	// real, y unos pocos casos degenerados.
	for _, esHoja := range []bool{true, false} {
		var n Node
		if esHoja {
			n = Of(page.New(1, page.TypeLeaf))
		} else {
			n = Of(page.New(1, page.TypeInternal))
		}
		for i := 0; i < 12; i++ {
			k := []byte{byte('a' + i)}
			if esHoja {
				_ = n.InsertHoja(i, k, bytes.Repeat([]byte{byte(i)}, 30))
			} else {
				_ = n.InsertInterno(i, k, uint64(i+100))
			}
		}
		p := n.Page()
		f.Add(p.Body, uint8(p.Type), p.NCells, p.FreeEnd)
	}
	f.Add(make([]byte, page.BodySize), uint8(page.TypeLeaf), uint16(0), uint16(page.Size))
	f.Add(make([]byte, page.BodySize), uint8(page.TypeInternal), uint16(1), uint16(page.Size))
	f.Add([]byte{}, uint8(page.TypeMeta), uint16(0), uint16(0))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, uint8(page.TypeLeaf), uint16(2), uint16(40))

	f.Fuzz(func(t *testing.T, body []byte, tipo uint8, nceldas, freeEnd uint16) {
		n := arma(body, tipo, nceldas, freeEnd)
		if err := n.Check(); err != nil {
			return
		}

		// Check dijo que sí: a partir de aquí nada puede desbordar.
		claves := make([][]byte, n.NCells())
		valores := make([][]byte, n.NCells())
		for i := 0; i < n.NCells(); i++ {
			claves[i] = bytes.Clone(n.Key(i))
			if n.IsLeaf() {
				v, err := n.Value(i)
				if err != nil {
					t.Fatalf("Value(%d) fallo en un nodo que paso Check: %v", i, err)
				}
				valores[i] = bytes.Clone(v)
			} else if _, err := n.Child(i); err != nil {
				t.Fatalf("Child(%d) fallo en un nodo que paso Check: %v", i, err)
			}
			if sz := n.CellSize(i); sz < n.cellOverhead() {
				t.Fatalf("CellSize(%d) = %d, por debajo de la cabecera de celda", i, sz)
			}
		}

		// El directorio y la búsqueda binaria tienen que contar lo mismo.
		for i, k := range claves {
			j, encontrada := n.Search(k)
			if !encontrada || j != i {
				t.Fatalf("Search de la clave %d devolvio (%d, %v)", i, j, encontrada)
			}
		}

		if libre := n.FreeTotal(); libre < 0 || libre > page.BodySize {
			t.Fatalf("FreeTotal = %d, fuera de [0, %d]", libre, page.BodySize)
		}

		// Compactar conserva el contenido y no deja bytes muertos.
		n.Compactar()
		if err := n.Check(); err != nil {
			t.Fatalf("Check tras compactar: %v", err)
		}
		if n.FreeContiguo() != n.FreeTotal() {
			t.Fatalf("tras compactar quedan bytes muertos: contiguo=%d, total=%d",
				n.FreeContiguo(), n.FreeTotal())
		}
		for i := range claves {
			if !bytes.Equal(n.Key(i), claves[i]) {
				t.Fatalf("compactar cambio la clave %d", i)
			}
			if !n.IsLeaf() {
				continue
			}
			v, err := n.Value(i)
			if err != nil {
				t.Fatalf("Value(%d) tras compactar: %v", i, err)
			}
			if !bytes.Equal(v, valores[i]) {
				t.Fatalf("compactar cambio el valor %d", i)
			}
		}
	})
}
