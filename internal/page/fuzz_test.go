package page

import "testing"

// FuzzDecode somete el deserializador de páginas a bytes arbitrarios, tal como exige
// DESIGN.md sec. 9.4: nunca debe entrar en pánico. Debe devolver un error de corrupción
// -- o una página coherente -- y nada más.
//
// Cuando devuelve una página, el fuzzer comprueba además que vuelva a codificarse en
// los mismos bytes. Un Decode que solo prometiera "no entrar en pánico" lo cumpliría
// devolviendo siempre ErrCorrupt; la ida y vuelta es lo que impide que la propiedad se
// satisfaga trivialmente, y de paso ata EncodeTo y Decode entre sí sobre entradas que
// nadie escribió a mano.
//
// Cómo correrlo:
//
//	go test ./internal/page -run=XXX -fuzz=FuzzDecode -fuzztime=60s -fuzzminimizetime=2s
//
// La cota de minimización no es opcional aquí. Sin ella el fuzzer se queda a unas pocas
// ejecuciones por corrida: la página es de tamaño fijo, así que cualquier byte que el
// minimizador quite cambia la longitud y hace que Decode salga por ErrBadSize, con lo
// que ninguna entrada reducida conserva jamás la cobertura de la original. Minimizar es
// fútil por construcción del formato. Ver BUGS.md, F1.
func FuzzDecode(f *testing.F) {
	// Semillas: páginas válidas de cada tipo, para que el fuzzer parta de estructura
	// real y no solo de ruido, y unos pocos casos borde de tamaño.
	for _, typ := range []Type{TypeInternal, TypeLeaf, TypeMeta} {
		buf := make([]byte, Size)
		p := New(1, typ)
		for i := range p.Body {
			p.Body[i] = byte(i)
		}
		if err := p.EncodeTo(buf); err != nil {
			f.Fatalf("EncodeTo: %v", err)
		}
		f.Add(buf, uint64(1))
	}
	f.Add([]byte{}, uint64(0))
	f.Add(make([]byte, Size), uint64(0))
	f.Add(make([]byte, Size-1), uint64(0))
	f.Add(make([]byte, HeaderSize), uint64(0))

	f.Fuzz(func(t *testing.T, data []byte, wantID uint64) {
		p, err := Decode(data, wantID)
		if err != nil {
			return
		}
		if p.ID != wantID {
			t.Fatalf("Decode devolvio la pagina %d pidiendo la %d", p.ID, wantID)
		}

		round := make([]byte, Size)
		if err := p.EncodeTo(round); err != nil {
			t.Fatalf("EncodeTo tras un Decode correcto: %v", err)
		}
		for i := range round {
			if round[i] != data[i] {
				t.Fatalf("la reserializacion difiere en el byte %d: %#x vs %#x",
					i, round[i], data[i])
			}
		}
	})
}
