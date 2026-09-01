// Package meta implementa las dos páginas meta de la sec. 5.2 del DESIGN.md: las ranuras
// 0 y 1 de datos.db, escritas alternadamente en cada checkpoint.
//
// # Qué es y qué no es
//
// La meta **no es la fuente de la verdad**. El estado global auténtico viaja en los
// registros de commit del WAL (sec. 7.3), y la meta solo existe para no tener que leer el
// log entero en cada Open. De ahí se sigue lo que más peso tiene en esta implementación:
// que las dos ranuras fallen su verificación **no es un archivo irrecuperable**. Es lo que
// Leer expresa devolviendo ErrSinMetaValida en vez de un error fatal, y lo que le toca
// hacer al llamador es reproducir el WAL desde el LSN 0 (sec. 8, paso 1).
//
// # Por qué dos, alternadas
//
// Al hacer checkpoint se escribe siempre sobre la **más antigua**. Si el sistema se cae
// escribiendo una, la otra sigue intacta. Es la misma técnica de BoltDB, y solo funciona
// si las metas quedan fuera del conjunto de páginas sucias del pager -- si estuvieran, el
// paso 1 del checkpoint escribiría la ranura nueva y el paso 3 la vieja, dejando las dos
// con estado de la misma época y evaporando la garantía. Lo impone internal/pager con
// ErrMetaPage.
package meta

import (
	"encoding/binary"

	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

// Ranuras es cuántas páginas meta hay: las 0 y 1, que es también pager.MetaPages.
const Ranuras = 2

// Version es la versión del formato en disco. Un archivo escrito por una versión que este
// código no conoce se rechaza en vez de interpretarse: los campos podrían significar otra
// cosa, y el CRC no distingue "íntegro" de "íntegro y de otro formato".
const Version uint32 = 1

// magico identifica el archivo como de este motor. Va en texto y no como un uint64 para
// que se reconozca de un vistazo en un volcado hexadecimal, que es donde se mira cuando
// una recuperación no cuadra.
var magico = [8]byte{'M', 'O', 'T', 'O', 'R', '-', 'K', 'V'}

// Offsets dentro del cuerpo de la página. El LSN del checkpoint **no está aquí**: es el
// page_lsn de la cabecera. Guardarlo también en el cuerpo sería un segundo lugar donde
// vive el mismo dato, que es donde el dato puede discrepar -- el razonamiento de D3 y D5
// en docs/DEUDA-DISENO.md, aplicado a la página donde más caro saldría.
const (
	offMagico     = 0
	offVersion    = 8
	offRootID     = 12
	offFreeHead   = 20
	offTotalPages = 28
	offEpoca      = 36
	tamCuerpo     = 40
)

// Meta es el contenido de una página meta.
type Meta struct {
	// RootID, FreeHead y TotalPages son el estado del asignador, el mismo que viaja en
	// cada registro de commit. FreeHead se escribe y no se lee: la sec. 6.1 decide
	// reconstruir el conjunto de libres al abrir en vez de persistirlo, y el campo se
	// conserva en el formato por si esa decisión cambiara.
	RootID     uint64
	FreeHead   uint64
	TotalPages uint64

	// Epoca es la generación del WAL en la que hay que buscar los registros posteriores a
	// este checkpoint.
	Epoca uint32

	// LSN es el del checkpoint que escribió esta meta. Viaja en el page_lsn de la
	// cabecera, no en el cuerpo, y es lo que decide cuál de las dos ranuras gana.
	LSN uint64
}

// Pagina serializa m como la página que ocupa la ranura indicada.
func (m Meta) Pagina(ranura uint64) (*page.Page, error) {
	if ranura >= Ranuras {
		return nil, ErrRanuraInvalida
	}
	p := page.New(ranura, page.TypeMeta)
	p.LSN = m.LSN
	copy(p.Body[offMagico:], magico[:])
	binary.LittleEndian.PutUint32(p.Body[offVersion:], Version)
	binary.LittleEndian.PutUint64(p.Body[offRootID:], m.RootID)
	binary.LittleEndian.PutUint64(p.Body[offFreeHead:], m.FreeHead)
	binary.LittleEndian.PutUint64(p.Body[offTotalPages:], m.TotalPages)
	binary.LittleEndian.PutUint32(p.Body[offEpoca:], m.Epoca)
	return p, nil
}

// DeLaPagina interpreta una página ya decodificada como meta. Presupone que page.Decode
// verificó CRC y page_id: lo que queda por comprobar aquí es que la página es una meta de
// este motor y de esta versión del formato.
func DeLaPagina(p *page.Page) (Meta, error) {
	if p.Type != page.TypeMeta {
		return Meta{}, ErrNoEsMeta
	}
	if len(p.Body) < tamCuerpo {
		return Meta{}, ErrNoEsMeta
	}
	if [8]byte(p.Body[offMagico:offVersion]) != magico {
		return Meta{}, ErrMagicoInvalido
	}
	if v := binary.LittleEndian.Uint32(p.Body[offVersion:]); v != Version {
		return Meta{}, ErrVersionInvalida
	}
	return Meta{
		RootID:     binary.LittleEndian.Uint64(p.Body[offRootID:]),
		FreeHead:   binary.LittleEndian.Uint64(p.Body[offFreeHead:]),
		TotalPages: binary.LittleEndian.Uint64(p.Body[offTotalPages:]),
		Epoca:      binary.LittleEndian.Uint32(p.Body[offEpoca:]),
		LSN:        p.LSN,
	}, nil
}

// Escribir serializa m en la ranura indicada de f. No sincroniza: el fsync es el paso 4
// del checkpoint y lo da el llamador, igual que el pager separa escribir de sincronizar.
func Escribir(f fsx.File, ranura uint64, m Meta) error {
	p, err := m.Pagina(ranura)
	if err != nil {
		return err
	}
	buf := make([]byte, page.Size)
	if err := p.EncodeTo(buf); err != nil {
		return err
	}
	_, err = f.WriteAt(buf, int64(ranura)*page.Size)
	return err
}

// Leer es el paso 1 de la sec. 8: leer las dos ranuras, descartar las que fallen, y
// quedarse con la válida de LSN más alto. Devuelve también la ranura en la que estaba, que
// es lo que decide dónde escribe el siguiente checkpoint.
//
// Un fallo de lectura de una ranura se trata igual que una ranura inválida y no se
// propaga. Es deliberado y es el punto entero de tener dos: un archivo recién creado mide
// dos páginas a ceros, y un archivo truncado por una caída puede no llegar a la segunda.
// Abortar aquí convertiría en pérdida permanente una situación que el WAL resuelve entera.
func Leer(f fsx.File) (Meta, uint64, error) {
	var (
		mejor   Meta
		ranura  uint64
		hallada bool
		buf     = make([]byte, page.Size)
	)
	for r := uint64(0); r < Ranuras; r++ {
		if _, err := f.ReadAt(buf, int64(r)*page.Size); err != nil {
			continue
		}
		p, err := page.Decode(buf, r)
		if err != nil {
			continue
		}
		m, err := DeLaPagina(p)
		if err != nil {
			continue
		}
		if !hallada || m.LSN > mejor.LSN {
			mejor, ranura, hallada = m, r, true
		}
	}
	if !hallada {
		return Meta{}, 0, ErrSinMetaValida
	}
	return mejor, ranura, nil
}

// RanuraSiguiente devuelve la ranura sobre la que toca escribir, dada la que ganó la
// última lectura: siempre la otra, que es la más antigua de las dos.
func RanuraSiguiente(ranura uint64) uint64 {
	return (ranura + 1) % Ranuras
}
