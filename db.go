// Package durakv es un motor de almacenamiento clave-valor embebido con durabilidad ante
// caídas.
//
// La garantía que ofrece, y a la que sirve todo lo demás: **si Put devuelve nil, ese dato
// sobrevive a cualquier caída posterior.** No porque esté en su sitio definitivo --
// probablemente no lo esté todavía -- sino porque está en el write-ahead log y ese log ya
// pasó por fsync.
//
// # Lo que no es
//
// No hay SQL, ni concurrencia, ni red, ni índices secundarios. Un solo hilo escritor y sin
// transacciones multi-operación. Ver NO-GOALS.md: son exclusiones permanentes, no tareas
// pendientes.
//
// # Estado de la implementación
//
// Delete todavía no está. La tabla de fases del DESIGN.md no se lo asigna a ninguna, y el
// criterio de terminación de la F3 -- un kill -9 y todas las claves confirmadas al reabrir
// -- no lo necesita. Se declarará cuando se decida su fase, no antes: un método público que
// devuelve "no implementado" es peor que uno que no está, porque compila en el código del
// llamador.
package durakv

import (
	"errors"

	"github.com/jsav2003/durable-kv/internal/fsx"
	"github.com/jsav2003/durable-kv/internal/node"
	"github.com/jsav2003/durable-kv/internal/recovery"
	"github.com/jsav2003/durable-kv/internal/tree"
)

// Los errores del contrato de la sec. 4, reexportados. Tienen que estarlo: los paquetes de
// internal/ no son importables desde fuera del módulo, así que sin esto un llamador no
// podría distinguir una clave ausente de un fallo de disco.
var (
	// ErrNotFound indica que la clave no está. Es flujo normal y no un fallo: distingue la
	// ausencia de un valor de longitud cero, que es legítimo.
	ErrNotFound = tree.ErrNotFound

	// ErrEntryTooLarge indica que clave + valor + 6 bytes de cabecera de celda pasa de
	// 1000 (sec. 4). El tope se deriva del objetivo de llenado: para que una hoja llena
	// siempre pueda dividirse en dos mitades razonables hacen falta al menos cuatro celdas
	// por página.
	ErrEntryTooLarge = node.ErrEntryTooLarge

	// ErrKeyTooLarge indica una clave de más de 512 bytes (sec. 4).
	ErrKeyTooLarge = node.ErrKeyTooLarge

	// ErrCerrada indica una operación sobre una base ya cerrada.
	ErrCerrada = errors.New("durakv: la base esta cerrada")
)

// DB es una base abierta. No es segura para uso concurrente: la sec. 2 declara un solo hilo
// escritor y NO-GOALS.md lo fija como exclusión permanente.
type DB struct {
	dir fsx.Dir
	est *recovery.Estado
}

// Open abre la base que vive en el directorio ruta, creándolo si no existe.
//
// ruta es un directorio y no un archivo porque una base son dos: datos.db con el árbol, y
// datos.wal.N con el log de la generación N (sec. 5). El WAL lleva la generación en el
// nombre porque no se trunca en sitio, se rota.
//
// Abrir es siempre recuperar. No hay un camino distinto para "abrir normal": Open recorre
// los diez pasos de la sec. 8 haya habido caída o no, incluso sobre una base recién creada.
// Un camino de recuperación que solo corriera después de una caída sería un camino que casi
// nunca se ejecuta y que por tanto casi nunca se prueba.
func Open(ruta string) (*DB, error) {
	dir, err := fsx.Crear(ruta)
	if err != nil {
		return nil, err
	}
	return abrirCon(dir, 0)
}

// abrirCon es Open sin el paso de crear el directorio en disco: recibe un fsx.Dir ya
// hecho y el umbral de checkpoint. Existe para el arnés de inyección de fallos de la F4,
// que abre la base sobre un disco falso con árbitro de orden (sec. 9.1) y necesita bajar
// el umbral para que el checkpoint entre dentro de la carga de prueba. Se expone a los
// tests por export_test.go.
func abrirCon(dir fsx.Dir, umbral int64) (*DB, error) {
	est, err := recovery.Recuperar(dir, umbral)
	if err != nil {
		return nil, err
	}
	return &DB{dir: dir, est: est}, nil
}

// Put inserta o sustituye el valor de key.
//
// Devuelve nil solo después de que el registro de commit de su grupo esté sincronizado en
// el log (sec. 7.2, pasos 4 a 6). A partir de ese nil, el dato sobrevive a cualquier caída.
//
// El checkpoint se comprueba **antes** de insertar y no después. Es deliberado: un
// checkpoint que fallara después de un Put ya confirmado obligaría a devolver un error
// sobre un dato que está durablemente escrito, y eso es mentir en la dirección más cara. Al
// comprobarlo antes, un fallo del checkpoint significa que este Put no llegó a ocurrir, y
// el error dice la verdad.
func (db *DB) Put(key, value []byte) error {
	if db.est == nil {
		return ErrCerrada
	}
	if db.est.Checkpoint.Toca() {
		if err := db.est.Checkpoint.Correr(db.est.Arbol.Root()); err != nil {
			return err
		}
	}
	return db.est.Arbol.Put(key, value)
}

// Get devuelve el valor de key, o ErrNotFound si no está.
//
// El valor es una copia y el llamador puede conservarlo. Tiene que serlo: los accesores del
// nodo entregan subsectores del cuerpo de la página, válidos solo hasta la siguiente
// mutación de ese nodo (docs/DEUDA-DISENO.md, D6). Devolverlos tal cual daría al llamador un
// slice que un Put posterior le reescribe bajo los pies -- el motor devolvería datos que
// nadie escribió, con la página coherente, el CRC correcto, y ninguna comprobación de
// integridad enterándose.
func (db *DB) Get(key []byte) ([]byte, error) {
	if db.est == nil {
		return nil, ErrCerrada
	}
	return db.est.Arbol.Get(key)
}

// Scan recorre el rango [start, end) en orden de clave y llama a fn con cada par. Un start
// nil empieza por la primera clave y un end nil llega hasta la última. Si fn devuelve false
// el recorrido para, y eso no es un fallo: Scan devuelve nil.
//
// A diferencia de Get, **k y v valen lo que dura la llamada a fn**. Quien los conserve,
// copia. Copiar en cada entrada pondría una asignación por clave en un recorrido que puede
// ser de cientos de miles, y aquí el llamador sí tiene el punto exacto donde decidirlo.
func (db *DB) Scan(start, end []byte, fn func(k, v []byte) bool) error {
	if db.est == nil {
		return ErrCerrada
	}
	return db.est.Arbol.Scan(start, end, fn)
}

// Validate comprueba los seis invariantes del árbol (sec. 6). Es caro -- recorre el árbol
// entero -- y está pensado para el modo depuración y para el arnés de inyección de fallos
// de la F4, donde el criterio (a) de la sec. 9.2 es exactamente esta llamada.
func (db *DB) Validate() error {
	if db.est == nil {
		return ErrCerrada
	}
	return db.est.Arbol.Validate()
}

// Close hace un checkpoint completo y cierra los archivos.
//
// El checkpoint no es lo que da durabilidad -- eso ya lo dio el fsync de cada Put -- sino lo
// que deja la base en su forma barata de abrir: todo en datos.db, la meta al día, y un log
// vacío. Cerrar sin él no pierde nada, solo hace más lenta la próxima apertura.
//
// Cerrar dos veces no es un error: la segunda no hace nada.
func (db *DB) Close() error {
	if db.est == nil {
		return nil
	}
	est := db.est
	db.est = nil

	err := est.Checkpoint.Correr(est.Arbol.Root())
	if cerr := est.Log.Close(); err == nil {
		err = cerr
	}
	if cerr := est.Datos.Close(); err == nil {
		err = cerr
	}
	return err
}
