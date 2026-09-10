package fsxtest_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
)

// carga escribe una secuencia fija a dos archivos con un Sync por medio, sobre un Disco ya
// configurado para caer. Devuelve el error de la escritura que disparó la caída.
func carga(d *fsxtest.Disco) error {
	datos, _ := d.Open("datos.db")
	wal, _ := d.Open("datos.wal.0")

	var err error
	paso := func(f fsx.File, p []byte, off int64) {
		if err != nil {
			return
		}
		_, e := f.WriteAt(p, off)
		if e != nil {
			err = e
		}
	}

	paso(wal, bytes.Repeat([]byte{'R'}, 200), 0)
	wal.Sync()
	paso(datos, bytes.Repeat([]byte{'A'}, 4096), 0)
	paso(wal, bytes.Repeat([]byte{'S'}, 200), 200)
	paso(datos, bytes.Repeat([]byte{'B'}, 4096), 4096)
	paso(wal, bytes.Repeat([]byte{'T'}, 200), 400)
	paso(datos, bytes.Repeat([]byte{'C'}, 4096), 8192)
	return err
}

// La misma semilla, la misma CaeEn y la misma carga dejan el disco bit a bit igual. Sin
// esto BUGS.md no puede citar un punto de caída (sec. 9.1).
func TestCaidaEsReproducibleConLaMismaSemilla(t *testing.T) {
	corre := func() ([]byte, []byte) {
		d := fsxtest.Nuevo()
		d.Volatil = true
		d.CaeEn = 5
		d.Semilla = 1234
		if err := carga(d); !errors.Is(err, fsxtest.ErrCaido) {
			t.Fatalf("la carga no cayó: %v", err)
		}
		return d.Bytes("datos.db"), d.Bytes("datos.wal.0")
	}

	db1, wal1 := corre()
	db2, wal2 := corre()

	if !bytes.Equal(db1, db2) || !bytes.Equal(wal1, wal2) {
		t.Errorf("dos corridas con semilla 1234 divergen:\n db  %d vs %d bytes\n wal %d vs %d bytes",
			len(db1), len(db2), len(wal1), len(wal2))
	}
}

// Tras la caída toda operación de E/S devuelve ErrCaido: el proceso está muerto.
//
// Incluidas las de directorio. El handle se toma **antes** de la caída porque después ni
// siquiera se puede abrir un archivo: un proceso al que matan no hace una llamada al
// sistema más.
func TestTrasLaCaidaTodoDevuelveErrCaido(t *testing.T) {
	d := fsxtest.Nuevo()
	d.Volatil = true
	d.CaeEn = 3
	d.Semilla = 7
	f, _ := d.Open("datos.db")
	carga(d)

	if !d.Caido {
		t.Fatal("el disco no se marcó como caído")
	}
	if _, err := d.Open("cualquiera"); !errors.Is(err, fsxtest.ErrCaido) {
		t.Errorf("Open tras la caída: %v, quiero ErrCaido", err)
	}
	if err := d.Remove("datos.db"); !errors.Is(err, fsxtest.ErrCaido) {
		t.Errorf("Remove tras la caída: %v, quiero ErrCaido", err)
	}
	if _, err := f.WriteAt([]byte("x"), 0); !errors.Is(err, fsxtest.ErrCaido) {
		t.Errorf("WriteAt tras la caída: %v, quiero ErrCaido", err)
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, fsxtest.ErrCaido) {
		t.Errorf("ReadAt tras la caída: %v, quiero ErrCaido", err)
	}
	if err := f.Sync(); !errors.Is(err, fsxtest.ErrCaido) {
		t.Errorf("Sync tras la caída: %v, quiero ErrCaido", err)
	}
	if err := d.Sync(); !errors.Is(err, fsxtest.ErrCaido) {
		t.Errorf("dir.Sync tras la caída: %v, quiero ErrCaido", err)
	}
}

// Lo que pasó por fsync sobrevive a la caída entero; lo que no, puede perderse. Es la
// frontera que separa una confirmación de un intento.
func TestLoSincronizadoSobreviveALaCaida(t *testing.T) {
	for semilla := int64(0); semilla < 200; semilla++ {
		d := fsxtest.Nuevo()
		d.Volatil = true
		d.CaeEn = 6
		d.Semilla = semilla
		carga(d)

		wal := d.Bytes("datos.wal.0")
		// El primer registro de 200 bytes 'R' pasó por Sync antes de la caída.
		quiero := bytes.Repeat([]byte{'R'}, 200)
		if len(wal) < 200 || !bytes.Equal(wal[:200], quiero) {
			t.Fatalf("semilla %d: el registro sincronizado no sobrevivió: %q", semilla, wal)
		}
	}
}

// cae() sobre una única escritura pendiente la deja en uno de tres estados y nunca en
// otro: perdida entera, aplicada entera, o partida en una frontera de sector. Recorrer
// muchas semillas ejerce las tres ramas.
func TestElDesgarroEsEnFronteraDeSector(t *testing.T) {
	const n = 4096
	pagina := bytes.Repeat([]byte{0xAA}, n)

	var vistas, perdidas, enteras, partidas int
	for semilla := int64(0); semilla < 300; semilla++ {
		d := fsxtest.Nuevo()
		d.Volatil = true
		d.CaeEn = 1
		d.Semilla = semilla

		f, _ := d.Open("datos.db")
		f.WriteAt(pagina, 0)

		got := d.Bytes("datos.db")
		vistas++
		switch {
		case len(got) == 0:
			perdidas++
		case bytes.Equal(got, pagina):
			enteras++
		default:
			partidas++
			if len(got)%512 != 0 || len(got) >= n {
				t.Fatalf("semilla %d: desgarro fuera de sector: %d bytes", semilla, len(got))
			}
			if !bytes.Equal(got, pagina[:len(got)]) {
				t.Fatalf("semilla %d: el prefijo desgarrado no coincide", semilla)
			}
		}
	}

	if perdidas == 0 || partidas == 0 {
		t.Errorf("en %d semillas no se ejercieron las tres ramas: perdidas=%d enteras=%d partidas=%d",
			vistas, perdidas, enteras, partidas)
	}
}

// CaeEnEventos es el segundo disparador de la caída, y existe porque CaeEn cuenta WriteAt:
// entre el create del log nuevo y el fsync del directorio no se escribe un solo byte, así
// que el corte de la fila 5a no se puede pedir por número de escritura.
//
// La secuencia importa: "dir:sync" ocurre dos veces por rotación y el que interesa es el
// primero **después** de crear la generación nueva.
func TestCaeEnEventosCortaJustoDespuesDelEvento(t *testing.T) {
	d := fsxtest.Nuevo()
	d.Volatil = true
	d.DirVolatil = true
	d.Semilla = 3
	d.CaeEnEventos = []string{"datos.wal.1:create", "dir:sync"}

	datos, _ := d.Open("datos.db")
	d.Sync()
	datos.WriteAt(bytes.Repeat([]byte{'A'}, 4096), 0)
	datos.Sync()

	if d.Caido {
		t.Fatal("el disco cayó antes de la secuencia")
	}
	if _, err := d.Open("datos.wal.1"); err != nil {
		t.Fatalf("el create no debía caer, es el primero de la secuencia: %v", err)
	}
	if d.Caido {
		t.Fatal("el disco cayó en el primer evento de la secuencia y no en el último")
	}

	if err := d.Sync(); !errors.Is(err, fsxtest.ErrCaido) {
		t.Fatalf("dir.Sync = %v, quiero ErrCaido: es el último de la secuencia", err)
	}
	if !d.Caido {
		t.Fatal("el disco no cayó al completarse la secuencia")
	}
	// El fsync del directorio no llegó a surtir efecto: la creación quedó a merced de la
	// caída, y datos.db, que sí tenía su fsync, sigue entero.
	if got := d.Bytes("datos.db"); len(got) != 4096 {
		t.Errorf("datos.db duradero mide %d, quiero 4096: su Sync fue anterior", len(got))
	}
}

// Las entradas de directorio que la caída decide salen del mismo rand sembrado que el
// descarte, así que la misma semilla deja el mismo directorio. Sin esto BUGS.md no puede
// citar un caso (sec. 9.1).
func TestLasEntradasPendientesSonReproduciblesConLaMismaSemilla(t *testing.T) {
	corre := func(semilla int64) []string {
		d := fsxtest.Nuevo()
		d.Volatil = true
		d.DirVolatil = true
		d.Semilla = semilla
		d.CaeEnEventos = []string{"dir:sync"}

		d.Open("datos.db")
		d.Open("datos.wal.0")
		d.Open("datos.wal.1")
		d.Sync()
		return d.NombresDuraderos()
	}

	a, b := corre(11), corre(11)
	if !slices.Equal(a, b) {
		t.Errorf("dos corridas con la semilla 11 dejan %v y %v", a, b)
	}
	// Y la decisión es de verdad una decisión: hay semillas que dejan directorios distintos.
	distinta := false
	for s := int64(0); s < 20 && !distinta; s++ {
		distinta = !slices.Equal(corre(s), a)
	}
	if !distinta {
		t.Error("ninguna de veinte semillas cambia qué entradas sobreviven: no se está decidiendo nada")
	}
}

// Una creación que la caída descarta se lleva por delante las escrituras al archivo, aunque
// hubieran sobrevivido: el archivo no existe.
func TestLaCreacionDescartadaSeLlevaSusEscrituras(t *testing.T) {
	// La semilla se elige para que la creación no sobreviva; el bucle la busca en vez de
	// fijar un número mágico que un cambio del arnés dejaría mintiendo.
	var semilla int64 = -1
	for s := int64(0); s < 50; s++ {
		d := fsxtest.Nuevo()
		d.Volatil = true
		d.DirVolatil = true
		d.Semilla = s
		d.CaeEnEventos = []string{"datos.wal.1:sync"}
		f, _ := d.Open("datos.wal.1")
		f.WriteAt([]byte("registro"), 0)
		f.Sync()
		if !d.Existe("datos.wal.1") {
			semilla = s
			break
		}
	}
	if semilla < 0 {
		t.Fatal("ninguna de cincuenta semillas descarta la creación")
	}

	d := fsxtest.Nuevo()
	d.Volatil = true
	d.DirVolatil = true
	d.Semilla = semilla
	d.CaeEnEventos = []string{"datos.wal.1:sync"}
	f, _ := d.Open("datos.wal.1")
	f.WriteAt([]byte("registro"), 0)
	f.Sync()

	if d.Existe("datos.wal.1") {
		t.Fatalf("con la semilla %d la creación sobrevivió, y se eligió por lo contrario", semilla)
	}
	if n := d.Reabrir(); n.Existe("datos.wal.1") {
		t.Errorf("el archivo reaparece al reabrir con %q", n.Bytes("datos.wal.1"))
	}
}
