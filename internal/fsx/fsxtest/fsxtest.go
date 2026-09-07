// Package fsxtest es el sistema de archivos en memoria con el que se prueban los
// paquetes que escriben a disco.
//
// Tiene dos modos. Por omisión no simula ningún fallo: cada WriteAt se aplica al vuelo y
// lo único que aporta sobre un buffer es la traza compartida por todos los archivos de un
// mismo Disco -- una versión en miniatura del árbitro de orden global de la sec. 9.1,
// porque lo que hay que demostrar del write-ahead no es que las escrituras ocurran, sino
// en qué orden ocurren **entre datos.db y el WAL**, y con una traza por componente ese
// orden relativo no se puede observar.
//
// Con Volatil en true entra el buffer de escrituras no sincronizadas de la sec. 9.1: un
// WriteAt no toca el contenido duradero hasta que un Sync lo vuelca, igual que el caché
// del sistema operativo. Las dos instancias de File (datos y WAL) comparten **una sola**
// cola de pendientes, etiquetada por nombre de archivo; Sync de un archivo vacía solo sus
// entradas. Esta es la base sobre la que la F4 monta el descarte, el reordenamiento y la
// escritura desgarrada de la caída.
package fsxtest

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
)

// Traza es la secuencia de eventos observados, compartida por todos los archivos de un
// Disco y por el propio directorio.
type Traza struct {
	mu      sync.Mutex
	eventos []string
}

// Anota registra un evento.
func (t *Traza) Anota(formato string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventos = append(t.eventos, fmt.Sprintf(formato, args...))
}

// Eventos devuelve una copia de lo observado hasta ahora.
func (t *Traza) Eventos() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.eventos)
}

// Limpia descarta lo observado. Sirve para acotar una aserción a la parte de la corrida
// que interesa -- por ejemplo, a un solo checkpoint.
func (t *Traza) Limpia() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.eventos = nil
}

// Indice devuelve la posición del primer evento igual a e, o -1. Comparar dos índices es
// como se afirma un orden entre dos escrituras de archivos distintos.
func (t *Traza) Indice(e string) int {
	return slices.Index(t.Eventos(), e)
}

// Contiene informa si la traza incluye el evento exacto e.
func (t *Traza) Contiene(e string) bool {
	return t.Indice(e) >= 0
}

// escritura es un WriteAt que aún no ha pasado por Sync. Vive en la cola global del Disco,
// no en el Archivo, porque el orden que importa modelar es el que hay entre las escrituras
// de datos.db y las del WAL.
type escritura struct {
	archivo string
	off     int64
	datos   []byte
}

// Disco es un fsx.Dir en memoria.
type Disco struct {
	Traza    *Traza
	archivos map[string]*Archivo
	// SyncFalla, si no es nil, es el error que devuelve el fsync del directorio. Modela
	// la plataforma donde la operación no está disponible, y el fallo real de E/S.
	SyncFalla error

	// Volatil activa el buffer de la sec. 9.1. Con Volatil en false (lo normal fuera de la
	// F4) cada WriteAt se aplica al vuelo y el Disco es un observador simple.
	Volatil bool

	// pendientes es la cola global de escrituras sin sincronizar, en orden de emisión.
	// Solo se usa con Volatil en true.
	pendientes []escritura

	// nEscrituras cuenta los WriteAt sobre todos los archivos del Disco. Es el número N de
	// "caer en la escritura N" de la sec. 9.2.
	nEscrituras int

	// CaeEn, si es > 0, es el número de escritura durante la cual el disco cae: al
	// emitirse ese WriteAt se procesa la cola de pendientes (descarte, reordenamiento y
	// una escritura desgarrada) y toda operación posterior devuelve ErrCaido. Requiere
	// Volatil.
	CaeEn int

	// Semilla fija el azar del descarte, el reordenamiento y el desgarro. La misma semilla
	// con la misma CaeEn y la misma carga reproduce la caída bit a bit (sec. 9.1).
	Semilla int64

	// Caido pasa a true cuando la caída ya ocurrió.
	Caido bool
}

// Nuevo devuelve un Disco vacío con su traza.
func Nuevo() *Disco {
	return &Disco{Traza: &Traza{}, archivos: make(map[string]*Archivo)}
}

// Open abre el archivo nombre, creándolo vacío si no existe. Devuelve siempre el mismo
// objeto para el mismo nombre: dos handles con contenidos distintos para el mismo archivo
// no es lo que hace un sistema de archivos.
func (d *Disco) Open(nombre string) (fsx.File, error) {
	if a, ok := d.archivos[nombre]; ok {
		d.Traza.Anota("%s:open", nombre)
		return a, nil
	}
	a := &Archivo{nombre: nombre, disco: d}
	d.archivos[nombre] = a
	d.Traza.Anota("%s:create", nombre)
	return a, nil
}

// Remove borra el archivo nombre.
func (d *Disco) Remove(nombre string) error {
	if d.Caido {
		return ErrCaido
	}
	if _, ok := d.archivos[nombre]; !ok {
		return fmt.Errorf("fsxtest: %s no existe", nombre)
	}
	delete(d.archivos, nombre)
	d.pendientes = slices.DeleteFunc(d.pendientes, func(e escritura) bool {
		return e.archivo == nombre
	})
	d.Traza.Anota("%s:remove", nombre)
	return nil
}

// Sync anota el fsync del directorio.
func (d *Disco) Sync() error {
	if d.Caido {
		return ErrCaido
	}
	d.Traza.Anota("dir:sync")
	return d.SyncFalla
}

// Existe informa si el archivo está en el directorio.
func (d *Disco) Existe(nombre string) bool {
	_, ok := d.archivos[nombre]
	return ok
}

// Nombres devuelve los archivos del directorio, ordenados.
func (d *Disco) Nombres() []string {
	return slices.Sorted(maps.Keys(d.archivos))
}

// Listar es Nombres con la firma de fsx.Dir.
func (d *Disco) Listar() ([]string, error) {
	return d.Nombres(), nil
}

// Bytes devuelve el contenido **duradero** de un archivo -- lo que sobreviviría a una
// caída ahora mismo --, o nil si no existe. Con Volatil en true, las escrituras que aún
// no han pasado por Sync no están aquí; se ven solo desde ReadAt, igual que el caché del
// sistema es coherente para el proceso pero no para el disco.
func (d *Disco) Bytes(nombre string) []byte {
	a, ok := d.archivos[nombre]
	if !ok {
		return nil
	}
	return slices.Clone(a.datos)
}

// Tamano es el tamano duradero de un archivo, o -1 si no existe.
func (d *Disco) Tamano(nombre string) int64 {
	a, ok := d.archivos[nombre]
	if !ok {
		return -1
	}
	return int64(len(a.datos))
}

// NEscrituras es cuántos WriteAt lleva el Disco sobre todos sus archivos.
func (d *Disco) NEscrituras() int {
	return d.nEscrituras
}

// Pendientes es cuántas escrituras sin sincronizar hay en la cola global.
func (d *Disco) Pendientes() int {
	return len(d.pendientes)
}

// aplica vuelca una escritura al contenido duradero de su archivo. Si el archivo ya no
// existe -- lo borró un Remove -- la escritura se pierde, que es lo correcto.
func (d *Disco) aplica(e escritura) {
	a, ok := d.archivos[e.archivo]
	if !ok {
		return
	}
	if fin := e.off + int64(len(e.datos)); fin > int64(len(a.datos)) {
		a.datos = append(a.datos, make([]byte, fin-int64(len(a.datos)))...)
	}
	copy(a.datos[e.off:], e.datos)
}

// sincroniza vuelca las pendientes de un archivo y las saca de la cola global. Las de los
// demás archivos se quedan: un fsync a datos.db no hace duraderas las escrituras al WAL.
func (d *Disco) sincroniza(nombre string) {
	resto := d.pendientes[:0:0]
	for _, e := range d.pendientes {
		if e.archivo == nombre {
			d.aplica(e)
		} else {
			resto = append(resto, e)
		}
	}
	d.pendientes = resto
}

// Archivo es un fsx.File en memoria que anota lo que hace en la traza del Disco.
type Archivo struct {
	nombre string
	disco  *Disco
	datos  []byte
	// Cerrado se pone a true en Close. Un archivo cerrado sigue legible desde el Disco:
	// lo que interesa comprobar es que el WAL cierra el que deja atrás, no impedirlo.
	Cerrado bool
}

// visible es el contenido que ve el proceso: el duradero con las pendientes de este
// archivo superpuestas en orden. Es lo que devuelve una lectura mientras el proceso sigue
// vivo, aunque nada de eso haya llegado al disco.
func (a *Archivo) visible() []byte {
	b := slices.Clone(a.datos)
	for _, e := range a.disco.pendientes {
		if e.archivo != a.nombre {
			continue
		}
		if fin := e.off + int64(len(e.datos)); fin > int64(len(b)) {
			b = append(b, make([]byte, fin-int64(len(b)))...)
		}
		copy(b[e.off:], e.datos)
	}
	return b
}

func (a *Archivo) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fsxtest: offset negativo %d", off)
	}
	if a.disco.Caido {
		return 0, ErrCaido
	}
	datos := a.visible()
	if off >= int64(len(datos)) {
		return 0, io.EOF
	}
	n := copy(p, datos[off:])
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (a *Archivo) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fsxtest: offset negativo %d", off)
	}
	if a.disco.Caido {
		return 0, ErrCaido
	}
	a.disco.nEscrituras++
	e := escritura{archivo: a.nombre, off: off, datos: slices.Clone(p)}
	a.disco.pendientes = append(a.disco.pendientes, e)
	a.disco.Traza.Anota("%s:write %d+%d", a.nombre, off, len(p))
	if a.disco.CaeEn > 0 && a.disco.nEscrituras >= a.disco.CaeEn {
		a.disco.cae()
		return 0, ErrCaido
	}
	if !a.disco.Volatil {
		a.disco.sincroniza(a.nombre)
	}
	return len(p), nil
}

func (a *Archivo) Sync() error {
	if a.disco.Caido {
		return ErrCaido
	}
	a.disco.Traza.Anota("%s:sync", a.nombre)
	a.disco.sincroniza(a.nombre)
	return nil
}

func (a *Archivo) Truncate(size int64) error {
	if a.disco.Caido {
		return ErrCaido
	}
	// El truncado vacía primero lo pendiente de este archivo: un archivo real no reordena
	// un truncado con las escrituras que ya tenía en el caché.
	a.disco.sincroniza(a.nombre)
	if size < int64(len(a.datos)) {
		a.datos = a.datos[:size]
	} else {
		a.datos = append(a.datos, make([]byte, size-int64(len(a.datos)))...)
	}
	a.disco.Traza.Anota("%s:truncate %d", a.nombre, size)
	return nil
}

func (a *Archivo) Close() error {
	a.Cerrado = true
	a.disco.Traza.Anota("%s:close", a.nombre)
	return nil
}

// Size es el tamaño visible del archivo: el duradero más lo que este proceso ha escrito y
// aún no ha sincronizado.
func (a *Archivo) Size() (int64, error) { return int64(len(a.visible())), nil }

var (
	_ fsx.Dir  = (*Disco)(nil)
	_ fsx.File = (*Archivo)(nil)
)
