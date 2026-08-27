package page

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"
)

// llena devuelve un cuerpo de página con un patrón no trivial. Un cuerpo a ceros haría
// pasar un EncodeTo que no copiara nada.
func llena() []byte {
	body := make([]byte, BodySize)
	for i := range body {
		body[i] = byte(i*7 + 3)
	}
	return body
}

// ejemplo devuelve una página con todos los campos de la cabecera en valores distintos
// entre sí. Que sean distintos es lo que hace que el round-trip detecte dos campos
// intercambiados: con ceros o con valores repetidos, leer FreeEnd donde va Free pasaría
// desapercibido.
func ejemplo(id uint64, t Type) *Page {
	return &Page{
		ID:      id,
		LSN:     0x0102030405060708,
		Type:    t,
		Flags:   0xAB,
		NCells:  1234,
		FreeEnd: 3000,
		Free:    600,
		Link:    0x1122334455667788,
		Body:    llena(),
	}
}

func iguales(t *testing.T, got, want *Page) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("ID: got %d, want %d", got.ID, want.ID)
	}
	if got.LSN != want.LSN {
		t.Errorf("LSN: got %d, want %d", got.LSN, want.LSN)
	}
	if got.Type != want.Type {
		t.Errorf("Type: got %d, want %d", got.Type, want.Type)
	}
	if got.Flags != want.Flags {
		t.Errorf("Flags: got %d, want %d", got.Flags, want.Flags)
	}
	if got.NCells != want.NCells {
		t.Errorf("NCells: got %d, want %d", got.NCells, want.NCells)
	}
	if got.FreeEnd != want.FreeEnd {
		t.Errorf("FreeEnd: got %d, want %d", got.FreeEnd, want.FreeEnd)
	}
	if got.Free != want.Free {
		t.Errorf("Free: got %d, want %d", got.Free, want.Free)
	}
	if got.Link != want.Link {
		t.Errorf("Link: got %d, want %d", got.Link, want.Link)
	}
	if !bytes.Equal(got.Body, want.Body) {
		t.Errorf("Body distinto")
	}
}

// TestRoundTrip es el criterio literal de la F1: se serializa una página y se relee.
func TestRoundTrip(t *testing.T) {
	for _, typ := range []Type{TypeInternal, TypeLeaf, TypeMeta} {
		t.Run(fmt.Sprintf("tipo=%d", typ), func(t *testing.T) {
			want := ejemplo(99, typ)

			buf := make([]byte, Size)
			if err := want.EncodeTo(buf); err != nil {
				t.Fatalf("EncodeTo: %v", err)
			}

			got, err := Decode(buf, 99)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			iguales(t, got, want)
		})
	}
}

// TestNewEstaVacia comprueba que una página recién creada declara todo el cuerpo libre.
func TestNewEstaVacia(t *testing.T) {
	p := New(7, TypeLeaf)
	if p.NCells != 0 {
		t.Errorf("NCells: got %d, want 0", p.NCells)
	}
	if p.Free != HeaderSize {
		t.Errorf("Free: got %d, want %d", p.Free, HeaderSize)
	}
	if p.FreeEnd != Size {
		t.Errorf("FreeEnd: got %d, want %d", p.FreeEnd, Size)
	}
	if len(p.Body) != BodySize {
		t.Fatalf("len(Body): got %d, want %d", len(p.Body), BodySize)
	}
	// El round-trip de una página vacía también tiene que cerrar: es el estado en el que
	// nace toda página nueva del pager.
	buf := make([]byte, Size)
	if err := p.EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	got, err := Decode(buf, 7)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	iguales(t, got, p)
}

// TestBitFlipCorruption es la otra mitad del criterio de la F1: detectar corrupción al
// alterar un byte. No se muestrea, se enumera: los 4096 bytes de la página por los 8
// bits de cada uno, 32768 casos. Cada uno debe dar ErrCorrupt.
//
// La enumeración completa incluye los bits del propio campo crc32 -- alterar el
// checksum es tan detectable como alterar lo que protege -- y los del relleno de la
// cabecera, que no tiene significado pero sí está cubierto por el CRC.
func TestBitFlipCorruption(t *testing.T) {
	orig := make([]byte, Size)
	if err := ejemplo(5, TypeLeaf).EncodeTo(orig); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}

	buf := make([]byte, Size)
	for off := 0; off < Size; off++ {
		for bit := 0; bit < 8; bit++ {
			copy(buf, orig)
			buf[off] ^= 1 << bit

			_, err := Decode(buf, 5)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("byte %d bit %d: got %v, want ErrCorrupt", off, bit, err)
			}
		}
	}
}

// TestWrongPageID es el caso que motiva el campo page_id (DESIGN.md sec. 5.1): una
// página íntegra en la ranura equivocada.
//
// La primera comprobación es la que da sentido a la segunda: el CRC de esta página está
// en verde. Si alguien borrase el campo page_id del formato, este escenario -- una
// escritura dirigida al offset equivocado, o una reproducción del log que aplica una
// imagen al lugar incorrecto -- pasaría por página buena sin que nada chistara.
func TestWrongPageID(t *testing.T) {
	buf := make([]byte, Size)
	if err := ejemplo(10, TypeLeaf).EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}

	if _, err := Decode(buf, 10); err != nil {
		t.Fatalf("la pagina en su ranura deberia decodificar: %v", err)
	}
	if _, err := Decode(buf, 11); !errors.Is(err, ErrWrongPage) {
		t.Fatalf("pagina 10 leida como 11: got %v, want ErrWrongPage", err)
	}
}

// TestOrdenDeValidacion fija el orden documentado en Decode: con el CRC en rojo, el
// page_id que se leería es basura, así que el error debe ser ErrCorrupt y no
// ErrWrongPage aunque el page_id tampoco cuadre.
func TestOrdenDeValidacion(t *testing.T) {
	buf := make([]byte, Size)
	if err := ejemplo(10, TypeLeaf).EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	buf[offPageLSN] ^= 0x01 // rompe el CRC sin tocar el page_id

	if _, err := Decode(buf, 11); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt (el crc manda sobre el page_id)", err)
	}
}

// TestTipoDesconocido cubre los tipos que el formato no define. El CRC se recalcula
// después de alterar el byte, porque este no es un caso de disco corrupto: es una
// página escrita íntegramente con un tipo que la sec. 5.1 no declara.
func TestTipoDesconocido(t *testing.T) {
	for _, typ := range []byte{0, 4, 255} {
		t.Run(fmt.Sprintf("tipo=%d", typ), func(t *testing.T) {
			buf := make([]byte, Size)
			if err := ejemplo(1, TypeLeaf).EncodeTo(buf); err != nil {
				t.Fatalf("EncodeTo: %v", err)
			}
			buf[offType] = typ
			sum := crc32.Checksum(buf[4:], castagnoliTable)
			binary.LittleEndian.PutUint32(buf[offCRC:], sum)

			if _, err := Decode(buf, 1); !errors.Is(err, ErrBadType) {
				t.Fatalf("got %v, want ErrBadType", err)
			}
		})
	}

	// EncodeTo rechaza el mismo caso antes de escribir nada, para que un tipo inválido
	// no llegue nunca al disco.
	p := ejemplo(1, Type(9))
	if err := p.EncodeTo(make([]byte, Size)); !errors.Is(err, ErrBadType) {
		t.Fatalf("EncodeTo con tipo 9: got %v, want ErrBadType", err)
	}
}

// TestTamanosInvalidos cubre los buffers que no miden una página completa, en ambos
// sentidos: la sec. 5.1 no admite leer ni escribir menos -- ni más -- que una página.
func TestTamanosInvalidos(t *testing.T) {
	for _, n := range []int{0, 1, HeaderSize, Size - 1, Size + 1} {
		if _, err := Decode(make([]byte, n), 0); !errors.Is(err, ErrBadSize) {
			t.Errorf("Decode con %d bytes: got %v, want ErrBadSize", n, err)
		}
		if err := New(0, TypeLeaf).EncodeTo(make([]byte, n)); !errors.Is(err, ErrBadSize) {
			t.Errorf("EncodeTo sobre %d bytes: got %v, want ErrBadSize", n, err)
		}
	}

	// Un cuerpo del tamaño equivocado es el mismo error visto desde el otro lado.
	p := New(0, TypeLeaf)
	p.Body = make([]byte, BodySize-1)
	if err := p.EncodeTo(make([]byte, Size)); !errors.Is(err, ErrBadSize) {
		t.Errorf("EncodeTo con cuerpo corto: got %v, want ErrBadSize", err)
	}
}

// TestRellenoACero comprueba que EncodeTo limpia los 4 bytes de relleno en vez de
// arrastrar lo que hubiera en un buffer reutilizado. Es lo que garantiza que las
// páginas escritas hoy tengan un valor conocido ahí el día que el relleno se convierta
// en un campo con significado (ver docs/DEUDA-DISENO.md, D3).
func TestRellenoACero(t *testing.T) {
	buf := make([]byte, Size)
	for i := range buf {
		buf[i] = 0xFF
	}
	if err := New(1, TypeMeta).EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	for off := offPadding; off < offLink; off++ {
		if buf[off] != 0 {
			t.Errorf("relleno en el byte %d: got %#x, want 0", off, buf[off])
		}
	}
}

// TestBodyNoAliasa comprueba que la página decodificada no comparte memoria con el
// buffer del que salió. El pager reutiliza sus marcos; si Body apuntara al buffer del
// caché, una Page cambiaría de contenido bajo los pies de quien la tiene en cuanto ese
// marco se reasignara a otra página. Ese bug es de los que no se ven hasta la F2.
func TestBodyNoAliasa(t *testing.T) {
	buf := make([]byte, Size)
	if err := ejemplo(3, TypeLeaf).EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	p, err := Decode(buf, 3)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	antes := append([]byte(nil), p.Body...)

	for i := range buf {
		buf[i] = 0
	}
	if !bytes.Equal(p.Body, antes) {
		t.Fatal("el cuerpo de la pagina aliasa el buffer de origen")
	}
}

// TestLayoutDeCabecera fija los offsets de cada campo frente al diagrama de la sec.
// 5.1. Un round-trip no lo cubre: si EncodeTo y Decode se equivocaran igual en un
// offset, cerraría en verde con un formato en disco que ningún otro lector entendería.
func TestLayoutDeCabecera(t *testing.T) {
	p := &Page{
		ID:      0x1111111111111111,
		LSN:     0x2222222222222222,
		Type:    TypeMeta,
		Flags:   0x44,
		NCells:  0x5555,
		FreeEnd: 0x6666,
		Free:    0x7777,
		Link:    0x8888888888888888,
		Body:    make([]byte, BodySize),
	}
	buf := make([]byte, Size)
	if err := p.EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}

	if got := binary.LittleEndian.Uint32(buf[0:4]); got != crc32.Checksum(buf[4:], castagnoliTable) {
		t.Errorf("crc32 no esta en los bytes 0..4")
	}
	if got := binary.LittleEndian.Uint64(buf[4:12]); got != p.ID {
		t.Errorf("page_id en 4..12: got %#x", got)
	}
	if got := binary.LittleEndian.Uint64(buf[12:20]); got != p.LSN {
		t.Errorf("page_lsn en 12..20: got %#x", got)
	}
	if buf[20] != byte(TypeMeta) {
		t.Errorf("tipo en el byte 20: got %#x", buf[20])
	}
	if buf[21] != p.Flags {
		t.Errorf("flags en el byte 21: got %#x", buf[21])
	}
	if got := binary.LittleEndian.Uint16(buf[22:24]); got != p.NCells {
		t.Errorf("nceldas en 22..24: got %#x", got)
	}
	if got := binary.LittleEndian.Uint16(buf[24:26]); got != p.FreeEnd {
		t.Errorf("libre_fin en 24..26: got %#x", got)
	}
	if got := binary.LittleEndian.Uint16(buf[26:28]); got != p.Free {
		t.Errorf("libre en 26..28: got %#x", got)
	}
	if got := binary.LittleEndian.Uint64(buf[32:40]); got != p.Link {
		t.Errorf("enlace en 32..40: got %#x", got)
	}
	if HeaderSize != 40 || Size != 4096 {
		t.Errorf("la pagina mide %d con cabecera de %d", Size, HeaderSize)
	}
}
