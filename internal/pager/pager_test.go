package pager

import (
	"errors"
	"slices"
	"testing"

	"github.com/jsav2003/motor-almacenamiento/internal/page"
)

// nuevoPager monta un pager sobre un archivo vacío y devuelve las tres piezas. La traza
// queda limpia: lo que Create escribe (las dos ranuras meta) ya está comprobado en
// TestCreateMaterializaLasMetas y estorba en los demás tests.
func nuevoPager(t *testing.T) (*Pager, *memFile, *logDePrueba, *traza) {
	t.Helper()
	tz := &traza{}
	f := nuevoMemFile(tz)
	lg := nuevoLog(tz)
	pg, err := Create(f, lg)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tz.limpia()
	return pg, f, lg, tz
}

// marca escribe un patrón reconocible en el cuerpo de p, para que un round-trip no pase
// con un cuerpo a ceros que nadie copió.
func marca(p *page.Page, semilla byte) {
	for i := range p.Body {
		p.Body[i] = semilla ^ byte(i*31+7)
	}
	p.NCells = uint16(semilla) + 1
}

// TestCreateMaterializaLasMetas comprueba que un archivo nuevo nace con las dos ranuras
// meta escritas y sincronizadas, y con total_pages en 2.
//
// Las metas quedan a ceros, o sea con CRC inválido, y eso es lo correcto: la sec. 8 paso
// 1 espera exactamente eso en un archivo sin checkpoint todavía -- ninguna meta válida,
// así que se reproduce el WAL desde el LSN 0.
func TestCreateMaterializaLasMetas(t *testing.T) {
	tz := &traza{}
	f := nuevoMemFile(tz)
	pg, err := Create(f, nuevoLog(tz))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := pg.TotalPages(); got != MetaPages {
		t.Errorf("TotalPages: got %d, want %d", got, MetaPages)
	}
	if got := f.paginas(); got != MetaPages {
		t.Errorf("paginas en el archivo: got %d, want %d", got, MetaPages)
	}
	want := []string{"datos:write 0", "datos:write 1", "datos:sync"}
	if !slices.Equal(tz.eventos, want) {
		t.Errorf("traza: got %v, want %v", tz.eventos, want)
	}
	if _, err := pg.Get(0); err == nil {
		t.Error("una meta a ceros deberia fallar el CRC, y no falla")
	}
}

// TestRoundTripPorElArchivo es el camino completo de una página: se asigna, se muta, se
// confirma, baja a datos.db en el checkpoint, y un pager nuevo la lee del archivo.
//
// El pager nuevo estrena un Log con el contador en cero, que es justo lo que la F3 no
// podrá hacer: tras una recuperación, el LSN se retoma del último registro leído, no de
// cero. Aquí no importa porque este pager solo lee.
func TestRoundTripPorElArchivo(t *testing.T) {
	pg, f, lg, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	marca(p, 0x5A)
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}

	otro, err := New(f, nuevoLog(&traza{}), pg.TotalPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	leida, err := otro.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if leida.Type != page.TypeLeaf || leida.NCells != p.NCells {
		t.Errorf("cabecera: got tipo=%d nceldas=%d, want tipo=%d nceldas=%d",
			leida.Type, leida.NCells, page.TypeLeaf, p.NCells)
	}
	if !slices.Equal(leida.Body, p.Body) {
		t.Error("el cuerpo leido del archivo no es el que se escribio")
	}
	// El page_lsn que quedó en disco es el que selló LogPage, no el cero con el que nació
	// la página. Es lo que hace comprobable la regla de la sec. 7.5 al releer.
	if leida.LSN != lg.lsn {
		t.Errorf("page_lsn en disco: got %d, want %d", leida.LSN, lg.lsn)
	}
}

// TestMutacionExigeGrupo es la sec. 7.3 hecha cumplible: ninguna mutación puede ocurrir
// fuera de un grupo, porque una mutación suelta es una que la recuperación no puede
// aplicar ni descartar entera.
func TestMutacionExigeGrupo(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)

	if _, err := pg.Alloc(page.TypeLeaf); !errors.Is(err, ErrNoGroup) {
		t.Errorf("Alloc sin grupo: got %v, want ErrNoGroup", err)
	}
	if err := pg.Free(2); !errors.Is(err, ErrNoGroup) {
		t.Errorf("Free sin grupo: got %v, want ErrNoGroup", err)
	}
	if err := pg.MarkDirty(page.New(2, page.TypeLeaf)); !errors.Is(err, ErrNoGroup) {
		t.Errorf("MarkDirty sin grupo: got %v, want ErrNoGroup", err)
	}
	if err := pg.CommitGroup(2); !errors.Is(err, ErrNoGroup) {
		t.Errorf("CommitGroup sin grupo: got %v, want ErrNoGroup", err)
	}
}

// TestGruposNoSeAnidan comprueba que un segundo BeginGroup falla, y que tras CommitGroup
// se puede abrir otro.
func TestGruposNoSeAnidan(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	if err := pg.BeginGroup(); !errors.Is(err, ErrGroupOpen) {
		t.Errorf("BeginGroup anidado: got %v, want ErrGroupOpen", err)
	}
	if err := pg.CommitGroup(0); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	if err := pg.BeginGroup(); err != nil {
		t.Errorf("BeginGroup tras cerrar el anterior: %v", err)
	}
}

// TestMetasFueraDelPager es la regla 1 de la sec. 5.2. Si una meta pudiera entrar en el
// conjunto de sucias, el paso 1 del checkpoint escribiría la ranura nueva y el paso 3 la
// vieja: las dos ranuras acabarían con estado de la misma época y la alternancia dejaría
// de proteger de nada.
func TestMetasFueraDelPager(t *testing.T) {
	pg, f, _, tz := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	for id := uint64(0); id < MetaPages; id++ {
		if err := pg.MarkDirty(page.New(id, page.TypeMeta)); !errors.Is(err, ErrMetaPage) {
			t.Errorf("MarkDirty(%d): got %v, want ErrMetaPage", id, err)
		}
		if err := pg.Free(id); !errors.Is(err, ErrMetaPage) {
			t.Errorf("Free(%d): got %v, want ErrMetaPage", id, err)
		}
	}

	// Alloc tampoco puede entregarlas nunca, ni sobre un archivo recién creado ni tirando
	// del conjunto de libres.
	for i := 0; i < 4; i++ {
		p, err := pg.Alloc(page.TypeLeaf)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if p.ID < MetaPages {
			t.Fatalf("Alloc entrego la pagina meta %d", p.ID)
		}
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
	}
	if err := pg.CommitGroup(2); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}

	if tz.contiene("datos:write 0") || tz.contiene("datos:write 1") {
		t.Errorf("el pager escribio en una ranura meta: %v", tz.eventos)
	}
	if got := f.paginas(); got != MetaPages+4 {
		t.Errorf("paginas: got %d, want %d", got, MetaPages+4)
	}
}

// TestExtensionAntesDelCommit fija el orden de la sec. 7.6: el archivo se extiende y se
// sincroniza **antes** de que el commit confirme un Put que use la página nueva.
//
// Al revés, el commit confirmaría un grupo cuyas imágenes apuntan a una página que el
// archivo no cubre, y la recuperación tendría que escribir en un agujero. El test observa
// el orden relativo entre los dos archivos, que es lo único que aquí significa algo.
func TestExtensionAntesDelCommit(t *testing.T) {
	pg, _, _, tz := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	// Hasta el commit, el archivo no ha crecido: la extensión es diferida a propósito,
	// para que un grupo que asigna cuatro páginas cueste un fsync y no cuatro.
	if tz.contiene("datos:sync") {
		t.Errorf("Alloc sincronizo el archivo antes de tiempo: %v", tz.eventos)
	}
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	iSync := tz.indice("datos:sync")
	iCommit := tz.indice("wal:commit root=2 total=3")
	if iSync < 0 || iCommit < 0 {
		t.Fatalf("faltan eventos en la traza: %v", tz.eventos)
	}
	if iSync > iCommit {
		t.Errorf("el fsync de extension llego despues del commit: %v", tz.eventos)
	}
}

// TestCommitLlevaElEstadoDelAsignador comprueba que root_id y total_pages viajan en el
// registro de commit (sec. 7.3, hallazgo H2).
//
// Sin ellos en el log, la raíz se divide, las páginas nuevas quedan escritas y perfectas
// tras la recuperación, y el motor arranca con la raíz vieja -- que ahora es solo la
// mitad izquierda del árbol. La mitad de las claves confirmadas queda inalcanzable y
// Validate() pasa en verde, porque una hoja como raíz es un B+tree legal.
func TestCommitLlevaElEstadoDelAsignador(t *testing.T) {
	pg, _, lg, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	var raiz *page.Page
	for i := 0; i < 3; i++ {
		p, err := pg.Alloc(page.TypeLeaf)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
		raiz = p
	}
	if err := pg.CommitGroup(raiz.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	got := lg.ultimoCommit()
	want := State{RootID: raiz.ID, FreeHead: 0, TotalPages: MetaPages + 3}
	if got != want {
		t.Errorf("estado del commit: got %+v, want %+v", got, want)
	}
}

// TestImagenesEnOrdenDePagina comprueba que las imágenes de un grupo se anexan en orden
// creciente de número de página.
//
// No hace falta para la correctitud: la recuperación aplica el grupo entero. Hace falta
// para la reproducibilidad de la sec. 9.1 -- recorrer un mapa en Go da un orden distinto
// en cada corrida, y con eso una semilla de la F4 dejaría de producir el mismo log dos
// veces seguidas.
func TestImagenesEnOrdenDePagina(t *testing.T) {
	pg, _, _, tz := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	var ids []uint64
	for i := 0; i < 5; i++ {
		p, err := pg.Alloc(page.TypeLeaf)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
		ids = append(ids, p.ID)
	}
	// Se ensucia otra vez en orden inverso: lo que decide el orden del log tiene que ser
	// el número de página, no el orden en que se tocaron.
	slices.Reverse(ids)
	for _, id := range ids {
		p, err := pg.Get(id)
		if err != nil {
			t.Fatalf("Get(%d): %v", id, err)
		}
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
	}
	if err := pg.CommitGroup(2); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	var imagenes []string
	for _, e := range tz.eventos {
		if len(e) > 10 && e[:10] == "wal:imagen" {
			imagenes = append(imagenes, e)
		}
	}
	want := []string{
		"wal:imagen 2 lsn=1", "wal:imagen 3 lsn=2", "wal:imagen 4 lsn=3",
		"wal:imagen 5 lsn=4", "wal:imagen 6 lsn=5",
	}
	if !slices.Equal(imagenes, want) {
		t.Errorf("orden de las imagenes: got %v, want %v", imagenes, want)
	}
}

// TestUnaImagenPorPaginaYGrupo comprueba que ensuciar la misma página tres veces dentro
// de un grupo produce una sola imagen. El log guarda la página entera, no el cambio: la
// última imagen ya contiene todo lo anterior, y anexar las tres sería multiplicar por
// tres el volumen del WAL sin añadir información.
func TestUnaImagenPorPaginaYGrupo(t *testing.T) {
	pg, _, lg, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	for i := 0; i < 3; i++ {
		marca(p, byte(i))
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	if lg.lsn != 1 {
		t.Errorf("imagenes anexadas: got %d, want 1", lg.lsn)
	}
}

// TestReglaDeDesalojo es la sec. 7.5. Una página sucia no baja a datos.db mientras
// wal_flushed_lsn < page_lsn; si el desalojo la encuentra así, fuerza antes el fsync del
// WAL.
//
// El Log de este test no pone al día lo sincronizado en el Commit, que es lo que fuerza a
// writePage a pedir el Sync. Un WAL correcto sí lo hace, y entonces la regla nunca
// bloquea -- pero el camino tiene que existir y estar probado, porque el día que un caché
// acotado desaloje entre commits será el único que impida una página rota sin reparación
// posible.
func TestReglaDeDesalojo(t *testing.T) {
	pg, _, lg, tz := nuevoPager(t)
	lg.commitSincroniza = false

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	tz.limpia()

	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	iSync := tz.indice("wal:sync")
	iWrite := tz.indice("datos:write 2")
	if iSync < 0 {
		t.Fatalf("FlushDirty bajo la pagina sin sincronizar antes el WAL: %v", tz.eventos)
	}
	if iSync > iWrite {
		t.Errorf("el fsync del WAL llego despues de la escritura de datos: %v", tz.eventos)
	}
}

// TestWriteAheadIrreparable cubre el caso en que ni el fsync del WAL pone al día lo
// sincronizado. Eso solo puede significar que la implementación de Log está rota, y la
// respuesta correcta es negarse a escribir: si esa escritura se desgarrara, el log no
// tendría con qué repararla porque su registro se perdió.
func TestWriteAheadIrreparable(t *testing.T) {
	pg, _, lg, tz := nuevoPager(t)
	lg.commitSincroniza = false
	lg.syncAlcanza = false

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	tz.limpia()

	if err := pg.FlushDirty(); !errors.Is(err, ErrWriteAhead) {
		t.Fatalf("FlushDirty: got %v, want ErrWriteAhead", err)
	}
	if tz.contiene("datos:write 2") {
		t.Errorf("se escribio la pagina pese al error: %v", tz.eventos)
	}
}

// TestFlushConGrupoAbierto comprueba que el checkpoint no corre a mitad de un grupo. Las
// páginas de un grupo sin commit son exactamente las que la sec. 7.5 prohíbe bajar, y
// detectarlo aquí da un error que dice qué pasó en vez de uno sobre el page_lsn.
func TestFlushConGrupoAbierto(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)
	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	if err := pg.FlushDirty(); !errors.Is(err, ErrGroupOpen) {
		t.Errorf("FlushDirty con grupo abierto: got %v, want ErrGroupOpen", err)
	}
}

// TestSuciasSobrevivenAlCommit comprueba que las páginas siguen sucias después del
// commit. Es la sec. 7.2 pasos 6 y 7: el dato está a salvo porque está en el log, no
// porque esté en su sitio definitivo. Bajar cada página en su commit convertiría cada
// operación en un fsync en medio del archivo, que es justo lo que el WAL evita.
func TestSuciasSobrevivenAlCommit(t *testing.T) {
	pg, _, _, tz := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	// Lo único escrito en datos.db hasta aquí es la extensión, a ceros.
	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if !tz.contiene("datos:write 2") {
		t.Fatalf("el checkpoint no bajo la pagina sucia: %v", tz.eventos)
	}

	// Y un segundo checkpoint sin cambios no vuelve a escribirla.
	tz.limpia()
	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}
	if tz.contiene("datos:write 2") {
		t.Errorf("el checkpoint reescribio una pagina limpia: %v", tz.eventos)
	}
}

// TestFreeYReasignacion recorre el ciclo que motiva el invariante 6: liberar una página,
// volver a asignarla, y comprobar que llega vacía y no con las claves de su vida
// anterior.
func TestFreeYReasignacion(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	var ids []uint64
	for i := 0; i < 3; i++ {
		p, err := pg.Alloc(page.TypeLeaf)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		marca(p, byte(0xF0+i))
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
		ids = append(ids, p.ID)
	}
	if err := pg.CommitGroup(ids[0]); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	// Se libera la del medio.
	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	if err := pg.Free(ids[1]); err != nil {
		t.Fatalf("Free: %v", err)
	}
	if err := pg.Free(ids[1]); !errors.Is(err, ErrDoubleFree) {
		t.Errorf("Free repetido: got %v, want ErrDoubleFree", err)
	}
	if got := pg.FreePages(); !slices.Equal(got, []uint64{ids[1]}) {
		t.Errorf("FreePages: got %v, want %v", got, []uint64{ids[1]})
	}

	// La siguiente asignación tiene que reusarla en vez de extender el archivo.
	antes := pg.TotalPages()
	nueva, err := pg.Alloc(page.TypeInternal)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if nueva.ID != ids[1] {
		t.Errorf("Alloc: got pagina %d, want la liberada %d", nueva.ID, ids[1])
	}
	if pg.TotalPages() != antes {
		t.Errorf("el archivo crecio teniendo una pagina libre: %d -> %d", antes, pg.TotalPages())
	}
	if nueva.NCells != 0 || nueva.Type != page.TypeInternal {
		t.Errorf("la pagina reciclada llego con estado viejo: nceldas=%d tipo=%d", nueva.NCells, nueva.Type)
	}
	for _, b := range nueva.Body {
		if b != 0 {
			t.Fatal("la pagina reciclada llego con el cuerpo de su vida anterior")
		}
	}
	if len(pg.FreePages()) != 0 {
		t.Errorf("la pagina asignada sigue en el conjunto de libres: %v", pg.FreePages())
	}
}

// TestPaginaObsoleta cubre ErrStalePage: mutar una copia que el caché ya no reconoce.
//
// El escenario real es el del ciclo anterior. Alguien guarda la Page de la 3, esa página
// se libera y se reasigna, y el objeto viejo sigue en su mano. Ensuciarlo pisaría en
// silencio la página viva, y el fallo aparecería miles de operaciones después de su
// causa -- el riesgo que la sec. 11 nombra explícitamente.
func TestPaginaObsoleta(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := pg.Free(p.ID); err != nil {
		t.Fatalf("Free: %v", err)
	}
	viva, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if viva.ID != p.ID {
		t.Fatalf("preparacion: se esperaba reciclar la %d, llego la %d", p.ID, viva.ID)
	}
	if err := pg.MarkDirty(p); !errors.Is(err, ErrStalePage) {
		t.Errorf("MarkDirty sobre la copia obsoleta: got %v, want ErrStalePage", err)
	}
	if err := pg.MarkDirty(viva); err != nil {
		t.Errorf("MarkDirty sobre la pagina viva: %v", err)
	}
}

// TestFueraDeRango cubre los tres accesos a una página que el archivo no cubre.
func TestFueraDeRango(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)
	fuera := pg.TotalPages()

	if _, err := pg.Get(fuera); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("Get fuera de rango: got %v, want ErrOutOfRange", err)
	}
	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	if err := pg.Free(fuera); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("Free fuera de rango: got %v, want ErrOutOfRange", err)
	}
	if err := pg.MarkDirty(page.New(fuera, page.TypeLeaf)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("MarkDirty fuera de rango: got %v, want ErrOutOfRange", err)
	}
	if _, err := New(nuevoMemFile(&traza{}), &NopLog{}, MetaPages-1); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("New con menos paginas que metas: got %v, want ErrOutOfRange", err)
	}
}

// TestAdoptFreeSet cubre la entrada del conjunto de libres reconstruido en cada Open
// (sec. 6.1). Las tres entradas que rechaza serían, cada una, una asignación futura
// encima de una meta o fuera del archivo, o una página entregada dos veces.
func TestAdoptFreeSet(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := pg.Alloc(page.TypeLeaf); err != nil {
			t.Fatalf("Alloc: %v", err)
		}
	}
	if err := pg.CommitGroup(2); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	if err := pg.AdoptFreeSet([]uint64{4, 3}); err != nil {
		t.Fatalf("AdoptFreeSet: %v", err)
	}
	if got := pg.FreePages(); !slices.Equal(got, []uint64{3, 4}) {
		t.Errorf("FreePages: got %v, want [3 4]", got)
	}

	casos := []struct {
		nombre string
		ids    []uint64
		want   error
	}{
		{"meta", []uint64{0}, ErrMetaPage},
		{"fuera del archivo", []uint64{pg.TotalPages()}, ErrOutOfRange},
		{"repetida", []uint64{3, 3}, ErrDoubleFree},
	}
	for _, c := range casos {
		if err := pg.AdoptFreeSet(c.ids); !errors.Is(err, c.want) {
			t.Errorf("AdoptFreeSet(%s): got %v, want %v", c.nombre, err, c.want)
		}
	}
	// Un conjunto rechazado no deja el anterior a medias.
	if got := pg.FreePages(); !slices.Equal(got, []uint64{3, 4}) {
		t.Errorf("tras los rechazos, FreePages: got %v, want [3 4]", got)
	}
}

// TestCorrupcionEnElArchivo comprueba que el pager no le cree a una página alterada en
// disco: propaga el error de page tal cual, sin envolverlo, porque cuál sea dice qué
// pasó.
func TestCorrupcionEnElArchivo(t *testing.T) {
	pg, f, _, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	marca(p, 0x11)
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}
	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}

	// Un bit alterado en el cuerpo de la página 2, ya en el archivo.
	f.datos[int(p.ID)*page.Size+100] ^= 0x01

	otro, err := New(f, &NopLog{}, pg.TotalPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := otro.Get(p.ID); !errors.Is(err, page.ErrCorrupt) {
		t.Errorf("Get de una pagina corrupta: got %v, want page.ErrCorrupt", err)
	}
}

// TestCacheDevuelveElMismoObjeto comprueba que dos Get de la misma página dan el mismo
// objeto. Si dieran copias distintas, mutar una y ensuciarla perdería lo hecho sobre la
// otra sin que nada avisara.
func TestCacheDevuelveElMismoObjeto(t *testing.T) {
	pg, _, _, _ := nuevoPager(t)

	if err := pg.BeginGroup(); err != nil {
		t.Fatalf("BeginGroup: %v", err)
	}
	p, err := pg.Alloc(page.TypeLeaf)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if err := pg.MarkDirty(p); err != nil {
		t.Fatalf("MarkDirty: %v", err)
	}
	if err := pg.CommitGroup(p.ID); err != nil {
		t.Fatalf("CommitGroup: %v", err)
	}

	a, err := pg.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	b, err := pg.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a != b || a != p {
		t.Error("el cache entrego objetos distintos para la misma pagina")
	}
}

// TestNopLogMantieneVivoElCamino comprueba que el Log de la F2 sella LSN crecientes y los
// declara sincronizados. Es lo que hace que la regla de la sec. 7.5 se ejecute en cada
// checkpoint de la F2 en vez de quedar como código muerto hasta la F3.
func TestNopLogMantieneVivoElCamino(t *testing.T) {
	tz := &traza{}
	f := nuevoMemFile(tz)
	pg, err := Create(f, &NopLog{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var ultima uint64
	for i := 0; i < 3; i++ {
		if err := pg.BeginGroup(); err != nil {
			t.Fatalf("BeginGroup: %v", err)
		}
		p, err := pg.Alloc(page.TypeLeaf)
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if err := pg.MarkDirty(p); err != nil {
			t.Fatalf("MarkDirty: %v", err)
		}
		if err := pg.CommitGroup(p.ID); err != nil {
			t.Fatalf("CommitGroup: %v", err)
		}
		if p.LSN <= ultima {
			t.Fatalf("LSN no creciente: %d tras %d", p.LSN, ultima)
		}
		ultima = p.LSN
	}
	if err := pg.FlushDirty(); err != nil {
		t.Errorf("FlushDirty con NopLog: %v", err)
	}
}
