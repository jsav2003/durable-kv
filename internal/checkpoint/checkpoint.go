// Package checkpoint ejecuta los cinco pasos de la sec. 7.4 del DESIGN.md: bajar las
// páginas sucias a datos.db, escribir la meta, y rotar el WAL.
//
// # Por qué es un paquete propio
//
// Los pasos 1 y 2 son del pager, el 3 y el 4 de la meta, y el 5 del WAL. El pager no
// puede orquestarlos: conoce el log como la interfaz pager.Log, que la F1 cerró con cinco
// operaciones y en la que no hay ninguna rotación, e importar internal/wal desde ahí sería
// un ciclo -- wal importa pager para State y para la propia interfaz. Así que el
// orquestador vive por encima de los tres, que es donde el orden entre archivos distintos
// se puede afirmar de una pieza.
//
// # El orden es todo
//
//	1 y 2. Escribir a datos.db todas las páginas sucias, y fsync. Las metas no están en
//	       ese conjunto (sec. 5.2, regla 1).
//	3 y 4. Escribir la meta **más antigua** de las dos, con el nuevo LSN y la generación
//	       N+1, y fsync.
//	5.     Crear datos.wal.N+1, fsync del directorio, borrar datos.wal.N.
//
// Liberar el log antes del paso 2 sería perder la única copia buena de lo confirmado que
// todavía no está en su sitio definitivo. Por eso el orden se prueba sobre una traza
// compartida entre los dos archivos y no con aserciones por componente: con una traza por
// componente, un checkpoint que hiciera las cosas al revés pasaría los tests.
//
// # Las dos ventanas de caída, y por qué ninguna pierde nada
//
// Entre el 4 y el 5: la meta ya dice época N+1 y solo existe datos.wal.N. Al reabrir se
// busca la N+1, que no está, y no se reproduce nada -- correcto, porque el paso 2 ya dejó
// en datos.db todo lo que el log tenía que aportar.
//
// Entre el 3 y el 4: la meta nueva se pierde y sigue valiendo la vieja, que apunta a la
// época N con un LSN anterior. El paso 5 no llegó a correr, así que datos.wal.N sigue ahí
// y se reproduce desde ese LSN. Reaplicar imágenes que ya estaban aplicadas es idempotente
// por construcción (sec. 8).
package checkpoint

import (
	"github.com/jsav2003/motor-almacenamiento/internal/fsx"
	"github.com/jsav2003/motor-almacenamiento/internal/meta"
	"github.com/jsav2003/motor-almacenamiento/internal/pager"
	"github.com/jsav2003/motor-almacenamiento/internal/wal"
)

// UmbralPorDefecto son los bytes de WAL acumulados que disparan un checkpoint. La sec. 7.4
// dice "cada N registros o cada N bytes de log" sin fijar N.
//
// Se cuenta por bytes y no por registros porque el volumen real de log para el mismo
// contador varía mucho: un grupo puede ser una imagen de 4 KiB o cinco. Lo que el
// checkpoint acota es el tamaño del WAL y por tanto lo que costará la próxima
// recuperación, y eso se mide en bytes.
const UmbralPorDefecto int64 = 4 << 20

// Checkpoint ejecuta y decide cuándo toca.
type Checkpoint struct {
	// datos es datos.db. Se sostiene aquí además de en el pager, y eso es correcto y no
	// una duplicación: las metas no pertenecen al pager (sec. 5.2, regla 1), así que su
	// escritura tiene que llegar al archivo por un camino que no pase por el conjunto de
	// sucias. Las dos mitades escriben regiones disjuntas -- el pager de la página 2 en
	// adelante, esto solo las ranuras 0 y 1.
	datos fsx.File

	pg  *pager.Pager
	log *wal.WAL

	// proxima es la ranura sobre la que escribe el siguiente checkpoint: siempre la más
	// antigua de las dos.
	proxima uint64

	umbral int64
}

// Nuevo prepara el checkpoint. proxima es la ranura sobre la que le toca escribir: la
// contraria a la que ganó la lectura de la meta al abrir, o la 0 si no había ninguna
// válida.
func Nuevo(datos fsx.File, pg *pager.Pager, log *wal.WAL, proxima uint64, umbral int64) *Checkpoint {
	if umbral <= 0 {
		umbral = UmbralPorDefecto
	}
	return &Checkpoint{datos: datos, pg: pg, log: log, proxima: proxima, umbral: umbral}
}

// Toca informa si el WAL acumulado desde el último checkpoint supera el umbral. Como el
// checkpoint siempre rota, lo acumulado es el tamaño de la generación actual.
func (c *Checkpoint) Toca() bool {
	return c.log.Bytes() >= c.umbral
}

// Ranura es la ranura sobre la que escribirá el próximo checkpoint.
func (c *Checkpoint) Ranura() uint64 { return c.proxima }

// Correr ejecuta los cinco pasos de la sec. 7.4 con rootID como raíz actual del árbol.
//
// No corre con un grupo abierto: FlushDirty lo rechaza con ErrGroupOpen. Un grupo a medias
// tiene páginas mutadas en memoria cuyo commit todavía no existe, y bajarlas a datos.db es
// exactamente lo que la sec. 7.5 prohíbe.
func (c *Checkpoint) Correr(rootID uint64) error {
	// Pasos 1 y 2.
	if err := c.pg.FlushDirty(); err != nil {
		return err
	}

	// Paso 3. La época que se escribe es la N+1, la que existirá tras el paso 5, no la
	// actual: la meta describe dónde buscar los registros **posteriores** a este
	// checkpoint, y esos irán a la generación nueva.
	m := meta.Meta{
		RootID: rootID,
		// free_head a cero: la sec. 6.1 decide no persistir el conjunto de libres y
		// reconstruirlo al abrir. El campo se conserva en el formato, se escribe y no se
		// lee, igual que en el registro de commit.
		FreeHead:   0,
		TotalPages: c.pg.TotalPages(),
		Epoca:      c.log.Epoca() + 1,
		LSN:        c.log.LSN(),
	}
	if err := meta.Escribir(c.datos, c.proxima, m); err != nil {
		return err
	}

	// Paso 4.
	if err := c.datos.Sync(); err != nil {
		return err
	}

	// Paso 5.
	if err := c.log.Rotar(); err != nil {
		return err
	}

	c.proxima = meta.RanuraSiguiente(c.proxima)
	return nil
}
