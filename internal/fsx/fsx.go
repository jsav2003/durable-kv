// Package fsx es la capa de sistema de archivos por la que pasa toda la E/S del motor.
//
// La interfaz File de la sec. 9.1 del DESIGN.md ya está declarada, por duplicado y a
// propósito, en internal/record y en internal/pager: en Go la declara quien la consume.
// Lo que ninguna de las dos cubre son las operaciones **de directorio** -- crear un
// archivo, borrarlo, y hacerle fsync al directorio -- que la rotación del WAL de la sec.
// 7.4 necesita y que no son operaciones sobre un archivo ya abierto.
//
// Existe además por una segunda razón, que es la que decide su forma: es la pieza que la
// F4 sustituye por el disco falso con árbitro de orden global (sec. 9.1). Si el WAL
// llamara a os directamente, ese arnés no tendría dónde engancharse y el orden relativo
// entre las escrituras a datos.db y las del log -- que *es* el write-ahead logging --
// nunca se modelaría.
package fsx

// File es un archivo abierto. Los cuatro primeros métodos son los de la sec. 9.1, así
// que un File satisface también las interfaces homónimas de internal/record y de
// internal/pager sin conversión.
//
// Close no está en la de la sec. 9.1 porque allí no hacía falta: el WAL sí lo necesita,
// porque la rotación deja atrás un archivo que hay que cerrar antes de borrarlo.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
	Close() error
}

// Dir es el directorio donde viven datos.db y las generaciones del WAL.
type Dir interface {
	// Open abre el archivo nombre, creándolo si no existe. No trunca: un archivo que ya
	// tenía contenido se abre con él, que es lo que la recuperación necesita.
	Open(nombre string) (File, error)

	// Remove borra el archivo nombre.
	Remove(nombre string) error

	// Sync hace fsync sobre el directorio: es lo que hace duradera la *creación* o el
	// *borrado* de un archivo, no su contenido.
	//
	// Sin él, la sec. 7.4 avisa de que en ext4/XFS la creación del WAL puede no ser
	// duradera -- Put devolvería nil y tras la caída el archivo de log no existiría.
	Sync() error
}
