// Package fsxtest es el sistema de archivos en memoria con el que se prueban los
// paquetes que escriben a disco.
//
// No simula ningún fallo: aquí solo hace falta observar qué se escribe, en qué archivo y
// en qué orden. La traza que comparten todos los archivos de un mismo Disco es una
// versión en miniatura del árbitro de orden global de la sec. 9.1 -- lo que hay que
// demostrar del write-ahead no es que las escrituras ocurran, sino en qué orden ocurren
// **entre datos.db y el WAL**, y con una traza por componente ese orden relativo no se
// puede observar.
//
// El disco falso de verdad -- con descartes, reordenamiento y escrituras desgarradas --
// es de la F4, y crece a partir de aquí.
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

// Disco es un fsx.Dir en memoria.
type Disco struct {
	Traza    *Traza
	archivos map[string]*Archivo
	// SyncFalla, si no es nil, es el error que devuelve el fsync del directorio. Modela
	// la plataforma donde la operación no está disponible, y el fallo real de E/S.
	SyncFalla error
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
	a := &Archivo{nombre: nombre, traza: d.Traza}
	d.archivos[nombre] = a
	d.Traza.Anota("%s:create", nombre)
	return a, nil
}

// Remove borra el archivo nombre.
func (d *Disco) Remove(nombre string) error {
	if _, ok := d.archivos[nombre]; !ok {
		return fmt.Errorf("fsxtest: %s no existe", nombre)
	}
	delete(d.archivos, nombre)
	d.Traza.Anota("%s:remove", nombre)
	return nil
}

// Sync anota el fsync del directorio.
func (d *Disco) Sync() error {
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

// Bytes devuelve el contenido de un archivo, o nil si no existe.
func (d *Disco) Bytes(nombre string) []byte {
	a, ok := d.archivos[nombre]
	if !ok {
		return nil
	}
	return slices.Clone(a.datos)
}

// Tamano es el tamano actual de un archivo, o -1 si no existe.
func (d *Disco) Tamano(nombre string) int64 {
	a, ok := d.archivos[nombre]
	if !ok {
		return -1
	}
	return int64(len(a.datos))
}

// Archivo es un fsx.File en memoria que anota lo que hace en la traza del Disco.
type Archivo struct {
	nombre string
	traza  *Traza
	datos  []byte
	// Cerrado se pone a true en Close. Un archivo cerrado sigue legible desde el Disco:
	// lo que interesa comprobar es que el WAL cierra el que deja atrás, no impedirlo.
	Cerrado bool
}

func (a *Archivo) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fsxtest: offset negativo %d", off)
	}
	if off >= int64(len(a.datos)) {
		return 0, io.EOF
	}
	n := copy(p, a.datos[off:])
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (a *Archivo) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("fsxtest: offset negativo %d", off)
	}
	if fin := off + int64(len(p)); fin > int64(len(a.datos)) {
		a.datos = append(a.datos, make([]byte, fin-int64(len(a.datos)))...)
	}
	copy(a.datos[off:], p)
	a.traza.Anota("%s:write %d+%d", a.nombre, off, len(p))
	return len(p), nil
}

func (a *Archivo) Sync() error {
	a.traza.Anota("%s:sync", a.nombre)
	return nil
}

func (a *Archivo) Truncate(size int64) error {
	if size < int64(len(a.datos)) {
		a.datos = a.datos[:size]
	} else {
		a.datos = append(a.datos, make([]byte, size-int64(len(a.datos)))...)
	}
	a.traza.Anota("%s:truncate %d", a.nombre, size)
	return nil
}

func (a *Archivo) Close() error {
	a.Cerrado = true
	a.traza.Anota("%s:close", a.nombre)
	return nil
}

// Tamano es el tamaño actual del archivo.
func (a *Archivo) Tamano() int64 { return int64(len(a.datos)) }

var (
	_ fsx.Dir  = (*Disco)(nil)
	_ fsx.File = (*Archivo)(nil)
)
