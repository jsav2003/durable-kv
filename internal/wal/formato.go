// Package wal implementa el write-ahead log de la sec. 7 del DESIGN.md: el archivo al
// que solo se agrega, donde un cambio queda descrito y sincronizado antes de tocar
// datos.db.
//
// Se apoya en internal/record, que ya da el marco físico -- lsn, tipo, época, longitud y
// crc32 Castagnoli -- y no conoce el significado de ninguna carga. Este paquete pone
// encima las dos cargas que la sec. 7.3 define, la disciplina de LSN y época de la sec.
// 7.4, y la rotación.
package wal

import (
	"encoding/binary"

	"github.com/jsav2003/durable-kv/internal/page"
	"github.com/jsav2003/durable-kv/internal/pager"
	"github.com/jsav2003/durable-kv/internal/record"
)

// Los dos tipos de registro de la sec. 7.3. internal/record transporta el campo sin
// interpretarlo; el significado se fija aquí.
const (
	// TipoImagen transporta la imagen completa de una página. No es una descripción
	// lógica de la operación ("insertar X") a propósito: un log lógico necesita que el
	// árbol de partida esté consistente para reaplicarse, y si el sistema se cayó
	// escribiendo una página no lo está. La imagen se puede reaplicar sobre cualquier
	// estado, incluso sobre una página corrupta, porque la sobrescribe entera.
	TipoImagen record.Type = 1

	// TipoCommit cierra un grupo y transporta el estado del asignador. Es lo que
	// convierte la atomicidad de registro que da el CRC en la atomicidad de grupo que
	// pide la sec. 7.3: un grupo sin su commit se descarta entero.
	TipoCommit record.Type = 2
)

const (
	// CargaImagen es page_id (8) + la página completa. El page_id va fuera de la página
	// además de dentro de ella: es redundante, y esa redundancia es la comprobación --
	// una imagen cuyo page_id exterior no coincida con el que la página lleva en su
	// cabecera es un registro que se escribió mal o que se leyó del sitio equivocado, y
	// aplicarla dejaría una página íntegra en la ranura que no le toca.
	CargaImagen = 8 + page.Size

	// CargaCommit es n_registros (4) + root_id (8) + free_head (8) + total_pages (8).
	CargaCommit = 4 + 8 + 8 + 8
)

// Offsets dentro de cada carga. Declarados una sola vez y usados por los dos lados, por
// la misma razón que en internal/page: dos juegos de literales en dos funciones pueden
// equivocarse igual y dejar pasar un round-trip con el formato en disco mal.
const (
	offImagenPageID = 0
	offImagenPagina = 8

	offCommitNRegistros = 0
	offCommitRootID     = 4
	offCommitFreeHead   = 12
	offCommitTotalPages = 20
)

// CodificaImagen serializa p como carga de un registro TipoImagen sobre dst, que debe
// medir exactamente CargaImagen bytes.
//
// Escribe sobre un buffer prestado en vez de devolver uno nuevo, igual que
// page.EncodeTo y por el mismo motivo: el WAL anexa una imagen por página ensuciada, así
// que una asignación aquí sería una asignación por página y por operación del árbol.
func CodificaImagen(dst []byte, p *page.Page) error {
	if len(dst) != CargaImagen {
		return ErrCargaInvalida
	}
	binary.LittleEndian.PutUint64(dst[offImagenPageID:], p.ID)
	return p.EncodeTo(dst[offImagenPagina:])
}

// DecodificaImagen interpreta la carga de un registro TipoImagen.
//
// La verificación del page_id exterior contra el interior sale gratis: page.Decode ya
// compara el page_id de la cabecera contra la ranura que se le declara, así que basta
// con pasarle el de la carga. Devuelve page.ErrWrongPage si discrepan y page.ErrCorrupt
// si el CRC de la página no cuadra -- los errores de page se propagan tal cual, porque
// el que sea dice qué pasó.
func DecodificaImagen(carga []byte) (*page.Page, error) {
	if len(carga) != CargaImagen {
		return nil, ErrCargaInvalida
	}
	id := binary.LittleEndian.Uint64(carga[offImagenPageID:])
	return page.Decode(carga[offImagenPagina:], id)
}

// CodificaCommit serializa el cierre de un grupo de n registros con el estado st del
// asignador. Devuelve un buffer nuevo: hay un commit por grupo, no uno por página, así
// que la asignación es por operación del árbol y no por página tocada.
func CodificaCommit(n uint32, st pager.State) []byte {
	buf := make([]byte, CargaCommit)
	binary.LittleEndian.PutUint32(buf[offCommitNRegistros:], n)
	binary.LittleEndian.PutUint64(buf[offCommitRootID:], st.RootID)
	binary.LittleEndian.PutUint64(buf[offCommitFreeHead:], st.FreeHead)
	binary.LittleEndian.PutUint64(buf[offCommitTotalPages:], st.TotalPages)
	return buf
}

// DecodificaCommit interpreta la carga de un registro TipoCommit y devuelve cuántas
// imágenes declara el grupo y el estado del asignador que transporta.
//
// n_registros es redundante con lo que el lector ya ha contado, y se conserva porque la
// sec. 7.3 lo declara en el formato: sirve de comprobación cruzada en la recuperación,
// donde un grupo que declara cuatro imágenes y trae tres es un log que no es lo que dice
// ser.
func DecodificaCommit(carga []byte) (uint32, pager.State, error) {
	if len(carga) != CargaCommit {
		return 0, pager.State{}, ErrCargaInvalida
	}
	n := binary.LittleEndian.Uint32(carga[offCommitNRegistros:])
	st := pager.State{
		RootID:     binary.LittleEndian.Uint64(carga[offCommitRootID:]),
		FreeHead:   binary.LittleEndian.Uint64(carga[offCommitFreeHead:]),
		TotalPages: binary.LittleEndian.Uint64(carga[offCommitTotalPages:]),
	}
	return n, st, nil
}
