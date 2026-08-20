// Package record implementa la capa física de registros de longitud variable: el marco
// binario que enmarca, delimita y protege con CRC un fragmento de datos dentro de un
// archivo de solo-agregar.
//
// Es el ancestro del formato de registro del WAL (DESIGN.md sec. 7.3). No conoce nada
// del significado de una imagen de página ni de un commit -- solo sabe escribir "aquí
// hay N bytes, protegidos" y leerlos de vuelta, deteniéndose exactamente en el primer
// registro que no cuadre (DESIGN.md sec. 8, paso 3).
package record

// File es la interfaz mínima de E/S que necesita este paquete (DESIGN.md sec. 9.1).
//
// *os.File la satisface directamente: sus métodos ReadAt, WriteAt, Sync y Truncate ya
// tienen exactamente esta firma, así que no hace falta un tipo envoltorio.
//
// En esta fase (F0) solo existe la implementación de producción, sobre *os.File. La
// implementación con inyección de fallos (disco falso, torn writes) llega en la F4.
// Definir la interfaz ahora -- en vez de programar Writer y Reader contra *os.File y
// adaptarlos después -- es lo que evita reescribirlos cuando llegue esa fase.
type File interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
}
