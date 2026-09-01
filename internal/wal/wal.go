package wal

import (
	"strconv"
	"strings"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
	"github.com/jsav2003/motor-almacenamiento/internal/record"
)

// prefijo es la parte fija del nombre de un archivo de log.
const prefijo = "datos.wal."

// Nombre es el del archivo de log de una generación (DESIGN.md sec. 5). La generación va
// en el nombre porque el WAL no se trunca en sitio, se rota: escribir registros nuevos
// encima de bytes de registros viejos es la causa de una clase entera de bugs de
// recuperación.
func Nombre(epoca uint32) string {
	return prefijo + strconv.FormatUint(uint64(epoca), 10)
}

// Generacion es la inversa de Nombre: devuelve la generación que nombra un archivo, y
// false si el nombre no es el de un log.
//
// Existe porque con las dos metas inválidas el nombre del archivo es lo único que lleva la
// generación (sec. 8, paso 1), y porque una caída en mitad de una rotación puede dejar dos
// generaciones en el directorio. Vive aquí, pegada a Nombre, para que la convención esté en
// un solo sitio: dos sitios que la conocen son dos sitios que pueden discrepar.
func Generacion(nombre string) (uint32, bool) {
	resto, ok := strings.CutPrefix(nombre, prefijo)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseUint(resto, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// WAL es el write-ahead log abierto para escritura. Implementa pager.Log, cuya firma
// congeló la F1 y aquí no se toca.
//
// # Las tres defensas de la sec. 7.4
//
// La regla de la sec. 8 -- "el primer registro con CRC inválido marca el punto exacto de
// la caída" -- solo es cierta si el log nunca se reescribe sobre bytes ya usados. Las
// tres defensas que la sostienen viven aquí:
//
//  1. El LSN es estrictamente monótono y **nunca se reinicia**, tampoco al rotar. La
//     lectura exige contigüidad y cualquier salto termina el log.
//  2. Cada registro lleva la época, y la lectura la verifica. Un registro de otra
//     generación termina el log.
//  3. Se rota en vez de truncar, con fsync de directorio (Rotar).
//
// Son tres para que ninguna sea la única: en Windows la tercera está capada (ver
// docs/DEUDA-DISENO.md, D10) y las otras dos siguen operativas.
type WAL struct {
	dir fsx.Dir
	f   fsx.File
	w   *record.Writer

	epoca uint32

	// lsn es el último LSN asignado; flushed, el último que está ya sincronizado. Que
	// puedan diferir es justo lo que la regla de desalojo de la sec. 7.5 consulta.
	lsn     uint64
	flushed uint64

	// nGrupo cuenta las imágenes anexadas desde el último commit. Viaja en el registro de
	// commit como n_registros.
	nGrupo uint32

	// buf es el marco reutilizado para la carga de una imagen. record.Writer copia al
	// serializar, así que reutilizarlo es seguro y ahorra una asignación de 4 KiB por
	// página ensuciada.
	buf []byte
}

// Abrir abre la generación epoca del log y sigue escribiendo en offset, con lsn como
// último LSN asignado.
//
// Los tres valores vienen de la recuperación (sec. 8) o de la meta, nunca del tamaño del
// archivo: el LSN no se reinicia al rotar, así que una generación nueva empieza en el
// offset 0 con un LSN que puede ser enorme.
//
// flushed arranca igual a lsn porque todo lo que hay antes de offset se leyó del disco:
// ya está ahí.
func Abrir(dir fsx.Dir, epoca uint32, lsn uint64, offset int64) (*WAL, error) {
	f, err := dir.Open(Nombre(epoca))
	if err != nil {
		return nil, err
	}
	return &WAL{
		dir:     dir,
		f:       f,
		w:       record.NewWriter(f, offset),
		epoca:   epoca,
		lsn:     lsn,
		flushed: lsn,
		buf:     make([]byte, CargaImagen),
	}, nil
}

// LogPage asigna el LSN del registro que va a describir a p, lo sella en p.LSN, y anexa
// la imagen completa. No sincroniza: eso es Commit.
//
// El orden -- sellar antes de serializar -- es el de la sec. 7.2, pasos 2 y 3. Al revés,
// la imagen que va al log llevaría dentro un page_lsn viejo, y al reaplicarla la
// recuperación dejaría en disco una página cuyo page_lsn miente sobre qué registro la
// modificó por última vez. Eso rompería la regla de desalojo en la siguiente corrida.
func (w *WAL) LogPage(p *page.Page) (uint64, error) {
	w.lsn++
	p.LSN = w.lsn
	if err := CodificaImagen(w.buf, p); err != nil {
		return 0, err
	}
	err := w.w.Append(record.Record{
		LSN:     w.lsn,
		Type:    TipoImagen,
		Epoch:   w.epoca,
		Payload: w.buf,
	})
	if err != nil {
		return 0, err
	}
	w.nGrupo++
	return w.lsn, nil
}

// Commit escribe el registro de commit del grupo abierto y hace fsync (sec. 7.2, pasos 4
// y 5). Hasta que devuelve nil nada del grupo es duradero; después, todo lo es, y es
// cuando Put puede devolver nil.
//
// El commit consume un LSN propio. Tiene que consumirlo: la contigüidad de la sec. 7.4 se
// comprueba sobre todos los registros, y un commit sin LSN dejaría un hueco que la
// lectura interpretaría como el punto de la caída.
func (w *WAL) Commit(st pager.State) error {
	w.lsn++
	err := w.w.Append(record.Record{
		LSN:     w.lsn,
		Type:    TipoCommit,
		Epoch:   w.epoca,
		Payload: CodificaCommit(w.nGrupo, st),
	})
	if err != nil {
		return err
	}
	if err := w.w.Sync(); err != nil {
		return err
	}
	w.flushed = w.lsn
	w.nGrupo = 0
	return nil
}

// FlushedLSN es el LSN más alto cuyo registro está ya sincronizado.
func (w *WAL) FlushedLSN() uint64 { return w.flushed }

// Sync fuerza el fsync del log fuera de un commit. Lo llama el pager cuando una página
// sucia tiene que bajar a datos.db y su page_lsn va por delante de lo sincronizado.
func (w *WAL) Sync() error {
	if err := w.w.Sync(); err != nil {
		return err
	}
	w.flushed = w.lsn
	return nil
}

// LSN es el último LSN asignado. Lo consume el checkpoint para escribirlo en la meta.
func (w *WAL) LSN() uint64 { return w.lsn }

// Epoca es la generación actual del log.
func (w *WAL) Epoca() uint32 { return w.epoca }

// Bytes es cuánto ocupa la generación actual. Como el checkpoint siempre rota, es también
// cuánto log se ha acumulado desde el último checkpoint, que es lo que dispara el
// siguiente (sec. 7.4).
func (w *WAL) Bytes() int64 { return w.w.Offset() }

// Rotar pasa a la generación siguiente: es el paso 5 del checkpoint (sec. 7.4).
//
// El orden no es negociable, y es la razón de que este método exista en vez de dejarlo al
// llamador: **crear el archivo nuevo y hacer fsync del directorio antes de borrar el
// viejo**. Al revés, una caída entre el borrado y la creación deja al motor sin ninguna
// de las dos generaciones, y el WAL es la única copia buena de lo confirmado que todavía
// no bajó a datos.db.
//
// El LSN no se reinicia. Es la defensa 1 de la sec. 7.4 y es lo que hace que un registro
// de la generación vieja que sobreviviera en cualquier parte rompa la contigüidad en vez
// de pasar por bueno.
func (w *WAL) Rotar() error {
	vieja := w.epoca
	nueva := vieja + 1

	f, err := w.dir.Open(Nombre(nueva))
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := w.dir.Sync(); err != nil {
		return err
	}

	// Recién ahora el archivo nuevo existe de forma duradera y se puede soltar el viejo.
	viejo := w.f
	w.f = f
	w.w = record.NewWriter(f, 0)
	w.epoca = nueva

	if err := viejo.Close(); err != nil {
		return err
	}
	if err := w.dir.Remove(Nombre(vieja)); err != nil {
		return err
	}
	return w.dir.Sync()
}

// Close cierra el archivo de log. No sincroniza: quien quiera durabilidad llama a Sync o
// a Commit, que es donde la sec. 7.2 la sitúa.
func (w *WAL) Close() error { return w.f.Close() }

var _ pager.Log = (*WAL)(nil)
