//go:build windows

package fsx

// syncDir no hace nada en Windows, y es una limitación conocida y no un olvido.
//
// La sec. 7.4 exige fsync de directorio tras crear, rotar o borrar un archivo. Windows no
// ofrece esa operación: FlushFileBuffers necesita un handle con permiso de escritura, y
// un handle de directorio no lo admite -- la llamada devuelve ERROR_ACCESS_DENIED. No hay
// equivalente en la API Win32, así que no es cuestión de encontrar la llamada correcta.
//
// Consecuencia concreta: en Windows, tras una caída inmediatamente posterior a una
// rotación, la entrada de directorio del WAL nuevo puede no haber llegado al disco. El
// checkpoint que la creó había hecho ya fsync de datos.db y de la meta (pasos 2 y 4), así
// que lo confirmado está a salvo; lo que puede faltar es un archivo de log vacío que la
// reapertura vuelve a crear.
//
// Se devuelve nil y no un error porque el llamador no tiene nada mejor que hacer con él:
// un checkpoint que abortara aquí dejaría el motor inutilizable en la plataforma de
// desarrollo. El camino real se ejercita en el CI de Linux (F6). Ver docs/DEUDA-DISENO.md.
func syncDir(string) error { return nil }
