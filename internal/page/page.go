// Package page implementa el formato en disco de la página de 4096 bytes: la cabecera
// de 40 bytes de la sec. 5.1 del DESIGN.md, su checksum, y la verificación de que una
// página leída es íntegra y además es la que se pidió.
//
// El cuerpo de la página -- directorio de slots, espacio libre y celdas -- es opaco para
// este paquete: aquí solo viaja como un bloque de bytes. Quien lo interpreta es el
// B+tree de la F2. La separación es deliberada: el criterio de terminación de la F1 es
// serializar una página, releerla y detectar corrupción al alterar un byte, y eso se
// demuestra sin saber nada de claves ni de celdas.
package page

import (
	"encoding/binary"
	"hash/crc32"
)

const (
	// Size es el tamaño de una página, igual al tamaño de bloque del sistema de
	// archivos (DESIGN.md sec. 5.1). Es la unidad mínima de lectura y de escritura.
	Size = 4096

	// HeaderSize es la cabecera: crc32(4) + page_id(8) + page_lsn(8) + tipo(1) +
	// flags(1) + nceldas(2) + libre_fin(2) + libre(2) + relleno(4) + enlace(8).
	// Ver docs/DEUDA-DISENO.md, D3, sobre los 4 bytes de relleno.
	HeaderSize = 40

	// BodySize es lo que queda para slots, espacio libre y celdas.
	BodySize = Size - HeaderSize
)

// Offsets de cada campo dentro de la cabecera. Se declaran una sola vez y se usan tanto
// al codificar como al decodificar: dos juegos de literales en dos funciones distintas
// es exactamente la clase de discrepancia que un round-trip no siempre detecta -- si
// ambos lados se equivocan igual, el test pasa y el formato en disco está mal.
const (
	offCRC     = 0
	offPageID  = 4
	offPageLSN = 12
	offType    = 20
	offFlags   = 21
	offNCells  = 22
	offFreeEnd = 24
	offFree    = 26
	offPadding = 28
	offLink    = 32
)

// Type es el campo tipo de la cabecera (DESIGN.md sec. 5.1).
type Type uint8

const (
	TypeInternal Type = 1 // nodo interno
	TypeLeaf     Type = 2 // hoja
	TypeMeta     Type = 3 // página meta (sec. 5.2)
)

// valid informa si t es uno de los tres tipos que declara el formato.
func (t Type) valid() bool {
	return t == TypeInternal || t == TypeLeaf || t == TypeMeta
}

// castagnoliTable fija el polinomio del CRC en Castagnoli, el mismo que usa
// internal/record. DESIGN.md pide "crc32" sin especificar cuál -- ver
// docs/DEUDA-DISENO.md, D2.
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// Page es una página decodificada: los campos de la cabecera, más el cuerpo sin
// interpretar.
type Page struct {
	// ID es el número de página que esta página cree ser. Se verifica contra la ranura
	// desde la que se leyó (ver ErrWrongPage).
	ID uint64

	// LSN es el LSN del último registro de log que modificó esta página. Es lo que hace
	// cumplible la regla del write-ahead frente al desalojo del caché (DESIGN.md sec.
	// 7.5). En la F1 nadie lo consulta todavía: viaja en el formato porque cambiar el
	// layout en la F3, con páginas ya escritas en disco, costaría una migración.
	LSN uint64

	Type  Type
	Flags uint8 // reservado por la sec. 5.1

	// NCells, FreeEnd y Free describen la ocupación del cuerpo. Este paquete los
	// transporta sin interpretarlos; el B+tree de la F2 es quien los mantiene.
	NCells  uint16
	FreeEnd uint16
	Free    uint16

	// Link es el puntero al hijo más a la derecha si Type es TypeInternal, y el puntero
	// a la hoja siguiente si es TypeLeaf (DESIGN.md sec. 5.1).
	Link uint64

	// Body son los BodySize bytes de slots, espacio libre y celdas. Opaco aquí.
	Body []byte
}

// New devuelve una página vacía del tipo indicado, con el cuerpo a ceros y la frontera
// del espacio libre en sus extremos: sin celdas, el espacio libre es todo el cuerpo.
func New(id uint64, t Type) *Page {
	return &Page{
		ID:      id,
		Type:    t,
		Free:    HeaderSize,
		FreeEnd: Size,
		Body:    make([]byte, BodySize),
	}
}

// EncodeTo serializa p sobre dst, que debe medir exactamente Size bytes, y calcula el
// CRC. Escribe sobre un buffer que le presta el llamador en vez de devolver uno nuevo
// porque el pager reutiliza los marcos de su caché: una asignación por escritura de
// página sería una asignación por operación del árbol.
func (p *Page) EncodeTo(dst []byte) error {
	if len(dst) != Size {
		return ErrBadSize
	}
	if len(p.Body) != BodySize {
		return ErrBadSize
	}
	if !p.Type.valid() {
		return ErrBadType
	}

	binary.LittleEndian.PutUint64(dst[offPageID:], p.ID)
	binary.LittleEndian.PutUint64(dst[offPageLSN:], p.LSN)
	dst[offType] = byte(p.Type)
	dst[offFlags] = p.Flags
	binary.LittleEndian.PutUint16(dst[offNCells:], p.NCells)
	binary.LittleEndian.PutUint16(dst[offFreeEnd:], p.FreeEnd)
	binary.LittleEndian.PutUint16(dst[offFree:], p.Free)
	// El relleno se pone a cero siempre, no se deja con lo que hubiera en el buffer
	// reutilizado: si algún día pasa a ser un campo con significado, las páginas
	// escritas hoy tendrán ahí un valor conocido y no basura del marco anterior.
	clear(dst[offPadding:offLink])
	binary.LittleEndian.PutUint64(dst[offLink:], p.Link)
	copy(dst[HeaderSize:], p.Body)

	// El CRC cubre los 4092 bytes que le siguen, cabecera incluida: si solo cubriera el
	// cuerpo, un page_id o un page_lsn alterados en disco serían indetectables.
	sum := crc32.Checksum(dst[offCRC+4:], castagnoliTable)
	binary.LittleEndian.PutUint32(dst[offCRC:], sum)
	return nil
}

// Decode interpreta src como la página que ocupa la ranura wantID. Verifica en este
// orden: tamaño, CRC, page_id y tipo.
//
// El orden importa. Con el CRC en rojo, el page_id y el tipo que se leerían son bytes
// sin garantía ninguna, así que informar de ellos sería informar de basura; y el tamaño
// se comprueba antes que nada porque sin él ni siquiera se puede leer el CRC.
func Decode(src []byte, wantID uint64) (*Page, error) {
	if len(src) != Size {
		return nil, ErrBadSize
	}

	want := binary.LittleEndian.Uint32(src[offCRC:])
	if crc32.Checksum(src[offCRC+4:], castagnoliTable) != want {
		return nil, ErrCorrupt
	}

	id := binary.LittleEndian.Uint64(src[offPageID:])
	if id != wantID {
		return nil, ErrWrongPage
	}

	t := Type(src[offType])
	if !t.valid() {
		return nil, ErrBadType
	}

	p := &Page{
		ID:      id,
		LSN:     binary.LittleEndian.Uint64(src[offPageLSN:]),
		Type:    t,
		Flags:   src[offFlags],
		NCells:  binary.LittleEndian.Uint16(src[offNCells:]),
		FreeEnd: binary.LittleEndian.Uint16(src[offFreeEnd:]),
		Free:    binary.LittleEndian.Uint16(src[offFree:]),
		Link:    binary.LittleEndian.Uint64(src[offLink:]),
		Body:    make([]byte, BodySize),
	}
	// Copia, no subsector de src: el pager reutiliza sus buffers, y una Page que
	// apuntara al buffer del caché cambiaría de contenido bajo los pies de quien la
	// tiene cuando ese marco se reasignara a otra página.
	copy(p.Body, src[HeaderSize:])
	return p, nil
}
