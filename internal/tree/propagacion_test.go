package tree

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/node"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

// Este archivo prueba la parte de la propagación que las inserciones normales casi nunca
// alcanzan: qué pasa cuando el padre también se divide y el hijo que se partió estaba
// justo en el punto del corte.
//
// La cobertura de la suite de put_test.go dejaba la rama p == m de insertaEnPadre sin
// ejecutar ni una vez. Es la más delicada de las tres: el hijo izquierdo de la celda que
// Split promociona deja de tener celda propia y pasa a ser el enlace derecho de la mitad
// izquierda, así que la separadora nueva ya no se coloca en una posición del directorio
// sino en el caso del enlace. Colocarla como si siguiera teniendo celda perdería un
// subárbol entero, y el árbol resultante seguiría descendiendo bien para casi todas las
// claves.

// claveLarga es una clave del tamaño máximo. Se usa para que un nodo interno se llene con
// unos pocos hijos: con separadoras cortas harían falta más de doscientas, y montar
// doscientas hojas reales por cada posición que se prueba no cabe en un test.
func claveLarga(i, j int) []byte {
	k := make([]byte, node.MaxKeySize)
	for x := range k {
		k[x] = 'a'
	}
	copy(k, fmt.Sprintf("k%03d%03d", i, j))
	return k
}

const hojasPorNodo = 4 // entradas por hoja, para que toda hoja se pueda dividir

// padreLleno arma un árbol de dos niveles cuya raíz es un nodo interno que ya no admite ni
// una separadora más, con una hoja real debajo de cada hijo. Devuelve el árbol y las hojas
// en orden, incluida la del enlace derecho.
func padreLleno(t *testing.T) (*constructor, *Tree, uint64, []uint64) {
	t.Helper()
	c := nuevoConstructor(t)

	nuevaHoja := func(i int) uint64 {
		pares := make([]par, 0, hojasPorNodo)
		for j := 0; j < hojasPorNodo; j++ {
			pares = append(pares, par{string(claveLarga(i, j)), fmt.Sprintf("v%03d%03d", i, j)})
		}
		return c.hoja(pares...)
	}

	padre := c.nueva(page.TypeInternal)
	var hojas []uint64
	for i := 0; ; i++ {
		h := nuevaHoja(i)
		hojas = append(hojas, h)
		// La separadora a la derecha de la hoja i es la primera clave de la hoja i+1.
		err := padre.InsertInterno(padre.NCells(), claveLarga(i+1, 0), h)
		if errors.Is(err, node.ErrNoSpace) {
			// No cabe una celda más: esta hoja se queda como hijo derecho, que es lo que la
			// deja sin separadora propia, y el padre está lleno.
			padre.SetLink(h)
			break
		}
		if err != nil {
			t.Fatalf("InsertInterno(%d): %v", i, err)
		}
	}

	c.encadena(hojas...)
	arbol := New(c.pg, padre.Page().ID)
	if err := arbol.Validate(); err != nil {
		t.Fatalf("el padre lleno no es un arbol valido: %v", err)
	}
	return c, arbol, padre.Page().ID, hojas
}

// TestPropagacionEnCadaPosicionDelPadre parte una hoja en cada una de las posiciones que
// puede ocupar bajo un padre lleno, incluida la del enlace derecho, y exige que el árbol
// resultante siga cumpliendo los seis invariantes y conservando todas sus claves.
//
// Recorrer todas las posiciones es lo que garantiza pasar por las tres ramas del reparto
// -- la mitad izquierda, la celda promocionada y la mitad derecha -- sin tener que saber de
// antemano por dónde va a cortar puntoDeCorte.
func TestPropagacionEnCadaPosicionDelPadre(t *testing.T) {
	_, _, _, hojas := padreLleno(t)
	posiciones := len(hojas)
	if posiciones < 4 {
		t.Fatalf("el padre solo admitio %d hijos: el test no separa las tres ramas", posiciones)
	}

	for p := 0; p < posiciones; p++ {
		t.Run(fmt.Sprintf("la hoja %d de %d se divide", p, posiciones-1), func(t *testing.T) {
			c, arbol, padreID, hojas := padreLleno(t)

			antes := clavesDelArbol(t, arbol)

			// La hoja p se divide, exactamente como lo haría insertaEnHoja.
			izqID := hojas[p]
			izq := c.nodoDe(izqID)
			pn, err := c.pg.Alloc(page.TypeLeaf)
			if err != nil {
				t.Fatalf("Alloc: %v", err)
			}
			nueva := node.Of(pn)
			sep, err := node.Split(izq, nueva)
			if err != nil {
				t.Fatalf("Split: %v", err)
			}
			nueva.SetLink(izq.Link())
			izq.SetLink(pn.ID)

			// Y la separadora sube al padre, que está lleno y tiene que partirse.
			padre := c.nodoDe(padreID)
			sube, nuevoPadre, err := arbol.insertaEnPadre(padre, p, sep, izqID, pn.ID)
			if err != nil {
				t.Fatalf("insertaEnPadre en la posicion %d: %v", p, err)
			}
			if sube == nil {
				t.Fatalf("el padre estaba lleno y aun asi absorbio la separadora")
			}
			if err := arbol.creceRaiz(padreID, sube, nuevoPadre); err != nil {
				t.Fatalf("creceRaiz: %v", err)
			}

			if err := arbol.Validate(); err != nil {
				t.Fatalf("el arbol no valida tras dividir en la posicion %d: %v", p, err)
			}
			if despues := clavesDelArbol(t, arbol); !mismasClaves(antes, despues) {
				t.Fatalf("la posicion %d perdio o duplico claves: habia %d y quedan %d",
					p, len(antes), len(despues))
			}
			// Y cada clave se sigue alcanzando por el descenso, no solo por la cadena.
			for _, k := range antes {
				if _, err := arbol.Get([]byte(k)); err != nil {
					t.Fatalf("Get(%q) tras dividir en la posicion %d: %v", k[:7], p, err)
				}
			}
		})
	}
}

// clavesDelArbol recorre el árbol entero por la cadena lateral.
func clavesDelArbol(t *testing.T, arbol *Tree) []string {
	t.Helper()
	var ks []string
	if err := arbol.Scan(nil, nil, func(k, _ []byte) bool {
		ks = append(ks, string(k))
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return ks
}

func mismasClaves(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
