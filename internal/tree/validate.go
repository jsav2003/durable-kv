package tree

import (
	"bytes"
	"fmt"

	"github.com/jsav2003/durable-kv/internal/pager"
)

// Validate comprueba los seis invariantes de la sec. 6 sobre el árbol entero y el conjunto
// de páginas libres. La sec. 6 la llama la herramienta de depuración más importante del
// proyecto, y la sec. 8 la invoca después de cada recuperación.
//
// # Por qué no basta con el CRC
//
// El CRC de la sec. 5.1 dice que los bytes de una página son los que se escribieron.
// node.Check sube un escalón y dice que esos bytes forman un nodo coherente: celdas dentro
// del área, sin solaparse, claves ordenadas. Ninguna de las dos ve nada que se decida entre
// páginas. Una hoja intachable colgando a la profundidad equivocada, alcanzable por dos
// caminos, o con un separador del padre que miente sobre lo que contiene, pasa el CRC y
// pasa node.Check. Ese hueco es este archivo.
//
// # Qué produce
//
// Un error envuelto en ErrBadTree que nombra el invariante roto y la página donde se rompe.
// El nombre y la página importan: el modo de fallo que este proyecto persigue es el que
// aparece miles de operaciones después de su causa, y un "árbol inválido" a secas no acorta
// esa distancia.
func (t *Tree) Validate() error {
	r, err := t.barrido()
	if err != nil {
		return err
	}
	if err := t.cadenaLateral(r.hojas); err != nil {
		return err
	}
	return t.particion(r.visitadas)
}

// ReconstruirLibres barre las páginas alcanzables desde la raíz y declara libre todo lo
// demás (sec. 6.1). Es lo que hay que llamar al abrir, y lo que hace que el invariante 6 sea
// cierto por construcción en vez de por vigilancia.
//
// La sec. 6.1 decide no persistir el conjunto de libres precisamente para esto: una lista
// enlazada en disco abre una ventana entre checkpoints en la que una página puede estar a la
// vez en el árbol y en la cabeza de la lista, y el siguiente Put que necesite espacio la
// reasigna y borra sus claves en silencio.
func (t *Tree) ReconstruirLibres() error {
	r, err := t.barrido()
	if err != nil {
		return err
	}
	libres := make([]uint64, 0)
	for id := uint64(pager.MetaPages); id < t.pg.TotalPages(); id++ {
		if !r.visitadas[id] {
			libres = append(libres, id)
		}
	}
	return t.pg.AdoptFreeSet(libres)
}

// recorrido acumula lo que el barrido en profundidad observa. Se pasa por puntero porque la
// visita es recursiva y las tres cosas que recoge son globales al árbol, no al subárbol.
type recorrido struct {
	// visitadas es el conjunto de páginas alcanzables. Sirve para dos cosas a la vez:
	// detectar que una página se alcanza por dos caminos, y ser el lado del árbol de la
	// partición del invariante 6.
	visitadas map[uint64]bool

	// hojas son las hojas en el orden en que las encuentra el barrido, que es el orden de
	// las claves: los hijos se visitan en el orden de sus separadoras y el enlace derecho al
	// final. Es contra esta lista contra la que se compara la cadena lateral.
	hojas []uint64

	// profundidadHoja es la profundidad de la primera hoja vista, o -1 si no hay ninguna
	// todavía. Las demás tienen que coincidir: ese es el invariante 2.
	profundidadHoja int
}

func (t *Tree) barrido() (*recorrido, error) {
	r := &recorrido{visitadas: make(map[uint64]bool), profundidadHoja: -1}
	if err := t.visita(t.root, 0, nil, nil, r); err != nil {
		return nil, err
	}
	return r, nil
}

// visita comprueba la página id y desciende por sus hijos.
//
// lo y hi son las cotas que el invariante 4 impone a este subárbol: toda clave suya debe
// caer en [lo, hi). Un nil es "sin cota por ese lado", que es lo que recibe la raíz. Las
// cotas viajan hacia abajo porque un separador solo dice algo sobre sus dos hijos
// inmediatos: la garantía de que ninguna clave se coló varios niveles por debajo de donde le
// toca solo aparece al arrastrar el rango entero.
//
// Las cotas son subsectores del cuerpo del padre y siguen siendo válidas durante toda la
// recursión: nada muta páginas aquí, y el caché del pager no desaloja (docs/DEUDA-DISENO.md,
// D4). Si algún día desalojara, estas cotas habría que copiarlas.
func (t *Tree) visita(id uint64, prof int, lo, hi []byte, r *recorrido) error {
	if prof > MaxProfundidad {
		return fmt.Errorf("%w: el barrido paso de %d niveles", ErrBadTree, MaxProfundidad)
	}
	if id < pager.MetaPages {
		return fmt.Errorf("%w: la pagina %d es una ranura meta y el arbol la alcanza",
			ErrBadTree, id)
	}

	// Esto es a la vez la detección de ciclos y la mitad "sin doble pertenencia" del
	// invariante 6. Sin ella un puntero a un ancestro no daría error: colgaría el barrido, y
	// un Validate que no termina es peor que uno que falla.
	if r.visitadas[id] {
		return fmt.Errorf("%w: la pagina %d es alcanzable por dos caminos", ErrBadTree, id)
	}
	r.visitadas[id] = true

	n, err := t.nodo(id)
	if err != nil {
		return err
	}

	// Invariante 1, y de paso toda la coherencia interna del cuerpo. A partir de aquí las
	// celdas son legibles, así que Key y Child ya no pueden desbordar.
	if err := n.Check(); err != nil {
		return fmt.Errorf("la pagina %d: %w", id, err)
	}

	nc := n.NCells()
	if nc == 0 {
		// Un nodo interno sin separadoras no tiene por dónde dirigir una búsqueda, y además
		// dejaría a Key(nc-1) indexando en -1 unas líneas más abajo.
		if !n.IsLeaf() {
			return fmt.Errorf("%w: el nodo interno %d no tiene separadoras", ErrBadTree, id)
		}
		// Invariante 3. La raíz es la única excepción: un árbol vacío es una hoja vacía.
		if id != t.root {
			return fmt.Errorf("%w: la pagina %d es alcanzable y no tiene celdas", ErrBadTree, id)
		}
	}

	// Invariante 4. Basta con la primera y la última clave: node.Check ya garantizó que las
	// de en medio están estrictamente ordenadas entre esas dos.
	if nc > 0 {
		if lo != nil && bytes.Compare(n.Key(0), lo) < 0 {
			return fmt.Errorf("%w: la pagina %d tiene la clave %q por debajo de su cota %q",
				ErrBadTree, id, n.Key(0), lo)
		}
		if hi != nil && bytes.Compare(n.Key(nc-1), hi) >= 0 {
			return fmt.Errorf("%w: la pagina %d tiene la clave %q en o por encima de su cota %q",
				ErrBadTree, id, n.Key(nc-1), hi)
		}
	}

	if n.IsLeaf() {
		// Invariante 2.
		switch {
		case r.profundidadHoja == -1:
			r.profundidadHoja = prof
		case prof != r.profundidadHoja:
			return fmt.Errorf("%w: la hoja %d cuelga a profundidad %d y otra a %d",
				ErrBadTree, id, prof, r.profundidadHoja)
		}
		r.hojas = append(r.hojas, id)
		return nil
	}

	// Un nodo interno tiene nc hijos por celda más el enlace derecho de la cabecera. El hijo
	// i cubre [separadora i-1, separadora i), y el enlace cubre de la última separadora hasta
	// la cota superior heredada.
	for i := 0; i < nc; i++ {
		c, err := n.Child(i)
		if err != nil {
			return err
		}
		sub := lo
		if i > 0 {
			sub = n.Key(i - 1)
		}
		if err := t.visita(c, prof+1, sub, n.Key(i), r); err != nil {
			return err
		}
	}
	if n.Link() == SinEnlace {
		return fmt.Errorf("%w: el nodo interno %d no tiene hijo derecho", ErrBadTree, id)
	}
	return t.visita(n.Link(), prof+1, n.Key(nc-1), hi, r)
}

// cadenaLateral comprueba el invariante 5: que los enlaces entre hojas recorren todas las
// claves en orden.
//
// Se comprueba comparando la cadena contra las hojas que encontró el barrido, en su orden.
// Comprobar solo que las claves salen ordenadas no bastaría: una cadena que se saltara una
// hoja entera daría una secuencia perfectamente ordenada a la que le faltan claves, y el
// Scan de rango devolvería menos de lo que el árbol contiene sin ninguna señal.
func (t *Tree) cadenaLateral(hojas []uint64) error {
	if len(hojas) == 0 {
		return nil
	}
	id := hojas[0]
	for i := 0; ; i++ {
		if i >= len(hojas) {
			return fmt.Errorf("%w: la cadena lateral visita mas de las %d hojas del arbol",
				ErrBadTree, len(hojas))
		}
		if id != hojas[i] {
			return fmt.Errorf("%w: la cadena lateral va a la hoja %d donde el arbol tiene la %d",
				ErrBadTree, id, hojas[i])
		}
		n, err := t.nodo(id)
		if err != nil {
			return err
		}
		if sig := n.Link(); sig != SinEnlace {
			id = sig
			continue
		}
		if i != len(hojas)-1 {
			return fmt.Errorf("%w: la cadena lateral acaba en la hoja %d y el arbol tiene %d mas",
				ErrBadTree, id, len(hojas)-1-i)
		}
		return nil
	}
}

// particion comprueba el invariante 6: toda página en [MetaPages, total_pages) está en
// exactamente uno de los dos conjuntos, el alcanzable o el libre.
//
// La doble pertenencia detecta el reciclado de una página que sigue en el árbol -- el Put
// siguiente la reasigna y borra sus claves en silencio. La pertenencia nula detecta la fuga:
// la versión 1 del diseño solo prohibía lo primero, y un motor que filtrara el 100% del
// archivo pasaba la validación.
//
// # Lo que esta comprobación todavía no hace
//
// El invariante 6 pide además que toda página de ambos conjuntos tenga CRC y page_id
// válidos. Las alcanzables lo cumplen por construcción: t.nodo las lee con pager.Get, que
// las decodifica y verifica las dos cosas. Las libres no se leen, y no es un olvido: en la
// F2 una página puede haberse asignado y liberado sin llegar nunca a datos.db, de forma que
// su ranura está a ceros y su CRC es inválido sin que nada esté mal. Esa mitad del
// invariante necesita que el checkpoint haya corrido, que es F3. Anotado en
// docs/DEUDA-DISENO.md, D8.
func (t *Tree) particion(alcanzables map[uint64]bool) error {
	libres := make(map[uint64]bool)
	for _, id := range t.pg.FreePages() {
		if alcanzables[id] {
			return fmt.Errorf("%w: la pagina %d esta en el arbol y en el conjunto de libres",
				ErrBadTree, id)
		}
		libres[id] = true
	}
	for id := uint64(pager.MetaPages); id < t.pg.TotalPages(); id++ {
		if !alcanzables[id] && !libres[id] {
			return fmt.Errorf("%w: la pagina %d no esta en el arbol ni en el conjunto de libres",
				ErrBadTree, id)
		}
	}
	return nil
}
