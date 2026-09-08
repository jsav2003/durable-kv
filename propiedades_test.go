package motor_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	motor "github.com/jsav2003/motor-almacenamiento"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
)

// Este archivo es el criterio de terminación de la F5 en la tabla de la sec. 10 del
// DESIGN.md: miles de operaciones aleatorias que coinciden con el modelo de referencia.
//
// La propiedad de la sec. 9.3 es una sola frase: se genera una secuencia aleatoria de
// operaciones, se aplican al motor y a un map[string]string, y los dos tienen que decir lo
// mismo. Todo lo demás de este archivo es la elección de qué operaciones se generan y con
// qué frecuencia, que es donde se decide si la propiedad encuentra algo o no.
//
// # Qué añade esto a lo que ya había
//
// FuzzArbol (internal/tree, F2) hace esta misma comparación, y por eso su comentario decía
// "la propiedad de la sec. 9.3 en miniatura, un ciclo antes de la F5". Lo que le falta es
// todo lo que hay debajo del árbol: corre sobre un pager en memoria, sin WAL, sin
// checkpoint y sin recuperación, y no cierra nunca la base. Aquí la secuencia entra por el
// contrato público de la sec. 4 y atraviesa la pila entera, con dos operaciones que allí no
// existían:
//
//   - la **reapertura**: Close y Open en mitad de la secuencia. El modelo no se reinicia, así
//     que toda clave escrita antes tiene que seguir estando después. Es el camino de la
//     sec. 8 ejercido con un estado arbitrario, no con la carga ordenada de un test escrito
//     a mano;
//   - el **rechazo por tamaño**: una entrada por encima del tope de la sec. 4 tiene que ser
//     rechazada *y no dejar rastro*. El modelo no se toca, así que si el motor escribiera a
//     medias antes de comprobar el tope, la siguiente comparación lo vería.
//
// La otra diferencia es de dirección: el fuzzer busca la entrada rara y minimiza; esto corre
// secuencias largas y densas con semilla fija. Son dos formas de perder el tiempo distintas
// y las dos hacen falta.
//
// # Lo que esta fase NO prueba
//
// Sin caídas. La F4 es la que prueba durabilidad; aquí el disco no falla nunca y lo que se
// comprueba es la **lógica**: que el árbol, el log y el checkpoint compuestos se comporten
// como el mapa. Son las dos clases de bug que la sec. 9.3 separa a propósito.
//
// Y sin borrado, que sigue sin existir (ver el comentario de paquete de db.go). El modelo
// solo crece, así que la partición del invariante 6 se comprueba sobre un conjunto de
// páginas libres que casi siempre está vacío. Mismo agujero que la F2, la F3 y la F4.

// Las semillas de la tabla. Cada una es una corrida independiente y reproducible por
// separado: si una falla, el mensaje la cita y basta con dejar esta lista en esa sola
// semilla para repetirla.
var semillasProp = []int64{
	0xF5_0001, 0xF5_0002, 0xF5_0003, 0xF5_0004, 0xF5_0005,
}

const (
	// opsProp son las operaciones por semilla. Cinco semillas dan las "miles de operaciones"
	// que pide la tabla de la sec. 10 con margen.
	opsProp = 3000

	// umbralProp es el umbral de checkpoint durante la corrida. El de producción son 4 MiB y
	// una secuencia de 3000 operaciones no lo cruzaría ni una vez: la corrida entera pasaría
	// por delante del checkpoint y de la rotación del WAL sin tocarlos.
	//
	// 1 MiB es el punto medio que hace falta, y el número sale de que el WAL transporta
	// **imágenes de página completas** (wal.CargaImagen: 8 bytes de page_id más los 4096 de
	// la página). Un Put ensucia entre una y tres páginas, así que son entre 4 y 12 KiB de
	// log por Put, y 1 MiB es un checkpoint cada 100-250 Put. Bajarlo más -- la primera
	// versión de este archivo tenía 64 KiB -- da un checkpoint cada ocho Put, y entonces el
	// log nunca acumula nada: toda reapertura encuentra un WAL casi vacío y la parte cara de
	// la sec. 8 no se ejerce. El umbral no es un parámetro de velocidad, es el que decide
	// cuánto trabajo tiene la recuperación.
	umbralProp = 1 << 20

	// cadaComparacion es cada cuántas operaciones se compara el modelo entero.
	//
	// La sec. 9.3 dice "después de cada operación, ambos deben coincidir", y eso es lo que se
	// hace: cada operación comprueba su propia poscondición contra el modelo en el acto. Lo
	// que no se hace en cada una es el barrido completo -- Scan de todo más un Get por clave
	// --, porque con miles de claves eso es cuadrático y la corrida deja de caber en el
	// tiempo de un test. El barrido va cada 250 operaciones y, además, siempre en una
	// reapertura y al final. La diferencia entre las dos lecturas es a lo sumo 250
	// operaciones de retraso en localizar el fallo, nunca en detectarlo.
	cadaComparacion = 250

	// logMinimoAReproducir es el WAL mínimo que alguna reapertura abandonada tiene que haber
	// encontrado por delante. Son 64 KiB, o sea unas dieciséis imágenes de página: bastante
	// más que el registro suelto que dejaría un checkpoint recién corrido, que es la
	// diferencia que esta cota existe para distinguir.
	logMinimoAReproducir = 64 << 10
)

// Los topes de la sec. 4, escritos aquí como números y no importados de internal/node a
// propósito: este es un test del contrato público, y si alguien cambiara la constante del
// paquete interno, un test que la importa cambiaría con ella en silencio. Estos números son
// la especificación.
const (
	topeClave     = 512
	topeEntrada   = 1000
	cabeceraCelda = 6
)

func topeValor(k []byte) int { return topeEntrada - cabeceraCelda - len(k) }

// sitio es de dónde sale la base. Las propiedades son las mismas sobre un directorio real y
// sobre el disco falso; lo que cambia es qué hay debajo y cuánto tarda. abrir se llama tanto
// para la apertura inicial como para cada reapertura de la secuencia.
type sitio struct {
	nombre string
	abrir  func() (*motor.DB, error)

	// abandonable dice si la secuencia puede reabrir **sin** cerrar antes. Ver reabre: es la
	// reapertura que de verdad ejerce la recuperación, y no todos los sitios la aguantan.
	abandonable bool

	// bytesDelLog son los bytes de WAL que hay ahora mismo en el sitio, o nil si no se pueden
	// mirar. Sirve para afirmar al final que alguna reapertura tuvo de verdad log que
	// reproducir, en vez de suponerlo.
	bytesDelLog func() int64
}

// sitioReal es un directorio de verdad, con los fsync de verdad del sistema operativo.
//
// No es abandonable. Abandonar una base deja sus descriptores abiertos, y en Windows un
// archivo abierto no se puede borrar: la siguiente rotación del WAL (sec. 7.4) intentaría
// quitar la generación anterior y fallaría, y al terminar el test el borrado de t.TempDir()
// también. La F3 ya se topó con esto y por eso su kill -9 corre en un subproceso de verdad
// (crash_test.go). Aquí la reapertura abandonada se prueba sobre el disco falso, donde un
// archivo es un slice y no hay descriptor que bloquee nada.
func sitioReal(t *testing.T) sitio {
	ruta := filepath.Join(t.TempDir(), "base")
	return sitio{
		nombre: "disco real",
		abrir:  func() (*motor.DB, error) { return motor.Open(ruta) },
	}
}

// sitioFalso es el disco en memoria de la F4 en su modo por omisión: sin volatilidad y sin
// caída, o sea un disco que no falla. Lo que aporta sobre el real es velocidad -- sin él las
// cinco semillas no caben -- y el umbral de checkpoint bajo, que en el real no se puede fijar
// porque Open no lo recibe.
//
// Devuelve también el acceso al disco actual, para poder afirmar al final que la corrida
// ejerció de verdad el checkpoint y la rotación del log.
func sitioFalso() (sitio, func() *fsxtest.Disco) {
	d := fsxtest.Nuevo()
	s := sitio{
		nombre:      "disco falso",
		abandonable: true,
		abrir: func() (*motor.DB, error) {
			// Reabrir hereda solo el contenido duradero. Sin volatilidad eso es todo lo
			// escrito, pero se pasa por ahí igual para que la reapertura de la secuencia sea
			// la misma operación que la de la F4 y no una versión más benigna: el proceso
			// nuevo arranca sobre un disco que no comparte ni un byte de memoria con el
			// anterior, así que nada de lo que el motor tuviera en su caché de páginas puede
			// colarse en la comprobación.
			d = d.Reabrir()
			return motor.AbrirCon(d, umbralProp)
		},
		bytesDelLog: func() int64 {
			var n int64
			for _, nombre := range d.Nombres() {
				if strings.HasPrefix(nombre, "datos.wal.") {
					n += max(d.Tamano(nombre), 0)
				}
			}
			return n
		},
	}
	return s, func() *fsxtest.Disco { return d }
}

// corrida es una secuencia de operaciones sobre una base y su modelo.
type corrida struct {
	t       *testing.T
	semilla int64
	r       *rand.Rand
	sitio   sitio
	db      *motor.DB

	// ref es el modelo de referencia de la sec. 9.3. orden son sus claves en el orden en que
	// aparecieron: hace falta para elegir una clave existente al azar de forma determinista,
	// que recorrer un map de Go no lo es.
	ref   map[string]string
	orden []string

	op int

	// Lo que la corrida llegó a ejercer. Se comprueba al final: una secuencia aleatoria que
	// resultara ser toda inserciones de claves nuevas probaría bastante menos de lo que
	// aparenta, y sin estas cuentas nadie se enteraría.
	puestas, sustituciones, rechazos, reaperturas, abandonos int

	// mayorLogAbandonado es el WAL más grande que una reapertura tuvo que reproducir. Es la
	// medida de cuánto trabajo se le dio de verdad a la recuperación.
	mayorLogAbandonado int64
}

func (c *corrida) fatalf(formato string, args ...any) {
	c.t.Helper()
	c.t.Fatalf("semilla=%#x op=%d (%s): "+formato,
		append([]any{c.semilla, c.op, c.sitio.nombre}, args...)...)
}

// claveProp genera la clave de un Put. La mezcla de distribuciones es el corazón del
// generador y no un detalle:
//
//   - un conjunto **caliente** y pequeño, para que la misma clave se sustituya muchas veces.
//     Cada sustitución es un borrado más una inserción (internal/tree, insertaEnHoja), así que
//     deja celdas muertas y acaba forzando la compactación de la hoja, que es el camino que
//     movía los bytes bajo los pies del llamador en D6;
//   - un conjunto **ancho**, para que el árbol crezca de verdad y las divisiones caigan en
//     cualquier posición del directorio del padre, no siempre por el extremo derecho;
//   - claves **raras**: la vacía, prefijos de otras, bytes arbitrarios que no son texto, y la
//     del tope exacto de 512. El orden del árbol es bytes.Compare y con claves formateadas
//     todas iguales eso nunca se distingue de una comparación de cadenas.
func claveProp(r *rand.Rand) []byte {
	switch r.IntN(16) {
	case 0, 1, 2, 3, 4:
		return fmt.Appendf(nil, "caliente-%02d", r.IntN(24))
	case 5:
		return nil // la clave vacía es legítima
	case 6:
		// Prefijos: "p", "pp", "ppp"... una clave que es prefijo de otra es el caso que
		// distingue un descenso correcto de uno que compara solo el primer byte.
		return bytes.Repeat([]byte{'p'}, 1+r.IntN(6))
	case 7:
		// Bytes arbitrarios, incluidos los que no son imprimibles y el 0xFF.
		b := make([]byte, 1+r.IntN(8))
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
		return b
	case 8:
		// El tope exacto de la sec. 4. Con un valor grande detrás, esta entrada sola ocupa
		// casi un cuarto de hoja.
		return bytes.Repeat([]byte{byte('a' + r.IntN(26))}, topeClave)
	default:
		return fmt.Appendf(nil, "ancha-%08d", r.IntN(50_000))
	}
}

// valorProp genera el valor de un Put. Los tamaños no son uniformes: la mayoría pequeños,
// para que quepan muchas celdas por hoja y el directorio de slots tenga trabajo, y una
// minoría en el tope exacto, que es la entrada más grande que el motor tiene que aceptar y la
// que fuerza la división con cuatro celdas por página de la que sale el número 1000.
//
// La marca es el número de operación. Va en el primer byte para que dos escrituras
// sucesivas de la misma clave caliente no produzcan nunca los mismos bytes: si lo hicieran,
// una sustitución perdida sería indistinguible de una aplicada.
func valorProp(r *rand.Rand, k []byte, marca byte) []byte {
	tope := topeValor(k)
	var n int
	switch r.IntN(12) {
	case 0:
		n = 0 // el valor vacío es legítimo y no es "no está": ErrNotFound distingue las dos cosas
	case 1:
		n = tope // el borde exacto de la sec. 4
	case 2, 3:
		n = r.IntN(tope + 1)
	default:
		n = r.IntN(48)
	}
	v := make([]byte, n)
	for i := range v {
		v[i] = marca + byte(i)
	}
	return v
}

// clavesOrdenadas es el modelo visto como lo ve un Scan.
func (c *corrida) clavesOrdenadas() []string { return slices.Sorted(maps.Keys(c.ref)) }

// --- las operaciones ---

// put inserta o sustituye, y comprueba en el acto que la clave quedó con el valor que dice
// el modelo. Es la poscondición barata de la sec. 9.3; la cara se hace cada cadaComparacion.
func (c *corrida) put() {
	k := claveProp(c.r)
	v := valorProp(c.r, k, byte(c.op))

	if _, ya := c.ref[string(k)]; ya {
		c.sustituciones++
	} else {
		c.orden = append(c.orden, string(k))
	}
	c.puestas++

	if err := c.db.Put(k, v); err != nil {
		c.fatalf("Put(clave de %d bytes, valor de %d): %v", len(k), len(v), err)
	}
	c.ref[string(k)] = string(v)

	got, err := c.db.Get(k)
	if err != nil {
		c.fatalf("Get justo despues de Put(%q): %v", k, err)
	}
	if string(got) != string(v) {
		c.fatalf("Get justo despues de Put(%q) dio %d bytes y se escribieron %d", k, len(got), len(v))
	}
}

// putRechazado intenta una entrada por encima del tope de la sec. 4. Tiene que ser rechazada
// con el error correcto y, sobre todo, no dejar rastro: el modelo no se toca, así que si el
// motor tocara el árbol antes de comprobar el tope, la siguiente comparación lo vería.
func (c *corrida) putRechazado() {
	c.rechazos++

	var k, v []byte
	var quiere error
	if c.r.IntN(2) == 0 {
		k = bytes.Repeat([]byte{'K'}, topeClave+1+c.r.IntN(64))
		v = []byte("v")
		quiere = motor.ErrKeyTooLarge
	} else {
		k = fmt.Appendf(nil, "grande-%04d", c.r.IntN(1000))
		v = bytes.Repeat([]byte{'V'}, topeValor(k)+1+c.r.IntN(64))
		quiere = motor.ErrEntryTooLarge
	}

	if err := c.db.Put(k, v); !errors.Is(err, quiere) {
		c.fatalf("Put(clave de %d, valor de %d) = %v, quiero %v", len(k), len(v), err, quiere)
	}
	// Y la clave rechazada no está. Si estuviera, el rechazo habría llegado tarde.
	if _, err := c.db.Get(k); !errors.Is(err, motor.ErrNotFound) {
		c.fatalf("tras el rechazo, Get de la clave de %d bytes = %v, quiero ErrNotFound", len(k), err)
	}
}

// getPresente lee una clave que el modelo tiene.
func (c *corrida) getPresente() {
	if len(c.orden) == 0 {
		return
	}
	k := c.orden[c.r.IntN(len(c.orden))]

	got, err := c.db.Get([]byte(k))
	if err != nil {
		c.fatalf("Get(%q), que el modelo tiene: %v", k, err)
	}
	if string(got) != c.ref[k] {
		c.fatalf("Get(%q) dio %d bytes y el modelo guarda %d", k, len(got), len(c.ref[k]))
	}
}

// getAusente lee una clave que el generador no produce nunca. El prefijo "ausente-" no
// aparece en claveProp, así que el modelo no puede tenerla.
func (c *corrida) getAusente() {
	k := fmt.Appendf(nil, "ausente-%08d", c.r.IntN(1_000_000))
	if _, err := c.db.Get(k); !errors.Is(err, motor.ErrNotFound) {
		c.fatalf("Get(%q), que nunca se escribio, = %v, quiero ErrNotFound", k, err)
	}
}

// scanRango recorre un rango al azar y lo compara con el mismo corte del modelo. Parte de las
// veces uno de los dos extremos es nil, que la sec. 4 define como "desde la primera" y "hasta
// la última" y son los dos casos que un rango cerrado no ejerce.
func (c *corrida) scanRango() {
	var ini, fin []byte
	switch c.r.IntN(4) {
	case 0:
		fin = claveProp(c.r)
	case 1:
		ini = claveProp(c.r)
	default:
		a, b := claveProp(c.r), claveProp(c.r)
		if bytes.Compare(a, b) > 0 {
			a, b = b, a
		}
		ini, fin = a, b
	}

	var quiere []string
	for _, k := range c.clavesOrdenadas() {
		if ini != nil && k < string(ini) {
			continue
		}
		if fin != nil && k >= string(fin) {
			continue
		}
		quiere = append(quiere, k)
	}

	var visto []string
	if err := c.db.Scan(ini, fin, func(k, v []byte) bool {
		esperado, hay := c.ref[string(k)]
		if !hay {
			c.fatalf("Scan[%q,%q) devolvio la clave %q, que el modelo no tiene", ini, fin, k)
		}
		if string(v) != esperado {
			c.fatalf("Scan[%q,%q) dio para %q un valor de %d bytes y el modelo guarda %d",
				ini, fin, k, len(v), len(esperado))
		}
		visto = append(visto, string(k))
		return true
	}); err != nil {
		c.fatalf("Scan[%q,%q): %v", ini, fin, err)
	}

	if !slices.Equal(visto, quiere) {
		c.fatalf("Scan[%q,%q) devolvio %d claves y el modelo tiene %d en ese rango",
			ini, fin, len(visto), len(quiere))
	}
}

// scanParcial para el recorrido a media altura. Parar no es un fallo (sec. 4) y lo que se ha
// visto hasta ahí tiene que ser el prefijo exacto del modelo ordenado.
func (c *corrida) scanParcial() {
	if len(c.ref) == 0 {
		return
	}
	tope := 1 + c.r.IntN(min(len(c.ref), 32))

	var visto []string
	if err := c.db.Scan(nil, nil, func(k, _ []byte) bool {
		visto = append(visto, string(k))
		return len(visto) < tope
	}); err != nil {
		c.fatalf("Scan interrumpido en la entrada %d: %v, quiero nil", tope, err)
	}
	if len(visto) != tope {
		c.fatalf("Scan interrumpido visito %d entradas y se pidio parar en %d", len(visto), tope)
	}
	if quiere := c.clavesOrdenadas()[:tope]; !slices.Equal(visto, quiere) {
		c.fatalf("el prefijo de %d claves del Scan interrumpido no es el del modelo", tope)
	}
}

func (c *corrida) valida() {
	if err := c.db.Validate(); err != nil {
		c.fatalf("Validate con %d claves: %v", len(c.ref), err)
	}
}

// reabre vuelve a abrir la base. El modelo no se reinicia: todo lo escrito antes tiene que
// seguir estando, con su valor, y el árbol tiene que seguir siendo un árbol. Es el camino de
// la sec. 8 con un estado arbitrario detrás.
//
// La mitad de las veces se cierra bien y la otra mitad se **abandona** la base sin Close.
// No es una variante decorativa, es la única de las dos que ejerce la recuperación:
//
//   - Close hace un checkpoint completo (sec. 7.4), que baja todo a datos.db, pone la meta al
//     día y rota el log. La reapertura que viene detrás encuentra un WAL vacío y los diez
//     pasos de la sec. 8 no tienen ni un registro que reproducir. Es el camino barato.
//   - Abandonar deja en el log todo lo escrito desde el último checkpoint -- con umbralProp
//     eso son hasta 1 MiB de imágenes de página -- y es lo que la apertura tiene que
//     reproducir para que el modelo siga cuadrando. Es el camino que importa.
//
// La primera versión de este archivo solo hacía la primera, y pasaba en verde: 69
// reaperturas por corrida sin reproducir un solo registro.
func (c *corrida) reabre() {
	c.reaperturas++

	abandona := c.sitio.abandonable && c.r.IntN(2) == 0
	if abandona {
		c.abandonos++
		if c.sitio.bytesDelLog != nil {
			c.mayorLogAbandonado = max(c.mayorLogAbandonado, c.sitio.bytesDelLog())
		}
		// Sin Close: se deja la base como la dejaría un proceso que muere. El objeto db se
		// suelta aquí y nadie vuelve a tocarlo.
	} else if err := c.db.Close(); err != nil {
		c.fatalf("Close para reabrir: %v", err)
	}

	db, err := c.sitio.abrir()
	if err != nil {
		c.fatalf("reabrir (abandonada=%v): %v", abandona, err)
	}
	c.db = db

	c.valida()
	if abandona {
		c.comparaTodo("tras reabrir una base abandonada")
	} else {
		c.comparaTodo("tras reabrir")
	}
}

// comparaTodo es la comparación completa: el modelo entero por las dos vías que tiene el
// motor de llegar a una clave.
//
// Las dos hacen falta y no son la misma. Get desciende por las separadoras; Scan recorre las
// hojas de izquierda a derecha. Get encuentra lo que hay, pero no dice si el árbol tiene
// además claves que nunca se insertaron -- eso solo lo ve el recorrido -- y un descenso roto
// puede no llegar a una clave que el recorrido sí visita.
func (c *corrida) comparaTodo(donde string) {
	quiere := c.clavesOrdenadas()

	var visto []string
	if err := c.db.Scan(nil, nil, func(k, v []byte) bool {
		esperado, hay := c.ref[string(k)]
		if !hay {
			c.fatalf("%s: el recorrido devolvio la clave %q, que nunca se inserto", donde, k)
		}
		if string(v) != esperado {
			c.fatalf("%s: el recorrido dio para %q un valor de %d bytes y el modelo guarda %d",
				donde, k, len(v), len(esperado))
		}
		visto = append(visto, string(k))
		return true
	}); err != nil {
		c.fatalf("%s: Scan completo: %v", donde, err)
	}

	if len(visto) != len(quiere) {
		c.fatalf("%s: el recorrido dio %d claves y el modelo tiene %d", donde, len(visto), len(quiere))
	}
	for i := range quiere {
		if visto[i] != quiere[i] {
			c.fatalf("%s: la entrada %d del recorrido es una clave de %d bytes y el modelo tiene una de %d",
				donde, i, len(visto[i]), len(quiere[i]))
		}
	}

	for k, v := range c.ref {
		got, err := c.db.Get([]byte(k))
		if err != nil {
			c.fatalf("%s: el descenso no llega a %q, que el recorrido si visita: %v", donde, k, err)
		}
		if string(got) != v {
			c.fatalf("%s: el descenso da para %q %d bytes y el modelo guarda %d",
				donde, k, len(got), len(v))
		}
	}
}

// paso ejecuta una operación. Los pesos no son uniformes: Put manda porque es lo único que
// cambia el estado, y la reapertura es cara -- arrastra un checkpoint completo y una
// recuperación -- así que sale poco, pero lo bastante para que una corrida de 3000
// operaciones pase por ella decenas de veces.
func (c *corrida) paso() {
	switch n := c.r.IntN(100); {
	case n < 50:
		c.put()
	case n < 66:
		c.getPresente()
	case n < 72:
		c.getAusente()
	case n < 84:
		c.scanRango()
	case n < 90:
		c.scanParcial()
	case n < 95:
		c.putRechazado()
	case n < 98:
		c.valida()
	default:
		c.reabre()
	}
}

// corre ejecuta la secuencia entera de una semilla.
func corre(t *testing.T, s sitio, semilla int64, ops int) *corrida {
	t.Helper()

	c := &corrida{
		t:       t,
		semilla: semilla,
		r:       rand.New(rand.NewPCG(uint64(semilla), 0x9E3779B9)),
		sitio:   s,
		ref:     make(map[string]string),
	}

	db, err := s.abrir()
	if err != nil {
		t.Fatalf("semilla=%#x: abrir la base: %v", semilla, err)
	}
	c.db = db
	t.Cleanup(func() { c.db.Close() })

	for c.op = 1; c.op <= ops; c.op++ {
		c.paso()
		if c.op%cadaComparacion == 0 {
			c.valida()
			c.comparaTodo("en la comparacion periodica")
		}
	}
	c.op = ops

	c.valida()
	c.comparaTodo("al final de la corrida")

	// Que la secuencia haya ejercido de verdad lo que dice ejercer. Una corrida aleatoria que
	// resultara ser toda inserciones de claves nuevas probaría bastante menos de lo que
	// aparenta, y sin esto pasaría en verde sin que nadie lo notara.
	if c.sustituciones < ops/20 {
		t.Errorf("semilla=%#x: solo %d sustituciones en %d operaciones; el conjunto caliente no esta haciendo su trabajo",
			semilla, c.sustituciones, ops)
	}
	if c.reaperturas == 0 {
		t.Errorf("semilla=%#x: la corrida no reabrio la base ni una vez", semilla)
	}
	if c.rechazos == 0 {
		t.Errorf("semilla=%#x: la corrida no intento ninguna entrada fuera de tope", semilla)
	}
	// Y que las reaperturas hayan sido las caras. Sin esto, una corrida en la que el
	// checkpoint se disparase justo antes de cada reapertura pasaría en verde sin haber
	// reproducido nunca un registro de log, que es exactamente lo que hacía la primera
	// versión de este archivo.
	if s.abandonable {
		if c.abandonos == 0 {
			t.Errorf("semilla=%#x: ninguna de las %d reaperturas abandono la base sin Close",
				semilla, c.reaperturas)
		}
		if c.mayorLogAbandonado < logMinimoAReproducir {
			t.Errorf("semilla=%#x: el WAL mas grande que una reapertura reprodujo fueron %d bytes, y hacen falta %d; la recuperacion no esta teniendo trabajo",
				semilla, c.mayorLogAbandonado, logMinimoAReproducir)
		}
	}

	return c
}

// TestPropiedadesContraElModelo es el criterio de terminación de la F5.
func TestPropiedadesContraElModelo(t *testing.T) {
	semillas := semillasProp
	ops := opsProp
	if testing.Short() {
		semillas, ops = semillas[:1], 400
	}

	for _, semilla := range semillas {
		t.Run(fmt.Sprintf("semilla=%#x", semilla), func(t *testing.T) {
			s, disco := sitioFalso()
			c := corre(t, s, semilla, ops)

			t.Logf("%d claves, %d puestas (%d sustituciones), %d rechazos, %d reaperturas (%d sin Close, el mayor log reproducido de %d KiB)",
				len(c.ref), c.puestas, c.sustituciones, c.rechazos, c.reaperturas,
				c.abandonos, c.mayorLogAbandonado>>10)

			// El checkpoint y la rotación del WAL tienen que haber corrido. Si el umbral no se
			// cruzara nunca, la corrida entera pasaría por delante de la sec. 7.4 sin tocarla y
			// este test valdría bastante menos de lo que parece.
			if gen := generacionDelLog(t, disco()); gen == 0 {
				t.Errorf("semilla=%#x: el log sigue en la generacion 0; no hubo ninguna rotacion",
					semilla)
			} else {
				t.Logf("el log llego a la generacion %d", gen)
			}
		})
	}
}

// TestElCicloVacioNoPierdeLoConfirmado es el caso al que la secuencia de arriba redujo la
// pérdida de datos confirmados de las dos ranuras meta (BUGS.md, F5). Son siete operaciones y
// ninguna caída.
//
// Un Close sin ningún Put por medio hace un checkpoint cuyo LSN es el mismo que el del
// anterior, así que las dos ranuras meta quedan **empatadas**. La sec. 5.2 decía "se elige la
// válida con el LSN más alto", y con un empate eso no elige nada: ganaba la ranura 0 por ser
// la primera que se lee, que la mitad de las veces es la vieja. Y una meta vieja nombra una
// generación del WAL que el paso 5 de la sec. 7.4 ya borró, así que la recuperación abría un
// log inexistente --- dir.Open lo crea vacío ---, leía cero grupos, y daba por buena una base
// sin ninguno de los Put confirmados desde entonces.
//
// Va sobre el disco falso y no sobre un directorio, por lo que dice sitioReal: abandonar la
// base sin Close deja sus descriptores abiertos, y en Windows la rotación del WAL no puede
// borrar un archivo que sigue abierto. Con un proceso que muere de verdad no pasa --- el
// sistema cierra sus descriptores ---, y por eso el kill -9 de la F3 corre en un subproceso.
func TestElCicloVacioNoPierdeLoConfirmado(t *testing.T) {
	s, _ := sitioFalso()

	abrir := func(que string) *motor.DB {
		db, err := s.abrir()
		if err != nil {
			t.Fatalf("%s: %v", que, err)
		}
		return db
	}

	db := abrir("abrir la base nueva")
	for i := range 5 {
		if err := db.Put(clave(i), valor(i)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// El ciclo vacío: abrir y cerrar sin tocar nada. Es lo que empata las dos metas.
	db = abrir("abrir para el ciclo vacio")
	if err := db.Close(); err != nil {
		t.Fatalf("Close del ciclo vacio: %v", err)
	}

	db = abrir("abrir tras el ciclo vacio")
	if err := db.Put([]byte("confirmada"), []byte("sobrevive")); err != nil {
		t.Fatalf("Put tras el ciclo vacio: %v", err)
	}
	// Put devolvió nil, así que la sec. 4 ya se comprometió. Se abandona la base sin Close.

	db = abrir("reabrir la base abandonada")
	defer db.Close()

	got, err := db.Get([]byte("confirmada"))
	if err != nil {
		t.Fatalf("la clave confirmada no sobrevivio a la reapertura: %v", err)
	}
	if !bytes.Equal(got, []byte("sobrevive")) {
		t.Errorf("la clave confirmada vale %q, quiero %q", got, "sobrevive")
	}
	// Y las cinco de antes del ciclo vacío siguen estando.
	for i := range 5 {
		if _, err := db.Get(clave(i)); err != nil {
			t.Errorf("Get(%d) tras el ciclo vacio: %v", i, err)
		}
	}
	if err := db.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestPropiedadesSobreDiscoReal corre la misma secuencia sobre un directorio de verdad.
//
// Es una semilla y menos operaciones porque cada Put confirma con un fsync real y eso son
// milisegundos, no nanosegundos. No está aquí por cobertura -- la da la de arriba -- sino
// para que el resultado no dependa de la fidelidad del disco falso: si fsxtest se equivocara
// en algo, las cinco corridas rápidas se equivocarían con él y esta no.
func TestPropiedadesSobreDiscoReal(t *testing.T) {
	if testing.Short() {
		t.Skip("la secuencia con fsync de verdad: -short la salta")
	}
	c := corre(t, sitioReal(t), semillasProp[0], 800)
	t.Logf("%d claves, %d puestas (%d sustituciones), %d reaperturas",
		len(c.ref), c.puestas, c.sustituciones, c.reaperturas)
}

// generacionDelLog devuelve la generación más alta de datos.wal.N que hay en el disco. Cada
// checkpoint completo rota el log (sec. 7.4), así que es la cuenta de rotaciones.
func generacionDelLog(t *testing.T, d *fsxtest.Disco) int {
	t.Helper()
	mayor := -1
	for _, n := range d.Nombres() {
		var gen int
		if _, err := fmt.Sscanf(n, "datos.wal.%d", &gen); err == nil && gen > mayor {
			mayor = gen
		}
	}
	if mayor < 0 {
		t.Fatalf("no hay ningun datos.wal.N en el disco; hay %v", d.Nombres())
	}
	return mayor
}
