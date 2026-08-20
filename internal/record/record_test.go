package record

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// memFile es un File en memoria. Se usa para manipular bytes con precisión exacta --
// truncar en un offset concreto, voltear un bit concreto -- sin depender de un archivo
// real en disco. Implementa el mismo contrato de io.ReaderAt que *os.File: si ReadAt
// devuelve menos bytes de los pedidos, siempre acompaña un error no nulo.
type memFile struct {
	data []byte
}

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if end > int64(len(m.data)) {
		grown := make([]byte, end)
		copy(grown, m.data)
		m.data = grown
	}
	copy(m.data[off:end], p)
	return len(p), nil
}

func (m *memFile) Sync() error { return nil }

func (m *memFile) Truncate(size int64) error {
	if size <= int64(len(m.data)) {
		m.data = m.data[:size]
		return nil
	}
	grown := make([]byte, size)
	copy(grown, m.data)
	m.data = grown
	return nil
}

var _ File = (*memFile)(nil)

// Comprobación en tiempo de compilación de la afirmación de file.go: *os.File satisface
// File sin necesidad de un tipo envoltorio.
var _ File = (*os.File)(nil)

// TestRoundTripSizes cubre los extremos de tamaño, incluidos los dos que la F3 usará de
// verdad (sec. 7.3): 28 bytes para un commit, 4104 para una imagen de página.
func TestRoundTripSizes(t *testing.T) {
	sizes := []int{0, 1, 28, 4104, MaxPayloadSize}
	for _, size := range sizes {
		size := size
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i)
			}
			want := Record{LSN: 42, Type: 1, Epoch: 7, Payload: payload}

			f := &memFile{}
			w := NewWriter(f, 0)
			if err := w.Append(want); err != nil {
				t.Fatalf("Append: %v", err)
			}

			r := NewReader(f, 0)
			got, err := r.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got.LSN != want.LSN || got.Type != want.Type || got.Epoch != want.Epoch {
				t.Fatalf("cabecera distinta: got %+v", got)
			}
			if !bytes.Equal(got.Payload, want.Payload) {
				t.Fatalf("carga distinta")
			}
			if r.Offset() != w.Offset() {
				t.Fatalf("Offset tras leer: got %d, want %d", r.Offset(), w.Offset())
			}

			if _, err := r.Next(); err != io.EOF {
				t.Fatalf("segundo Next: got %v, want io.EOF", err)
			}
		})
	}
}

// TestPayloadTooLarge: Append rechaza una carga que excede MaxPayloadSize antes de
// escribir nada.
func TestPayloadTooLarge(t *testing.T) {
	f := &memFile{}
	w := NewWriter(f, 0)
	err := w.Append(Record{Payload: make([]byte, MaxPayloadSize+1)})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if len(f.data) != 0 {
		t.Fatalf("Append escribió %d bytes pese al error", len(f.data))
	}
}

func TestEmptyFile(t *testing.T) {
	f := &memFile{}
	r := NewReader(f, 0)
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

// TestTruncation trunca un único registro en cada offset posible. Antes del final
// (cut==0) debe leerse como archivo vacío; a mitad (0<cut<total) siempre debe ser
// ErrTruncated, nunca un registro parcialmente aceptado.
func TestTruncation(t *testing.T) {
	rec := Record{LSN: 1, Type: 2, Epoch: 3, Payload: []byte("hola mundo, contenido de prueba")}

	full := &memFile{}
	w := NewWriter(full, 0)
	if err := w.Append(rec); err != nil {
		t.Fatal(err)
	}
	total := len(full.data)

	for cut := 0; cut < total; cut++ {
		cut := cut
		t.Run(fmt.Sprintf("cut=%d", cut), func(t *testing.T) {
			f := &memFile{data: append([]byte(nil), full.data[:cut]...)}
			r := NewReader(f, 0)
			_, err := r.Next()

			if cut == 0 {
				if err != io.EOF {
					t.Fatalf("cut=0: got %v, want io.EOF", err)
				}
				return
			}
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("cut=%d: got %v, want ErrTruncated", cut, err)
			}
			// Idempotencia: una segunda llamada da el mismo error, sin volver a tocar
			// el archivo (sec. 8 paso 3: no hay búsqueda hacia adelante).
			if _, err2 := r.Next(); !errors.Is(err2, ErrTruncated) {
				t.Fatalf("cut=%d: segunda llamada dio %v, want ErrTruncated de nuevo", cut, err2)
			}
		})
	}

	t.Run("completo", func(t *testing.T) {
		r := NewReader(full, 0)
		got, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !bytes.Equal(got.Payload, rec.Payload) {
			t.Fatalf("carga distinta")
		}
	})
}

// TestTruncatedLastOfMultiple: con dos registros, el primero siempre se recupera
// entero y el segundo (el último, truncado) se descarta entero -- nunca a medias.
func TestTruncatedLastOfMultiple(t *testing.T) {
	first := Record{LSN: 1, Type: 1, Payload: []byte("primero")}
	second := Record{LSN: 2, Type: 1, Payload: []byte("segundo, este es mas largo que el primero")}

	full := &memFile{}
	w := NewWriter(full, 0)
	if err := w.Append(first); err != nil {
		t.Fatal(err)
	}
	firstEnd := w.Offset()
	if err := w.Append(second); err != nil {
		t.Fatal(err)
	}
	total := len(full.data)

	for cut := int(firstEnd); cut < total; cut++ {
		cut := cut
		t.Run(fmt.Sprintf("cut=%d", cut), func(t *testing.T) {
			f := &memFile{data: append([]byte(nil), full.data[:cut]...)}
			r := NewReader(f, 0)

			got, err := r.Next()
			if err != nil {
				t.Fatalf("primer registro: got err %v, want nil", err)
			}
			if !bytes.Equal(got.Payload, first.Payload) {
				t.Fatalf("primer registro: carga distinta")
			}
			if r.Offset() != firstEnd {
				t.Fatalf("Offset tras el primero: got %d, want %d", r.Offset(), firstEnd)
			}

			_, err = r.Next()
			if cut == int(firstEnd) {
				if err != io.EOF {
					t.Fatalf("segundo registro ausente: got %v, want io.EOF", err)
				}
				return
			}
			if !errors.Is(err, ErrTruncated) {
				t.Fatalf("segundo registro truncado: got %v, want ErrTruncated", err)
			}
		})
	}
}

// TestBitFlipCorruption voltea cada bit del registro -- cabecera, carga y crc -- y
// exige que el fallo se detecte siempre. Los cuatro bytes del campo de longitud son un
// caso aparte: cambiarlos puede dar ErrTooLarge en vez de ErrCorrupt, pero nunca nil.
func TestBitFlipCorruption(t *testing.T) {
	rec := Record{
		LSN:     0x0102030405060708,
		Type:    9,
		Epoch:   0xAABBCCDD,
		Payload: []byte("contenido de prueba, suficiente para varios bytes de carga"),
	}

	base := &memFile{}
	w := NewWriter(base, 0)
	if err := w.Append(rec); err != nil {
		t.Fatal(err)
	}
	recordEnd := int(w.Offset())

	// Relleno generoso detrás del registro: si el bit volteado cae en el campo de
	// longitud, siempre hay bytes de sobra para completar la lectura, así que lo único
	// que puede fallar es el CRC o el límite de tamaño -- nunca un truncamiento. Eso
	// mantiene la aserción de este test simple y sin ambigüedad.
	filler := make([]byte, MaxPayloadSize+crcSize+headerSize)
	base.data = append(base.data, filler...)

	totalBits := recordEnd * 8
	for bit := 0; bit < totalBits; bit++ {
		bit := bit
		t.Run(fmt.Sprintf("bit=%d", bit), func(t *testing.T) {
			data := append([]byte(nil), base.data...)
			byteIdx := bit / 8
			bitIdx := uint(bit % 8)
			data[byteIdx] ^= 1 << bitIdx

			f := &memFile{data: data}
			r := NewReader(f, 0)
			_, err := r.Next()

			isLengthByte := byteIdx >= 13 && byteIdx < headerSize
			switch {
			case err == nil:
				t.Fatalf("bit %d: se aceptó un registro corrupto", bit)
			case isLengthByte:
				if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrTooLarge) {
					t.Fatalf("bit %d (campo de longitud): got %v, want ErrCorrupt o ErrTooLarge", bit, err)
				}
			default:
				if !errors.Is(err, ErrCorrupt) {
					t.Fatalf("bit %d: got %v, want ErrCorrupt", bit, err)
				}
			}
		})
	}
}

// TestNoForwardSearch: bueno · malo · bueno. Tras el fallo del segundo registro, Next
// no debe saltar al tercero aunque esté intacto y perfectamente leíble más adelante.
// Esta es la regla de la sec. 8 paso 3 puesta a prueba directamente.
func TestNoForwardSearch(t *testing.T) {
	good1 := Record{LSN: 1, Type: 1, Payload: []byte("uno")}
	bad := Record{LSN: 2, Type: 1, Payload: []byte("dos, este se corrompe")}
	good2 := Record{LSN: 3, Type: 1, Payload: []byte("tres")}

	f := &memFile{}
	w := NewWriter(f, 0)
	if err := w.Append(good1); err != nil {
		t.Fatal(err)
	}
	badStart := w.Offset()
	if err := w.Append(bad); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(good2); err != nil {
		t.Fatal(err)
	}

	// Corrompe el primer byte de la carga de "bad", dejando good1 y good2 intactos.
	f.data[badStart+headerSize] ^= 0xFF

	r := NewReader(f, 0)

	got1, err := r.Next()
	if err != nil {
		t.Fatalf("primer registro: %v", err)
	}
	if !bytes.Equal(got1.Payload, good1.Payload) {
		t.Fatalf("primer registro: carga distinta")
	}

	if _, err := r.Next(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("segundo registro: got %v, want ErrCorrupt", err)
	}

	// Tercera llamada: debe repetir el mismo error, no saltar a good2.
	if _, err := r.Next(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tercera llamada: got %v, want el mismo ErrCorrupt (sin busqueda hacia adelante)", err)
	}
}

// TestAbsurdLength: una cabecera con un campo de longitud disparatado nunca debe
// provocar una reserva de memoria desmedida ni un pánico -- solo un error.
func TestAbsurdLength(t *testing.T) {
	cases := []uint32{0xFFFFFFFF, MaxPayloadSize + 1, 1 << 20}
	for _, length := range cases {
		length := length
		t.Run(fmt.Sprintf("long=%d", length), func(t *testing.T) {
			hbuf := make([]byte, headerSize)
			binary.LittleEndian.PutUint64(hbuf[0:8], 1)
			hbuf[8] = 1
			binary.LittleEndian.PutUint32(hbuf[9:13], 0)
			binary.LittleEndian.PutUint32(hbuf[13:17], length)

			f := &memFile{data: hbuf}
			r := NewReader(f, 0)
			if _, err := r.Next(); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("long=%d: got %v, want ErrTooLarge", length, err)
			}
		})
	}
}

// TestRealFileRoundTrip: el ciclo completo escribir→leer sobre un archivo real, no uno
// en memoria -- el criterio de terminación de la F0 (sec. 10) lo pide explícitamente.
func TestRealFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	recs := []Record{
		{LSN: 1, Type: 1, Epoch: 0, Payload: bytes.Repeat([]byte{0xAB}, 4104)}, // imagen de página, sec. 7.3
		{LSN: 2, Type: 2, Epoch: 0, Payload: bytes.Repeat([]byte{0xCD}, 28)},   // commit, sec. 7.3
	}

	w := NewWriter(f, 0)
	for _, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	r := NewReader(f, 0)
	for i, want := range recs {
		got, err := r.Next()
		if err != nil {
			t.Fatalf("registro %d: %v", i, err)
		}
		if got.LSN != want.LSN || got.Type != want.Type {
			t.Fatalf("registro %d: cabecera distinta", i)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("registro %d: carga distinta", i)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}
