package motor_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	motor "github.com/jsav2003/motor-almacenamiento"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
	"github.com/jsav2003/motor-almacenamiento/internal/wal"
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
// Lo que este arnés NO modela está en BUGS.md, sección F4: el disco no corrompe un sector
// ya escrito, solo descarta, reordena y desgarra escrituras sin sincronizar. La creación de
// un archivo sí se puede modelar desde la F6 (fsxtest.DirVolatil), pero el barrido corre con
// ella apagada: el segundo test de este archivo la enciende aparte.

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

// semilla5a fija el segundo test. Va aparte de semillaBase para que el barrido de arriba y
// el corte de la fila 5a no se pisen la reproducibilidad.
const semilla5a int64 = 0xF6_5A00

// epoca5a es la generación del WAL en cuya creación se corta, y el número está elegido.
//
// No es la 1: esa la crea el checkpoint del paso 10 durante la apertura, y una caída ahí no
// tendría ni una sola clave confirmada que exigir. Tampoco una de las primeras: con el
// umbral de umbralCaida la carga limpia crea 101 generaciones para sus 180 claves --el log
// de un Put pasa de los 6000 bytes casi por sí solo--, así que cortar en la 3 dejaría un
// árbol de una hoja y cuatro claves. En la 50 el árbol ya se dividió varias veces y hay
// cerca de noventa claves confirmadas que exigir, que es cuando la fila dice algo.
const epoca5a = 50

// minConfirmadas5a es el guardarraíl del número anterior. Si un cambio en el motor adelanta
// la generación 50, el test dejaría de probar lo que dice probar en silencio; así falla.
const minConfirmadas5a = 60

// El corte de la fila 5a del argumento de correctitud (TESTING.md, sec. 4.1): la caída entre
// el Open de la generación nueva del WAL y el fsync del directorio de la rotación.
//
// Es la última de las diez filas que ningún test ejercitaba con inyección de fallos, y no
// por la plataforma sino por el arnés: hasta la F6 un archivo del disco falso nacía duradero
// y la ventana no existía. Con fsxtest.DirVolatil sí existe.
//
// El estado que produce es el que ninguna otra prueba alcanza: **la meta dice época N+1 y en
// el directorio solo está la N**. La meta ya se escribió y sincronizó en los pasos 3 y 4 del
// checkpoint, con Epoca = actual+1, antes de que el paso 5 rotara. Lo que se afirma es que
// eso es seguro, porque el paso 2 ya bajó a datos.db todo lo que el log tenía que aportar.
//
// Va fuera del barrido de los 500 puntos porque enciende un modo que el barrido no usa. Las
// dos salidas del azar -- la generación nueva sobrevive o no -- tienen que dar las tres
// verificaciones en verde, así que el test recorre semillas hasta ver las dos.
func TestCaidaEntreLaCreacionDelLogYElFsyncDelDirectorio(t *testing.T) {
	if testing.Short() {
		t.Skip("aperturas y recuperaciones repetidas: -short lo salta")
	}

	nuevaSobrevive, nuevaSePierde := 0, 0

	for k := range 20 {
		semilla := semilla5a + int64(k)
		t.Run(fmt.Sprintf("semilla=%#x", semilla), func(t *testing.T) {
			d := fsxtest.Nuevo()
			d.Volatil = true
			d.DirVolatil = true
			d.Semilla = semilla
			d.CaeEnEventos = []string{wal.Nombre(epoca5a) + ":create", "dir:sync"}

			confirmadas := make(map[int]bool)
			intentadas := make(map[int]bool)

			db, err := motor.AbrirCon(d, umbralCaida)
			if err != nil {
				t.Fatalf("abrir la base (semilla=%d): %v", semilla, err)
			}
			for i := range nClavesCaida {
				intentadas[i] = true
				err := db.Put(clave(i), valorCaida(i))
				if err == nil {
					confirmadas[i] = true
					continue
				}
				if !errors.Is(err, fsxtest.ErrCaido) {
					t.Fatalf("Put(%d) devolvió un error que no es la caída (semilla=%d): %v",
						i, semilla, err)
				}
				break
			}

			if !d.Caido {
				t.Fatalf("la carga terminó sin que el disco cayera (semilla=%d): ¿llegó a rotar a la época %d?",
					semilla, epoca5a)
			}
			if len(confirmadas) < minConfirmadas5a {
				t.Fatalf("solo %d claves confirmadas antes del corte (semilla=%d), quiero al menos %d: la generación %d llega demasiado pronto y el test dejaría de exigir nada",
					len(confirmadas), semilla, minConfirmadas5a, epoca5a)
			}

			t.Logf("%d claves confirmadas antes del corte; el directorio duradero es %v",
				len(confirmadas), d.Reabrir().Nombres())

			// El estado en disco, antes de tocarlo: la generación vieja sigue ahí --su borrado
			// va después del fsync del directorio-- y la nueva está a merced de la caída.
			n := d.Reabrir()
			if !n.Existe(wal.Nombre(epoca5a - 1)) {
				t.Fatalf("falta la generación %d (semilla=%d): el directorio duradero es %v",
					epoca5a-1, semilla, n.Nombres())
			}
			if n.Existe(wal.Nombre(epoca5a)) {
				nuevaSobrevive++
			} else {
				nuevaSePierde++
			}

			db2, err := motor.AbrirCon(n, umbralCaida)
			if err != nil {
				t.Fatalf("recuperar tras el corte de la 5a (semilla=%d): %v", semilla, err)
			}
			defer db2.Close()

			// (a)
			if err := db2.Validate(); err != nil {
				t.Fatalf("(a) Validate tras el corte de la 5a (semilla=%d): %v", semilla, err)
			}

			// (b)
			for i := range confirmadas {
				got, err := db2.Get(clave(i))
				if err != nil {
					t.Fatalf("(b) la clave confirmada %d no está (semilla=%d): %v", i, semilla, err)
				}
				if !bytes.Equal(got, valorCaida(i)) {
					t.Fatalf("(b) la clave confirmada %d tiene otro valor (semilla=%d)", i, semilla)
				}
			}

			// (c)
			if err := db2.Scan(nil, nil, func(k, v []byte) bool {
				var i int
				if _, e := fmt.Sscanf(string(k), "clave-%06d", &i); e != nil {
					t.Fatalf("(c) clave con formato inesperado %q (semilla=%d)", k, semilla)
				}
				if !intentadas[i] {
					t.Fatalf("(c) apareció la clave %d, que nunca se intentó escribir (semilla=%d)",
						i, semilla)
				}
				return true
			}); err != nil {
				t.Fatalf("(c) Scan tras el corte de la 5a (semilla=%d): %v", semilla, err)
			}
		})
	}

	// Que las dos salidas ocurran. Si la creación sobreviviera siempre, el test estaría
	// comprobando la mitad de lo que dice comprobar, y justo la mitad fácil.
	t.Logf("la generación nueva sobrevive en %d semillas y se pierde en %d",
		nuevaSobrevive, nuevaSePierde)
	if nuevaSobrevive == 0 || nuevaSePierde == 0 {
		t.Errorf("en veinte semillas solo se dio una de las dos salidas (sobrevive=%d, se pierde=%d): el azar no está decidiendo nada",
			nuevaSobrevive, nuevaSePierde)
	}
}
