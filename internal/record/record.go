package record

import (
	"encoding/binary"
	"hash/crc32"
)

// Type identifica la clase de carga que transporta un registro. Este paquete no
// interpreta la carga: solo la enmarca, delimita y protege con CRC. El significado de
// cada valor de Type lo fija la fase que lo use -- la F3, para tipo=1 (imagen de
// página) y tipo=2 (commit), DESIGN.md sec. 7.3.
type Type uint8

const (
	// headerSize es el tamaño fijo de la cabecera, antes de la carga:
	// lsn(8) + tipo(1) + epoca(4) + longitud(4) = 17 bytes.
	headerSize = 17
	// crcSize es el ancho del crc32 al final del registro.
	crcSize = 4
	// frameOverhead es lo que un registro añade a su carga: cabecera + crc.
	frameOverhead = headerSize + crcSize
)

// MaxPayloadSize acota el campo de longitud de un registro. Si ese campo está
// corrompido, este límite evita reservar memoria arbitraria antes de haber podido
// siquiera verificar el CRC (DESIGN.md sec. 9.4: el lector nunca debe entrar en pánico
// ante bytes arbitrarios). 64 KiB es holgado frente a los 4104 bytes de una imagen de
// página completa (sec. 7.3), que es el registro más grande previsto.
const MaxPayloadSize = 64 * 1024

// castagnoliTable fija el polinomio del CRC en Castagnoli. DESIGN.md pide "crc32" sin
// especificar cuál -- ver docs/DEUDA-DISENO.md, D2.
var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// Record es un registro ya decodificado: cabecera más carga.
type Record struct {
	LSN     uint64
	Type    Type
	Epoch   uint32
	Payload []byte
}

// header es la cabecera de un registro, antes de leer su carga.
type header struct {
	lsn        uint64
	typ        Type
	epoch      uint32
	payloadLen uint32
}

// encode serializa r en su representación de disco: cabecera, carga, y un crc32 que
// cubre ambas -- si el crc solo cubriera la carga, una longitud corrompida en la
// cabecera sería indetectable antes de usarla para decidir cuántos bytes leer.
func encode(r Record) ([]byte, error) {
	if len(r.Payload) > MaxPayloadSize {
		return nil, ErrTooLarge
	}
	total := frameOverhead + len(r.Payload)
	buf := make([]byte, total)
	binary.LittleEndian.PutUint64(buf[0:8], r.LSN)
	buf[8] = byte(r.Type)
	binary.LittleEndian.PutUint32(buf[9:13], r.Epoch)
	binary.LittleEndian.PutUint32(buf[13:17], uint32(len(r.Payload)))
	copy(buf[headerSize:], r.Payload)

	sum := crc32.Checksum(buf[:headerSize+len(r.Payload)], castagnoliTable)
	binary.LittleEndian.PutUint32(buf[total-crcSize:], sum)
	return buf, nil
}

// decodeHeader interpreta los headerSize bytes de la cabecera de un registro. No valida
// el CRC: para eso hace falta también la carga, y es responsabilidad del Reader, que es
// quien la lee después de conocer su longitud.
func decodeHeader(buf []byte) (header, error) {
	if len(buf) != headerSize {
		return header{}, ErrCorrupt
	}
	h := header{
		lsn:        binary.LittleEndian.Uint64(buf[0:8]),
		typ:        Type(buf[8]),
		epoch:      binary.LittleEndian.Uint32(buf[9:13]),
		payloadLen: binary.LittleEndian.Uint32(buf[13:17]),
	}
	if h.payloadLen > MaxPayloadSize {
		return header{}, ErrTooLarge
	}
	return h, nil
}

// verifyCRC recalcula el crc32 de cabecera+carga y lo compara con el que trae el
// registro.
func verifyCRC(headerAndPayload []byte, want uint32) bool {
	return crc32.Checksum(headerAndPayload, castagnoliTable) == want
}
