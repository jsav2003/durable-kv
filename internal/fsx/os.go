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

// Crear es Abrir creando el directorio si no existe, con sus padres. Lo usa quien abre una
// base de datos: ahí, una ruta que no existe es "todavía no hay base", no una equivocación.
func Crear(ruta string) (Dir, error) {
	if err := os.MkdirAll(ruta, 0o755); err != nil {
		return nil, err
	}
	return Abrir(ruta)
}

func (d *dirOS) Open(nombre string) (File, error) {
	// Sin O_TRUNC y sin O_APPEND: el WAL se posiciona por offset explícito (record.Writer
	// lleva el suyo), y truncar al abrir borraría justo el log que la recuperación va a
	// leer.
	f, err := os.OpenFile(filepath.Join(d.ruta, nombre), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return archivoOS{f}, nil
}

func (d *dirOS) Listar() ([]string, error) {
	entradas, err := os.ReadDir(d.ruta)
	if err != nil {
		return nil, err
	}
	nombres := make([]string, 0, len(entradas))
	for _, e := range entradas {
		if !e.IsDir() {
			nombres = append(nombres, e.Name())
		}
	}
	return nombres, nil
}

// archivoOS envuelve *os.File solo para dar Size, que os.File expone como Stat().Size().
// Los otros cinco métodos ya tienen la firma exacta de File y pasan por promoción.
type archivoOS struct {
	*os.File
}

func (a archivoOS) Size() (int64, error) {
	info, err := a.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (d *dirOS) Remove(nombre string) error {
	return os.Remove(filepath.Join(d.ruta, nombre))
}

func (d *dirOS) Sync() error {
	return syncDir(d.ruta)
}
