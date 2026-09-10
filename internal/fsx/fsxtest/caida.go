package fsxtest

import (
	"errors"
	"maps"
	"math/rand"
	"slices"
)

// ErrCaido lo devuelve toda operación de E/S sobre un Disco que ya cayó. Un proceso al que
// matan no vuelve a hacer una llamada al sistema; aquí, como no se puede "no retornar", el
// motor recibe este error y para. El aparato de pruebas descarta el handle y reabre sobre
// otro Disco que hereda solo los bytes duraderos.
var ErrCaido = errors.New("fsxtest: el disco cayo")

// cae procesa la cola de pendientes como lo haría el hardware en su peor momento y deja el
// Disco muerto. La secuencia es la de la sec. 9.1:
//
//  1. Descarta una parte de las escrituras sin sincronizar. Son las que el sistema aún no
//     había llevado al plato: pueden perderse todas, algunas o ninguna.
//  2. Reordena las que sí aplica. El disco no promete escribir en el orden en que se le
//     pidió salvo que medie un fsync -- y las que mediaba ya no están en esta cola.
//  3. Una de las supervivientes se aplica solo hasta un límite de sector (512 B): es la
//     escritura desgarrada, lo que ocurre cuando la atomicidad solo se garantiza por
//     sector y la caída parte una página por la mitad.
//  4. Con DirVolatil, decide además qué entradas de directorio sin fsync llegaron al plato:
//     una creación puede no existir, y un borrado puede no haber ocurrido.
//
// Todo el azar sale de un rand sembrado con d.Semilla, así que la misma semilla con la
// misma CaeEn y la misma carga reproduce el estado en disco bit a bit.
func (d *Disco) cae() {
	r := rand.New(rand.NewSource(d.Semilla))

	cola := d.pendientes
	d.pendientes = nil
	d.Caido = true

	// 1. descarte
	viven := make([]escritura, 0, len(cola))
	for _, e := range cola {
		if r.Intn(2) == 0 {
			continue
		}
		viven = append(viven, e)
	}

	// 2. reordenamiento
	r.Shuffle(len(viven), func(i, j int) { viven[i], viven[j] = viven[j], viven[i] })

	// 3. desgarro de una
	desgarrada := -1
	if len(viven) > 0 {
		desgarrada = r.Intn(len(viven))
	}
	for k, e := range viven {
		if k == desgarrada {
			e.datos = e.datos[:corteDeSector(r, len(e.datos))]
		}
		d.aplica(e)
	}

	// 4. las entradas de directorio sin fsync
	d.caenLasEntradas(r)

	d.Traza.Anota("CAIDA en escritura %d (semilla %d, %d de %d pendientes sobreviven)",
		d.nEscrituras, d.Semilla, len(viven), len(cola))
}

// caenLasEntradas decide qué creaciones y qué borrados sin fsync del directorio llegaron al
// plato. Un archivo creado y no sincronizado puede no existir tras la caída; un archivo
// borrado y no sincronizado puede seguir ahí.
//
// Va al final de cae() a propósito. Los números que consumen el descarte, el reordenamiento
// y el desgarro salen del mismo r, así que tomar aquí los primeros habría corrido el flujo y
// cambiado el estado que produce cada semilla del barrido de la sec. 9.2 -- que es
// exactamente lo que este modo no puede hacer. Con DirVolatil apagado, o sin entradas
// pendientes, no se toca r ni una vez.
func (d *Disco) caenLasEntradas(r *rand.Rand) {
	if !d.DirVolatil {
		return
	}
	// El recorrido va por nombre ordenado y no por el mapa: el orden de un mapa en Go es
	// aleatorio, y con él la misma semilla dejaría un directorio distinto en cada corrida.
	for _, nombre := range slices.Sorted(maps.Keys(d.archivos)) {
		a := d.archivos[nombre]
		if !a.creacionPendiente {
			continue
		}
		if r.Intn(2) == 0 {
			// La creación no llegó al disco: el archivo no existe, y con él se van las
			// escrituras que acabaran de aplicársele.
			delete(d.archivos, nombre)
			d.Traza.Anota("CAIDA se pierde la creacion de %s", nombre)
			continue
		}
		a.creacionPendiente = false
	}
	for _, nombre := range slices.Sorted(maps.Keys(d.borrados)) {
		if r.Intn(2) == 0 {
			a := d.borrados[nombre]
			a.creacionPendiente = false
			d.archivos[nombre] = a
			d.Traza.Anota("CAIDA se deshace el borrado de %s", nombre)
		}
	}
	clear(d.borrados)
}

// sector es la unidad de atomicidad que un disco garantiza. Una escritura de varios
// sectores puede quedar partida en cualquier frontera de sector.
const sector = 512

// corteDeSector devuelve cuántos bytes de una escritura de n bytes sobreviven al desgarro:
// un múltiplo de sector estrictamente menor que n, o n/2 si la escritura no llega a dos
// sectores.
func corteDeSector(r *rand.Rand, n int) int {
	if n < 2*sector {
		return n / 2
	}
	sectores := n / sector // >= 2
	return (1 + r.Intn(sectores-1)) * sector
}
