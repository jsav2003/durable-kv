package fsx_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx/fsxtest"
)

func abre(t *testing.T) (fsx.Dir, string) {
	t.Helper()
	ruta := t.TempDir()
	d, err := fsx.Abrir(ruta)
	if err != nil {
		t.Fatalf("Abrir: %v", err)
	}
	return d, ruta
}

func TestAbrirExigeUnDirectorioQueExista(t *testing.T) {
	if _, err := fsx.Abrir(filepath.Join(t.TempDir(), "no-existe")); err == nil {
		t.Fatal("Abrir sobre una ruta inexistente no dio error")
	}
}

// La ruta de un archivo se abre sin problema y aun así no sirve como directorio, así que
// el error de os.Stat no distingue este caso: por eso es un error propio.
func TestAbrirRechazaUnArchivo(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "archivo")
	if err := os.WriteFile(ruta, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := fsx.Abrir(ruta)
	if !errors.Is(err, fsx.ErrNoEsDirectorio) {
		t.Fatalf("error = %v, quiero ErrNoEsDirectorio", err)
	}
}

func TestOpenCreaYConserva(t *testing.T) {
	d, ruta := abre(t)

	f, err := d.Open("datos.wal.0")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.WriteAt([]byte("hola"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reabrir no trunca: es lo que la recuperación necesita, porque el archivo que va a
	// leer se abre antes de leerlo.
	f2, err := d.Open("datos.wal.0")
	if err != nil {
		t.Fatalf("Open (segunda): %v", err)
	}
	defer f2.Close()
	buf := make([]byte, 4)
	if _, err := f2.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "hola" {
		t.Errorf("contenido = %q, quiero \"hola\"", buf)
	}

	if _, err := os.Stat(filepath.Join(ruta, "datos.wal.0")); err != nil {
		t.Errorf("el archivo no esta en el directorio: %v", err)
	}
}

func TestRemove(t *testing.T) {
	d, ruta := abre(t)

	f, err := d.Open("datos.wal.7")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()

	if err := d.Remove("datos.wal.7"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ruta, "datos.wal.7")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("el archivo sigue ahi: %v", err)
	}
}

// El Sync del directorio devuelve nil en las dos plataformas, pero por razones
// distintas: en Unix porque el fsync ocurrió, y en Windows porque la operación no existe
// y se documenta como no-op (ver os_windows.go y docs/DEUDA-DISENO.md). El test fija que
// el llamador no tiene que distinguirlas.
func TestSyncDelDirectorio(t *testing.T) {
	d, _ := abre(t)

	f, err := d.Open("datos.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	f.Close()

	if err := d.Sync(); err != nil {
		t.Fatalf("Sync en %s: %v", runtime.GOOS, err)
	}
}

// fsx.File es un superconjunto de la interfaz de la sec. 9.1, así que vale directamente
// donde internal/record y internal/pager piden la suya. Que eso siga siendo cierto se
// comprueba en tiempo de compilación y no de ejecución.
func TestFileSirveDondePidenLaDeLaSec91(t *testing.T) {
	var f fsx.File = &fsxtest.Archivo{}
	var _ interface {
		ReadAt(p []byte, off int64) (int, error)
		WriteAt(p []byte, off int64) (int, error)
		Sync() error
		Truncate(size int64) error
	} = f
}
