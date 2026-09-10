package tree

import (
	"bytes"
	"io"
	"maps"
	"slices"
	"testing"

	"github.com/jsav2003/durable-kv/internal/node"
)

// FuzzArbol construye árboles con secuencias de Put sacadas de la entrada y exige que el
// resultado cumpla los seis invariantes y coincida con un mapa de referencia.
//
// # Qué busca aquí el fuzzer que no busca en internal/node
//
// FuzzNode ataca el formato: mete bytes arbitrarios en el cuerpo de una página y comprueba
// que Check los rechaza sin desbordar. Este otro no corrompe nada -- todas las páginas las
// escribe el propio árbol -- y ataca la otra mitad: las **distribuciones de claves** que un
// test escrito a mano no se le ocurren. Claves vacías, claves que son prefijo de otras,
// claves repetidas que fuerzan sustitución justo antes de una división, valores de cero
// bytes, rachas de entradas del tamaño máximo seguidas de entradas diminutas.
//
// La propiedad es la de la sec. 9.3 en miniatura, un ciclo antes de la F5: el árbol y un
// mapa ordenado tienen que decir lo mismo, y además el árbol tiene que seguir siendo un
// árbol.
func FuzzArbol(f *testing.F) {
	// El corpus de arranque nombra los casos que sí se saben interesantes; el fuzzer se
	// encarga del resto.
	f.Add([]byte{})                                  // ningún Put: la raíz vacía
	f.Add([]byte{0, 0})                              // la clave vacía con valor vacío
	f.Add([]byte{1, 'a', 0, 1, 'a', 0})              // la misma clave dos veces
	f.Add([]byte{1, 'a', 0, 2, 'a', 'b', 0})         // una clave que es prefijo de otra
	f.Add([]byte{1, 'z', 255, 1, 'a', 255})          // dos entradas grandes en orden inverso
	f.Add(bytes.Repeat([]byte{1, 'k', 200}, 40))     // rachas que fuerzan divisiones
	f.Add(bytes.Repeat([]byte{4, 'a', 'b', 'c'}, 8)) // entradas truncadas a mitad

	f.Fuzz(func(t *testing.T, datos []byte) {
		arbol, _ := arbolNuevo(t)
		modelo := make(map[string]string)

		r := bytes.NewReader(datos)
		// El tope de operaciones acota lo que tarda una ejecución. Sin él, una entrada larga
		// de entradas máximas construye un árbol de miles de páginas y el fuzzer avanza a
		// razón de unas pocas ejecuciones por segundo, que es como no tener fuzzer.
		for ops := 0; ops < 300; ops++ {
			k, v, hay := siguienteEntrada(r)
			if !hay {
				break
			}
			if err := arbol.Put(k, v); err != nil {
				t.Fatalf("Put(clave de %d bytes, valor de %d): %v", len(k), len(v), err)
			}
			modelo[string(k)] = string(v)
		}

		if err := arbol.Validate(); err != nil {
			t.Fatalf("Validate con %d claves: %v", len(modelo), err)
		}

		// Cada clave del modelo se alcanza por el descenso y conserva su valor.
		for k, v := range modelo {
			got, err := arbol.Get([]byte(k))
			if err != nil {
				t.Fatalf("Get(clave de %d bytes): %v", len(k), err)
			}
			if string(got) != v {
				t.Fatalf("la clave de %d bytes devolvio %d bytes de valor y guardaba %d",
					len(k), len(got), len(v))
			}
		}

		// Y el recorrido lateral devuelve exactamente el modelo, en orden. Es la comprobación
		// que ninguna de las anteriores hace: Get encuentra lo que hay, pero no dice si el
		// árbol tiene además claves que nunca se insertaron.
		quiere := slices.Sorted(maps.Keys(modelo))
		var visto []string
		if err := arbol.Scan(nil, nil, func(k, _ []byte) bool {
			visto = append(visto, string(k))
			return true
		}); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(visto) != len(quiere) {
			t.Fatalf("el arbol tiene %d claves y se insertaron %d", len(visto), len(quiere))
		}
		for i := range quiere {
			if visto[i] != quiere[i] {
				t.Fatalf("la entrada %d del recorrido tiene %d bytes y deberia tener %d",
					i, len(visto[i]), len(quiere[i]))
			}
		}
	})
}

// siguienteEntrada saca un par del flujo: un byte de longitud de clave, la clave, y un byte
// del que sale el tamaño del valor.
//
// El valor se genera repitiendo la clave en vez de leerse del flujo. Así un valor de 800
// bytes cuesta un byte de entrada, y el fuzzer puede llegar a las divisiones con entradas
// cortas -- que son las que sabe minimizar. Es la misma lección que la F1 anotó en BUGS.md
// sobre el minimizador y las páginas de tamaño fijo: lo que el fuzzer no puede acortar, no
// lo puede explicar.
func siguienteEntrada(r *bytes.Reader) (k, v []byte, hay bool) {
	klen, err := r.ReadByte()
	if err != nil {
		return nil, nil, false
	}
	k = make([]byte, min(int(klen), node.MaxKeySize))
	if _, err := io.ReadFull(r, k); err != nil {
		return nil, nil, false
	}

	factor, err := r.ReadByte()
	if err != nil {
		return nil, nil, false
	}
	// El tope de la sec. 4 se respeta aquí: Put lo rechazaría con ErrEntryTooLarge y el
	// fuzzer estaría probando la validación de entrada en vez del árbol.
	vlen := min(int(factor)*4, node.MaxEntrySize-node.LeafCellOverhead-len(k))
	v = make([]byte, max(vlen, 0))
	for i := range v {
		if len(k) == 0 {
			v[i] = 0xAB
			continue
		}
		v[i] = k[i%len(k)]
	}
	return k, v, true
}
