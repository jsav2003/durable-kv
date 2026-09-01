//go:build !windows

package fsx

import "os"

// syncDir hace fsync sobre el directorio. Es la operación que vuelve duradera la entrada
// de un archivo en su directorio -- su creación o su borrado --, y sin ella el paso 5 del
// checkpoint (sec. 7.4) puede dejar tras una caída un WAL nuevo que no existe o un WAL
// viejo que sigue existiendo.
func syncDir(ruta string) error {
	d, err := os.Open(ruta)
	if err != nil {
		return err
	}
	// El Sync es lo que importa; el Close se cierra igual aunque el Sync falle, y el
	// error que se devuelve es el del Sync, que es el que describe el fallo de
	// durabilidad.
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
