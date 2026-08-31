package node

import "errors"

var (
	// ErrNoSpace indica que la celda no cabe en la página ni siquiera después de
	// compactar. No es un fallo: es la señal de que hay que dividir, y el árbol de la
	// etapa siguiente la trata como flujo normal, no como error.
	//
	// Que la compactación esté dentro de Insert y no en el llamador es lo que hace útil
	// esta distinción: cuando aparece ErrNoSpace ya se ha intentado todo lo que no exige
	// tocar la estructura del árbol.
	ErrNoSpace = errors.New("node: la celda no cabe en la pagina")

	// ErrKeyTooLarge indica una clave de más de MaxKeySize bytes (DESIGN.md sec. 4).
	//
	// Es un error distinto de ErrEntryTooLarge a propósito. El tope de la clave no sale
	// del mismo sitio que el de la entrada: la clave viaja además a los nodos internos
	// como separadora, donde no hay valor que la acompañe, así que un límite de entrada
	// holgado no protegería al nodo interno de una clave desmedida.
	ErrKeyTooLarge = errors.New("node: clave demasiado larga")

	// ErrEntryTooLarge indica que clave + valor + LeafCellOverhead pasa de MaxEntrySize.
	// Es el error que la sec. 4 del DESIGN.md nombra explícitamente para Put, y la razón
	// por la que este motor no necesita páginas de desbordamiento (NO-GOALS.md).
	ErrEntryTooLarge = errors.New("node: entrada demasiado larga")

	// ErrWrongType indica que se pidió a un nodo algo que su tipo no tiene: un valor a un
	// nodo interno, un hijo a una hoja, o cualquier cosa a una página meta.
	//
	// Los dos formatos de celda de la sec. 5.1 tienen sobrecargas distintas -- 6 bytes en
	// la hoja, 10 en el nodo interno -- así que leer una con el formato de la otra no da
	// un error, da bytes plausibles: la clave saldría desplazada cuatro posiciones y aun
	// así tendría una longitud creíble. Por eso el tipo se comprueba en el accesor y no
	// se deja a que el resultado "parezca raro".
	ErrWrongType = errors.New("node: el tipo de pagina no admite esta operacion")

	// ErrOutOfRange indica un índice de celda fuera de rango. En los accesores el rango es
	// [0, NCells); en los Insert es [0, NCells], porque insertar al final es legítimo.
	ErrOutOfRange = errors.New("node: indice de celda fuera de rango")

	// ErrCannotSplit indica que Split no puede repartir este nodo: el destino no está vacío
	// o del mismo tipo, o el origen no tiene celdas suficientes para que las dos mitades
	// queden con al menos una.
	//
	// A diferencia de ErrNoSpace, esto no es flujo normal: un nodo llega a Split porque un
	// Insert devolvió ErrNoSpace, y un nodo que no admite una celda más tiene por fuerza
	// varias. Si aparece, o el llamador dividió algo que no hacía falta dividir o la página
	// no es lo que dice ser.
	ErrCannotSplit = errors.New("node: el nodo no se puede dividir")

	// ErrBadNode indica que el cuerpo de la página no es un nodo coherente: lo devuelve
	// Check envuelto con el detalle de qué falló.
	//
	// Un CRC en verde no implica esto. El CRC garantiza que los bytes son los que se
	// escribieron; si quien los escribió dejó un slot apuntando fuera del área de celdas,
	// el CRC bendice la página y el error aparece al leerla, miles de operaciones después
	// de su causa. Esta comprobación es la que lo convierte en un fallo con dirección.
	ErrBadNode = errors.New("node: nodo mal formado")
)
