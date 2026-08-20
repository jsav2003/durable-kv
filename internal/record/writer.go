package record

// Writer agrega registros al final de un File.
//
// No hace fsync automáticamente en Append: DESIGN.md sec. 7.2 separa "agregar el
// registro al log" (paso 3) de "fsync sobre el log" (paso 5) precisamente porque varios
// registros de un mismo grupo se agregan antes de un único fsync -- eso es lo que evita
// que cada operación cueste una sincronización. Esa política de agrupación es de la F3;
// aquí solo está el mecanismo que la hace posible.
//
// Tampoco impone contigüidad de LSN ni verifica la época: el llamador elige el LSN de
// cada registro. Esa disciplina (LSN monótono, época verificada contra la meta) es
// responsabilidad de la F3 -- DESIGN.md sec. 7.4.
type Writer struct {
	f      File
	offset int64
}

// NewWriter crea un Writer que empieza a agregar en offset. El llamador decide ese
// punto de partida: el tamaño actual del archivo si es nuevo, o el desplazamiento donde
// se detuvo un Reader tras una recuperación (DESIGN.md sec. 8).
func NewWriter(f File, offset int64) *Writer {
	return &Writer{f: f, offset: offset}
}

// Append serializa r y lo agrega al final del archivo, avanzando el desplazamiento
// interno. Devuelve ErrTooLarge si la carga excede MaxPayloadSize.
func (w *Writer) Append(r Record) error {
	buf, err := encode(r)
	if err != nil {
		return err
	}
	if _, err := w.f.WriteAt(buf, w.offset); err != nil {
		return err
	}
	w.offset += int64(len(buf))
	return nil
}

// Sync fuerza a disco todo lo escrito hasta ahora.
func (w *Writer) Sync() error {
	return w.f.Sync()
}

// Offset devuelve la posición donde se escribirá el próximo registro.
func (w *Writer) Offset() int64 {
	return w.offset
}
