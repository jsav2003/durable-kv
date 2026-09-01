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

var (
	// ErrLSNNoContiguo indica que un registro no lleva el LSN que le tocaba. Es la
	// defensa 1 de la sec. 7.4 disparando: el LSN es estrictamente monótono y nunca se
	// reinicia, así que un salto significa que lo que sigue no pertenece a esta
	// secuencia -- típicamente bytes de una generación anterior que sobrevivieron donde
	// no debían.
	ErrLSNNoContiguo = errors.New("wal: el lsn no es contiguo")

	// ErrEpocaAjena indica un registro de otra generación del log. Es la defensa 2 de la
	// sec. 7.4. Un registro así tiene CRC perfectamente válido -- es un registro real, de
	// antes de una rotación -- y sin esta comprobación se reproduciría encima de datos
	// nuevos.
	ErrEpocaAjena = errors.New("wal: el registro es de otra epoca")

	// ErrGrupoIncompleto indica que el n_registros del commit no coincide con las
	// imágenes leídas desde el commit anterior. No es un grupo a medias por una caída
	// --ese caso es un grupo sin commit, y se descarta en silencio--: es un commit que
	// llegó entero declarando un grupo que no está entero, y eso significa que el log no
	// es lo que dice ser.
	ErrGrupoIncompleto = errors.New("wal: el commit declara mas imagenes de las leidas")
)
