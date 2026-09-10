package tree

import (
	"io"
	"testing"

	"github.com/jsav2003/durable-kv/internal/node"
	"github.com/jsav2003/durable-kv/internal/page"
	"github.com/jsav2003/durable-kv/internal/pager"
)

// Este archivo arma árboles a mano, celda a celda, sin pasar por Put -- que todavía no
// existe.
//
// Que no exista es deliberado y es lo que da valor a los tests de Validate: un árbol
// construido por el mismo código que Validate vigila solo puede demostrar que los dos están
// de acuerdo, no que ninguno de los dos se equivoca. Aquí las páginas se colocan una por una
// con los primitivos de internal/node, así que un árbol roto se puede escribir a propósito
// -- y esa es la única forma de comprobar que Validate se pone rojo cuando toca.

// memFile es un File en memoria. No simula ningún fallo: el disco falso con descartes,
// reordenamiento y escrituras desgarradas es de la F4. Aquí solo hace falta un sitio donde
// las páginas puedan bajar y volver a subir.
type memFile struct{ datos []byte }

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(f.datos)) {
		return 0, io.EOF
	}
	n := copy(p, f.datos[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	if fin := off + int64(len(p)); fin > int64(len(f.datos)) {
		f.datos = append(f.datos, make([]byte, fin-int64(len(f.datos)))...)
	}
	return copy(f.datos[off:], p), nil
}

func (f *memFile) Sync() error { return nil }

func (f *memFile) Truncate(size int64) error {
	for int64(len(f.datos)) < size {
		f.datos = append(f.datos, 0)
	}
	f.datos = f.datos[:size]
	return nil
}

// par es una entrada de hoja.
type par struct{ k, v string }

// constructor coloca páginas sueltas sobre un pager y las deja en el sitio que le pidan.
//
// Mantiene un grupo de commit abierto desde el principio porque pager.Alloc lo exige: toda
// mutación de la estructura pertenece a un grupo (sec. 7.3), y el pager lo impone con
// ErrNoGroup aunque el WAL detrás no exista todavía. Quien necesite bajar las páginas a
// disco cierra el grupo con termina.
type constructor struct {
	t  *testing.T
	f  *memFile
	pg *pager.Pager
}

func nuevoConstructor(t *testing.T) *constructor {
	t.Helper()
	f := &memFile{}
	pg, err := pager.Create(f, &pager.NopLog{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	return &constructor{t: t, f: f, pg: pg}
}

// termina cierra el grupo abierto. Solo lo llaman los tests que además bajan a disco:
// FlushDirty se niega a correr con un grupo a medias, y con razón (sec. 7.5).
func (c *constructor) termina(raiz uint64) {
	c.t.Helper()
	if err := c.pg.CommitGroup(raiz); err != nil {
		c.t.Fatalf("CommitGroup: %v", err)
	}
}

// nueva asigna una página del tipo pedido y la devuelve envuelta. No hace falta MarkDirty:
// Alloc ya deja la página en el grupo y en el conjunto de sucias.
func (c *constructor) nueva(tipo page.Type) node.Node {
	c.t.Helper()
	p, err := c.pg.Alloc(tipo)
	if err != nil {
		c.t.Fatalf("Alloc: %v", err)
	}
	return node.Of(p)
}

// nodoDe devuelve el nodo que vive en la página id.
func (c *constructor) nodoDe(id uint64) node.Node {
	c.t.Helper()
	p, err := c.pg.Get(id)
	if err != nil {
		c.t.Fatalf("Get(%d): %v", id, err)
	}
	return node.Of(p)
}

// hoja arma una hoja con los pares dados, cada uno en su posición ordenada.
func (c *constructor) hoja(pares ...par) uint64 {
	c.t.Helper()
	n := c.nueva(page.TypeLeaf)
	for _, e := range pares {
		i, _ := n.Search([]byte(e.k))
		if err := n.InsertHoja(i, []byte(e.k), []byte(e.v)); err != nil {
			c.t.Fatalf("InsertHoja(%q): %v", e.k, err)
		}
	}
	return n.Page().ID
}

// interno arma un nodo interno: una separadora por hijo izquierdo, más el hijo derecho en
// el enlace de la cabecera.
func (c *constructor) interno(seps []string, hijos []uint64, derecho uint64) uint64 {
	c.t.Helper()
	if len(seps) != len(hijos) {
		c.t.Fatalf("interno: %d separadoras para %d hijos", len(seps), len(hijos))
	}
	n := c.nueva(page.TypeInternal)
	for i, s := range seps {
		if err := n.InsertInterno(i, []byte(s), hijos[i]); err != nil {
			c.t.Fatalf("InsertInterno(%q): %v", s, err)
		}
	}
	n.SetLink(derecho)
	return n.Page().ID
}

// encadena enlaza las hojas en el orden dado y cierra la cadena en la última.
func (c *constructor) encadena(hojas ...uint64) {
	c.t.Helper()
	for i, id := range hojas {
		sig := uint64(SinEnlace)
		if i+1 < len(hojas) {
			sig = hojas[i+1]
		}
		c.nodoDe(id).SetLink(sig)
	}
}

// canonico es el árbol de tres niveles que usan casi todos los tests:
//
//	             raiz [k070]
//	            /           \
//	  A [k030 k050]          B [k090]
//	   /    |    \            /     \
//	 L1    L2    L3         L4      L5
//	010   030   050        070     090
//	020   040   060        080     100
//
// Las cuatro separadoras -- k030, k050, k070 y k090 -- son además claves reales del árbol,
// a propósito: el invariante 4 manda la clave igual a la separadora al subárbol derecho, y
// un descenso que se equivoque en esa igualdad devuelve ErrNotFound sobre una clave que sí
// está. Con separadoras que no existieran como dato, ese error no se vería.
type canonico struct {
	c                  *constructor
	arbol              *Tree
	raiz, a, b         uint64
	l1, l2, l3, l4, l5 uint64
}

// clavesCanonicas son las diez claves del árbol canónico, en orden.
var clavesCanonicas = []string{
	"k010", "k020", "k030", "k040", "k050", "k060", "k070", "k080", "k090", "k100",
}

// valorDe es el valor que el árbol canónico guarda para una clave.
func valorDe(k string) string { return "v" + k }

func arbolCanonico(t *testing.T) *canonico {
	t.Helper()
	c := nuevoConstructor(t)

	l1 := c.hoja(par{"k010", valorDe("k010")}, par{"k020", valorDe("k020")})
	l2 := c.hoja(par{"k030", valorDe("k030")}, par{"k040", valorDe("k040")})
	l3 := c.hoja(par{"k050", valorDe("k050")}, par{"k060", valorDe("k060")})
	l4 := c.hoja(par{"k070", valorDe("k070")}, par{"k080", valorDe("k080")})
	l5 := c.hoja(par{"k090", valorDe("k090")}, par{"k100", valorDe("k100")})

	a := c.interno([]string{"k030", "k050"}, []uint64{l1, l2}, l3)
	b := c.interno([]string{"k090"}, []uint64{l4}, l5)
	raiz := c.interno([]string{"k070"}, []uint64{a}, b)

	c.encadena(l1, l2, l3, l4, l5)

	return &canonico{
		c: c, arbol: New(c.pg, raiz),
		raiz: raiz, a: a, b: b,
		l1: l1, l2: l2, l3: l3, l4: l4, l5: l5,
	}
}
