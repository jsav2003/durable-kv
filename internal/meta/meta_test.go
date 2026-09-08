package meta_test

import (
	"errors"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
	"github.com/jsav2003/motor-almacenamiento/internal/meta"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

func archivo(t *testing.T) fsx.File {
	t.Helper()
	d := fsxtest.Nuevo()
	f, err := d.Open("datos.db")
	if err != nil {
		t.Fatal(err)
	}
	// Las dos ranuras materializadas a ceros, que es como las deja pager.Create.
	if _, err := f.WriteAt(make([]byte, meta.Ranuras*page.Size), 0); err != nil {
		t.Fatal(err)
	}
	return f
}

func ejemplo(lsn uint64) meta.Meta {
	return meta.Meta{RootID: 5, FreeHead: 0, TotalPages: 903, Epoca: 7, LSN: lsn}
}

func TestRoundTrip(t *testing.T) {
	f := archivo(t)
	quiero := ejemplo(1234)

	if err := meta.Escribir(f, 1, quiero); err != nil {
		t.Fatalf("Escribir: %v", err)
	}
	got, ranura, err := meta.Leer(f)
	if err != nil {
		t.Fatalf("Leer: %v", err)
	}
	if ranura != 1 {
		t.Errorf("ranura = %d, quiero 1", ranura)
	}
	if got != quiero {
		t.Errorf("meta = %+v, quiero %+v", got, quiero)
	}
}

// Los campos son casi todos uint64 y contiguos: un intercambio entre dos de ellos
// sobrevive a un round-trip si los valores son iguales o cero. Se prueban con valores
// distintos entre sí y ninguno cero.
func TestNoIntercambiaCampos(t *testing.T) {
	f := archivo(t)
	quiero := meta.Meta{RootID: 11, FreeHead: 22, TotalPages: 33, Epoca: 44, LSN: 55}

	if err := meta.Escribir(f, 0, quiero); err != nil {
		t.Fatalf("Escribir: %v", err)
	}
	got, _, err := meta.Leer(f)
	if err != nil {
		t.Fatalf("Leer: %v", err)
	}
	if got != quiero {
		t.Errorf("meta = %+v, quiero %+v", got, quiero)
	}
}

// Gana la de LSN más alto, esté en la ranura que esté. Se prueban las dos disposiciones
// porque un desempate escrito al revés pasa la mitad de las veces.
func TestGanaElLSNMasAltoEnCualquieraDeLasDos(t *testing.T) {
	casos := []struct {
		nombre       string
		lsn0, lsn1   uint64
		quieroRanura uint64
	}{
		{"la nueva en la 1", 100, 200, 1},
		{"la nueva en la 0", 200, 100, 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			f := archivo(t)
			if err := meta.Escribir(f, 0, ejemplo(c.lsn0)); err != nil {
				t.Fatal(err)
			}
			if err := meta.Escribir(f, 1, ejemplo(c.lsn1)); err != nil {
				t.Fatal(err)
			}

			got, ranura, err := meta.Leer(f)
			if err != nil {
				t.Fatalf("Leer: %v", err)
			}
			if ranura != c.quieroRanura {
				t.Errorf("ranura = %d, quiero %d", ranura, c.quieroRanura)
			}
			if quiero := max(c.lsn0, c.lsn1); got.LSN != quiero {
				t.Errorf("lsn = %d, quiero %d", got.LSN, quiero)
			}
		})
	}
}

// Con el mismo LSN en las dos ranuras gana la de época más alta, en la ranura que sea.
//
// Dos metas con el mismo LSN no son una rareza: el LSN de la meta es el del último
// checkpoint, así que dos checkpoints sin ningún commit por medio escriben el mismo número.
// Abrir y cerrar una base sin tocarla ya lo produce.
//
// Lo que estaba en juego cuando la F5 encontró esto: con una comparación solo por LSN el
// empate lo ganaba la ranura 0 por ser la primera que se lee, y si la vieja era esa, la
// recuperación arrancaba con una época del WAL que la rotación de la sec. 7.4 ya había
// borrado. Leía un log vacío y daba por buena una base sin ninguno de los Put confirmados
// desde el checkpoint. La versión de este test que solo miraba el LSN pasaba en verde.
func TestConElMismoLSNGanaLaEpocaMasAlta(t *testing.T) {
	casos := []struct {
		nombre         string
		epoca0, epoca1 uint32
		quieroRanura   uint64
	}{
		{"la nueva en la 1", 3, 4, 1},
		{"la nueva en la 0", 4, 3, 0},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			const mismoLSN = 12

			f := archivo(t)
			m0, m1 := ejemplo(mismoLSN), ejemplo(mismoLSN)
			m0.Epoca, m1.Epoca = c.epoca0, c.epoca1
			if err := meta.Escribir(f, 0, m0); err != nil {
				t.Fatal(err)
			}
			if err := meta.Escribir(f, 1, m1); err != nil {
				t.Fatal(err)
			}

			got, ranura, err := meta.Leer(f)
			if err != nil {
				t.Fatalf("Leer: %v", err)
			}
			if ranura != c.quieroRanura {
				t.Errorf("ranura = %d, quiero %d", ranura, c.quieroRanura)
			}
			if quiero := max(c.epoca0, c.epoca1); got.Epoca != quiero {
				t.Errorf("epoca = %d, quiero %d: se eligio la meta vieja", got.Epoca, quiero)
			}
		})
	}
}

// Y el LSN sigue mandando cuando difieren: la época solo decide lo que el LSN deja sin
// decidir. Una época más alta con un LSN más bajo no puede ocurrir --- las dos crecen en el
// mismo checkpoint --- pero si ocurriera, mandar sobre el LSN sería reproducir el log desde
// un punto posterior al último commit y perder lo que hay entre medias.
func TestElLSNMandaSobreLaEpoca(t *testing.T) {
	f := archivo(t)
	vieja, nueva := ejemplo(100), ejemplo(200)
	vieja.Epoca, nueva.Epoca = 9, 2
	if err := meta.Escribir(f, 0, vieja); err != nil {
		t.Fatal(err)
	}
	if err := meta.Escribir(f, 1, nueva); err != nil {
		t.Fatal(err)
	}

	got, ranura, err := meta.Leer(f)
	if err != nil {
		t.Fatalf("Leer: %v", err)
	}
	if ranura != 1 || got.LSN != 200 {
		t.Errorf("gano la ranura %d con lsn %d, quiero la 1 con 200", ranura, got.LSN)
	}
}

// El punto entero de tener dos: si el sistema se cae escribiendo una, la otra sirve. Se
// corrompe la de LSN más alto, que es la que se estaría escribiendo.
func TestUnaCorruptaYLaOtraSirve(t *testing.T) {
	for ranuraRota := uint64(0); ranuraRota < meta.Ranuras; ranuraRota++ {
		t.Run(map[uint64]string{0: "se rompe la 0", 1: "se rompe la 1"}[ranuraRota], func(t *testing.T) {
			f := archivo(t)
			buena := meta.RanuraSiguiente(ranuraRota)
			if err := meta.Escribir(f, buena, ejemplo(100)); err != nil {
				t.Fatal(err)
			}
			if err := meta.Escribir(f, ranuraRota, ejemplo(200)); err != nil {
				t.Fatal(err)
			}

			// Un bit en el cuerpo de la ranura rota: CRC en rojo.
			b := make([]byte, 1)
			off := int64(ranuraRota)*page.Size + page.HeaderSize + 3
			if _, err := f.ReadAt(b, off); err != nil {
				t.Fatal(err)
			}
			b[0] ^= 0x01
			if _, err := f.WriteAt(b, off); err != nil {
				t.Fatal(err)
			}

			got, ranura, err := meta.Leer(f)
			if err != nil {
				t.Fatalf("Leer: %v", err)
			}
			if ranura != buena || got.LSN != 100 {
				t.Errorf("ranura = %d con lsn = %d, quiero %d con 100", ranura, got.LSN, buena)
			}
		})
	}
}

// Dos metas inválidas **no** son un archivo irrecuperable: la durabilidad de Put descansa
// en el fsync del WAL, así que el log contiene todo lo confirmado. Declarar muerto el
// archivo aquí convertiría en pérdida permanente algo que el WAL resuelve entero.
func TestLasDosInvalidasNoEsUnFalloFatal(t *testing.T) {
	f := archivo(t)

	_, _, err := meta.Leer(f)
	if !errors.Is(err, meta.ErrSinMetaValida) {
		t.Fatalf("error = %v, quiero ErrSinMetaValida", err)
	}
}

// Un archivo más corto que las dos ranuras -- truncado por una caída, o recién creado y a
// medias -- tampoco es fatal: la lectura de la ranura que falta se trata como una ranura
// inválida y no se propaga.
func TestArchivoMasCortoQueLasDosRanuras(t *testing.T) {
	d := fsxtest.Nuevo()
	f, _ := d.Open("datos.db")
	if err := meta.Escribir(f, 0, ejemplo(50)); err != nil {
		t.Fatal(err)
	}
	// El archivo mide una sola página: la ranura 1 no existe.
	if d.Tamano("datos.db") != page.Size {
		t.Fatalf("tamano = %d, quiero %d", d.Tamano("datos.db"), page.Size)
	}

	got, ranura, err := meta.Leer(f)
	if err != nil {
		t.Fatalf("Leer: %v", err)
	}
	if ranura != 0 || got.LSN != 50 {
		t.Errorf("ranura = %d con lsn = %d, quiero 0 con 50", ranura, got.LSN)
	}
}

// Una página íntegra con el page_id de la otra ranura es un caso distinto de la
// corrupción: el CRC valida contenido, no ubicación. page.Decode ya lo distingue y aquí se
// comprueba que meta.Leer no lo deja pasar.
func TestUnaMetaEnLaRanuraEquivocadaSeDescarta(t *testing.T) {
	f := archivo(t)
	if err := meta.Escribir(f, 0, ejemplo(100)); err != nil {
		t.Fatal(err)
	}

	// La página de la ranura 0, escrita tal cual sobre la ranura 1.
	p, err := ejemplo(999).Pagina(0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, page.Size)
	if err := p.EncodeTo(buf); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(buf, page.Size); err != nil {
		t.Fatal(err)
	}

	got, ranura, err := meta.Leer(f)
	if err != nil {
		t.Fatalf("Leer: %v", err)
	}
	if ranura != 0 || got.LSN != 100 {
		t.Errorf("ranura = %d con lsn = %d, quiero 0 con 100: la 1 dice ser la 0",
			ranura, got.LSN)
	}
}

func TestRechazaMagicoYVersion(t *testing.T) {
	base, err := ejemplo(10).Pagina(0)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("magico ajeno", func(t *testing.T) {
		p, _ := ejemplo(10).Pagina(0)
		copy(p.Body[0:8], []byte("SQLite 3"))
		if _, err := meta.DeLaPagina(p); !errors.Is(err, meta.ErrMagicoInvalido) {
			t.Errorf("error = %v, quiero ErrMagicoInvalido", err)
		}
	})

	t.Run("version futura", func(t *testing.T) {
		p, _ := ejemplo(10).Pagina(0)
		p.Body[8] = byte(meta.Version + 1)
		if _, err := meta.DeLaPagina(p); !errors.Is(err, meta.ErrVersionInvalida) {
			t.Errorf("error = %v, quiero ErrVersionInvalida", err)
		}
	})

	t.Run("no es una meta", func(t *testing.T) {
		p, _ := ejemplo(10).Pagina(0)
		p.Type = page.TypeLeaf
		if _, err := meta.DeLaPagina(p); !errors.Is(err, meta.ErrNoEsMeta) {
			t.Errorf("error = %v, quiero ErrNoEsMeta", err)
		}
	})

	// La base sin tocar sí pasa: si no, los tres casos de arriba no probarían nada.
	if _, err := meta.DeLaPagina(base); err != nil {
		t.Errorf("la meta intacta no paso: %v", err)
	}
}

func TestRanuraSiguienteAlterna(t *testing.T) {
	if got := meta.RanuraSiguiente(0); got != 1 {
		t.Errorf("RanuraSiguiente(0) = %d, quiero 1", got)
	}
	if got := meta.RanuraSiguiente(1); got != 0 {
		t.Errorf("RanuraSiguiente(1) = %d, quiero 0", got)
	}
}

func TestRanuraFueraDeRango(t *testing.T) {
	if _, err := ejemplo(1).Pagina(2); !errors.Is(err, meta.ErrRanuraInvalida) {
		t.Errorf("error = %v, quiero ErrRanuraInvalida", err)
	}
}

// El LSN del checkpoint vive en el page_lsn de la cabecera y no en el cuerpo. Que siga
// siendo así importa: un segundo lugar donde vive el mismo dato es un lugar donde puede
// discrepar, y aquí una discrepancia elegiría la meta equivocada.
func TestElLSNViajaEnLaCabecera(t *testing.T) {
	p, err := ejemplo(4321).Pagina(0)
	if err != nil {
		t.Fatal(err)
	}
	if p.LSN != 4321 {
		t.Errorf("page_lsn = %d, quiero 4321", p.LSN)
	}
}
