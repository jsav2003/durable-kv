package node

import (
	"bytes"

	"github.com/jsav2003/durable-kv/internal/page"
)

// Split reparte el contenido de izq entre izq y der, y devuelve la clave separadora que el
// padre tiene que recibir. der debe ser un nodo recién asignado, vacío y del mismo tipo.
//
// Sigue siendo aritmética de offsets sobre dos páginas y nada más: no toca el pager, no sabe
// quién es el padre y no arregla la cadena lateral de hojas. Eso es del árbol, que es quien
// conoce esas tres cosas. Aquí se puede probar con dos páginas sueltas.
//
// # Por bytes, no por número de celdas
//
// El corte se elige acumulando bytes hasta pasar de la mitad, no partiendo el directorio por
// el índice del medio. Con celdas de tamaño variable las dos cosas no se parecen: una hoja
// con una celda de 3000 bytes y tres de 20 partida por el índice deja una mitad al 74% y la
// otra al 1%, y la mitad llena se vuelve a partir en la inserción siguiente.
//
// # Las dos divisiones no son la misma operación
//
// En una hoja las claves no se pierden: la mitad derecha conserva la primera, que es además
// la separadora que sube. En un nodo interno la celda del corte **se promociona** -- su clave
// sube al padre y su hijo izquierdo pasa a ser el enlace derecho de la mitad izquierda --,
// así que esa clave desaparece de este nivel. Tratar los dos casos igual duplicaría una clave
// separadora en dos niveles, y el descenso seguiría funcionando: el fallo aparecería mucho
// más tarde, en una fusión que no encuentra lo que espera.
func Split(izq, der Node) ([]byte, error) {
	if izq.p.Type != der.p.Type {
		return nil, ErrWrongType
	}
	if der.NCells() != 0 || der.p.FreeEnd != uint16(page.Size) {
		return nil, ErrCannotSplit
	}

	nc := izq.NCells()
	m := izq.puntoDeCorte()

	if izq.IsLeaf() {
		// La mitad izquierda se queda [0, m) y la derecha [m, nc). Las dos tienen que acabar
		// con al menos una celda: el invariante 3 no admite una página alcanzable vacía.
		if nc < 2 {
			return nil, ErrCannotSplit
		}
		m = min(max(m, 1), nc-1)

		for i := m; i < nc; i++ {
			v, err := izq.Value(i)
			if err != nil {
				return nil, err
			}
			if err := der.InsertHoja(i-m, izq.Key(i), v); err != nil {
				return nil, err
			}
		}
		sep := bytes.Clone(der.Key(0))
		izq.truncar(m)
		return sep, nil
	}

	// Nodo interno: [0, m) se queda, la celda m se promociona y (m, nc) se va a la derecha.
	// Hacen falta tres celdas para que las dos mitades queden con una como mínimo. Nunca
	// faltan: una celda interna mide como mucho InternalCellOverhead + MaxKeySize + su slot,
	// así que tres caben de sobra en el cuerpo y ErrNoSpace no puede aparecer antes.
	if nc < 3 {
		return nil, ErrCannotSplit
	}
	m = min(max(m, 1), nc-2)

	for i := m + 1; i < nc; i++ {
		c, err := izq.Child(i)
		if err != nil {
			return nil, err
		}
		if err := der.InsertInterno(i-m-1, izq.Key(i), c); err != nil {
			return nil, err
		}
	}
	der.SetLink(izq.Link())

	// Los dos se leen antes de truncar: truncar compacta, y Key devuelve un subsector.
	sep := bytes.Clone(izq.Key(m))
	derechoDeIzq, err := izq.Child(m)
	if err != nil {
		return nil, err
	}

	izq.truncar(m)
	izq.SetLink(derechoDeIzq)
	return sep, nil
}

// puntoDeCorte devuelve cuántas celdas dejar a la izquierda para que esa mitad sea la
// primera que pasa de la mitad de los bytes ocupados, slots incluidos.
//
// Devuelve al menos 1 y como mucho NCells; quien llama lo recorta al rango que su tipo de
// nodo admite. Que la mitad izquierda quede por encima de la mitad y no por debajo es lo que
// acota la derecha a total/2, y de ahí sale que la celda nueva siempre quepa en la mitad que
// le toque -- ver TestDivisionDejaSitioParaLaCeldaNueva.
func (n Node) puntoDeCorte() int {
	total := 0
	for i := 0; i < n.NCells(); i++ {
		total += n.CellSize(i) + SlotSize
	}
	acc := 0
	for i := 0; i < n.NCells(); i++ {
		acc += n.CellSize(i) + SlotSize
		if acc*2 >= total {
			return i + 1
		}
	}
	return n.NCells()
}

// truncar deja solo las primeras m celdas y compacta para recuperar los bytes de las demás.
// Sin la compactación la mitad izquierda seguiría creyéndose llena: los bytes de las celdas
// que se fueron son espacio muerto hasta que alguien los reclama.
func (n Node) truncar(m int) {
	for n.NCells() > m {
		// Delete solo falla con un índice fuera de rango, y el bucle garantiza que no lo es.
		_ = n.Delete(n.NCells() - 1)
	}
	n.Compactar()
}
