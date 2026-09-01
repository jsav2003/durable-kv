package tree

import (
	"math/rand/v2"
	"testing"
)

// Este archivo es el criterio de terminación de la F2 en la tabla de la sec. 10 del
// DESIGN.md: Put/Get/Scan con 100.000 claves y Validate() en verde.
//
// A esta escala el árbol tiene tres niveles y el orden de inserción deja de ser un detalle.
// En orden creciente toda división ocurre por el extremo derecho y la separadora siempre se
// anexa al final del padre; en orden aleatorio cae en cualquier posición del directorio, que
// es donde vive la aritmética de coloca y de insertaEnPadre. Los dos ordenes son el mismo
// número de claves y no el mismo test.

const cienMil = 100_000

// valorEscala da valores de tamaño variable, entre 8 y 500 bytes, para que el reparto por
// bytes de la división tenga algo que repartir. Con todos los valores iguales, cortar por
// bytes y cortar por índice dan lo mismo y la mitad interesante del algoritmo no se ejerce.
func valorEscala(i int) []byte { return valorN(i, 8+(i*37)%493) }

func TestCienMilClaves(t *testing.T) {
	if testing.Short() {
		t.Skip("100.000 claves: se salta con -short")
	}

	for _, caso := range []struct {
		nombre string
		orden  func(n int) []int
	}{
		{"en orden creciente", func(n int) []int {
			ids := make([]int, n)
			for i := range ids {
				ids[i] = i
			}
			return ids
		}},
		{"en orden aleatorio", func(n int) []int {
			// Semilla fija: un fallo a esta escala tiene que poder repetirse.
			return rand.New(rand.NewPCG(20260831, 1)).Perm(n)
		}},
	} {
		t.Run(caso.nombre, func(t *testing.T) {
			arbol, pg := arbolNuevo(t)

			// Validar tras cada Put serían 100.000 barridos del árbol entero. Cada 10.000 sí
			// cabe, y localiza el problema dentro de una ventana en vez de dejarlo para el
			// final.
			for j, i := range caso.orden(cienMil) {
				if err := arbol.Put(claveN(i), valorEscala(i)); err != nil {
					t.Fatalf("Put(%d) en la posicion %d: %v", i, j, err)
				}
				if (j+1)%10_000 == 0 {
					if err := arbol.Validate(); err != nil {
						t.Fatalf("Validate tras %d claves: %v", j+1, err)
					}
				}
			}

			if err := arbol.Validate(); err != nil {
				t.Fatalf("Validate al final: %v", err)
			}

			// Tres niveles con 100.000 claves es lo que la sec. 6 promete de la aridad. Si
			// saliera un árbol mucho más alto, la división estaría cortando mal aunque todos
			// los invariantes se cumplieran.
			if h := profundidad(t, arbol); h < 2 || h > 4 {
				t.Errorf("el arbol quedo con %d niveles por debajo de la raiz", h)
			}

			for i := 0; i < cienMil; i++ {
				v, err := arbol.Get(claveN(i))
				if err != nil {
					t.Fatalf("Get(%d): %v", i, err)
				}
				if string(v) != string(valorEscala(i)) {
					t.Fatalf("la clave %d perdio su valor", i)
				}
			}

			// El recorrido lateral las devuelve todas y en orden: el invariante 5 a escala.
			vistas := 0
			anterior := ""
			if err := arbol.Scan(nil, nil, func(k, v []byte) bool {
				if vistas > 0 && string(k) <= anterior {
					t.Fatalf("Scan devolvio %q despues de %q", k, anterior)
				}
				anterior = string(k)
				if string(k) != string(claveN(vistas)) {
					t.Fatalf("la entrada %d del recorrido es %q", vistas, k)
				}
				vistas++
				return true
			}); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if vistas != cienMil {
				t.Fatalf("Scan devolvio %d claves de %d", vistas, cienMil)
			}

			// Y un Scan de rango en medio del árbol, que es el caso para el que existe el
			// enlace lateral.
			const desde, hasta = 42_000, 42_500
			enRango := 0
			if err := arbol.Scan(claveN(desde), claveN(hasta), func(_, _ []byte) bool {
				enRango++
				return true
			}); err != nil {
				t.Fatalf("Scan de rango: %v", err)
			}
			if enRango != hasta-desde {
				t.Errorf("el rango [%d, %d) devolvio %d claves", desde, hasta, enRango)
			}

			t.Logf("%d claves en %d paginas, %d niveles bajo la raiz",
				cienMil, pg.TotalPages(), profundidad(t, arbol))
		})
	}
}

// BenchmarkPut no es un objetivo del proyecto -- la sec. 2 dice que no compite en
// rendimiento -- pero da una cifra con la que notar una regresión de orden de magnitud, que
// casi siempre significa que algo se dejó de compartir o se empezó a copiar.
func BenchmarkPut(b *testing.B) {
	arbol, _ := arbolNuevo(b)
	for i := 0; b.Loop(); i++ {
		if err := arbol.Put(claveN(i), valorN(i, 64)); err != nil {
			b.Fatalf("Put(%d): %v", i, err)
		}
	}
}

func BenchmarkGet(b *testing.B) {
	arbol, _ := arbolNuevo(b)
	const n = 50_000
	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i, 64)); err != nil {
			b.Fatalf("preparando: %v", err)
		}
	}
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := arbol.Get(claveN(i % n)); err != nil {
			b.Fatalf("Get: %v", err)
		}
	}
}
