package node

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/jsav2003/durable-kv/internal/page"
)

// hoja y interno devuelven un nodo vacío del tipo que toca, sobre una página recién
// creada. page.New ya deja libre y libre_fin en sus extremos, así que este paquete no
// necesita inicializador propio.
func hoja() Node    { return Of(page.New(7, page.TypeLeaf)) }
func interno() Node { return Of(page.New(7, page.TypeInternal)) }

// clave produce claves de longitud fija y orden lexicográfico igual al numérico. Con
// "10" < "9" en lexicográfico, media suite de un B+tree pasa por accidente.
func clave(i int) []byte { return []byte(fmt.Sprintf("k%06d", i)) }

func valor(i, n int) []byte {
	v := make([]byte, n)
	for j := range v {
		v[j] = byte(i + j)
	}
	return v
}

// insertaOrdenado mete una entrada por el camino que usará el árbol: buscar y luego
// insertar en el punto que devuelve la búsqueda.
func insertaOrdenado(t *testing.T, n Node, k, v []byte) error {
	t.Helper()
	i, encontrada := n.Search(k)
	if encontrada {
		t.Fatalf("la clave %q ya estaba", k)
	}
	return n.InsertHoja(i, k, v)
}

func valorDe(t *testing.T, n Node, i int) []byte {
	t.Helper()
	v, err := n.Value(i)
	if err != nil {
		t.Fatalf("Value(%d): %v", i, err)
	}
	return v
}

// exigeCoherente comprueba Check y, además, las dos fronteras a mano. Check podría estar
// mal; las fronteras son la comprobación mínima que no depende de él.
func exigeCoherente(t *testing.T, n Node) {
	t.Helper()
	if err := n.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if n.p.Free > n.p.FreeEnd {
		t.Fatalf("libre=%d ha pasado a libre_fin=%d", n.p.Free, n.p.FreeEnd)
	}
	if int(n.p.FreeEnd) > page.Size {
		t.Fatalf("libre_fin=%d se sale de la pagina", n.p.FreeEnd)
	}
}

// TestRoundTripPorLaPagina ata esta capa al formato ya probado de la F1: un nodo escrito
// aquí tiene que sobrevivir al EncodeTo/Decode de internal/page y leerse igual. Sin este
// test, node podría mantener una contabilidad coherente consigo misma y no con el disco.
func TestRoundTripPorLaPagina(t *testing.T) {
	n := hoja()
	n.SetLink(0x1122334455667788)
	for i := 0; i < 5; i++ {
		if err := insertaOrdenado(t, n, clave(i), valor(i, 20+i)); err != nil {
			t.Fatalf("InsertHoja: %v", err)
		}
	}

	buf := make([]byte, page.Size)
	if err := n.Page().EncodeTo(buf); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	p2, err := page.Decode(buf, 7)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	m := Of(p2)
	exigeCoherente(t, m)
	if m.NCells() != 5 {
		t.Fatalf("nceldas tras el viaje: %d", m.NCells())
	}
	if m.Link() != 0x1122334455667788 {
		t.Fatalf("enlace tras el viaje: %#x", m.Link())
	}
	for i := 0; i < 5; i++ {
		if !bytes.Equal(m.Key(i), clave(i)) {
			t.Errorf("clave %d: %q", i, m.Key(i))
		}
		if !bytes.Equal(valorDe(t, m, i), valor(i, 20+i)) {
			t.Errorf("valor %d distinto", i)
		}
	}
}

// TestOrdenEstricto es el invariante 1 de la sec. 6. Se insertan las mismas claves en
// tres órdenes distintos y el directorio tiene que quedar igual en los tres.
func TestOrdenEstricto(t *testing.T) {
	const nclaves = 20

	ordenes := map[string][]int{}
	var creciente, decreciente, alterno []int
	for i := 0; i < nclaves; i++ {
		creciente = append(creciente, i)
		decreciente = append(decreciente, nclaves-1-i)
	}
	for i := 0; i < nclaves; i += 2 {
		alterno = append(alterno, i)
	}
	for i := 1; i < nclaves; i += 2 {
		alterno = append(alterno, i)
	}
	ordenes["creciente"] = creciente
	ordenes["decreciente"] = decreciente
	ordenes["alterno"] = alterno

	// Semilla fija: un orden aleatorio distinto en cada corrida daría un fallo que no se
	// puede reproducir, y BUGS.md no podría citarlo.
	r := rand.New(rand.NewPCG(20260831, 2))
	aleatorio := append([]int(nil), creciente...)
	r.Shuffle(len(aleatorio), func(i, j int) { aleatorio[i], aleatorio[j] = aleatorio[j], aleatorio[i] })
	ordenes["aleatorio"] = aleatorio

	for nombre, orden := range ordenes {
		t.Run(nombre, func(t *testing.T) {
			n := hoja()
			for _, i := range orden {
				if err := insertaOrdenado(t, n, clave(i), valor(i, 10)); err != nil {
					t.Fatalf("InsertHoja(%d): %v", i, err)
				}
				exigeCoherente(t, n)
			}
			if n.NCells() != nclaves {
				t.Fatalf("nceldas: %d", n.NCells())
			}
			for i := 0; i < nclaves; i++ {
				if !bytes.Equal(n.Key(i), clave(i)) {
					t.Fatalf("slot %d: %q, se esperaba %q", i, n.Key(i), clave(i))
				}
				if !bytes.Equal(valorDe(t, n, i), valor(i, 10)) {
					t.Fatalf("valor %d distinto", i)
				}
			}
		})
	}
}

// TestBusqueda comprueba que Search devuelve el punto de inserción y no solo si encontró.
// El descenso del árbol depende de ese entero tanto como de que sea correcto el booleano.
func TestBusqueda(t *testing.T) {
	n := hoja()
	// Claves pares: 0, 2, 4, 6, 8.
	for i := 0; i < 10; i += 2 {
		if err := insertaOrdenado(t, n, clave(i), valor(i, 4)); err != nil {
			t.Fatalf("InsertHoja: %v", err)
		}
	}

	for i := 0; i < 10; i++ {
		got, encontrada := n.Search(clave(i))
		quiere := i / 2
		if i%2 == 0 {
			if !encontrada || got != quiere {
				t.Errorf("Search(%d) = (%d, %v), se esperaba (%d, true)", i, got, encontrada, quiere)
			}
			continue
		}
		if encontrada || got != quiere+1 {
			t.Errorf("Search(%d) = (%d, %v), se esperaba (%d, false)", i, got, encontrada, quiere+1)
		}
	}

	// Antes de la primera y después de la última.
	if i, encontrada := n.Search([]byte("a")); encontrada || i != 0 {
		t.Errorf("Search antes de la primera = (%d, %v)", i, encontrada)
	}
	if i, encontrada := n.Search([]byte("z")); encontrada || i != 5 {
		t.Errorf("Search despues de la ultima = (%d, %v)", i, encontrada)
	}
	// Nodo vacío.
	if i, encontrada := hoja().Search(clave(0)); encontrada || i != 0 {
		t.Errorf("Search en nodo vacio = (%d, %v)", i, encontrada)
	}
}

// TestLlenarHastaNoSpace mete celdas hasta que la página dice basta, con varios tamaños, y
// comprueba la coherencia tras *cada* inserción. Una página que solo queda mal cuando la
// mira el insert siguiente es una página que ya bajó a disco rota.
func TestLlenarHastaNoSpace(t *testing.T) {
	for _, tam := range []int{0, 1, 7, 100, 987} { // 987 + clave(7) + 6 = 1000, el tope
		t.Run(fmt.Sprintf("valor=%d", tam), func(t *testing.T) {
			n := hoja()
			metidas := 0
			for i := 0; ; i++ {
				err := insertaOrdenado(t, n, clave(i), valor(i, tam))
				if errors.Is(err, ErrNoSpace) {
					break
				}
				if err != nil {
					t.Fatalf("InsertHoja(%d): %v", i, err)
				}
				metidas++
				exigeCoherente(t, n)
				if i > page.Size {
					t.Fatal("la pagina no se llena nunca")
				}
			}
			if metidas == 0 {
				t.Fatal("no cupo ni una celda")
			}
			// Tras ErrNoSpace la página sigue siendo la que era.
			exigeCoherente(t, n)
			if n.NCells() != metidas {
				t.Fatalf("ErrNoSpace cambio nceldas: %d, se esperaba %d", n.NCells(), metidas)
			}
			for i := 0; i < metidas; i++ {
				if !bytes.Equal(n.Key(i), clave(i)) {
					t.Fatalf("slot %d corrupto tras llenar", i)
				}
			}
		})
	}
}

// TestCuatroCeldasDeMilBytes es la comprobación directa del número de la sec. 4: el tope de
// 1000 bytes existe para que cuatro celdas del tamaño máximo quepan en una página, que es
// lo que hace que una hoja llena siempre se pueda partir en dos mitades razonables.
//
// La derivación del documento (4096 - 40 = 4056; 4056/4 ~ 1014) no descuenta el directorio
// de slots, que son 2 bytes por celda: el espacio real para cuatro celdas es 4048, y
// 4048/4 = 1012. El tope de 1000 sigue siendo válido con holgura -- y eso es justo lo que
// mide este test.
func TestCuatroCeldasDeMilBytes(t *testing.T) {
	n := hoja()
	const klen = 7 // len(clave(i))
	for i := 0; i < 4; i++ {
		v := valor(i, MaxEntrySize-LeafCellOverhead-klen)
		if err := insertaOrdenado(t, n, clave(i), v); err != nil {
			t.Fatalf("no caben cuatro celdas del tamano maximo: en la %d, %v", i, err)
		}
		exigeCoherente(t, n)
	}
	if n.NCells() != 4 {
		t.Fatalf("nceldas: %d", n.NCells())
	}
	t.Logf("cuatro celdas de %d bytes dejan %d bytes libres", MaxEntrySize, n.FreeTotal())
}

func TestLimitesDeTamano(t *testing.T) {
	casos := []struct {
		nombre     string
		klen, vlen int
		quiere     error
	}{
		{"clave en el tope", MaxKeySize, 0, nil},
		{"clave pasada del tope", MaxKeySize + 1, 0, ErrKeyTooLarge},
		{"entrada en el tope", 100, MaxEntrySize - LeafCellOverhead - 100, nil},
		{"entrada pasada del tope", 100, MaxEntrySize - LeafCellOverhead - 100 + 1, ErrEntryTooLarge},
		{"clave vacia", 0, 10, nil},
		{"valor vacio", 10, 0, nil},
		{"los dos vacios", 0, 0, nil},
		// El límite de la clave se comprueba antes que el de la entrada: una clave de 600
		// bytes es ilegal aunque la entrada quepa, porque esa clave viajará a un nodo
		// interno como separadora.
		{"clave larga con entrada legal", 600, 0, ErrKeyTooLarge},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			n := hoja()
			err := n.InsertHoja(0, make([]byte, c.klen), make([]byte, c.vlen))
			if !errors.Is(err, c.quiere) {
				t.Fatalf("InsertHoja(%d, %d) = %v, se esperaba %v", c.klen, c.vlen, err, c.quiere)
			}
			if c.quiere != nil && n.NCells() != 0 {
				t.Fatalf("el rechazo dejo %d celdas", n.NCells())
			}
			exigeCoherente(t, n)
		})
	}
}

// TestCompactacion: borrar deja bytes muertos, y la compactación tiene que recuperarlos sin
// que el llamador se entere. Quien la dispara es Insert, no el árbol.
func TestCompactacion(t *testing.T) {
	n := hoja()
	const nclaves = 7
	for i := 0; i < nclaves; i++ {
		if err := insertaOrdenado(t, n, clave(i), valor(i, 500)); err != nil {
			t.Fatalf("InsertHoja(%d): %v", i, err)
		}
	}

	// Borrar las impares, de mayor a menor para no mover los índices de las que quedan.
	for i := nclaves - 1; i >= 0; i-- {
		if i%2 == 1 {
			if err := n.Delete(i); err != nil {
				t.Fatalf("Delete(%d): %v", i, err)
			}
		}
	}
	exigeCoherente(t, n)

	contiguo, total := n.FreeContiguo(), n.FreeTotal()
	if contiguo >= total {
		t.Fatalf("borrar no dejo bytes muertos: contiguo=%d, total=%d", contiguo, total)
	}

	// Una entrada del tamaño máximo no cabe en el hueco contiguo pero sí en el total: el
	// camino que obliga a compactar.
	grande := valor(99, MaxEntrySize-LeafCellOverhead-7)
	if LeafCellOverhead+7+len(grande)+SlotSize <= contiguo {
		t.Fatalf("el caso no ejercita la compactacion: cabe en el hueco contiguo (%d)", contiguo)
	}
	if err := insertaOrdenado(t, n, clave(99), grande); err != nil {
		t.Fatalf("InsertHoja tras compactar: %v", err)
	}
	exigeCoherente(t, n)

	// Los supervivientes siguen intactos y en orden, y la compactación no dejó huecos.
	quiere := []int{0, 2, 4, 6, 99}
	if n.NCells() != len(quiere) {
		t.Fatalf("nceldas: %d, se esperaba %d", n.NCells(), len(quiere))
	}
	for pos, i := range quiere {
		if !bytes.Equal(n.Key(pos), clave(i)) {
			t.Errorf("slot %d: %q, se esperaba %q", pos, n.Key(pos), clave(i))
		}
	}
	if !bytes.Equal(valorDe(t, n, 4), grande) {
		t.Error("el valor recien insertado no coincide")
	}
	if n.FreeContiguo() != n.FreeTotal() {
		t.Errorf("tras compactar quedan bytes muertos: contiguo=%d, total=%d",
			n.FreeContiguo(), n.FreeTotal())
	}
}

// TestBorradoDeCadaPosicion borra la posición i para todo i, no una de ejemplo. El
// corrimiento del directorio es aritmética de índices y falla en los extremos.
func TestBorradoDeCadaPosicion(t *testing.T) {
	const nclaves = 6
	for borrada := 0; borrada < nclaves; borrada++ {
		t.Run(fmt.Sprintf("borra=%d", borrada), func(t *testing.T) {
			n := hoja()
			for i := 0; i < nclaves; i++ {
				if err := insertaOrdenado(t, n, clave(i), valor(i, 30+i)); err != nil {
					t.Fatalf("InsertHoja(%d): %v", i, err)
				}
			}
			if err := n.Delete(borrada); err != nil {
				t.Fatalf("Delete(%d): %v", borrada, err)
			}
			exigeCoherente(t, n)

			if n.NCells() != nclaves-1 {
				t.Fatalf("nceldas: %d", n.NCells())
			}
			pos := 0
			for i := 0; i < nclaves; i++ {
				if i == borrada {
					continue
				}
				if !bytes.Equal(n.Key(pos), clave(i)) {
					t.Fatalf("slot %d: %q, se esperaba %q", pos, n.Key(pos), clave(i))
				}
				if !bytes.Equal(valorDe(t, n, pos), valor(i, 30+i)) {
					t.Fatalf("valor del slot %d distinto", pos)
				}
				pos++
			}
			if _, encontrada := n.Search(clave(borrada)); encontrada {
				t.Error("la clave borrada sigue apareciendo en la busqueda")
			}
		})
	}
}

func TestBorradoFueraDeRango(t *testing.T) {
	n := hoja()
	if err := insertaOrdenado(t, n, clave(0), valor(0, 4)); err != nil {
		t.Fatalf("InsertHoja: %v", err)
	}
	for _, i := range []int{-1, 1, 99} {
		if err := n.Delete(i); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Delete(%d) = %v, se esperaba ErrOutOfRange", i, err)
		}
	}
	if n.NCells() != 1 {
		t.Fatalf("un Delete rechazado cambio nceldas: %d", n.NCells())
	}
}

// TestSustituirValor comprueba la decisión de que reemplazar es Delete + Insert: un solo
// camino de mutación, aunque el valor nuevo mida lo mismo que el viejo.
func TestSustituirValor(t *testing.T) {
	n := hoja()
	for i := 0; i < 4; i++ {
		if err := insertaOrdenado(t, n, clave(i), valor(i, 50)); err != nil {
			t.Fatalf("InsertHoja: %v", err)
		}
	}
	i, encontrada := n.Search(clave(2))
	if !encontrada {
		t.Fatal("no se encontro la clave a sustituir")
	}
	nuevo := valor(200, 900)
	if err := n.Delete(i); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := n.InsertHoja(i, clave(2), nuevo); err != nil {
		t.Fatalf("InsertHoja: %v", err)
	}
	exigeCoherente(t, n)

	if n.NCells() != 4 {
		t.Fatalf("nceldas: %d", n.NCells())
	}
	j, encontrada := n.Search(clave(2))
	if !encontrada || j != 2 {
		t.Fatalf("Search tras sustituir = (%d, %v)", j, encontrada)
	}
	if !bytes.Equal(valorDe(t, n, j), nuevo) {
		t.Error("el valor sustituido no es el nuevo")
	}
}

// TestNodoInterno cubre el otro formato de celda y los accesores que dependen del tipo.
func TestNodoInterno(t *testing.T) {
	n := interno()
	n.SetLink(999) // el hijo más a la derecha

	for i := 0; i < 8; i++ {
		j, encontrada := n.Search(clave(i))
		if encontrada {
			t.Fatalf("la separadora %d ya estaba", i)
		}
		if err := n.InsertInterno(j, clave(i), uint64(100+i)); err != nil {
			t.Fatalf("InsertInterno(%d): %v", i, err)
		}
		exigeCoherente(t, n)
	}

	// n separadoras, n+1 hijos: los n izquierdos en las celdas, el derecho en el enlace.
	for i := 0; i < 8; i++ {
		if !bytes.Equal(n.Key(i), clave(i)) {
			t.Errorf("separadora %d: %q", i, n.Key(i))
		}
		hijo, err := n.Child(i)
		if err != nil {
			t.Fatalf("Child(%d): %v", i, err)
		}
		if hijo != uint64(100+i) {
			t.Errorf("hijo %d: %d", i, hijo)
		}
	}
	if n.Link() != 999 {
		t.Errorf("hijo derecho: %d", n.Link())
	}

	// Los accesores del otro tipo tienen que negarse, no devolver bytes plausibles.
	if _, err := n.Value(0); !errors.Is(err, ErrWrongType) {
		t.Errorf("Value sobre un nodo interno = %v", err)
	}
	if err := n.InsertHoja(0, clave(0), nil); !errors.Is(err, ErrWrongType) {
		t.Errorf("InsertHoja sobre un nodo interno = %v", err)
	}
	h := hoja()
	if _, err := h.Child(0); !errors.Is(err, ErrWrongType) {
		t.Errorf("Child sobre una hoja = %v", err)
	}
	if err := h.InsertInterno(0, clave(0), 1); !errors.Is(err, ErrWrongType) {
		t.Errorf("InsertInterno sobre una hoja = %v", err)
	}
	// Una meta no es un nodo por ninguna de las dos puntas.
	m := Of(page.New(0, page.TypeMeta))
	if err := m.Check(); !errors.Is(err, ErrWrongType) {
		t.Errorf("Check sobre una meta = %v", err)
	}
}

// TestLayoutDeCelda fija los bytes en disco de una celda de cada tipo contra literales
// escritos a mano, igual que TestLayoutDeCabecera hace con la cabecera en internal/page.
// Sin esto, cambiar el formato de celda pasa en verde mientras las dos puntas se equivoquen
// igual, y las páginas escritas ayer dejan de leerse sin que nada avise.
func TestLayoutDeCelda(t *testing.T) {
	t.Run("hoja", func(t *testing.T) {
		n := hoja()
		if err := n.InsertHoja(0, []byte("ab"), []byte("xyz")); err != nil {
			t.Fatalf("InsertHoja: %v", err)
		}
		// key_len(2) + val_len(4) + 2 + 3 = 11 bytes, pegados al final de la página.
		const sz = 11
		off := uint16(page.Size - sz)
		if n.p.FreeEnd != off {
			t.Fatalf("libre_fin: %d, se esperaba %d", n.p.FreeEnd, off)
		}
		if n.p.Free != page.HeaderSize+SlotSize {
			t.Fatalf("libre: %d, se esperaba %d", n.p.Free, page.HeaderSize+SlotSize)
		}
		if got := binary.LittleEndian.Uint16(n.p.Body[0:]); got != off {
			t.Fatalf("slot 0: %d, se esperaba %d", got, off)
		}
		quiere := []byte{
			0x02, 0x00, // key_len = 2
			0x03, 0x00, 0x00, 0x00, // val_len = 3
			'a', 'b',
			'x', 'y', 'z',
		}
		if got := n.p.Body[idx(off) : idx(off)+sz]; !bytes.Equal(got, quiere) {
			t.Fatalf("celda de hoja:\n got %v\nwant %v", got, quiere)
		}
	})

	t.Run("interno", func(t *testing.T) {
		n := interno()
		if err := n.InsertInterno(0, []byte("ab"), 0x0807060504030201); err != nil {
			t.Fatalf("InsertInterno: %v", err)
		}
		// key_len(2) + hijo_izq(8) + 2 = 12 bytes.
		const sz = 12
		off := uint16(page.Size - sz)
		if n.p.FreeEnd != off {
			t.Fatalf("libre_fin: %d, se esperaba %d", n.p.FreeEnd, off)
		}
		quiere := []byte{
			0x02, 0x00, // key_len = 2
			0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // hijo_izq, little endian
			'a', 'b',
		}
		if got := n.p.Body[idx(off) : idx(off)+sz]; !bytes.Equal(got, quiere) {
			t.Fatalf("celda de nodo interno:\n got %v\nwant %v", got, quiere)
		}
	})
}

// TestLibreEsDerivable es la comprobación de D5: el campo libre de la cabecera es siempre
// HeaderSize + 2*nceldas. Se escribe porque el formato lo declara, pero no es una fuente de
// verdad independiente, y por eso Check lo contrasta en vez de creerselo.
func TestLibreEsDerivable(t *testing.T) {
	n := hoja()
	for i := 0; i < 10; i++ {
		if err := insertaOrdenado(t, n, clave(i), valor(i, 20)); err != nil {
			t.Fatalf("InsertHoja: %v", err)
		}
		if quiere := uint16(page.HeaderSize + SlotSize*n.NCells()); n.p.Free != quiere {
			t.Fatalf("tras insertar %d: libre=%d, derivado=%d", i, n.p.Free, quiere)
		}
	}
	for n.NCells() > 0 {
		if err := n.Delete(0); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if quiere := uint16(page.HeaderSize + SlotSize*n.NCells()); n.p.Free != quiere {
			t.Fatalf("tras borrar: libre=%d, derivado=%d", n.p.Free, quiere)
		}
	}
	exigeCoherente(t, n)
}

// TestCheckDetectaCorrupcion pone a Check en rojo a mano. Un Check que solo se ha visto en
// verde no ha demostrado nada: es el aparato de verificación de la etapa D, y si no
// detecta un nodo roto, Validate() dará por buenos árboles que no lo son.
func TestCheckDetectaCorrupcion(t *testing.T) {
	// sano construye un nodo correcto de cinco celdas.
	sano := func(t *testing.T) Node {
		t.Helper()
		n := hoja()
		for i := 0; i < 5; i++ {
			if err := insertaOrdenado(t, n, clave(i), valor(i, 40)); err != nil {
				t.Fatalf("InsertHoja: %v", err)
			}
		}
		return n
	}

	casos := []struct {
		nombre  string
		rompe   func(n Node)
		esperar error
	}{
		{
			"slot dentro del espacio libre",
			func(n Node) { n.setSlot(2, n.p.Free) },
			ErrBadNode,
		},
		{
			"slot pasado del final de la pagina",
			func(n Node) { n.setSlot(2, page.Size-2) },
			ErrBadNode,
		},
		{
			"libre desincronizado de nceldas",
			func(n Node) { n.p.Free += SlotSize },
			ErrBadNode,
		},
		{
			"nceldas de mas",
			func(n Node) { n.p.NCells++ },
			ErrBadNode,
		},
		{
			"libre_fin por delante de libre",
			func(n Node) { n.p.FreeEnd = n.p.Free - 2 },
			ErrBadNode,
		},
		{
			"claves desordenadas",
			func(n Node) {
				a, b := n.slot(1), n.slot(3)
				n.setSlot(1, b)
				n.setSlot(3, a)
			},
			ErrBadNode,
		},
		{
			"celda que se sale de la pagina",
			func(n Node) {
				off := n.slot(0)
				binary.LittleEndian.PutUint32(n.p.Body[idx(off)+offValLen:], 4000)
			},
			ErrBadNode,
		},
		{
			"dos celdas solapadas",
			func(n Node) {
				// Dos slots distintos apuntando a la misma celda: los tamaños siguen
				// sumando lo que cabe, así que solo un chequeo de solapamiento lo ve.
				n.setSlot(3, n.slot(4))
			},
			ErrBadNode,
		},
		{
			"tipo que no es de nodo",
			func(n Node) { n.p.Type = page.TypeMeta },
			ErrWrongType,
		},
	}

	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			n := sano(t)
			if err := n.Check(); err != nil {
				t.Fatalf("el nodo de partida ya estaba mal: %v", err)
			}
			c.rompe(n)
			err := n.Check()
			if !errors.Is(err, c.esperar) {
				t.Fatalf("Check = %v, se esperaba %v", err, c.esperar)
			}
			t.Logf("detectado: %v", err)
		})
	}
}
