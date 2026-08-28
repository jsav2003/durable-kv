package pager

import (
	"fmt"
	"io"

	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

// Este archivo tiene los dos dobles de prueba del paquete: un File en memoria y un Log
// instrumentado. Los dos anotan lo que hacen en una traza compartida.
//
// La traza compartida es una versión en miniatura del árbitro de orden global de la sec.
// 9.1: lo que importa de la sec. 7.6 y de la 7.2 no es que la extensión y el commit
// ocurran, sino en qué orden ocurren **entre los dos archivos**. Con una traza por
// componente ese orden relativo no se puede observar, y un pager que hiciera las cosas al
// revés pasaría los tests. El disco falso de verdad, con descartes, reordenamiento y
// escrituras desgarradas, es de la F4.

// traza es la secuencia de eventos observados, compartida por el File y el Log.
type traza struct {
	eventos []string
}

func (t *traza) anota(formato string, args ...any) {
	t.eventos = append(t.eventos, fmt.Sprintf(formato, args...))
}

func (t *traza) limpia() { t.eventos = nil }

// contiene informa si la traza incluye el evento exacto e.
func (t *traza) contiene(e string) bool {
	for _, v := range t.eventos {
		if v == e {
			return true
		}
	}
	return false
}

// indice devuelve la posición del primer evento igual a e, o -1.
func (t *traza) indice(e string) int {
	for i, v := range t.eventos {
		if v == e {
			return i
		}
	}
	return -1
}

// memFile es un File en memoria. No simula ningún fallo: aquí solo hace falta observar
// qué se escribe y en qué orden.
type memFile struct {
	datos []byte
	tz    *traza
}

func nuevoMemFile(tz *traza) *memFile { return &memFile{tz: tz} }

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("memFile: offset negativo %d", off)
	}
	if off >= int64(len(f.datos)) {
		return 0, io.EOF
	}
	n := copy(p, f.datos[off:])
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("memFile: offset negativo %d", off)
	}
	if fin := off + int64(len(p)); fin > int64(len(f.datos)) {
		f.datos = append(f.datos, make([]byte, fin-int64(len(f.datos)))...)
	}
	copy(f.datos[off:], p)
	f.tz.anota("datos:write %d", off/page.Size)
	return len(p), nil
}

func (f *memFile) Sync() error {
	f.tz.anota("datos:sync")
	return nil
}

func (f *memFile) Truncate(size int64) error {
	if size < int64(len(f.datos)) {
		f.datos = f.datos[:size]
	} else {
		f.datos = append(f.datos, make([]byte, size-int64(len(f.datos)))...)
	}
	f.tz.anota("datos:truncate %d", size)
	return nil
}

// paginas devuelve cuántas páginas completas cubre el archivo.
func (f *memFile) paginas() uint64 { return uint64(len(f.datos) / page.Size) }

// logDePrueba es un Log instrumentado. A diferencia de NopLog, permite que lo
// sincronizado se quede por detrás de lo asignado, que es la única forma de ejercitar la
// regla de desalojo de la sec. 7.5.
type logDePrueba struct {
	tz  *traza
	lsn uint64
	// sincronizado es el FlushedLSN. Un WAL correcto lo pone al día en cada Commit;
	// commitSincroniza a false modela uno que no lo hace, para forzar a writePage a pedir
	// el Sync.
	sincronizado     uint64
	commitSincroniza bool
	// syncAlcanza a false modela un Log roto: ni siquiera Sync pone al día lo
	// sincronizado. Es lo que debe acabar en ErrWriteAhead.
	syncAlcanza bool
	commits     []State
}

func nuevoLog(tz *traza) *logDePrueba {
	return &logDePrueba{tz: tz, commitSincroniza: true, syncAlcanza: true}
}

func (l *logDePrueba) LogPage(p *page.Page) (uint64, error) {
	l.lsn++
	p.LSN = l.lsn
	l.tz.anota("wal:imagen %d lsn=%d", p.ID, l.lsn)
	return l.lsn, nil
}

func (l *logDePrueba) Commit(st State) error {
	l.commits = append(l.commits, st)
	if l.commitSincroniza {
		l.sincronizado = l.lsn
	}
	l.tz.anota("wal:commit root=%d total=%d", st.RootID, st.TotalPages)
	return nil
}

func (l *logDePrueba) FlushedLSN() uint64 { return l.sincronizado }

func (l *logDePrueba) Sync() error {
	if l.syncAlcanza {
		l.sincronizado = l.lsn
	}
	l.tz.anota("wal:sync")
	return nil
}

// ultimoCommit devuelve el estado del último registro de commit escrito.
func (l *logDePrueba) ultimoCommit() State {
	return l.commits[len(l.commits)-1]
}
