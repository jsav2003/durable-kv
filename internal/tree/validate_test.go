package tree

import (
	"errors"
	"strings"
	"testing"

	"github.com/jsav2003/durable-kv/internal/page"
	"github.com/jsav2003/durable-kv/internal/pager"
)

func TestValidateArbolCanonico(t *testing.T) {
	k := arbolCanonico(t)
	if err := k.arbol.Validate(); err != nil {
		t.Fatalf("el arbol canonico deberia ser valido: %v", err)
	}
}

// TestRaizVacia: un árbol recién creado es una hoja sin celdas, y eso es válido. Es la única
// excepción del invariante 3, y la que hará falta en cuanto Put exista.
func TestRaizVacia(t *testing.T) {
	c := nuevoConstructor(t)
	raiz := c.hoja()
	arbol := New(c.pg, raiz)

	if err := arbol.Validate(); err != nil {
		t.Fatalf("una raiz vacia deberia ser valida: %v", err)
	}
	if _, err := arbol.Get([]byte("k010")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get sobre un arbol vacio: se esperaba ErrNotFound y se obtuvo %v", err)
	}
	if visto := recoge(t, arbol, nil, nil); len(visto) != 0 {
		t.Errorf("Scan sobre un arbol vacio dio %v", visto)
	}
}

// TestValidateTrasBajarADisco hace el viaje completo: se cierra el grupo, las páginas bajan
// a datos.db y se vuelven a leer con un pager nuevo y un caché vacío. Sin esto, todos los
// demás tests validarían objetos Page que nunca pasaron por el CRC ni por page.Decode.
func TestValidateTrasBajarADisco(t *testing.T) {
	k := arbolCanonico(t)
	k.c.termina(k.raiz)
	if err := k.c.pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}

	pg2, err := pager.New(k.c.f, &pager.NopLog{}, k.c.pg.TotalPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	relectura := New(pg2, k.raiz)
	if err := relectura.Validate(); err != nil {
		t.Fatalf("el arbol releido del archivo no valida: %v", err)
	}
	for _, clave := range clavesCanonicas {
		v, err := relectura.Get([]byte(clave))
		if err != nil {
			t.Fatalf("Get(%q) tras releer: %v", clave, err)
		}
		if string(v) != valorDe(clave) {
			t.Errorf("Get(%q) tras releer = %q, se esperaba %q", clave, v, valorDe(clave))
		}
	}
}

// TestReconstruirLibres es la sec. 6.1: el conjunto de libres no se persiste, se deduce del
// complemento de lo alcanzable.
func TestReconstruirLibres(t *testing.T) {
	k := arbolCanonico(t)

	// Dos páginas asignadas y nunca colgadas del árbol. Es lo que dejaría una caída entre
	// el Alloc y el commit que iba a usarlas.
	sueltas := []uint64{
		k.c.nueva(page.TypeLeaf).Page().ID,
		k.c.nueva(page.TypeInternal).Page().ID,
	}

	// Antes de reconstruir, esas dos no están en ninguno de los dos conjuntos y el
	// invariante 6 no se cumple.
	if err := k.arbol.Validate(); err == nil {
		t.Fatal("con dos paginas huerfanas, Validate deberia fallar por el invariante 6")
	}

	if err := k.arbol.ReconstruirLibres(); err != nil {
		t.Fatalf("ReconstruirLibres: %v", err)
	}
	libres := k.c.pg.FreePages()
	if len(libres) != len(sueltas) {
		t.Fatalf("el conjunto de libres es %v, se esperaba %v", libres, sueltas)
	}
	for i, id := range sueltas {
		if libres[i] != id {
			t.Errorf("libre %d es la pagina %d, se esperaba la %d", i, libres[i], id)
		}
	}
	if err := k.arbol.Validate(); err != nil {
		t.Fatalf("tras reconstruir, Validate deberia pasar: %v", err)
	}
}

// TestValidateDetectaCadaRotura es el test que da valor a Validate.
//
// Cada caso parte del árbol canónico -- que TestValidateArbolCanonico deja probado en verde
// -- rompe exactamente una cosa, y exige que el error nombre esa cosa. Que el fragmento
// esperado sea distinto en cada fila es la mitad importante: un Validate que devolviera
// siempre el mismo error genérico pasaría un test que solo comprobara "devuelve error", y
// desde fuera se vería igual que uno que funciona.
func TestValidateDetectaCadaRotura(t *testing.T) {
	casos := []struct {
		nombre string
		rompe  func(*canonico)
		quiere string
	}{
		{
			// Invariante 1. InsertHoja acepta el índice que le den: colocar las dos claves
			// al revés deja el directorio desordenado sin que nada se queje en el momento.
			nombre: "inv1 claves desordenadas dentro de una hoja",
			rompe: func(k *canonico) {
				n := k.c.nodoDe(k.l1)
				for n.NCells() > 0 {
					if err := n.Delete(0); err != nil {
						k.c.t.Fatalf("Delete: %v", err)
					}
				}
				if err := n.InsertHoja(0, []byte("k020"), []byte("x")); err != nil {
					k.c.t.Fatalf("InsertHoja: %v", err)
				}
				if err := n.InsertHoja(1, []byte("k010"), []byte("x")); err != nil {
					k.c.t.Fatalf("InsertHoja: %v", err)
				}
			},
			quiere: "no estan estrictamente ordenadas",
		},
		{
			// Invariante 2. La raíz cuelga una hoja donde antes colgaba un nivel entero.
			nombre: "inv2 una hoja a distinta profundidad",
			rompe:  func(k *canonico) { k.c.nodoDe(k.raiz).SetLink(k.l5) },
			quiere: "cuelga a profundidad",
		},
		{
			// Invariante 3. Una hoja sin celdas no aporta claves y no se puede descender por
			// ella; solo la raíz puede estar vacía.
			nombre: "inv3 hoja alcanzable sin celdas",
			rompe: func(k *canonico) {
				k.c.nodoDe(k.b).SetLink(k.c.hoja())
			},
			quiere: "no tiene celdas",
		},
		{
			// Invariante 4. La clave se cuela en la hoja equivocada: L1 está acotada por la
			// separadora k030 y k999 la desborda. node.Check no lo ve -- dentro de la hoja
			// las tres claves siguen ordenadas.
			nombre: "inv4 clave fuera de la cota de su hoja",
			rompe: func(k *canonico) {
				n := k.c.nodoDe(k.l1)
				if err := n.InsertHoja(n.NCells(), []byte("k999"), []byte("x")); err != nil {
					k.c.t.Fatalf("InsertHoja: %v", err)
				}
			},
			quiere: "por encima de su cota",
		},
		{
			// Invariante 5. La cadena se salta L3: las claves que entrega siguen ordenadas,
			// pero faltan dos. Un Scan devolvería menos de lo que el árbol contiene.
			nombre: "inv5 la cadena lateral se salta una hoja",
			rompe:  func(k *canonico) { k.c.nodoDe(k.l2).SetLink(k.l4) },
			quiere: "donde el arbol tiene la",
		},
		{
			// Invariante 5, la otra forma de romperla.
			nombre: "inv5 la cadena lateral acaba antes de tiempo",
			rompe:  func(k *canonico) { k.c.nodoDe(k.l3).SetLink(SinEnlace) },
			quiere: "acaba en la hoja",
		},
		{
			// Invariante 6, doble pertenencia: el Alloc siguiente reasignaría L3 y borraría
			// sus claves en silencio.
			nombre: "inv6 pagina en el arbol y en el conjunto de libres",
			rompe: func(k *canonico) {
				if err := k.c.pg.AdoptFreeSet([]uint64{k.l3}); err != nil {
					k.c.t.Fatalf("AdoptFreeSet: %v", err)
				}
			},
			quiere: "en el arbol y en el conjunto de libres",
		},
		{
			// Invariante 6, pertenencia nula: la fuga que la versión 1 del diseño no
			// detectaba.
			nombre: "inv6 pagina que no esta en ninguno de los dos conjuntos",
			rompe:  func(k *canonico) { k.c.nueva(page.TypeLeaf) },
			quiere: "no esta en el arbol ni en el conjunto de libres",
		},
		{
			// Sin esta detección el barrido no falla: se cuelga.
			nombre: "ciclo hacia un ancestro",
			rompe:  func(k *canonico) { k.c.nodoDe(k.a).SetLink(k.raiz) },
			quiere: "alcanzable por dos caminos",
		},
		{
			nombre: "una ranura meta colgando del arbol",
			rompe:  func(k *canonico) { k.c.nodoDe(k.a).SetLink(1) },
			quiere: "ranura meta",
		},
		{
			nombre: "nodo interno sin hijo derecho",
			rompe:  func(k *canonico) { k.c.nodoDe(k.b).SetLink(SinEnlace) },
			quiere: "no tiene hijo derecho",
		},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			k := arbolCanonico(t)
			c.rompe(k)

			err := k.arbol.Validate()
			if err == nil {
				t.Fatalf("Validate paso en verde sobre un arbol roto")
			}
			if !errors.Is(err, ErrBadTree) && !strings.Contains(err.Error(), "node:") {
				t.Errorf("el error no es ni ErrBadTree ni uno de node: %v", err)
			}
			if !strings.Contains(err.Error(), c.quiere) {
				t.Errorf("Validate dijo %q, se esperaba algo que contuviera %q", err, c.quiere)
			}
		})
	}
}
