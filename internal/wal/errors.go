package wal

import "errors"

var (
	// ErrCargaInvalida indica que la carga de un registro no mide lo que su tipo declara:
	// una imagen que no son CargaImagen bytes, o un commit que no son CargaCommit.
	//
	// Es un error propio y no un page.ErrBadSize porque el fallo está un nivel más
	// arriba: los bytes pueden ser una página perfectamente íntegra, y lo que está mal es
	// el registro que la envuelve. internal/record ya garantizó que esos bytes son los
	// que se escribieron -- si además no son tantos como el tipo pide, quien los escribió
	// usó otro formato.
	ErrCargaInvalida = errors.New("wal: la carga no mide lo que su tipo declara")

	// ErrTipoDesconocido indica un registro cuyo tipo no es TipoImagen ni TipoCommit. Con
	// el CRC en verde no puede venir de un bit alterado en el disco: significa que el
	// registro lo escribió una versión del formato que este código no conoce, y seguir
	// leyendo sería adivinar.
	ErrTipoDesconocido = errors.New("wal: tipo de registro desconocido")
)
