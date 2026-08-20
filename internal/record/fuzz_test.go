package record

import "testing"

// FuzzReader alimenta a Reader con bytes arbitrarios, tal como exige DESIGN.md sec.
// 9.4: nunca debe entrar en pánico, pase lo que pase en el archivo. Debe devolver un
// error de corrupción -- o io.EOF -- y nada más.
func FuzzReader(f *testing.F) {
	// Semillas: un archivo con un par de registros válidos, para que el fuzzer parta
	// de estructura real y no solo de ruido; y unos pocos casos borde a mano.
	seed := &memFile{}
	w := NewWriter(seed, 0)
	_ = w.Append(Record{LSN: 1, Type: 1, Epoch: 1, Payload: []byte("semilla")})
	_ = w.Append(Record{LSN: 2, Type: 2, Epoch: 1, Payload: []byte{}})
	f.Add(seed.data)
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(make([]byte, headerSize-1))
	f.Add(make([]byte, headerSize))
	f.Add(make([]byte, headerSize+crcSize))

	f.Fuzz(func(t *testing.T, data []byte) {
		mf := &memFile{data: append([]byte(nil), data...)}
		r := NewReader(mf, 0)

		// Cota superior de iteraciones: cada lectura exitosa avanza el offset al
		// menos frameOverhead bytes, así que este número de llamadas basta para
		// agotar cualquier entrada. Si se excede, el offset no está avanzando --
		// un bug real que conviene que falle rápido y explícito, no como un
		// timeout del fuzzer.
		maxIter := len(data)/frameOverhead + 2
		for i := 0; i < maxIter; i++ {
			if _, err := r.Next(); err != nil {
				return
			}
		}
		t.Fatalf("Next() no terminó tras %d llamadas: el offset no avanza", maxIter)
	})
}
