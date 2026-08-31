// Package node interpreta el cuerpo de la página: el directorio de slots, el espacio
// libre y las celdas de la sec. 5.1 del DESIGN.md. Es la mitad del formato que la F1
// dejó deliberadamente opaca (ver internal/page/page.go), y sobre la que se apoyan el
// descenso, la división y la fusión del B+tree.
//
// # Qué no hace
//
// No conoce el árbol. No sabe qué página es la raíz, no desciende, no divide y no habla
// con el pager. Un nodo es una página y nada más: quien decide que una hoja llena debe
// partirse es el árbol, que recibe ErrNoSpace y actúa.
//
// Tampoco marca nada como sucio. Mutar un nodo no ensucia su página; eso lo hace el árbol
// con pager.MarkDirty, dentro del grupo de commit que abrió. Mezclar las dos cosas aquí
// produciría un paquete que necesita un pager para probarse, y estos son 4096 bytes de
// aritmética de offsets que deben poder probarse solos.
//
// # Offsets absolutos
//
// Los offsets del formato -- libre, libre_fin y cada entrada del directorio de slots --
// son relativos a la página, no al cuerpo. El cuerpo empieza en page.HeaderSize, así que
// toda traducción pasa por idx, una sola vez y en un solo sitio. Es el mismo argumento que
// internal/page/page.go da para declarar sus offsets una vez: dos aritméticas en dos
// funciones distintas es la clase de discrepancia que un round-trip no detecta, porque si
// las dos se equivocan igual el test pasa.
package node

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"

	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

const (
	// MaxKeySize es el tope de la clave (DESIGN.md sec. 4).
	MaxKeySize = 512

	// MaxEntrySize es el tope de clave + valor + LeafCellOverhead (DESIGN.md sec. 4). El
	// número sale de exigir cuatro celdas por página para que una hoja llena siempre se
	// pueda dividir en dos mitades razonables; lo comprueba TestCuatroCeldasDeMilBytes.
	MaxEntrySize = 1000

	// LeafCellOverhead es key_len(2) + val_len(4). Son los "6 bytes de cabecera de celda"
	// que la sec. 4 suma dentro de MaxEntrySize.
	LeafCellOverhead = 6

	// InternalCellOverhead es key_len(2) + hijo_izq(8).
	InternalCellOverhead = 10

	// SlotSize es lo que ocupa una entrada del directorio: un uint16 con el offset
	// absoluto de su celda.
	SlotSize = 2
)

// Desplazamientos dentro de una celda, declarados una vez igual que los de la cabecera.
const (
	offKeyLen   = 0 // en los dos formatos
	offValLen   = 2 // solo hoja
	offChild    = 2 // solo nodo interno
	offLeafKey  = LeafCellOverhead
	offInnerKey = InternalCellOverhead
)

// Node es la vista de nodo de una página. Es un envoltorio sin estado: toda la verdad
// sigue viviendo en la página, que es lo que el pager cachea y lo que el WAL fotografía en
// CommitGroup. Un Node con campos derivados -- un contador de celdas propio, un índice de
// claves -- se desincronizaría de la imagen que se registra, y el disco acabaría contando
// una historia distinta de la memoria.
type Node struct {
	p *page.Page
}

// Of envuelve p. No valida nada: para eso está Check, que es caro y se invoca donde toca.
func Of(p *page.Page) Node { return Node{p} }

// Page devuelve la página envuelta, que es la que hay que pasar a pager.MarkDirty.
func (n Node) Page() *page.Page { return n.p }

// IsLeaf informa si el nodo es una hoja. Las hojas guardan valores; los nodos internos,
// punteros a hijos.
func (n Node) IsLeaf() bool { return n.p.Type == page.TypeLeaf }

// NCells es cuántas celdas contiene el nodo.
func (n Node) NCells() int { return int(n.p.NCells) }

// Link es el puntero al hijo más a la derecha si el nodo es interno, y el puntero a la
// hoja siguiente si es hoja (DESIGN.md sec. 5.1). Los dos usos comparten campo porque son
// excluyentes por tipo.
func (n Node) Link() uint64 { return n.p.Link }

// SetLink fija ese puntero. Quien mantiene la cadena lateral entre hojas -- de la que
// depende el invariante 5 -- es el árbol, no este paquete.
func (n Node) SetLink(v uint64) { n.p.Link = v }

// idx traduce un offset absoluto de página a un índice del cuerpo.
func idx(off uint16) int { return int(off) - page.HeaderSize }

// cellOverhead es la sobrecarga de celda del tipo de este nodo.
func (n Node) cellOverhead() int {
	if n.IsLeaf() {
		return LeafCellOverhead
	}
	return InternalCellOverhead
}

// slot devuelve el offset absoluto de la celda i.
func (n Node) slot(i int) uint16 {
	return binary.LittleEndian.Uint16(n.p.Body[i*SlotSize:])
}

func (n Node) setSlot(i int, off uint16) {
	binary.LittleEndian.PutUint16(n.p.Body[i*SlotSize:], off)
}

// syncFree recalcula el campo libre de la cabecera. El área de slots es
// [HeaderSize, HeaderSize + SlotSize*nceldas), así que libre es siempre derivable de
// nceldas: se escribe porque el formato lo declara, nunca se lee como fuente de verdad, y
// Check comprueba que las dos cuentas coinciden. Ver docs/DEUDA-DISENO.md, D5.
func (n Node) syncFree() {
	n.p.Free = uint16(page.HeaderSize + SlotSize*int(n.p.NCells))
}

// cellSize es lo que ocupa la celda que empieza en off, cabecera de celda incluida.
// Presupone que la celda es legible; en el camino no confiable eso lo garantiza Check.
func (n Node) cellSize(off uint16) int {
	b := n.p.Body[idx(off):]
	klen := int(binary.LittleEndian.Uint16(b[offKeyLen:]))
	if !n.IsLeaf() {
		return InternalCellOverhead + klen
	}
	return LeafCellOverhead + klen + int(binary.LittleEndian.Uint32(b[offValLen:]))
}

// CellSize es lo que ocupa la celda i en el área de celdas, sin contar su slot. Lo consume
// la división, que necesita repartir por bytes y no por número de celdas: con celdas de
// tamaño variable, partir por la mitad del índice puede dejar una página al 90% y la otra
// al 10%.
func (n Node) CellSize(i int) int { return n.cellSize(n.slot(i)) }

// Key es la clave de la celda i. Exige 0 <= i < NCells; fuera de ese rango indexa mal, como
// cualquier acceso a un slice.
//
// No devuelve error, a diferencia de Value y Child, porque no puede confundirse de formato:
// key_len vive en el offset 0 de los dos tipos de celda y el desplazamiento hasta la clave
// sale de cellOverhead. El único riesgo que le queda es el índice, y está en el camino
// caliente -- cada nivel de cada descenso hace log2(200) comparaciones.
//
// Devuelve un subsector del cuerpo de la página, no una copia: válido hasta la siguiente
// mutación de este nodo. Copiar pondría una asignación en cada una de esas comparaciones, y
// la página ya paga una copia al decodificarse (internal/page/page.go, Decode). Quien
// necesite conservar la clave más allá de la siguiente mutación, copia; el Get público de
// la F3 copiará antes de devolver. Ver docs/DEUDA-DISENO.md, D6.
func (n Node) Key(i int) []byte {
	off := idx(n.slot(i))
	b := n.p.Body
	klen := int(binary.LittleEndian.Uint16(b[off+offKeyLen:]))
	ini := off + n.cellOverhead()
	return b[ini : ini+klen]
}

// Value es el valor de la celda i. Solo en hojas. Rige la misma regla de validez que Key.
func (n Node) Value(i int) ([]byte, error) {
	if !n.IsLeaf() {
		return nil, ErrWrongType
	}
	if i < 0 || i >= n.NCells() {
		return nil, ErrOutOfRange
	}
	off := idx(n.slot(i))
	b := n.p.Body
	klen := int(binary.LittleEndian.Uint16(b[off+offKeyLen:]))
	vlen := int(binary.LittleEndian.Uint32(b[off+offValLen:]))
	ini := off + LeafCellOverhead + klen
	return b[ini : ini+vlen], nil
}

// Child es el hijo izquierdo de la celda i. Solo en nodos internos; el hijo derecho del
// nodo entero es Link.
func (n Node) Child(i int) (uint64, error) {
	if n.p.Type != page.TypeInternal {
		return 0, ErrWrongType
	}
	if i < 0 || i >= n.NCells() {
		return 0, ErrOutOfRange
	}
	off := idx(n.slot(i))
	return binary.LittleEndian.Uint64(n.p.Body[off+offChild:]), nil
}

// SetChild cambia el hijo izquierdo de la celda i. Solo en nodos internos.
//
// Lo necesita la propagación de una división: cuando el hijo de la celda i se parte en dos,
// la celda nueva hereda la mitad izquierda y la que ya estaba -- que conserva su clave y
// pasa a la posición i+1 -- tiene que apuntar a la derecha. Sin este setter esa operación
// serían un borrado y dos inserciones, y un ErrNoSpace en la segunda dejaría al padre con un
// separador de menos.
//
// No mueve bytes: el puntero mide 8 en los dos casos, así que no puede fallar por espacio ni
// alterar el layout.
func (n Node) SetChild(i int, hijo uint64) error {
	if n.p.Type != page.TypeInternal {
		return ErrWrongType
	}
	if i < 0 || i >= n.NCells() {
		return ErrOutOfRange
	}
	off := idx(n.slot(i))
	binary.LittleEndian.PutUint64(n.p.Body[off+offChild:], hijo)
	return nil
}

// Search hace búsqueda binaria sobre el directorio de slots. Devuelve el índice de la clave
// si está, y si no el punto donde habría que insertarla -- que es exactamente lo que
// necesitan tanto InsertHoja como el descenso, y por eso no devuelve solo un booleano.
func (n Node) Search(key []byte) (int, bool) {
	lo, hi := 0, n.NCells()
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		switch bytes.Compare(n.Key(mid), key) {
		case 0:
			return mid, true
		case -1:
			lo = mid + 1
		default:
			hi = mid
		}
	}
	return lo, false
}

// FreeContiguo es el hueco entre el final del directorio de slots y el principio del área
// de celdas: lo que se puede usar sin compactar.
func (n Node) FreeContiguo() int { return int(n.p.FreeEnd) - int(n.p.Free) }

// FreeTotal es FreeContiguo más los bytes muertos que dejaron las celdas borradas.
//
// Se deriva recorriendo el directorio en vez de guardarse en un contador, que es lo que D3
// decidió para los 4 bytes de relleno de la cabecera y por la misma razón: un contador
// desactualizado tras un borrado produce una página que se cree llena estando vacía, y el
// fallo aparece miles de operaciones después de su causa. Recorrer son <=200 iteraciones
// sobre memoria que ya está en el caché.
func (n Node) FreeTotal() int {
	vivos := 0
	for i := 0; i < n.NCells(); i++ {
		vivos += n.cellSize(n.slot(i))
	}
	return page.Size - int(n.p.Free) - vivos
}

// Compactar reconstruye el área de celdas dejándolas contiguas contra el final de la
// página, en el orden del directorio. Recupera los bytes muertos.
//
// Deja a cero el espacio libre resultante. No hace falta para la correctitud -- son bytes
// inalcanzables --, pero hace que la imagen de la página sea función de su contenido
// lógico y no de la historia de borrados que la trajo hasta aquí. Dos páginas con las
// mismas claves se comparan byte a byte, que es justo lo que se quiere tener a mano cuando
// la F4 esté comparando páginas recuperadas contra páginas esperadas.
func (n Node) Compactar() {
	tmp := make([]byte, page.BodySize)
	fin := uint16(page.Size)
	for i := 0; i < n.NCells(); i++ {
		off := n.slot(i)
		sz := n.cellSize(off)
		fin -= uint16(sz)
		copy(tmp[idx(fin):], n.p.Body[idx(off):idx(off)+sz])
		n.setSlot(i, fin)
	}
	clear(n.p.Body[idx(n.p.Free):idx(fin)])
	copy(n.p.Body[idx(fin):], tmp[idx(fin):])
	n.p.FreeEnd = fin
}

// hazSitio garantiza espacio contiguo para need bytes, compactando si hace falta.
func (n Node) hazSitio(need int) error {
	if n.FreeContiguo() >= need {
		return nil
	}
	if n.FreeTotal() < need {
		return ErrNoSpace
	}
	n.Compactar()
	if n.FreeContiguo() < need {
		// Inalcanzable: tras compactar, contiguo y total coinciden. Se comprueba igual,
		// porque si alguna vez dejaran de coincidir el paso siguiente escribiría celdas
		// encima del directorio de slots.
		return ErrNoSpace
	}
	return nil
}

// abreSlot inserta un hueco en la posición i del directorio y actualiza la contabilidad.
// Presupone que el espacio ya está reservado.
func (n Node) abreSlot(i int, off uint16) {
	b := n.p.Body
	copy(b[(i+1)*SlotSize:(n.NCells()+1)*SlotSize], b[i*SlotSize:n.NCells()*SlotSize])
	n.p.NCells++
	n.syncFree()
	n.setSlot(i, off)
}

// InsertHoja inserta una celda de hoja en la posición i del directorio. El llamador obtiene
// i de Search; insertar en NCells -- al final -- es legítimo.
//
// Devuelve ErrNoSpace si no cabe ni compactando, que es la señal de división. Cuando
// devuelve error, el contenido lógico de la página queda intacto: como mucho ha
// compactado, que no cambia lo que la página dice.
func (n Node) InsertHoja(i int, key, val []byte) error {
	if !n.IsLeaf() {
		return ErrWrongType
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	if len(key)+len(val)+LeafCellOverhead > MaxEntrySize {
		return ErrEntryTooLarge
	}
	if i < 0 || i > n.NCells() {
		return ErrOutOfRange
	}

	sz := LeafCellOverhead + len(key) + len(val)
	if err := n.hazSitio(sz + SlotSize); err != nil {
		return err
	}

	off := n.p.FreeEnd - uint16(sz)
	c := n.p.Body[idx(off):]
	binary.LittleEndian.PutUint16(c[offKeyLen:], uint16(len(key)))
	binary.LittleEndian.PutUint32(c[offValLen:], uint32(len(val)))
	copy(c[offLeafKey:], key)
	copy(c[offLeafKey+len(key):], val)

	n.p.FreeEnd = off
	n.abreSlot(i, off)
	return nil
}

// InsertInterno inserta una celda separadora con su hijo izquierdo en la posición i.
//
// No lleva comprobación de MaxEntrySize: una celda de nodo interno son 10 bytes más la
// clave, así que el tope de MaxKeySize ya la acota en 522 bytes.
func (n Node) InsertInterno(i int, key []byte, hijo uint64) error {
	if n.p.Type != page.TypeInternal {
		return ErrWrongType
	}
	if len(key) > MaxKeySize {
		return ErrKeyTooLarge
	}
	if i < 0 || i > n.NCells() {
		return ErrOutOfRange
	}

	sz := InternalCellOverhead + len(key)
	if err := n.hazSitio(sz + SlotSize); err != nil {
		return err
	}

	off := n.p.FreeEnd - uint16(sz)
	c := n.p.Body[idx(off):]
	binary.LittleEndian.PutUint16(c[offKeyLen:], uint16(len(key)))
	binary.LittleEndian.PutUint64(c[offChild:], hijo)
	copy(c[offInnerKey:], key)

	n.p.FreeEnd = off
	n.abreSlot(i, off)
	return nil
}

// Delete quita la celda i del directorio. Los bytes de la celda quedan muertos hasta la
// siguiente compactación: recuperarlos aquí obligaría a mover el área entera en cada
// borrado, y la compactación ya la dispara Insert cuando de verdad hace falta el espacio.
func (n Node) Delete(i int) error {
	if i < 0 || i >= n.NCells() {
		return ErrOutOfRange
	}
	b := n.p.Body
	copy(b[i*SlotSize:], b[(i+1)*SlotSize:n.NCells()*SlotSize])
	n.p.NCells--
	n.syncFree()
	return nil
}

// Check comprueba que el cuerpo es un nodo coherente: el directorio cuadra con nceldas,
// cada celda cae entera dentro del área de celdas, ninguna se solapa con otra, y las claves
// están estrictamente ordenadas (invariante 1 de la sec. 6).
//
// Es la comprobación local que Validate() usará página a página, y la propiedad que el
// fuzzer exige: si Check devuelve nil, recorrer el nodo entero con Key, Value y Child no
// puede entrar en pánico. Por eso valida las longitudes antes de fiarse de ellas, y no al
// revés.
//
// Un CRC en verde no da nada de esto. El CRC dice que los bytes son los que se escribieron;
// si quien los escribió dejó un slot apuntando fuera del área de celdas, la página pasa el
// CRC y el fallo aparece al leerla.
//
// Ordena una copia de los intervalos de celda para detectar solapamientos. Sumar tamaños no
// bastaría: dos celdas que se pisan pueden sumar exactamente lo que hay disponible.
func (n Node) Check() error {
	if len(n.p.Body) != page.BodySize {
		return fmt.Errorf("%w: el cuerpo mide %d y no %d", ErrBadNode, len(n.p.Body), page.BodySize)
	}
	if n.p.Type != page.TypeLeaf && n.p.Type != page.TypeInternal {
		return fmt.Errorf("%w: tipo %d", ErrWrongType, n.p.Type)
	}

	nc := n.NCells()
	if quiere := page.HeaderSize + SlotSize*nc; int(n.p.Free) != quiere {
		return fmt.Errorf("%w: libre es %d y nceldas=%d exige %d",
			ErrBadNode, n.p.Free, nc, quiere)
	}
	if int(n.p.FreeEnd) < int(n.p.Free) || int(n.p.FreeEnd) > page.Size {
		return fmt.Errorf("%w: libre_fin es %d, fuera de [%d, %d]",
			ErrBadNode, n.p.FreeEnd, n.p.Free, page.Size)
	}

	ov := n.cellOverhead()
	type intervalo struct{ ini, fin int }
	celdas := make([]intervalo, 0, nc)

	for i := 0; i < nc; i++ {
		off := int(n.slot(i))
		if off < int(n.p.FreeEnd) || off > page.Size-ov {
			return fmt.Errorf("%w: el slot %d apunta a %d, fuera de [%d, %d]",
				ErrBadNode, i, off, n.p.FreeEnd, page.Size-ov)
		}
		b := n.p.Body[idx(uint16(off)):]
		fin := off + ov + int(binary.LittleEndian.Uint16(b[offKeyLen:]))
		if n.IsLeaf() {
			fin += int(binary.LittleEndian.Uint32(b[offValLen:]))
		}
		if fin > page.Size {
			return fmt.Errorf("%w: la celda %d acaba en %d, pasada la pagina", ErrBadNode, i, fin)
		}
		celdas = append(celdas, intervalo{off, fin})
	}

	// A partir de aquí toda celda es legible, así que Key ya no puede desbordar.
	for i := 1; i < nc; i++ {
		if bytes.Compare(n.Key(i-1), n.Key(i)) >= 0 {
			return fmt.Errorf("%w: las claves %d y %d no estan estrictamente ordenadas",
				ErrBadNode, i-1, i)
		}
	}

	slices.SortFunc(celdas, func(a, b intervalo) int { return a.ini - b.ini })
	for i := 1; i < nc; i++ {
		if celdas[i].ini < celdas[i-1].fin {
			return fmt.Errorf("%w: dos celdas se solapan en [%d, %d)",
				ErrBadNode, celdas[i].ini, celdas[i-1].fin)
		}
	}
	return nil
}
