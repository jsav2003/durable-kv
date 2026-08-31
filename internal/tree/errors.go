package tree

import "errors"

var (
	// ErrNotFound indica que la clave no está en el árbol. Es flujo normal, no un fallo:
	// Get lo devuelve para distinguirlo de un valor de longitud cero, que es legítimo y
	// que un `nil, nil` confundiría con la ausencia.
	ErrNotFound = errors.New("tree: la clave no esta en el arbol")

	// ErrBadTree indica que la estructura del árbol no cumple alguno de los seis
	// invariantes de la sec. 6, o que el descenso se topó con algo imposible. Lo devuelve
	// Validate envuelto con el detalle de qué invariante falló y en qué página.
	//
	// Es el hermano de node.ErrBadNode un nivel más arriba, y la separación importa:
	// node.Check valida una página aislada -- que sus celdas caigan dentro del área y sus
	// claves estén ordenadas -- y no puede detectar nada que se decida entre páginas. Una
	// hoja perfectamente formada colgando a la profundidad equivocada, o alcanzable por
	// dos caminos a la vez, pasa node.Check sin una queja.
	ErrBadTree = errors.New("tree: el arbol no cumple sus invariantes")
)
