// Package recovery implementa los diez pasos de la sec. 8 del DESIGN.md: el camino que
// recorre toda apertura del motor, haya habido caída o no.
//
// No hay un modo "abrir normal" y otro "recuperar". Es el mismo camino siempre, y eso es
// deliberado: un camino de recuperación que solo corre después de una caída es un camino
// que casi nunca se ejecuta y que por tanto casi nunca se prueba. Aquí lo ejercita cada
// Open, incluido el de una base recién creada.
//
// # Las dos propiedades que sostienen esto
//
// **Idempotencia.** Reaplicar el mismo log dos veces produce el mismo resultado. Es
// obligatorio porque el sistema puede caerse *durante* la recuperación, y con imágenes de
// página completas sale gratis: la imagen sobrescribe la página entera, así que aplicarla
// una vez o cinco da lo mismo.
//
// **Atomicidad de grupo.** Un grupo sin registro de commit se descarta entero. El CRC solo
// cubre el registro individual; lo que hace falta es que las cuatro páginas que toca una
// división estén todas o ninguna, y eso lo da el registro de commit (sec. 7.3).
package recovery

import (
	"errors"
	"io"
	"slices"

	"github.com/jsav2003/motor-almacenamiento/internal/checkpoint"
	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/meta"
	"github.com/jsav2003/motor-almacenamiento/internal/page"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
	"github.com/jsav2003/motor-almacenamiento/internal/tree"
	"github.com/jsav2003/motor-almacenamiento/internal/wal"
)

// NombreDatos es el archivo del árbol (DESIGN.md sec. 5).
const NombreDatos = "datos.db"

// Estado es el motor listo para operar tras la recuperación.
type Estado struct {
	Datos      fsx.File
	Pager      *pager.Pager
	Arbol      *tree.Tree
	Log        *wal.WAL
	Checkpoint *checkpoint.Checkpoint

	// Informe es lo que la recuperación encontró y hizo. Existe porque el paso 10 hace la
	// recuperación **observable**, y sin algo que mirar esa observabilidad no llega a los
	// tests ni a BUGS.md.
	Informe Informe
}

// Informe describe una corrida de la recuperación.
type Informe struct {
	// MetaValida es false cuando las dos ranuras fallaron y hubo que reproducir el log sin
	// referencia. No es un error: es el caso que la sec. 8, paso 1, se niega a tratar como
	// archivo irrecuperable.
	MetaValida bool
	MetaRanura uint64

	// Epocas son las generaciones del WAL que se leyeron, en orden.
	Epocas []uint32

	// DesdeLSN es el LSN tras el que se empezó a leer: el del último checkpoint, o 0 si no
	// había meta válida y hubo que anclar en el primer registro.
	DesdeLSN uint64

	// Grupos e Imagenes son lo que se aplicó.
	Grupos   int
	Imagenes int

	// Motivo es por qué terminó la lectura del log. io.EOF es el final limpio; cualquier
	// otro es el punto exacto donde ocurrió la caída.
	Motivo error

	// RootID y TotalPages son el estado con el que arranca el motor.
	RootID     uint64
	TotalPages uint64

	// ArbolCreado es true cuando no había nada que recuperar y hubo que crear la hoja raíz:
	// una base nueva.
	ArbolCreado bool
}

// Recuperar abre el motor que vive en dir y devuelve todo lo necesario para operarlo.
//
// Los diez pasos de la sec. 8, en orden, están marcados en el cuerpo. El umbral es el de
// bytes de WAL que disparan un checkpoint; 0 usa el de checkpoint.UmbralPorDefecto.
func Recuperar(dir fsx.Dir, umbral int64) (*Estado, error) {
	datos, err := dir.Open(NombreDatos)
	if err != nil {
		return nil, err
	}

	// --- Paso 1: las dos metas ---
	//
	// Que ninguna sea válida no aborta. La durabilidad de Put descansa exclusivamente en
	// el fsync del WAL, así que el log contiene todo lo confirmado; declarar muerto el
	// archivo aquí convertiría una situación totalmente recuperable en pérdida permanente.
	// Es además lo que se encuentra en una base recién creada.
	m, ranura, errMeta := meta.Leer(datos)
	sinMeta := errors.Is(errMeta, meta.ErrSinMetaValida)
	if errMeta != nil && !sinMeta {
		return nil, errMeta
	}
	inf := Informe{MetaValida: !sinMeta, MetaRanura: ranura}

	// proxima es la ranura sobre la que escribirá el checkpoint del paso 10: la contraria a
	// la que ganó, o la 0 si no ganó ninguna.
	proxima := uint64(0)
	if !sinMeta {
		proxima = meta.RanuraSiguiente(ranura)
	}

	// --- Paso 2: localizar el log ---
	presentes, err := generaciones(dir)
	if err != nil {
		return nil, err
	}
	var aLeer []uint32
	if sinMeta {
		// Sin meta no hay generación indicada, así que se leen todas las que haya, en orden
		// ascendente. El LSN no se reinicia al rotar, de modo que las generaciones
		// sucesivas forman una sola secuencia contigua y wal.Leer las encadena.
		aLeer = presentes
		inf.DesdeLSN = 0
	} else {
		aLeer = []uint32{m.Epoca}
		inf.DesdeLSN = m.LSN
	}

	// --- Pasos 3 y 4: leer y agrupar ---
	var (
		grupos []wal.Grupo
		desde  = inf.DesdeLSN
		fin    int64
	)
	inf.Motivo = io.EOF
	for _, e := range aLeer {
		f, err := dir.Open(wal.Nombre(e))
		if err != nil {
			return nil, err
		}
		gs, hasta, motivo := wal.Leer(f, e, desde)
		f.Close()

		inf.Epocas = append(inf.Epocas, e)
		grupos = append(grupos, gs...)
		inf.Motivo, fin = motivo, hasta

		if !wal.EsFinDeLog(motivo) {
			// Un fallo de E/S de verdad, no el final del log. Tratarlo como el punto de una
			// caída descartaría datos confirmados y llamaría a eso una recuperación
			// correcta.
			return nil, motivo
		}
		if !errors.Is(motivo, io.EOF) {
			// El log se cortó aquí. La sec. 8 paso 3 es explícita: se descarta todo lo que
			// siga, y eso incluye las generaciones posteriores.
			break
		}
		if n := len(gs); n > 0 {
			desde = gs[n-1].LSNCommit
		}
	}
	inf.Grupos = len(grupos)

	// --- Paso 5: extender datos.db ---
	//
	// Si el log trae una imagen de la página 900 y datos.db mide 800, escribir en el offset
	// 900x4096 deja un archivo disperso con un agujero en las páginas 800-899 que se leen
	// como ceros y fallan el CRC. Y la alternativa --no aplicar imágenes más allá de
	// total_pages-- descarta datos confirmados. Las dos salidas rompen algo; la extensión
	// explícita es la única que no.
	total := max(m.TotalPages, pager.MetaPages)
	for _, g := range grupos {
		total = max(total, g.Estado.TotalPages)
		for _, p := range g.Imagenes {
			total = max(total, p.ID+1)
			inf.Imagenes++
		}
	}
	if err := extender(datos, total); err != nil {
		return nil, err
	}

	// --- Paso 6: aplicar las imágenes de los grupos completos ---
	buf := make([]byte, page.Size)
	for _, g := range grupos {
		for _, p := range g.Imagenes {
			if err := p.EncodeTo(buf); err != nil {
				return nil, err
			}
			if _, err := datos.WriteAt(buf, int64(p.ID)*page.Size); err != nil {
				return nil, err
			}
		}
	}
	if err := datos.Sync(); err != nil {
		return nil, err
	}

	// --- Paso 7: el estado del último commit aplicado ---
	root, lsn := m.RootID, m.LSN
	if n := len(grupos); n > 0 {
		root = grupos[n-1].Estado.RootID
		lsn = grupos[n-1].LSNCommit
	}
	epoca := epocaActual(sinMeta, m, presentes)

	log, err := wal.Abrir(dir, epoca, lsn, fin)
	if err != nil {
		return nil, err
	}
	pg, err := pager.New(datos, log, total)
	if err != nil {
		log.Close()
		return nil, err
	}

	arbol := tree.New(pg, root)
	if root < pager.MetaPages {
		// No había raíz: ni meta válida ni un solo grupo confirmado. Es una base nueva, y
		// su primer grupo de commit es el que registra la hoja raíz.
		arbol, err = tree.Crear(pg)
		if err != nil {
			log.Close()
			return nil, err
		}
		inf.ArbolCreado = true
	}

	// --- Paso 8: reconstruir el conjunto de páginas libres (sec. 6.1) ---
	if err := arbol.ReconstruirLibres(); err != nil {
		log.Close()
		return nil, err
	}

	// --- Paso 9: los seis invariantes ---
	if err := arbol.Validate(); err != nil {
		log.Close()
		return nil, err
	}

	// --- Paso 10: terminar con un checkpoint completo ---
	//
	// Sin él, el motor arranca operando con el root_id de la meta vieja, que es justo el
	// estado que los pasos anteriores acaban de demostrar obsoleto: el primer Put
	// descendería por el árbol viejo y divergiría más. Y el WAL no se liberaría nunca en un
	// ciclo caída-recuperación-caída, así que cada recuperación sería más lenta que la
	// anterior.
	cp := checkpoint.Nuevo(datos, pg, log, proxima, umbral)
	if err := cp.Correr(arbol.Root()); err != nil {
		log.Close()
		return nil, err
	}
	if err := barrerGeneracionesViejas(dir, log.Epoca()); err != nil {
		log.Close()
		return nil, err
	}

	inf.RootID = arbol.Root()
	inf.TotalPages = pg.TotalPages()
	return &Estado{
		Datos: datos, Pager: pg, Arbol: arbol, Log: log, Checkpoint: cp, Informe: inf,
	}, nil
}

// epocaActual decide en qué generación seguirá escribiendo el log. Con meta válida, la que
// ella indica; sin meta, la más alta que haya en el directorio, o la 0 si no hay ninguna.
func epocaActual(sinMeta bool, m meta.Meta, presentes []uint32) uint32 {
	if !sinMeta {
		return m.Epoca
	}
	if n := len(presentes); n > 0 {
		return presentes[n-1]
	}
	return 0
}

// generaciones devuelve las del WAL presentes en el directorio, ordenadas.
func generaciones(dir fsx.Dir) ([]uint32, error) {
	nombres, err := dir.Listar()
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, n := range nombres {
		if e, ok := wal.Generacion(n); ok {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out, nil
}

// barrerGeneracionesViejas borra los logs anteriores al actual.
//
// Normalmente no hay ninguno: la rotación borra el que deja atrás. Pero una caída entre la
// creación del log nuevo y el borrado del viejo deja los dos, y sin este barrido cada ciclo
// caída-recuperación acumularía un archivo más -- el mismo modo de fallo que la sec. 8 evita
// con el paso 10, por otra vía.
func barrerGeneracionesViejas(dir fsx.Dir, actual uint32) error {
	presentes, err := generaciones(dir)
	if err != nil {
		return err
	}
	for _, e := range presentes {
		if e < actual {
			if err := dir.Remove(wal.Nombre(e)); err != nil {
				return err
			}
		}
	}
	return nil
}

// extender materializa datos.db hasta total páginas con **páginas cero explícitas**.
//
// No es un Truncate. Un archivo disperso deja agujeros que se leen como ceros y fallan el
// CRC igual, pero solo se materializan al escribirlos: el espacio podría no existir, y el
// fallo aparecería en el peor momento posible. Es la misma escritura explícita que hace
// pager.extend en operación normal.
func extender(f fsx.File, total uint64) error {
	tam, err := f.Size()
	if err != nil {
		return err
	}
	// División entera hacia abajo a propósito: una página a medias por un archivo truncado
	// no es una página, y se reescribe a ceros. Si alguna imagen la cubre, el paso 6 la
	// sobrescribe entera de todas formas.
	desde := uint64(tam) / page.Size
	if desde >= total {
		return nil
	}
	cero := make([]byte, page.Size)
	for id := desde; id < total; id++ {
		if _, err := f.WriteAt(cero, int64(id)*page.Size); err != nil {
			return err
		}
	}
	return f.Sync()
}
