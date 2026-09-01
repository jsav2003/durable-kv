package meta

import "errors"

var (
	// ErrSinMetaValida indica que ninguna de las dos ranuras pasó su verificación.
	//
	// **No es un archivo irrecuperable**, y por eso es un error nombrado y no un fallo
	// fatal: la durabilidad de Put descansa exclusivamente en el fsync del WAL, así que el
	// log contiene todo lo confirmado. Declarar muerto el archivo aquí convertiría una
	// situación totalmente recuperable en pérdida permanente, teniendo el WAL entero
	// intacto en el disco. Lo que toca es reproducir el log desde el LSN 0 (sec. 8, paso
	// 1), y es también lo que se encuentra en un archivo recién creado, con las dos
	// ranuras a ceros.
	ErrSinMetaValida = errors.New("meta: ninguna de las dos ranuras es valida")

	// ErrNoEsMeta indica que la página es íntegra y está en la ranura correcta, pero su
	// tipo no es TypeMeta o su cuerpo es más corto de lo que el formato necesita.
	ErrNoEsMeta = errors.New("meta: la pagina no es una meta")

	// ErrMagicoInvalido indica que el número mágico no es el de este motor. Con el CRC en
	// verde no puede venir de un bit alterado: es un archivo de otra cosa, o una ranura
	// que quedó con contenido de otra época.
	ErrMagicoInvalido = errors.New("meta: numero magico ajeno")

	// ErrVersionInvalida indica una versión del formato que este código no conoce. Se
	// rechaza en vez de interpretarse: los campos podrían significar otra cosa, y el CRC
	// no distingue "íntegro" de "íntegro y de otro formato".
	ErrVersionInvalida = errors.New("meta: version del formato desconocida")

	// ErrRanuraInvalida indica una ranura fuera de [0, Ranuras). Es un error de
	// programación del llamador, no una corrupción del disco.
	ErrRanuraInvalida = errors.New("meta: ranura fuera de las dos que hay")
)
