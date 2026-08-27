package page

import "errors"

var (
	// ErrBadSize indica que el buffer que se intenta decodificar -- o al que se intenta
	// escribir -- no mide exactamente Size bytes. La sec. 5.1 del DESIGN.md es explícita:
	// la página es la unidad mínima de lectura y de escritura, y nunca se lee ni se
	// escribe menos que una página completa. Un buffer de otro tamaño es un error de
	// programación del llamador, no una corrupción del disco.
	ErrBadSize = errors.New("page: el buffer no mide una pagina completa")

	// ErrCorrupt indica que la página falló su verificación de CRC: los 4092 bytes que
	// siguen al checksum no son los que se escribieron. Es lo que distingue "página
	// válida" de "página escrita a medias" (DESIGN.md sec. 5.1) -- sin esta comprobación
	// se leería basura y se le creería.
	ErrCorrupt = errors.New("page: crc invalido")

	// ErrWrongPage indica que la página es íntegra pero no es la que se pidió: su campo
	// page_id no coincide con el número de página desde el que se leyó.
	//
	// Es un error distinto de ErrCorrupt a propósito, porque describe un fallo distinto:
	// el CRC valida contenido, no ubicación (DESIGN.md sec. 5.1). Una escritura dirigida
	// al offset equivocado -- por aritmética mal hecha, o por una reproducción del log
	// que aplica una imagen al lugar incorrecto -- deja una página con CRC perfectamente
	// válido en la ranura equivocada. Confundir los dos casos en el mismo error haría
	// que el día que aparezca ese bug se diagnostique como disco corrupto.
	ErrWrongPage = errors.New("page: page_id no coincide con la ranura")

	// ErrBadType indica que el campo tipo no es ninguno de los tres que declara la sec.
	// 5.1 (interno, hoja, meta). Con el CRC en verde esto no puede venir de un bit
	// alterado en el disco: significa que quien la escribió puso un tipo que este
	// formato no define.
	ErrBadType = errors.New("page: tipo de pagina desconocido")
)
