package motor_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	motor "github.com/jsav2003/motor-almacenamiento"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
)

// Este archivo es el criterio de terminación de la F4, tal como lo pide la tabla de la
// sec. 10: una tabla con 500 puntos de caída y cero pérdidas.
//
// El ciclo es el de la sec. 9.2. Para cada N de 1 a 500:
//
//   - abrir la base sobre un disco falso configurado para caer en la escritura N, con una
//     semilla fija que hace el descarte, el reordenamiento y el desgarro reproducibles
//     bit a bit (sec. 9.1);
//   - escribir un conjunto conocido de claves, anotando cuáles devolvió OK cada Put;
//   - la caída ocurre -- puede haber sido durante la apertura, si N es muy bajo;
//   - reabrir sobre un disco que hereda solo los bytes duraderos;
//   - verificar:
//     (a) Validate() pasa: los seis invariantes, incluida la partición;
//     (b) toda clave cuyo Put devolvió OK está, y con su valor. Es la afirmación central
//     del proyecto entero;
//     (c) toda clave presente pertenece al conjunto de las que se intentaron escribir.
//     No "ninguna no confirmada aparece", que es falso y esperable: una clave anexada al
//     WAL cuyo fsync no retornó puede sobrevivir porque el sistema volcó esos bytes por
//     su cuenta.
//
// Lo que este arnés NO modela está en BUGS.md, sección F4: la creación de un archivo es
// duradera en el acto (no hay caída entre el open y el fsync del directorio), y el disco
// no corrompe un sector ya escrito, solo descarta, reordena y desgarra escrituras sin
// sincronizar.

// semillaBase fija toda la corrida. El punto de caída N usa semillaBase+N, así que cada
// fila de la tabla es reproducible por separado y la tabla entera desde esta única
// constante. Si una fila falla, BUGS.md cita "caída en escritura N, semilla semillaBase+N".
const semillaBase int64 = 0xF4_0000

// umbralCaida fuerza un checkpoint cada pocas decenas de Put, para que los puntos de caída
// caigan también dentro de un checkpoint y de una rotación del WAL y no solo en el camino
// de un Put.
const umbralCaida int64 = 6000

// nClavesCaida es el tamaño de la carga conocida. Se elige para que la corrida limpia pase
// holgadamente de 500 escrituras -- así hay 500 puntos de caída distintos que probar, con
// margen para que un cambio menor en el motor no deje la tabla sin arrancar -- sin inflar
// cada subtest. Con el umbral de abajo entran unos siete checkpoints, así que hay puntos
// de caída dentro de un checkpoint y de una rotación del WAL, no solo en el camino de un
// Put.
const nClavesCaida = 180

func valorCaida(i int) []byte {
	return append(fmt.Appendf(nil, "valor-%06d:", i), bytes.Repeat([]byte("x"), 200)...)
}

// escriturasDeLaCargaLimpia corre la carga entera sin caída y devuelve cuántas escrituras
// emitió: la cota superior de N que tiene sentido probar.
func escriturasDeLaCargaLimpia(t *testing.T) int {
	t.Helper()
	d := fsxtest.Nuevo()
	d.Volatil = true
	db, err := motor.AbrirCon(d, umbralCaida)
	if err != nil {
		t.Fatalf("abrir la base para medir: %v", err)
	}
	for i := range nClavesCaida {
		if err := db.Put(clave(i), valorCaida(i)); err != nil {
			t.Fatalf("Put(%d) en la corrida limpia: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close en la corrida limpia: %v", err)
	}
	return d.NEscrituras()
}

func TestQuinientosPuntosDeCaida(t *testing.T) {
	if testing.Short() {
		t.Skip("500 aperturas y recuperaciones: -short lo salta")
	}

	total := escriturasDeLaCargaLimpia(t)
	puntos := 500
	if total < puntos {
		t.Fatalf("la carga limpia solo emite %d escrituras y hacen falta 500 puntos de caída; sube nClavesCaida",
			total)
	}
	t.Logf("la carga limpia emite %d escrituras; se prueban los primeros %d puntos de caída", total, puntos)

	for n := 1; n <= puntos; n++ {
		t.Run(fmt.Sprintf("N=%03d", n), func(t *testing.T) {
			semilla := semillaBase + int64(n)

			d := fsxtest.Nuevo()
			d.Volatil = true
			d.CaeEn = n
			d.Semilla = semilla

			confirmadas := make(map[int]bool)
			intentadas := make(map[int]bool)

			db, err := motor.AbrirCon(d, umbralCaida)
			switch {
			case err == nil:
				for i := range nClavesCaida {
					intentadas[i] = true
					err := db.Put(clave(i), valorCaida(i))
					if err == nil {
						confirmadas[i] = true
						continue
					}
					if !errors.Is(err, fsxtest.ErrCaido) {
						t.Fatalf("Put(%d) devolvió un error que no es la caída (N=%d, semilla=%d): %v",
							i, n, semilla, err)
					}
					break
				}
			case errors.Is(err, fsxtest.ErrCaido):
				// La caída ocurrió durante la apertura (el checkpoint del paso 10). Ninguna
				// clave se llegó a intentar; el estado en disco es el que dejó la caída.
			default:
				t.Fatalf("abrir la base falló con un error que no es la caída (N=%d, semilla=%d): %v",
					n, semilla, err)
			}

			if !d.Caido {
				t.Fatalf("N=%d (semilla=%d): la carga terminó sin que el disco cayera", n, semilla)
			}

			// Reabrir sobre los bytes duraderos y recuperar.
			db2, err := motor.AbrirCon(d.Reabrir(), umbralCaida)
			if err != nil {
				t.Fatalf("recuperar tras la caída en la escritura %d (semilla=%d): %v", n, semilla, err)
			}
			defer db2.Close()

			// (a)
			if err := db2.Validate(); err != nil {
				t.Fatalf("(a) Validate tras la caída en %d (semilla=%d): %v", n, semilla, err)
			}

			// (b)
			for i := range confirmadas {
				got, err := db2.Get(clave(i))
				if err != nil {
					t.Fatalf("(b) la clave confirmada %d no está tras la caída en %d (semilla=%d): %v",
						i, n, semilla, err)
				}
				if !bytes.Equal(got, valorCaida(i)) {
					t.Fatalf("(b) la clave confirmada %d tiene otro valor tras la caída en %d (semilla=%d)",
						i, n, semilla)
				}
			}

			// (c)
			if err := db2.Scan(nil, nil, func(k, v []byte) bool {
				var i int
				if _, e := fmt.Sscanf(string(k), "clave-%06d", &i); e != nil {
					t.Fatalf("(c) clave con formato inesperado %q tras la caída en %d (semilla=%d)",
						k, n, semilla)
				}
				if !intentadas[i] {
					t.Fatalf("(c) apareció la clave %d, que nunca se intentó escribir, tras la caída en %d (semilla=%d)",
						i, n, semilla)
				}
				return true
			}); err != nil {
				t.Fatalf("(c) Scan tras la caída en %d (semilla=%d): %v", n, semilla, err)
			}
		})
	}
}
