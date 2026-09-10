package recovery_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jsav2003/durable-kv/internal/fsx/fsxtest"
	"github.com/jsav2003/durable-kv/internal/meta"
	"github.com/jsav2003/durable-kv/internal/page"
	"github.com/jsav2003/durable-kv/internal/pager"
	"github.com/jsav2003/durable-kv/internal/record"
	"github.com/jsav2003/durable-kv/internal/recovery"
	"github.com/jsav2003/durable-kv/internal/tree"
	"github.com/jsav2003/durable-kv/internal/wal"
)

// El modelo de caída de estos tests: se abre con Recuperar, se hacen Put --que confirman
// con su fsync-- y **se suelta el Estado sin checkpoint ni Close**. El disco en memoria
// conserva lo escrito, que es exactamente lo que deja un kill -9: datos.db con lo que
// bajara el último checkpoint, y el WAL con todo lo confirmado después.

func recupera(t *testing.T, d *fsxtest.Disco) *recovery.Estado {
	t.Helper()
	est, err := recovery.Recuperar(d, 0)
	if err != nil {
		t.Fatalf("Recuperar: %v", err)
	}
	return est
}

func clave(i int) []byte { return fmt.Appendf(nil, "clave-%06d", i) }
func valor(i int) []byte { return fmt.Appendf(nil, "valor-%06d", i) }

// pon inserta las claves [desde, hasta) y devuelve la última confirmada.
func pon(t *testing.T, a *tree.Tree, desde, hasta int) {
	t.Helper()
	for i := desde; i < hasta; i++ {
		if err := a.Put(clave(i), valor(i)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
}

// exige comprueba que las claves [desde, hasta) están con su valor.
func exige(t *testing.T, a *tree.Tree, desde, hasta int) {
	t.Helper()
	for i := desde; i < hasta; i++ {
		got, err := a.Get(clave(i))
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if !bytes.Equal(got, valor(i)) {
			t.Fatalf("Get(%d) = %q, quiero %q", i, got, valor(i))
		}
	}
}

// paginasDelArbol son los bytes de datos.db de la página 2 en adelante: el árbol sin las
// dos ranuras meta, que cambian en cada checkpoint por diseño.
func paginasDelArbol(d *fsxtest.Disco) []byte {
	b := d.Bytes(recovery.NombreDatos)
	if len(b) < pager.MetaPages*page.Size {
		return nil
	}
	return b[pager.MetaPages*page.Size:]
}

// clona copia el contenido de todos los archivos a un disco nuevo: el mismo estado de
// partida para dos recuperaciones independientes.
func clona(t *testing.T, d *fsxtest.Disco) *fsxtest.Disco {
	t.Helper()
	c := fsxtest.Nuevo()
	for _, n := range d.Nombres() {
		f, err := c.Open(n)
		if err != nil {
			t.Fatal(err)
		}
		if b := d.Bytes(n); len(b) > 0 {
			if _, err := f.WriteAt(b, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	return c
}

// --- Base nueva -------------------------------------------------------------------

// No hay un modo "abrir normal" y otro "recuperar": es el mismo camino siempre. Una base
// recién creada recorre los diez pasos con las dos metas a ceros, que es justo el caso que
// el paso 1 se niega a tratar como archivo irrecuperable.
func TestBaseNueva(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)

	if est.Informe.MetaValida {
		t.Error("informe: dice que habia meta valida en una base nueva")
	}
	if !est.Informe.ArbolCreado {
		t.Error("informe: no marca que hubo que crear la raiz")
	}
	if est.Arbol.Root() < pager.MetaPages {
		t.Errorf("raiz = %d, quiero una pagina real", est.Arbol.Root())
	}
	if err := est.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	// El paso 10 dejó una meta escrita: la recuperación es observable.
	m, _, err := meta.Leer(est.Datos)
	if err != nil {
		t.Fatalf("meta.Leer tras el paso 10: %v", err)
	}
	if m.RootID != est.Arbol.Root() {
		t.Errorf("la meta dice root=%d y el arbol %d", m.RootID, est.Arbol.Root())
	}
}

// --- La afirmación central: lo confirmado sobrevive ---------------------------------

// El punto (b) de la sec. 9.2: toda clave cuyo Put devolvió OK está presente y con el valor
// correcto. Aquí sin checkpoint por medio, así que todo viene del log.
func TestLoConfirmadoSobreviveSinCheckpoint(t *testing.T) {
	d := fsxtest.Nuevo()

	est := recupera(t, d)
	pon(t, est.Arbol, 0, 200)
	// Caída: ni checkpoint ni Close.

	est2 := recupera(t, d)
	if est2.Informe.Grupos != 200 {
		t.Errorf("grupos recuperados = %d, quiero 200", est2.Informe.Grupos)
	}
	exige(t, est2.Arbol, 0, 200)
	if err := est2.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Con divisiones de por medio: 2000 claves pasan de una hoja a un árbol de varios niveles,
// y cada división es un grupo de cuatro páginas que tiene que aplicarse entero.
func TestSobreviveConDivisiones(t *testing.T) {
	d := fsxtest.Nuevo()

	est := recupera(t, d)
	pon(t, est.Arbol, 0, 2000)
	raiz := est.Arbol.Root()

	est2 := recupera(t, d)
	if est2.Arbol.Root() != raiz {
		t.Errorf("raiz = %d, quiero %d: el root_id viene del ultimo commit",
			est2.Arbol.Root(), raiz)
	}
	exige(t, est2.Arbol, 0, 2000)
	if err := est2.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Un checkpoint por medio parte la historia en dos: lo anterior está en datos.db y lo
// posterior solo en el log. Las dos mitades tienen que estar al reabrir.
func TestSobreviveConCheckpointPorMedio(t *testing.T) {
	d := fsxtest.Nuevo()

	est := recupera(t, d)
	pon(t, est.Arbol, 0, 300)
	if err := est.Checkpoint.Correr(est.Arbol.Root()); err != nil {
		t.Fatalf("Correr: %v", err)
	}
	pon(t, est.Arbol, 300, 600)

	est2 := recupera(t, d)
	if !est2.Informe.MetaValida {
		t.Error("informe: no encontro la meta que el checkpoint escribio")
	}
	if est2.Informe.Grupos != 300 {
		t.Errorf("grupos recuperados = %d, quiero 300: solo lo posterior al checkpoint",
			est2.Informe.Grupos)
	}
	exige(t, est2.Arbol, 0, 600)
	if err := est2.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// --- Atomicidad de grupo ------------------------------------------------------------

// El paso 4 de la sec. 8. Se corta el log justo detrás del último commit y se le anexa un
// grupo de imágenes sin cerrar: están íntegras, con CRC bueno, y aun así no cuentan.
func TestUnGrupoSinCommitSeDescartaEntero(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 50)

	// Un grupo huérfano: dos imágenes anexadas sin commit, con LSN contiguo al último.
	f, _ := d.Open(wal.Nombre(est.Log.Epoca()))
	tam, _ := f.Size()
	wr := record.NewWriter(f, tam)
	carga := make([]byte, wal.CargaImagen)
	lsn := est.Log.LSN()
	for i := range 2 {
		p := page.New(est.Arbol.Root(), page.TypeLeaf)
		p.LSN = lsn + uint64(i) + 1
		if err := wal.CodificaImagen(carga, p); err != nil {
			t.Fatal(err)
		}
		err := wr.Append(record.Record{
			LSN: p.LSN, Type: wal.TipoImagen, Epoch: est.Log.Epoca(), Payload: carga,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	est2 := recupera(t, d)
	if est2.Informe.Grupos != 50 {
		t.Errorf("grupos = %d, quiero 50: el huerfano no cuenta", est2.Informe.Grupos)
	}
	// Y sobre todo, no se aplicó: la hoja raíz vacía que traían las imágenes huérfanas
	// habría borrado las claves.
	exige(t, est2.Arbol, 0, 50)
}

// --- La regla del primer registro que falla -----------------------------------------

// Sec. 8, paso 3: el primero que falle marca el punto de la caída, y se descarta todo lo
// que siga **aunque más adelante haya grupos perfectamente válidos**.
func TestUnCRCMaloCortaYLoPosteriorSeDescarta(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	epoca := est.Log.Epoca()

	pon(t, est.Arbol, 0, 20)
	f, _ := d.Open(wal.Nombre(epoca))
	corte, _ := f.Size()
	pon(t, est.Arbol, 20, 40)

	// Un bit en el primer registro escrito tras el corte.
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, corte+20); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x01
	if _, err := f.WriteAt(b, corte+20); err != nil {
		t.Fatal(err)
	}

	est2 := recupera(t, d)
	if !errors.Is(est2.Informe.Motivo, record.ErrCorrupt) {
		t.Errorf("motivo = %v, quiero record.ErrCorrupt", est2.Informe.Motivo)
	}
	if est2.Informe.Grupos != 20 {
		t.Fatalf("grupos = %d, quiero 20", est2.Informe.Grupos)
	}
	exige(t, est2.Arbol, 0, 20)
	if _, err := est2.Arbol.Get(clave(30)); !errors.Is(err, tree.ErrNotFound) {
		t.Errorf("la clave 30 esta detras del corte y aparecio: %v", err)
	}
	if err := est2.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Defensa 1 de la sec. 7.4 vista desde la recuperación.
func TestUnSaltoDeLSNCortaLaRecuperacion(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 10)

	// Un registro con el LSN saltado, tras el último commit.
	f, _ := d.Open(wal.Nombre(est.Log.Epoca()))
	tam, _ := f.Size()
	wr := record.NewWriter(f, tam)
	carga := make([]byte, wal.CargaImagen)
	p := page.New(est.Arbol.Root(), page.TypeLeaf)
	p.LSN = est.Log.LSN() + 7
	if err := wal.CodificaImagen(carga, p); err != nil {
		t.Fatal(err)
	}
	wr.Append(record.Record{LSN: p.LSN, Type: wal.TipoImagen, Epoch: est.Log.Epoca(), Payload: carga})

	est2 := recupera(t, d)
	if !errors.Is(est2.Informe.Motivo, wal.ErrLSNNoContiguo) {
		t.Errorf("motivo = %v, quiero ErrLSNNoContiguo", est2.Informe.Motivo)
	}
	exige(t, est2.Arbol, 0, 10)
}

// Defensa 2 de la sec. 7.4. Un registro de otra generación tiene CRC válido -- es real, de
// antes de una rotación -- y sin la comprobación se reproduciría encima de datos nuevos.
func TestUnRegistroDeOtraEpocaCortaLaRecuperacion(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 10)

	f, _ := d.Open(wal.Nombre(est.Log.Epoca()))
	tam, _ := f.Size()
	wr := record.NewWriter(f, tam)
	carga := make([]byte, wal.CargaImagen)
	p := page.New(est.Arbol.Root(), page.TypeLeaf)
	p.LSN = est.Log.LSN() + 1
	if err := wal.CodificaImagen(carga, p); err != nil {
		t.Fatal(err)
	}
	wr.Append(record.Record{
		LSN: p.LSN, Type: wal.TipoImagen, Epoch: est.Log.Epoca() - 1, Payload: carga,
	})

	est2 := recupera(t, d)
	if !errors.Is(est2.Informe.Motivo, wal.ErrEpocaAjena) {
		t.Errorf("motivo = %v, quiero ErrEpocaAjena", est2.Informe.Motivo)
	}
	exige(t, est2.Arbol, 0, 10)
}

// --- Paso 5: la extensión explícita --------------------------------------------------

// El caso que el paso 5 existe para cubrir, y que la sec. 7.6 nombra: **crecer datos.db es
// una operación de metadatos que ninguna imagen de página describe**. Por eso total_pages
// viaja en el registro de commit, y por eso puede haber un total_pages confirmado más
// grande que la mayor imagen del log.
//
// Si la recuperación no extiende, el archivo se queda corto: escribir después en el offset
// de una página que no existe deja un archivo disperso con un agujero que se lee como ceros
// y falla el CRC. Y la alternativa --no aplicar más allá de total_pages-- descarta datos
// confirmados. Las dos salidas rompen algo.
func TestElArchivoSeExtiendeHastaElTotalPagesConfirmado(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 100)

	// Un grupo sin imágenes que solo confirma una extensión del archivo, que es
	// exactamente la forma que tiene en el log una operación de metadatos.
	quiero := est.Pager.TotalPages() + 10
	f, _ := d.Open(wal.Nombre(est.Log.Epoca()))
	tam, _ := f.Size()
	err := record.NewWriter(f, tam).Append(record.Record{
		LSN:   est.Log.LSN() + 1,
		Type:  wal.TipoCommit,
		Epoch: est.Log.Epoca(),
		Payload: wal.CodificaCommit(0, pager.State{
			RootID: est.Arbol.Root(), TotalPages: quiero,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	est2 := recupera(t, d)
	if est2.Informe.TotalPages != quiero {
		t.Fatalf("total_pages = %d, quiero %d: sale del ultimo commit",
			est2.Informe.TotalPages, quiero)
	}
	tam2, _ := est2.Datos.Size()
	if uint64(tam2) < quiero*page.Size {
		t.Errorf("datos.db mide %d bytes y total_pages es %d: quedan %d paginas sin "+
			"materializar, que en disco real son un agujero disperso",
			tam2, quiero, quiero-uint64(tam2)/page.Size)
	}
	exige(t, est2.Arbol, 0, 100)
	if err := est2.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Y el mismo paso desde el otro lado: con datos.db recortado por debajo de lo que el log
// describe, las páginas del hueco se escriben **explícitamente** y se sincronizan antes de
// aplicar ninguna imagen. No es un Truncate: un archivo disperso se materializa solo al
// escribirlo, y el espacio podría no existir.
func TestLasPaginasDelHuecoSeEscribenAntesDeAplicarImagenes(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 300)
	total := est.Pager.TotalPages()
	if total <= pager.MetaPages+1 {
		t.Fatalf("total_pages = %d: el arbol no crecio y el test no probaria nada", total)
	}

	datos, _ := d.Open(recovery.NombreDatos)
	if err := datos.Truncate(pager.MetaPages * page.Size); err != nil {
		t.Fatal(err)
	}
	d.Traza.Limpia()

	est2 := recupera(t, d)
	exige(t, est2.Arbol, 0, 300)

	eventos := d.Traza.Eventos()
	primerSync := d.Traza.Indice("datos.db:sync")
	if primerSync < 0 {
		t.Fatalf("no hubo fsync de datos.db: %q", eventos)
	}
	escritas := make(map[string]bool)
	for _, e := range eventos[:primerSync] {
		if strings.HasPrefix(e, "datos.db:write ") {
			escritas[e] = true
		}
	}
	for id := uint64(pager.MetaPages); id < total; id++ {
		e := fmt.Sprintf("datos.db:write %d+%d", int64(id)*page.Size, page.Size)
		if !escritas[e] {
			t.Fatalf("la pagina %d no se escribio antes del fsync del paso 5 "+
				"(%d de %d paginas)", id, len(escritas), total-pager.MetaPages)
		}
	}
}

// --- Idempotencia ---------------------------------------------------------------------

// Reaplicar el mismo log dos veces produce el mismo resultado. Es obligatorio porque el
// sistema puede caerse **durante** la recuperación, y sin esta propiedad la segunda
// recuperación no puede confiar en lo que dejó la primera.
//
// El caso que se construye es exactamente ese: un disco donde las imágenes ya se aplicaron
// (paso 6) pero la meta y el log siguen siendo los de antes, porque la caída ocurrió entre
// el paso 6 y el 10. Recuperarlo tiene que dar el mismo árbol que recuperar el original.
func TestIdempotenciaDeLaRecuperacion(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 400)

	// A: la recuperación completa desde el estado de la caída.
	a := clona(t, d)
	estA := recupera(t, a)
	exige(t, estA.Arbol, 0, 400)

	// B: el estado de la caída, pero con las páginas del árbol ya aplicadas -- una caída
	// entre el paso 6 y el paso 10.
	b := clona(t, d)
	datosB, _ := b.Open(recovery.NombreDatos)
	if _, err := datosB.WriteAt(paginasDelArbol(a), pager.MetaPages*page.Size); err != nil {
		t.Fatal(err)
	}
	estB := recupera(t, b)
	exige(t, estB.Arbol, 0, 400)

	if estA.Arbol.Root() != estB.Arbol.Root() {
		t.Errorf("raiz A=%d B=%d", estA.Arbol.Root(), estB.Arbol.Root())
	}
	if !bytes.Equal(paginasDelArbol(a), paginasDelArbol(b)) {
		t.Error("las paginas del arbol difieren entre las dos recuperaciones")
	}
}

// Dos recuperaciones independientes del mismo estado dan el mismo árbol byte a byte. Es lo
// que hace que una semilla de la F4 sea reproducible.
func TestDosRecuperacionesDelMismoEstadoCoinciden(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 250)

	a, b := clona(t, d), clona(t, d)
	recupera(t, a)
	recupera(t, b)

	if !bytes.Equal(paginasDelArbol(a), paginasDelArbol(b)) {
		t.Error("dos recuperaciones del mismo estado dieron arboles distintos")
	}
}

// --- Paso 1: dos metas inválidas -------------------------------------------------------

// Declarar muerto el archivo porque las dos metas fallaron convertiría una situación
// totalmente recuperable en pérdida permanente, teniendo el WAL entero intacto en el disco.
func TestLasDosMetasInvalidasReproducenElLogEntero(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 100)
	if err := est.Checkpoint.Correr(est.Arbol.Root()); err != nil {
		t.Fatal(err)
	}
	pon(t, est.Arbol, 100, 150)

	// Las dos ranuras a la basura. La generación del WAL solo la lleva ya el nombre del
	// archivo, y el LSN del primer registro no es el 1: es donde lo dejó la rotación.
	datos, _ := d.Open(recovery.NombreDatos)
	if _, err := datos.WriteAt(bytes.Repeat([]byte{0xEE}, pager.MetaPages*page.Size), 0); err != nil {
		t.Fatal(err)
	}

	est2 := recupera(t, d)
	if est2.Informe.MetaValida {
		t.Error("informe: dice que habia meta valida")
	}
	if est2.Informe.Grupos != 50 {
		t.Errorf("grupos = %d, quiero 50: los de la generacion que quedo", est2.Informe.Grupos)
	}
	// Las 100 primeras estaban en datos.db por el checkpoint; las 50 siguientes vienen del
	// log reproducido sin referencia.
	exige(t, est2.Arbol, 0, 150)
	if err := est2.Arbol.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// --- Paso 10: la recuperación es observable ---------------------------------------------

// Sin el paso 10 el motor arrancaría con el root_id de la meta vieja, que es justo el
// estado que los pasos anteriores demostraron obsoleto. Como efecto secundario útil, la
// meta que queda escrita es lo que hace la recuperación comprobable desde fuera.
func TestElPaso10DejaLaMetaAlDiaYElLogLimpio(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 120)
	epocaAntes := est.Log.Epoca()

	est2 := recupera(t, d)
	m, _, err := meta.Leer(est2.Datos)
	if err != nil {
		t.Fatalf("meta.Leer: %v", err)
	}
	if m.RootID != est2.Arbol.Root() {
		t.Errorf("la meta dice root=%d y el arbol %d", m.RootID, est2.Arbol.Root())
	}
	if m.TotalPages != est2.Pager.TotalPages() {
		t.Errorf("la meta dice total=%d y el pager %d", m.TotalPages, est2.Pager.TotalPages())
	}
	if est2.Log.Epoca() != epocaAntes+1 {
		t.Errorf("epoca = %d, quiero %d: el paso 10 rota", est2.Log.Epoca(), epocaAntes+1)
	}
	if est2.Log.Bytes() != 0 {
		t.Errorf("el log nuevo mide %d bytes, quiero 0", est2.Log.Bytes())
	}
	if d.Existe(wal.Nombre(epocaAntes)) {
		t.Error("la generacion vieja del log sigue en el directorio")
	}
}

// Un ciclo caída-recuperación-caída no puede dejar generaciones acumulándose: sin el
// barrido, cada recuperación arrastraría un archivo más y sería más lenta que la anterior.
func TestNoSeAcumulanGeneracionesDelLog(t *testing.T) {
	d := fsxtest.Nuevo()
	for i := range 6 {
		est := recupera(t, d)
		pon(t, est.Arbol, i*10, i*10+10)
	}

	logs := 0
	for _, n := range d.Nombres() {
		if _, ok := wal.Generacion(n); ok {
			logs++
		}
	}
	if logs != 1 {
		t.Errorf("generaciones del log en el directorio = %d, quiero 1: %q", logs, d.Nombres())
	}

	est := recupera(t, d)
	exige(t, est.Arbol, 0, 60)
}

// Una generación huérfana que dejó una caída en mitad de una rotación se barre, pero solo
// después del checkpoint del paso 10: nunca antes de que lo que contenía esté a salvo.
//
// La huérfana es siempre una generación **anterior** a la que la meta indica. Rotar crea la
// N+1, hace fsync del directorio y solo entonces borra la N; una caída entre esos dos
// puntos deja las dos, con la meta --escrita antes de rotar-- apuntando ya a la N+1.
func TestSeBarreLaGeneracionHuerfanaDeUnaRotacionAMedias(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 30)

	huerfana := est.Log.Epoca() - 1
	f, err := d.Open(wal.Nombre(huerfana))
	if err != nil {
		t.Fatal(err)
	}
	// Con contenido, para que borrarla no sea gratis por estar vacia.
	if _, err := f.WriteAt(bytes.Repeat([]byte{0xAA}, 128), 0); err != nil {
		t.Fatal(err)
	}

	est2 := recupera(t, d)
	exige(t, est2.Arbol, 0, 30)
	if d.Existe(wal.Nombre(huerfana)) {
		t.Errorf("la generacion huerfana %d sigue en el directorio: %q", huerfana, d.Nombres())
	}
	for _, n := range d.Nombres() {
		if e, ok := wal.Generacion(n); ok && e < est2.Log.Epoca() {
			t.Errorf("quedo la generacion %d por debajo de la actual %d", e, est2.Log.Epoca())
		}
	}
}

// --- Log vacío -------------------------------------------------------------------------

// Reabrir una base cerrada limpiamente no encuentra nada que reproducir, y eso es un final
// limpio y no un fallo.
func TestReaperturaLimpiaNoReproduceNada(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 80)
	if err := est.Checkpoint.Correr(est.Arbol.Root()); err != nil {
		t.Fatal(err)
	}
	est.Log.Close()

	est2 := recupera(t, d)
	if est2.Informe.Grupos != 0 {
		t.Errorf("grupos = %d, quiero 0", est2.Informe.Grupos)
	}
	if !errors.Is(est2.Informe.Motivo, io.EOF) {
		t.Errorf("motivo = %v, quiero io.EOF", est2.Informe.Motivo)
	}
	exige(t, est2.Arbol, 0, 80)
}

// Lo que TestCaidaYReapertura no puede probar, probado aquí.
//
// Un kill -9 no distingue "hubo fsync" de "no lo hubo": el caché del sistema operativo
// sobrevive a la muerte del proceso, así que los bytes llegan al disco de todas formas y el
// motor recupera igual. Quitarle el fsync al commit del WAL no pone en rojo la prueba de
// caída. Lo que sí lo distingue es el **orden de las llamadas**, y eso la traza compartida
// lo ve: Put no puede devolver nil sin que su registro de commit haya pasado por el fsync
// del log.
//
// Es la mitad de la sec. 7.2 que se puede afirmar sin disco falso. La otra mitad -- que el
// fsync sirva de algo, es decir, que el disco no reordene ni descarte -- es la F4.
func TestPutNoDevuelveSinElFsyncDelLog(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	nombre := wal.Nombre(est.Log.Epoca())
	d.Traza.Limpia()

	if err := est.Arbol.Put(clave(1), valor(1)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	eventos := d.Traza.Eventos()
	ultimaEscritura, sync := -1, -1
	for i, e := range eventos {
		if strings.HasPrefix(e, nombre+":write ") {
			ultimaEscritura = i
		}
		if e == nombre+":sync" {
			sync = i
		}
	}
	if ultimaEscritura < 0 {
		t.Fatalf("el Put no escribio nada en el log: %q", eventos)
	}
	if sync < 0 {
		t.Fatalf("el Put devolvio nil sin fsync del log: %q", eventos)
	}
	if sync < ultimaEscritura {
		t.Errorf("el fsync (%d) precede a la ultima escritura del grupo (%d): %q",
			sync, ultimaEscritura, eventos)
	}
	// Y nada bajó a datos.db en el camino: el dato está a salvo por estar en el log, no por
	// estar en su sitio definitivo (sec. 7.2, el paso 6 antes del 7).
	for _, e := range eventos {
		if strings.HasPrefix(e, recovery.NombreDatos+":write ") {
			t.Errorf("el Put escribio en datos.db: %q", eventos)
			break
		}
	}
}

// --- Invariante 6, la mitad de las páginas libres (D8) --------------------------------

// extiendeSinImagenes anexa al log un grupo sin imágenes que solo confirma una extensión del
// archivo. Es la forma que tiene en el log una operación de metadatos (sec. 7.6), y deja
// páginas en el conjunto de libres, que es lo que hace falta para probar la comprobación.
func extiendeSinImagenes(t *testing.T, d *fsxtest.Disco, est *recovery.Estado, n uint64) uint64 {
	t.Helper()
	total := est.Pager.TotalPages() + n
	f, _ := d.Open(wal.Nombre(est.Log.Epoca()))
	tam, _ := f.Size()
	err := record.NewWriter(f, tam).Append(record.Record{
		LSN:   est.Log.LSN() + 1,
		Type:  wal.TipoCommit,
		Epoch: est.Log.Epoca(),
		Payload: wal.CodificaCommit(0, pager.State{
			RootID: est.Arbol.Root(), TotalPages: total,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return est.Pager.TotalPages()
}

// Una ranura libre a ceros es legítima: es la que materializó una extensión que ninguna
// imagen describe. Un CRC de ceros es inválido, así que exigirlo sin más pondría en rojo un
// árbol sano -- justo el falso positivo que la sec. 6 declara inaceptable.
func TestUnaPaginaLibreACerosEsLegitima(t *testing.T) {
	d := fsxtest.Nuevo()
	est := recupera(t, d)
	pon(t, est.Arbol, 0, 40)
	primeraLibre := extiendeSinImagenes(t, d, est, 5)

	est2 := recupera(t, d)
	libres := est2.Pager.FreePages()
	if len(libres) != 5 {
		t.Fatalf("paginas libres = %v, quiero 5 desde la %d", libres, primeraLibre)
	}
	exige(t, est2.Arbol, 0, 40)
}

// Y una que no es ni una página íntegra ni una ranura a ceros sí es un fallo: es basura
// ilegible en un sitio del que un día se sacará una página.
func TestUnaPaginaLibreIlegibleSeDetecta(t *testing.T) {
	casos := map[string]func(buf []byte, id uint64){
		"basura con crc malo": func(buf []byte, _ uint64) {
			for i := range buf {
				buf[i] = byte(i%251 + 1)
			}
		},
		"una pagina integra de otra ranura": func(buf []byte, id uint64) {
			p := page.New(id+1, page.TypeLeaf)
			if err := p.EncodeTo(buf); err != nil {
				panic(err)
			}
		},
	}
	for nombre, romper := range casos {
		t.Run(nombre, func(t *testing.T) {
			d := fsxtest.Nuevo()
			est := recupera(t, d)
			pon(t, est.Arbol, 0, 40)
			libre := extiendeSinImagenes(t, d, est, 5)

			datos, _ := d.Open(recovery.NombreDatos)
			buf := make([]byte, page.Size)
			romper(buf, libre)
			if _, err := datos.WriteAt(buf, int64(libre)*page.Size); err != nil {
				t.Fatal(err)
			}

			_, err := recovery.Recuperar(d, 0)
			if !errors.Is(err, recovery.ErrLibreIlegible) {
				t.Fatalf("Recuperar = %v, quiero ErrLibreIlegible", err)
			}
		})
	}
}
