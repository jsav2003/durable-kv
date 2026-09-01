package wal

import (
	"errors"
	"io"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
	"github.com/jsav2003/motor-almacenamiento/internal/record"
)

// Grupo es un conjunto de imágenes cerrado por su registro de commit: la unidad que la
// recuperación aplica entera o descarta entera.
//
// Que la unidad sea el grupo y no el registro es el hallazgo H1. El CRC da atomicidad de
// registro, que es otra cosa: con cuatro imágenes anexadas y la última a medias, aplicar
// registro por registro deja el padre apuntando a una hoja cuya imagen faltó -- y si esa
// hoja era una página reciclada, contiene datos viejos con CRC válido, Validate() pasa en
// verde y el árbol devuelve claves de otra época.
type Grupo struct {
	// Imagenes son las páginas del grupo, en el orden en que se anexaron.
	Imagenes []*page.Page

	// Estado es el del asignador en el momento del commit: dónde está la raíz y cuánto
	// mide el archivo. Sin él en el log, una raíz que se divide deja el motor arrancando
	// con la mitad izquierda del árbol y Validate() en verde.
	Estado pager.State

	// LSNCommit es el LSN del registro de commit. Es el LSN hasta el que la recuperación
	// puede decir que llegó.
	LSNCommit uint64
}

// Leer devuelve los grupos **completos** que contiene f, que debe ser el log de la
// generación epoca, y el desplazamiento donde termina el último de ellos.
//
// Implementa los pasos 3 y 4 de la sec. 8:
//
//   - De cada registro se valida CRC (lo hace record.Reader), contigüidad del LSN y
//     época. El primero que falle cualquiera de las tres marca el punto exacto donde
//     ocurrió la caída: ahí se detiene la lectura y se descarta todo lo que siga, **aunque
//     más adelante haya registros que parezcan válidos**.
//   - Un grupo sin su registro de commit se descarta entero.
//
// El segundo valor devuelto es el final del último grupo completo, no el del último
// registro válido: los registros huérfanos que quedaran detrás de él no existen a efectos
// de recuperación, y un escritor que reanudara más allá los daría por buenos en la
// siguiente caída.
//
// El error nunca es nil: describe por qué terminó la lectura, y io.EOF es el final limpio.
// EsFinDeLog lo clasifica.
func Leer(f fsx.File, epoca uint32, desdeLSN uint64) ([]Grupo, int64, error) {
	var (
		grupos   []Grupo
		abierto  []*page.Page
		esperado = desdeLSN + 1
		fin      int64
		r        = record.NewReader(f, 0)
	)

	for {
		rec, err := r.Next()
		if err != nil {
			return grupos, fin, err
		}
		// Contigüidad y época se comprueban antes de mirar la carga: un registro que no
		// pertenece a esta secuencia no merece que se interprete su contenido.
		if rec.LSN != esperado {
			return grupos, fin, ErrLSNNoContiguo
		}
		if rec.Epoch != epoca {
			return grupos, fin, ErrEpocaAjena
		}

		switch rec.Type {
		case TipoImagen:
			p, err := DecodificaImagen(rec.Payload)
			if err != nil {
				return grupos, fin, err
			}
			abierto = append(abierto, p)

		case TipoCommit:
			n, st, err := DecodificaCommit(rec.Payload)
			if err != nil {
				return grupos, fin, err
			}
			// n_registros es redundante con lo contado, y por eso sirve: un grupo que
			// declara cuatro imágenes y trae tres es un log que no es lo que dice ser, y
			// aplicarlo sería aplicar medio grupo con el commit por delante.
			if int(n) != len(abierto) {
				return grupos, fin, ErrGrupoIncompleto
			}
			grupos = append(grupos, Grupo{Imagenes: abierto, Estado: st, LSNCommit: rec.LSN})
			abierto = nil
			fin = r.Offset()

		default:
			return grupos, fin, ErrTipoDesconocido
		}
		esperado++
	}
}

// EsFinDeLog informa si err es una de las formas de "aquí termina el log": el final
// limpio, o cualquiera de las maneras en que la sec. 8 reconoce el punto de una caída.
//
// Lo que deja fuera es un fallo de E/S de verdad. Los dos casos llegan a Leer por el
// mismo camino y con la misma pinta -- una lectura que no devuelve lo que se pedía --,
// pero significan cosas opuestas: uno es un archivo que termina donde el sistema se cayó,
// y el otro es un disco que no responde. Tratar el segundo como el primero descartaría
// datos confirmados y llamaría a eso una recuperación correcta.
func EsFinDeLog(err error) bool {
	switch {
	case errors.Is(err, io.EOF):
		return true
	case errors.Is(err, record.ErrCorrupt), errors.Is(err, record.ErrTruncated),
		errors.Is(err, record.ErrTooLarge):
		return true
	case errors.Is(err, ErrLSNNoContiguo), errors.Is(err, ErrEpocaAjena),
		errors.Is(err, ErrGrupoIncompleto), errors.Is(err, ErrTipoDesconocido),
		errors.Is(err, ErrCargaInvalida):
		return true
	case errors.Is(err, page.ErrCorrupt), errors.Is(err, page.ErrWrongPage),
		errors.Is(err, page.ErrBadType), errors.Is(err, page.ErrBadSize):
		return true
	}
	return false
}
