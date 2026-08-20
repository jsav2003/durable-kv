package record

import "errors"

var (
	// ErrCorrupt indica que un registro falló su verificación de CRC: la cabecera o la
	// carga leídas no coinciden con lo que se escribió. Según DESIGN.md sec. 8 paso 3,
	// este es el punto exacto donde ocurrió una caída: la lectura se detiene aquí y todo
	// lo que sigue en el archivo se descarta, incluso si más adelante hubiera bytes que
	// parecieran un registro válido.
	ErrCorrupt = errors.New("record: crc invalido")

	// ErrTruncated indica que el archivo terminó a mitad de un registro: tras leer una
	// cabecera completa, quedaban menos bytes de los que esa cabecera prometía.
	//
	// A efectos de recuperación se trata igual que ErrCorrupt -- el registro se descarta
	// entero, sin buscar más allá -- pero se distingue como error propio porque no es lo
	// mismo "el archivo terminó limpio" que "aquí se cortó a mitad de un registro": para
	// BUGS.md y para los tests, esa diferencia es justamente lo que hay que demostrar.
	ErrTruncated = errors.New("record: registro truncado")

	// ErrTooLarge indica que el campo de longitud de un registro excede
	// MaxPayloadSize. Es la defensa contra una longitud corrupta: sin este límite, un
	// campo de longitud con un valor absurdo (por ejemplo 0xFFFFFFFF) intentaría una
	// reserva de memoria de varios GiB antes de poder siquiera verificar el CRC.
	ErrTooLarge = errors.New("record: carga demasiado grande")
)
