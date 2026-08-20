package record

import (
	"encoding/binary"
	"io"
)

// Reader lee registros secuencialmente desde un File, a partir de un desplazamiento
// dado.
//
// Implementa la regla de DESIGN.md sec. 8 paso 3: el primer registro que falle su
// verificación marca el punto exacto donde ocurrió una caída, y ahí se detiene la
// lectura -- sin buscar hacia adelante, aunque más allá del fallo hubiera bytes que
// parecieran un registro válido. Por eso Next es idempotente tras un error: una vez que
// devuelve io.EOF o uno de los errores de este paquete, sigue devolviendo exactamente
// eso en cualquier llamada posterior, en vez de intentar continuar.
type Reader struct {
	f       File
	offset  int64
	lastErr error
}

// NewReader crea un Reader que empieza a leer en offset.
func NewReader(f File, offset int64) *Reader {
	return &Reader{f: f, offset: offset}
}

// Offset devuelve la posición donde se detuvo la lectura: el final del último registro
// válido devuelto por Next, o el punto de partida si todavía no se ha leído ninguno.
// Tras un error, es el punto exacto donde ocurrió la caída (sec. 8) -- la F3 reanuda la
// escritura justo ahí.
func (r *Reader) Offset() int64 {
	return r.offset
}

// Next lee y devuelve el siguiente registro.
//
//   - Devuelve io.EOF cuando el archivo termina exactamente al final de un registro:
//     terminación limpia, no un fallo.
//   - Devuelve ErrTruncated cuando el archivo termina a mitad de un registro: hay menos
//     bytes de los que la cabecera prometía.
//   - Devuelve ErrCorrupt cuando el crc32 no coincide con lo leído.
//   - Devuelve ErrTooLarge cuando el campo de longitud de la cabecera excede
//     MaxPayloadSize -- una cabecera corrupta no debe provocar una reserva de memoria
//     arbitraria.
//
// Cualquier otro error proviene directamente del File subyacente (por ejemplo, un fallo
// real de lectura) y se propaga tal cual.
//
// En cualquiera de estos casos, Next queda "enganchado" en ese resultado: llamadas
// posteriores devuelven el mismo error sin volver a tocar el archivo. Ver el comentario
// del tipo Reader.
func (r *Reader) Next() (Record, error) {
	if r.lastErr != nil {
		return Record{}, r.lastErr
	}
	rec, err := r.next()
	if err != nil {
		r.lastErr = err
	}
	return rec, err
}

func (r *Reader) next() (Record, error) {
	hbuf := make([]byte, headerSize)
	if err := r.readFull(hbuf, r.offset); err != nil {
		if err == io.ErrUnexpectedEOF {
			return Record{}, ErrTruncated
		}
		return Record{}, err // io.EOF (final limpio) u otro error de E/S
	}

	h, err := decodeHeader(hbuf)
	if err != nil {
		return Record{}, err
	}

	// tail son la carga y el crc32 juntos: se leen de una vez porque el crc cubre
	// cabecera+carga, y hace falta el crc para poder verificar cualquiera de las dos.
	tail := make([]byte, int(h.payloadLen)+crcSize)
	if err := r.readFull(tail, r.offset+headerSize); err != nil {
		// A esta altura ya se leyó una cabecera completa: si el archivo se corta aquí,
		// nunca es un final limpio, siempre es un registro a medias.
		return Record{}, ErrTruncated
	}

	payload := tail[:h.payloadLen]
	want := binary.LittleEndian.Uint32(tail[h.payloadLen:])

	full := make([]byte, headerSize+int(h.payloadLen))
	copy(full, hbuf)
	copy(full[headerSize:], payload)
	if !verifyCRC(full, want) {
		return Record{}, ErrCorrupt
	}

	rec := Record{
		LSN:     h.lsn,
		Type:    h.typ,
		Epoch:   h.epoch,
		Payload: payload[:len(payload):len(payload)],
	}
	r.offset += int64(len(full) + crcSize)
	return rec, nil
}

// readFull llena buf leyendo desde off. Devuelve io.EOF si no había ningún byte
// disponible (final limpio del archivo) o io.ErrUnexpectedEOF si había algunos pero no
// los suficientes (registro truncado) -- exactamente la distinción que io.ReadFull ya
// hace sobre un io.Reader, aplicada aquí a un File de acceso aleatorio.
func (r *Reader) readFull(buf []byte, off int64) error {
	_, err := io.ReadFull(io.NewSectionReader(r.f, off, int64(len(buf))), buf)
	return err
}
