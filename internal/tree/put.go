package tree

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/jsav2003/motor-almacenamiento/internal/node"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
)

// Put inserta o sustituye el valor de key. Es lo que la sec. 6 llama el momento peligroso
// del proyecto: una sola llamada puede convertirse en tres, cuatro o más escrituras que
// deben aplicarse todas o ninguna, porque si se aplican a medias el padre queda apuntando a
// una página que no existe o que está a medias y el árbol se corrompe sin ningún aviso.
//
// Lo que convierte ese "todas o ninguna" en algo real es el grupo de commit de la sec. 7.3,
// y por eso Put entero cabe entre BeginGroup y CommitGroup. Durante la F2 el WAL detrás no
// existe y CommitGroup solo sella LSN, tal como la sec. 10 anticipa; lo que sí existe ya es
// la delimitación, que es lo que la F3 necesita encontrar hecho.
//
// # Qué pasa si falla a mitad de camino
//
// El grupo se queda abierto y toda operación posterior falla con ErrNoGroup o ErrGroupOpen.
// Es deliberado: los tamaños se comprueban antes de abrir el grupo, así que un error que
// llegue hasta aquí solo puede venir de una página que no es lo que dice ser -- CRC malo,
// page_id cambiado, un nodo que Check rechazaría. Con esa clase de fallo el árbol en memoria
// ya está a medias y no hay forma de deshacerlo sin el log. Un pager envenenado que se niega
// a seguir es más honesto que uno que continúa sobre una estructura rota. El camino de
// deshacer de verdad llega con el WAL en la F3; ver docs/DEUDA-DISENO.md, D9.
func (t *Tree) Put(key, val []byte) error {
	// Los límites de la sec. 4 se comprueban aquí y no dentro del grupo: son un error del
	// llamador, no una corrupción, y abrir un grupo para abortarlo dejaría el pager
	// envenenado por una clave demasiado larga.
	if len(key) > node.MaxKeySize {
		return node.ErrKeyTooLarge
	}
	if len(key)+len(val)+node.LeafCellOverhead > node.MaxEntrySize {
		return node.ErrEntryTooLarge
	}

	if err := t.pg.BeginGroup(); err != nil {
		return err
	}
	if err := t.inserta(key, val); err != nil {
		return err
	}
	return t.pg.CommitGroup(t.root)
}

// inserta baja hasta la hoja, mete el par y propaga hacia arriba las divisiones que haga
// falta, nivel a nivel, hasta que un nivel absorba la separadora o se acabe el camino y el
// árbol tenga que crecer.
//
// Recorre la ruta que ya devolvió el descenso en vez de volver a bajar: cada nivel que se
// releyera sería una lectura más por nivel del árbol, justo en el camino que la sec. 6
// quiere de tres lecturas.
func (t *Tree) inserta(key, val []byte) error {
	ruta, err := t.camino(key)
	if err != nil {
		return err
	}

	nivel := len(ruta) - 1
	sep, der, err := t.insertaEnHoja(ruta[nivel], key, val)
	if err != nil {
		return err
	}

	for sep != nil {
		if nivel == 0 {
			// Se dividió la raíz: el árbol gana un nivel.
			return t.creceRaiz(ruta[0], sep, der)
		}
		nivel--
		padre, err := t.nodo(ruta[nivel])
		if err != nil {
			return err
		}
		// La posición del hijo que se dividió es la misma que eligió el descenso, y el padre
		// todavía no ha cambiado, así que se recalcula igual en vez de arrastrarse.
		p := indiceDeHijo(padre, key)
		sep, der, err = t.insertaEnPadre(padre, p, sep, ruta[nivel+1], der)
		if err != nil {
			return err
		}
	}
	return nil
}

// insertaEnHoja mete el par en la hoja id. Devuelve la separadora que sube y la página nueva
// si hubo que dividir, o (nil, 0) si la hoja lo absorbió.
func (t *Tree) insertaEnHoja(id uint64, key, val []byte) ([]byte, uint64, error) {
	hoja, err := t.nodo(id)
	if err != nil {
		return nil, 0, err
	}
	if !hoja.IsLeaf() {
		return nil, 0, fmt.Errorf("%w: el descenso acabo en la pagina %d, que no es una hoja",
			ErrBadTree, id)
	}

	// Sustituir un valor es borrar e insertar, que es lo que ya decidió internal/node: un
	// solo camino de mutación es un solo camino que probar. Tras el borrado el punto de
	// inserción sigue siendo i.
	i, hit := hoja.Search(key)
	if hit {
		if err := hoja.Delete(i); err != nil {
			return nil, 0, err
		}
	}

	err = hoja.InsertHoja(i, key, val)
	if err == nil {
		return nil, 0, t.pg.MarkDirty(hoja.Page())
	}
	if !errors.Is(err, node.ErrNoSpace) {
		return nil, 0, err
	}

	// No cabe ni compactando: la hoja se divide.
	p, err := t.pg.Alloc(page.TypeLeaf)
	if err != nil {
		return nil, 0, err
	}
	nueva := node.Of(p)

	sep, err := node.Split(hoja, nueva)
	if err != nil {
		return nil, 0, err
	}

	// La hoja nueva se cuela en la cadena lateral justo detrás de la vieja. Es el invariante
	// 5, y mantenerlo aquí y no después es lo que hace que la cadena nunca exista rota:
	// entre Split y este par de líneas no hay nada que pueda fallar.
	nueva.SetLink(hoja.Link())
	hoja.SetLink(p.ID)

	// El par va a la mitad que le corresponde. La comparación es >= por el invariante 4 --
	// la clave igual a la separadora cae a la derecha -- aunque aquí la igualdad no puede
	// darse: sep es una clave que sigue en la hoja y key acaba de salir de ella, si es que
	// estaba. Se escribe >= igualmente porque es la regla del invariante y no una
	// consecuencia de por dónde se llegó hasta esta línea.
	destino := hoja
	if bytes.Compare(key, sep) >= 0 {
		destino = nueva
	}
	j, _ := destino.Search(key)
	if err := destino.InsertHoja(j, key, val); err != nil {
		return nil, 0, err
	}

	if err := t.pg.MarkDirty(hoja.Page()); err != nil {
		return nil, 0, err
	}
	return sep, p.ID, nil
}

// insertaEnPadre coloca la separadora en el padre, dividiéndolo también si no cabe.
// Devuelve lo que haya que subir al abuelo, o (nil, 0) si el padre lo absorbió.
func (t *Tree) insertaEnPadre(padre node.Node, p int, sep []byte, izq, der uint64) ([]byte, uint64, error) {
	err := coloca(padre, p, sep, izq, der)
	if err == nil {
		return nil, 0, t.pg.MarkDirty(padre.Page())
	}
	if !errors.Is(err, node.ErrNoSpace) {
		return nil, 0, err
	}

	// El padre tampoco admite una celda más. Que coloca sea todo o nada importa justo aquí:
	// al llegar a este punto el padre está exactamente como estaba.
	pn, err := t.pg.Alloc(page.TypeInternal)
	if err != nil {
		return nil, 0, err
	}
	nuevo := node.Of(pn)

	sube, err := node.Split(padre, nuevo)
	if err != nil {
		return nil, 0, err
	}

	// Dónde acabó el hijo que se dividió. m es cuántas celdas conservó la mitad izquierda, y
	// también el índice de la celda que Split promocionó.
	//
	// Con p <= m el hijo sigue en la mitad izquierda y su posición no cambia. Los dos casos
	// que eso agrupa parecen distintos y no lo son: con p < m sigue siendo el hijo izquierdo
	// de su misma celda, y con p == m era el hijo de la celda promocionada, que Split acaba
	// de convertir en el enlace derecho de la mitad izquierda -- y p == m == NCells() de esa
	// mitad es justo el valor con el que coloca trata el caso del enlace. La coincidencia no
	// es casual: es la misma numeración de indiceDeHijo aplicada a un nodo que perdió sus
	// celdas de la m en adelante.
	m := padre.NCells()
	destino, q := padre, p
	if p > m {
		// Se fue a la mitad derecha, desplazado por las m+1 celdas que ya no están delante.
		// Si era el enlace derecho del padre (p == nc), la cuenta da justo NCells() de la
		// mitad derecha, que vuelve a ser el caso del enlace.
		destino, q = nuevo, p-m-1
	}
	if err := coloca(destino, q, sep, izq, der); err != nil {
		return nil, 0, err
	}

	if err := t.pg.MarkDirty(padre.Page()); err != nil {
		return nil, 0, err
	}
	return sube, pn.ID, nil
}

// coloca mete la separadora sep en el nodo interno n, sabiendo que el hijo que se dividió
// ocupaba la posición p -- la misma numeración que usa indiceDeHijo, donde NCells() es el
// enlace derecho.
//
// Es todo o nada: la única operación que puede fallar es la inserción, y va primero. Si
// devuelve ErrNoSpace, n está como estaba y quien llama puede dividirlo con la garantía de
// no haberlo dejado a medias.
func coloca(n node.Node, p int, sep []byte, izq, der uint64) error {
	if p == n.NCells() {
		// izq era el hijo derecho: la separadora se anexa con izq debajo y der pasa a ser el
		// hijo derecho nuevo.
		if err := n.InsertInterno(p, sep, izq); err != nil {
			return err
		}
		n.SetLink(der)
		return nil
	}
	// izq era el hijo izquierdo de la celda p. La celda nueva se queda con izq, y la que ya
	// estaba -- que conserva su clave y ahora está en p+1 -- pasa a apuntar a der.
	if err := n.InsertInterno(p, sep, izq); err != nil {
		return err
	}
	return n.SetChild(p+1, der)
}

// creceRaiz cuelga las dos mitades de una raíz nueva. Es la única operación que cambia la
// altura del árbol, y por eso la única que puede romper el invariante 2 de un golpe.
//
// La raíz nueva es una página nueva y no la vieja reescrita. Puede serlo porque
// pager.CommitGroup lleva el rootID dentro de cada commit (sec. 7.3), así que la raíz no
// está obligada a quedarse en la misma página entre reinicios. Reescribirla en sitio
// obligaría a mover antes su contenido a otra página: el mismo trabajo con un paso más y una
// página en un estado intermedio que no es un nodo válido.
func (t *Tree) creceRaiz(izq uint64, sep []byte, der uint64) error {
	p, err := t.pg.Alloc(page.TypeInternal)
	if err != nil {
		return err
	}
	nueva := node.Of(p)
	if err := nueva.InsertInterno(0, sep, izq); err != nil {
		return err
	}
	nueva.SetLink(der)
	t.root = p.ID
	return nil
}

// Crear devuelve un árbol nuevo sobre un pager recién creado: una sola hoja vacía por raíz.
// Es el único árbol en el que el invariante 3 admite una página alcanzable sin celdas.
func Crear(pg *pager.Pager) (*Tree, error) {
	if err := pg.BeginGroup(); err != nil {
		return nil, err
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		return nil, err
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		return nil, err
	}
	return New(pg, p.ID), nil
}
