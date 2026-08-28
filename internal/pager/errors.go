package pager

import "errors"

var (
	// ErrNoGroup indica que se intentó ensuciar una página o cerrar un grupo sin haberlo
	// abierto. No es una comodidad de la API: es la sec. 7.3 hecha cumplible. Una
	// mutación fuera de un grupo es una mutación que la recuperación no puede aplicar ni
	// descartar entera, y ese es exactamente el hallazgo H1 -- atomicidad de registro
	// confundida con atomicidad de grupo.
	ErrNoGroup = errors.New("pager: no hay grupo abierto")

	// ErrGroupOpen indica que se llamó a BeginGroup con un grupo ya abierto. Los grupos no
	// se anidan: el registro de commit delimita uno y solo uno, así que dos grupos
	// solapados producirían un commit que confirma mutaciones de los dos y descarta a
	// medias las del interior.
	ErrGroupOpen = errors.New("pager: ya hay un grupo abierto")

	// ErrMetaPage indica que se intentó ensuciar, liberar o asignar una de las dos páginas
	// meta. Es la regla 1 de la sec. 5.2: las metas no pertenecen al conjunto de páginas
	// sucias. Si estuvieran, el paso 1 del checkpoint escribiría la ranura nueva y el paso
	// 3 la vieja, dejando las dos ranuras con estado de la misma época -- y la garantía
	// "si una se corrompe, la otra sirve" se evaporaría.
	ErrMetaPage = errors.New("pager: las paginas meta no son del pager")

	// ErrOutOfRange indica que el número de página está fuera de [0, total_pages).
	ErrOutOfRange = errors.New("pager: numero de pagina fuera del archivo")

	// ErrDoubleFree indica que se liberó una página que ya estaba en el conjunto de
	// libres. Es media mitad del invariante 6 (la partición) comprobada en el punto donde
	// se rompe, y no miles de operaciones después en Validate(): una página en el conjunto
	// de libres dos veces se asigna dos veces, y la segunda asignación borra en silencio
	// las claves de la primera.
	ErrDoubleFree = errors.New("pager: la pagina ya estaba libre")

	// ErrStalePage indica que se ensució un objeto Page que no es el que el caché tiene
	// para ese número de página. Significa que quien lo mutó trabajaba sobre una copia
	// obsoleta, y sus cambios se perderían -- o peor, pisarían los del objeto vivo.
	ErrStalePage = errors.New("pager: la pagina no es la que el cache tiene para ese id")

	// ErrWriteAhead indica que una página sucia iba a bajar a datos.db con su page_lsn por
	// delante de lo sincronizado en el WAL, y que forzar el fsync del log no lo arregló.
	// Es la sec. 7.5 en rojo: significa que la implementación de Log está rota, porque
	// después de un Sync correcto no puede quedar nada asignado sin sincronizar.
	//
	// Es un error y no un aviso porque lo que evita no tiene reparación: si esa escritura
	// se desgarra, el log no tiene con qué arreglarla, ya que su registro se perdió.
	ErrWriteAhead = errors.New("pager: escritura adelantada al wal (sec. 7.5)")
)
