package wal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/jsav2003/durable-kv/internal/page"
	"github.com/jsav2003/durable-kv/internal/pager"
)

// paginaDePrueba devuelve una hoja con contenido reconocible en el cuerpo, para que un
// round-trip que perdiera bytes no pase por casualidad con un cuerpo a ceros.
func paginaDePrueba(id uint64) *page.Page {
	p := page.New(id, page.TypeLeaf)
	p.LSN = 700
	p.NCells = 3
	p.FreeEnd = page.Size - 100
	p.Free = page.HeaderSize + 6
	p.Link = id + 1
	for i := range p.Body {
		p.Body[i] = byte(i)
	}
	return p
}

func TestImagenRoundTrip(t *testing.T) {
	orig := paginaDePrueba(40)

	carga := make([]byte, CargaImagen)
	if err := CodificaImagen(carga, orig); err != nil {
		t.Fatalf("CodificaImagen: %v", err)
	}

	got, err := DecodificaImagen(carga)
	if err != nil {
		t.Fatalf("DecodificaImagen: %v", err)
	}
	if got.ID != orig.ID || got.LSN != orig.LSN || got.Type != orig.Type {
		t.Errorf("cabecera perdida: id=%d lsn=%d tipo=%d", got.ID, got.LSN, got.Type)
	}
	if got.NCells != orig.NCells || got.FreeEnd != orig.FreeEnd || got.Free != orig.Free {
		t.Errorf("ocupacion perdida: nceldas=%d libre_fin=%d libre=%d",
			got.NCells, got.FreeEnd, got.Free)
	}
	if got.Link != orig.Link {
		t.Errorf("enlace = %d, quiero %d", got.Link, orig.Link)
	}
	if !bytes.Equal(got.Body, orig.Body) {
		t.Error("el cuerpo no sobrevivio al round-trip")
	}
}

// El page_id exterior de la imagen y el de la cabecera de la página son el mismo dato en
// dos sitios. Que discrepen significa que el registro se escribió mal o que se leyó del
// sitio equivocado, y aplicar esa imagen dejaría una página íntegra en la ranura que no
// le toca -- exactamente lo que page.ErrWrongPage existe para distinguir de la
// corrupción.
func TestImagenDetectaPageIDDiscrepante(t *testing.T) {
	carga := make([]byte, CargaImagen)
	if err := CodificaImagen(carga, paginaDePrueba(40)); err != nil {
		t.Fatalf("CodificaImagen: %v", err)
	}
	// Solo el page_id exterior: la página de dentro queda intacta, con su CRC en verde.
	binary.LittleEndian.PutUint64(carga[offImagenPageID:], 41)

	_, err := DecodificaImagen(carga)
	if !errors.Is(err, page.ErrWrongPage) {
		t.Fatalf("error = %v, quiero page.ErrWrongPage", err)
	}
}

// Un byte alterado dentro de la página tiene que salir como corrupción de página y no
// confundirse con un problema del registro: internal/record ya certificó que estos bytes
// son los que se escribieron, así que si el CRC de la página falla, se corrompió antes de
// entrar al log.
func TestImagenPropagaCorrupcionDePagina(t *testing.T) {
	carga := make([]byte, CargaImagen)
	if err := CodificaImagen(carga, paginaDePrueba(40)); err != nil {
		t.Fatalf("CodificaImagen: %v", err)
	}
	carga[offImagenPagina+page.HeaderSize+7] ^= 0x01

	_, err := DecodificaImagen(carga)
	if !errors.Is(err, page.ErrCorrupt) {
		t.Fatalf("error = %v, quiero page.ErrCorrupt", err)
	}
}

func TestImagenExigeLongitudExacta(t *testing.T) {
	casos := map[string]int{
		"una pagina sin el page_id": page.Size,
		"un byte de mas":            CargaImagen + 1,
		"vacia":                     0,
	}
	for nombre, n := range casos {
		t.Run(nombre, func(t *testing.T) {
			if err := CodificaImagen(make([]byte, n), paginaDePrueba(40)); !errors.Is(err, ErrCargaInvalida) {
				t.Errorf("CodificaImagen: error = %v, quiero ErrCargaInvalida", err)
			}
			if _, err := DecodificaImagen(make([]byte, n)); !errors.Is(err, ErrCargaInvalida) {
				t.Errorf("DecodificaImagen: error = %v, quiero ErrCargaInvalida", err)
			}
		})
	}
}

func TestCommitRoundTrip(t *testing.T) {
	quiero := pager.State{RootID: 5, FreeHead: 0, TotalPages: 903}

	n, got, err := DecodificaCommit(CodificaCommit(4, quiero))
	if err != nil {
		t.Fatalf("DecodificaCommit: %v", err)
	}
	if n != 4 {
		t.Errorf("n_registros = %d, quiero 4", n)
	}
	if got != quiero {
		t.Errorf("estado = %+v, quiero %+v", got, quiero)
	}
}

// Los tres campos del commit ocupan offsets distintos y los tres son uint64. Un
// intercambio entre dos de ellos sobrevive a un round-trip con valores iguales o a
// ceros, así que se prueban con valores distintos entre sí y ninguno cero.
func TestCommitNoIntercambiaCampos(t *testing.T) {
	quiero := pager.State{RootID: 11, FreeHead: 22, TotalPages: 33}

	_, got, err := DecodificaCommit(CodificaCommit(1, quiero))
	if err != nil {
		t.Fatalf("DecodificaCommit: %v", err)
	}
	if got.RootID != 11 {
		t.Errorf("root_id = %d, quiero 11", got.RootID)
	}
	if got.FreeHead != 22 {
		t.Errorf("free_head = %d, quiero 22", got.FreeHead)
	}
	if got.TotalPages != 33 {
		t.Errorf("total_pages = %d, quiero 33", got.TotalPages)
	}
}

func TestCommitExigeLongitudExacta(t *testing.T) {
	for _, n := range []int{0, CargaCommit - 1, CargaCommit + 1} {
		if _, _, err := DecodificaCommit(make([]byte, n)); !errors.Is(err, ErrCargaInvalida) {
			t.Errorf("con %d bytes: error = %v, quiero ErrCargaInvalida", n, err)
		}
	}
}

// La sec. 7.3 dibuja los dos registros con tamaños concretos. Fijarlos en un test es lo
// que hace que un cambio de layout se note aquí y no en la recuperación de un archivo
// escrito por la versión anterior.
func TestTamanosDelFormato(t *testing.T) {
	if CargaImagen != 4104 {
		t.Errorf("CargaImagen = %d, quiero 4104 (page_id + 4096)", CargaImagen)
	}
	if CargaCommit != 28 {
		t.Errorf("CargaCommit = %d, quiero 28", CargaCommit)
	}
}
