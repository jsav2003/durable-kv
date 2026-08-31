// Package tree es el B+tree de la sec. 6 del DESIGN.md: descenso, lectura y validación
// sobre las páginas que entrega el pager.
//
// # Qué hace y qué no
//
// Este archivo es la mitad que solo lee: el descenso, Get y Scan. La mitad que muta --
// Put, la división y el crecimiento de la raíz -- llega después, y llega con Validate() ya
// escrito. El orden no es casual. La sec. 6 llama a Put "el momento peligroso del
// proyecto" porque una sola llamada se convierte en tres o cuatro escrituras que deben
// aplicarse todas o ninguna, y un validador escrito después de la división tiende a heredar
// sus mismas suposiciones: quien acaba de escribirla mira su propio código para decidir qué
// comprobar. Escrito antes, Validate() sale del formato de la sec. 5.1 y de los seis
// invariantes de la sec. 6, que están fijados y no dependen de cómo se implemente el árbol.
//
// # Reparto de responsabilidades
//
// El árbol no interpreta bytes: eso es internal/node, que no sabe que existe un árbol. El
// árbol no lee ni escribe el archivo: eso es internal/pager, que no sabe que existe un
// B+tree. Aquí solo vive lo que se decide entre páginas -- por qué hijo se baja, a qué
// profundidad cuelga una hoja, qué páginas son alcanzables.
package tree

import (
	"bytes"
	"fmt"

	"github.com/jsav2003/motor-almacenamiento/internal/node"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
)

// SinEnlace cierra la cadena lateral de hojas y marca a un nodo interno sin hijo derecho.
//
// El cero puede usarse como centinela porque la página 0 es una de las dos ranuras meta de
// la sec. 5.2 y no puede ser nunca un nodo del árbol: no hay ninguna página real cuyo
// número sea 0, así que el centinela no colisiona con ningún enlace legítimo. Lo garantiza
// pager.MetaPages, que es de donde sale la reserva.
const SinEnlace = 0

// MaxProfundidad acota el descenso. No es una limitación del formato sino un cortafuegos:
// un puntero a un ancestro convierte el bucle del descenso en infinito, y un test que se
// cuelga no dice qué pasó mientras que un error sí. Con aridad 2 -- el mínimo absoluto --
// 64 niveles serían 2^64 hojas, así que ningún árbol sano se acerca.
const MaxProfundidad = 64

// Tree es un B+tree apoyado en un pager. No guarda nada más que la raíz: toda la verdad
// vive en las páginas, por la misma razón que node.Node no guarda campos derivados.
type Tree struct {
	pg   *pager.Pager
	root uint64
}

// New envuelve un pager con la raíz indicada. No valida nada: para eso está Validate.
func New(pg *pager.Pager, root uint64) *Tree { return &Tree{pg: pg, root: root} }

// Root es la página raíz, que es lo que hay que pasar a pager.CommitGroup.
func (t *Tree) Root() uint64 { return t.root }

// nodo lee la página id y la envuelve. Rechaza los tipos que no son nodo antes de que nadie
// interprete su cuerpo: una meta leída como hoja daría celdas plausibles a partir de bytes
// que no lo son, que es el mismo argumento de node.ErrWrongType.
func (t *Tree) nodo(id uint64) (node.Node, error) {
	p, err := t.pg.Get(id)
	if err != nil {
		return node.Node{}, err
	}
	if p.Type != page.TypeLeaf && p.Type != page.TypeInternal {
		return node.Node{}, fmt.Errorf("%w: la pagina %d es de tipo %d y no es un nodo",
			ErrBadTree, id, p.Type)
	}
	return node.Of(p), nil
}

// hijo elige por qué hijo sigue el descenso de key en un nodo interno.
//
// El invariante 4 dice que la separadora es mayor que toda clave del subárbol izquierdo y
// menor o igual que toda clave del derecho. Así que una clave igual a la separadora i baja
// por la derecha, no por la izquierda, y de ahí el i++ cuando Search acierta: sin él, una
// clave que además es separadora se buscaría en el subárbol donde el invariante 4 garantiza
// que no está, y Get devolvería ErrNotFound sobre una clave presente.
func hijo(n node.Node, key []byte) (uint64, error) {
	i, hit := n.Search(key)
	if hit {
		i++
	}
	if i == n.NCells() {
		return n.Link(), nil
	}
	return n.Child(i)
}

// primerHijo es el hijo más a la izquierda, por el que baja un Scan sin cota inferior.
func primerHijo(n node.Node) (uint64, error) {
	if n.NCells() == 0 {
		return n.Link(), nil
	}
	return n.Child(0)
}

// desciende recorre de la raíz a una hoja y devuelve el camino completo, no solo la hoja.
//
// El camino se devuelve entero porque la división lo recorre al revés para propagar la
// separadora hacia arriba: volver a descender para eso releería las mismas páginas, y con
// una división que llega hasta la raíz serían tantas lecturas como niveles tiene el árbol.
func (t *Tree) desciende(elige func(node.Node) (uint64, error)) ([]uint64, error) {
	ruta := make([]uint64, 0, 8)
	id := t.root
	for {
		if len(ruta) >= MaxProfundidad {
			return nil, fmt.Errorf("%w: el descenso paso de %d niveles sin llegar a una hoja",
				ErrBadTree, MaxProfundidad)
		}
		ruta = append(ruta, id)

		n, err := t.nodo(id)
		if err != nil {
			return nil, err
		}
		if n.IsLeaf() {
			return ruta, nil
		}
		if id, err = elige(n); err != nil {
			return nil, err
		}
	}
}

// camino es el descenso hasta la hoja donde vive key, esté o no.
func (t *Tree) camino(key []byte) ([]uint64, error) {
	return t.desciende(func(n node.Node) (uint64, error) { return hijo(n, key) })
}

// caminoIzquierdo es el descenso por el borde izquierdo, hasta la primera hoja.
//
// Existe como función aparte en vez de resolverse pasando nil a camino porque bytes.Compare
// no distingue nil de la clave vacía: con una separadora vacía, camino(nil) se saltaría el
// subárbol izquierdo entero. Un árbol válido no puede tener esa separadora -- dejaría al
// subárbol izquierdo sin ninguna clave y eso rompe el invariante 3 --, pero hacer que Scan
// dependa de un invariante para elegir bien el hijo es apoyarse justo en lo que Scan no
// comprueba.
func (t *Tree) caminoIzquierdo() ([]uint64, error) {
	return t.desciende(primerHijo)
}

// Get devuelve una copia del valor de key, o ErrNotFound.
//
// Copia porque node.Value entrega un subsector del cuerpo de la página, válido solo hasta
// la siguiente mutación de ese nodo (docs/DEUDA-DISENO.md, D6). Devolverlo tal cual daría al
// llamador un slice que un Put posterior reescribe bajo sus pies, y el dato cambiaría de
// valor sin que nadie lo tocara.
//
// ErrNotFound y no un nil silencioso: un valor de longitud cero es legítimo, y "nil, nil"
// no lo distingue de la ausencia.
func (t *Tree) Get(key []byte) ([]byte, error) {
	ruta, err := t.camino(key)
	if err != nil {
		return nil, err
	}
	n, err := t.nodo(ruta[len(ruta)-1])
	if err != nil {
		return nil, err
	}
	i, hit := n.Search(key)
	if !hit {
		return nil, ErrNotFound
	}
	v, err := n.Value(i)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(v), nil
}

// Scan recorre el rango [start, end) en orden y llama a fn con cada par. Un start nil
// empieza por la primera clave y un end nil llega hasta la última. Si fn devuelve false, el
// recorrido para y Scan devuelve nil: es fin de trabajo, no fallo.
//
// Baja al árbol una sola vez, para localizar la hoja de start, y de ahí sigue el enlace
// lateral sin volver a subir (sec. 6). Ese enlace es la razón de que el Scan de rango sea
// barato y de que las hojas lo lleven en la cabecera.
//
// k y v son subsectores de la página y valen lo que dura la llamada a fn. Quien los
// conserve, copia. Es la regla de D6 otra vez, y aquí pesa más que en Get: copiar en cada
// entrada pondría una asignación por clave en el recorrido de 100.000 que cierra la fase.
func (t *Tree) Scan(start, end []byte, fn func(k, v []byte) bool) error {
	var ruta []uint64
	var err error
	if start == nil {
		ruta, err = t.caminoIzquierdo()
	} else {
		ruta, err = t.camino(start)
	}
	if err != nil {
		return err
	}

	id := ruta[len(ruta)-1]

	// Dentro de la primera hoja se arranca donde caiga start; en las siguientes, desde el
	// principio, porque el enlace lateral solo lleva a hojas enteramente posteriores.
	desde := 0
	if start != nil {
		n, err := t.nodo(id)
		if err != nil {
			return err
		}
		desde, _ = n.Search(start)
	}

	// La cadena no puede tener más hojas que páginas tiene el archivo. Sin esta cota, un
	// enlace que apunte hacia atrás deja a Scan girando para siempre sobre las mismas hojas.
	for vueltas := uint64(0); id != SinEnlace; vueltas++ {
		if vueltas >= t.pg.TotalPages() {
			return fmt.Errorf("%w: la cadena lateral de hojas no termina", ErrBadTree)
		}
		n, err := t.nodo(id)
		if err != nil {
			return err
		}
		if !n.IsLeaf() {
			return fmt.Errorf("%w: la pagina %d esta en la cadena lateral y no es una hoja",
				ErrBadTree, id)
		}

		for i := desde; i < n.NCells(); i++ {
			k := n.Key(i)
			if end != nil && bytes.Compare(k, end) >= 0 {
				return nil
			}
			v, err := n.Value(i)
			if err != nil {
				return err
			}
			if !fn(k, v) {
				return nil
			}
		}

		id = n.Link()
		desde = 0
	}
	return nil
}
