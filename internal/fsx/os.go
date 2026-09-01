package fsx

import (
	"os"
	"path/filepath"
)

// dirOS es el Dir de producción: un directorio real del sistema de archivos.
type dirOS struct {
	ruta string
}

// Abrir devuelve el Dir de producción sobre ruta, que debe existir ya. No la crea: el
// directorio de una base de datos lo elige el usuario, y crearlo en silencio convertiría
// una ruta mal escrita en una base vacía en el sitio equivocado.
func Abrir(ruta string) (Dir, error) {
	info, err := os.Stat(ruta)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, ErrNoEsDirectorio
	}
	return &dirOS{ruta: ruta}, nil
}

func (d *dirOS) Open(nombre string) (File, error) {
	// Sin O_TRUNC y sin O_APPEND: el WAL se posiciona por offset explícito (record.Writer
	// lleva el suyo), y truncar al abrir borraría justo el log que la recuperación va a
	// leer.
	return os.OpenFile(filepath.Join(d.ruta, nombre), os.O_RDWR|os.O_CREATE, 0o644)
}

func (d *dirOS) Remove(nombre string) error {
	return os.Remove(filepath.Join(d.ruta, nombre))
}

func (d *dirOS) Sync() error {
	return syncDir(d.ruta)
}
