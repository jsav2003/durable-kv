package node

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/jsav2003/durable-kv/internal/page"
)

// llenaHoja mete entradas de n bytes de valor hasta que la hoja dice ErrNoSpace, y devuelve
// cuántas entraron. Es el estado exacto en el que el árbol pide una división.
func llenaHoja(t *testing.T, h Node, tamValor int) int {
	t.Helper()
	for i := 0; ; i++ {
		err := insertaOrdenado(t, h, clave(i), valor(i, tamValor))
		if errors.Is(err, ErrNoSpace) {
			return i
		}
		if err != nil {
			t.Fatalf("InsertHoja(%d): %v", i, err)
		}
	}
}

// clavesDe saca las claves de un nodo en el orden del directorio.
func clavesDe(n Node) []string {
	ks := make([]string, n.NCells())
	for i := range ks {
		ks[i] = string(n.Key(i))
	}
	return ks
}

// TestDivisionDeHojaConservaTodo es la propiedad básica: la unión de las dos mitades es lo
// que había, en el mismo orden y con los mismos valores.
func TestDivisionDeHojaConservaTodo(t *testing.T) {
	izq := hoja()
	n := llenaHoja(t, izq, 40)

	antes := clavesDe(izq)
	valores := make(map[string]string, n)
	for i := 0; i < izq.NCells(); i++ {
		valores[string(izq.Key(i))] = string(valorDe(t, izq, i))
	}

	der := hoja()
	sep, err := Split(izq, der)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}

	exigeCoherente(t, izq)
	exigeCoherente(t, der)

	if izq.NCells() == 0 || der.NCells() == 0 {
		t.Fatalf("una mitad quedo vacia: izq=%d der=%d", izq.NCells(), der.NCells())
	}

	despues := append(clavesDe(izq), clavesDe(der)...)
	if len(despues) != len(antes) {
		t.Fatalf("habia %d claves y quedan %d", len(antes), len(despues))
	}
	for i := range antes {
		if antes[i] != despues[i] {
			t.Fatalf("la clave %d era %q y ahora es %q", i, antes[i], despues[i])
		}
	}
	for _, m := range []Node{izq, der} {
		for i := 0; i < m.NCells(); i++ {
			k := string(m.Key(i))
			if v := string(valorDe(t, m, i)); v != valores[k] {
				t.Errorf("la clave %q perdio su valor en la division", k)
			}
		}
	}

	// La separadora de una hoja es la primera clave de la mitad derecha: en una hoja no se
	// promociona nada, las claves se quedan las dos veces donde estaban.
	if got := string(der.Key(0)); string(sep) != got {
		t.Errorf("la separadora es %q y la primera clave de la derecha es %q", sep, got)
	}
	if string(izq.Key(izq.NCells()-1)) >= string(sep) {
		t.Errorf("la ultima clave de la izquierda (%q) no es menor que la separadora (%q)",
			izq.Key(izq.NCells()-1), sep)
	}
}

// TestDivisionRepartePorBytesYNoPorIndice es la razón de que puntoDeCorte acumule tamaños.
// La hoja lleva una celda enorme y muchas diminutas: partir por el índice del medio dejaría
// la celda grande sola en una mitad que sigue casi llena.
func TestDivisionRepartePorBytesYNoPorIndice(t *testing.T) {
	izq := hoja()
	if err := insertaOrdenado(t, izq, clave(0), valor(0, 900)); err != nil {
		t.Fatalf("la celda grande: %v", err)
	}
	for i := 1; i <= 40; i++ {
		if err := insertaOrdenado(t, izq, clave(i), valor(i, 4)); err != nil {
			t.Fatalf("la celda %d: %v", i, err)
		}
	}

	der := hoja()
	if _, err := Split(izq, der); err != nil {
		t.Fatalf("Split: %v", err)
	}

	// Por índice el corte caería en la celda 20 y la mitad izquierda se llevaría los 900
	// bytes más veinte celdas; por bytes se lleva la grande y poco más.
	if izq.NCells() > 8 {
		t.Errorf("la mitad izquierda se quedo con %d celdas: el corte fue por indice y no por bytes",
			izq.NCells())
	}
	usadoIzq := page.BodySize - izq.FreeTotal()
	usadoDer := page.BodySize - der.FreeTotal()
	if usadoIzq > 2*usadoDer || usadoDer > 2*usadoIzq {
		t.Errorf("el reparto por bytes quedo desequilibrado: izq=%d der=%d", usadoIzq, usadoDer)
	}
}

// TestDivisionDejaSitioParaLaCeldaNueva es la propiedad de la que depende que Put no se
// quede sin sitio después de dividir: tras partir una hoja llena, la celda que provocó la
// división cabe en la mitad que le toque, sea cual sea.
//
// Es la derivación del tope de 1000 bytes de la sec. 4 vista desde el otro lado, y por eso
// se prueba con entradas del tamaño máximo.
func TestDivisionDejaSitioParaLaCeldaNueva(t *testing.T) {
	// El caso peor para la mitad izquierda: celdas del tamaño máximo, donde una sola celda
	// por encima de la mitad la deja lo más llena posible.
	for _, tam := range []int{MaxEntrySize - LeafCellOverhead - 7, 400, 40, 4} {
		t.Run(fmt.Sprintf("valores de %d bytes", tam), func(t *testing.T) {
			izq := hoja()
			llenaHoja(t, izq, tam)

			der := hoja()
			if _, err := Split(izq, der); err != nil {
				t.Fatalf("Split: %v", err)
			}

			// La celda más grande que la sec. 4 admite, con su slot.
			necesita := MaxEntrySize + SlotSize
			for nombre, m := range map[string]Node{"izquierda": izq, "derecha": der} {
				if libre := m.FreeTotal(); libre < necesita {
					t.Errorf("la mitad %s dejo %d bytes libres y una celda maxima necesita %d",
						nombre, libre, necesita)
				}
			}
		})
	}
}

// TestDivisionDeNodoInternoPromociona comprueba la diferencia que separa las dos divisiones:
// en un nodo interno la clave del corte sube y desaparece de este nivel, mientras que la
// secuencia de hijos se conserva entera.
func TestDivisionDeNodoInternoPromociona(t *testing.T) {
	izq := interno()
	const nc = 30
	for i := 0; i < nc; i++ {
		if err := izq.InsertInterno(i, clave(i), uint64(100+i)); err != nil {
			t.Fatalf("InsertInterno(%d): %v", i, err)
		}
	}
	izq.SetLink(uint64(100 + nc))

	// La secuencia de hijos, con el enlace derecho al final.
	hijosAntes := make([]uint64, 0, nc+1)
	for i := 0; i < nc; i++ {
		c, err := izq.Child(i)
		if err != nil {
			t.Fatalf("Child(%d): %v", i, err)
		}
		hijosAntes = append(hijosAntes, c)
	}
	hijosAntes = append(hijosAntes, izq.Link())
	clavesAntes := clavesDe(izq)

	der := interno()
	sep, err := Split(izq, der)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	exigeCoherente(t, izq)
	exigeCoherente(t, der)

	// Las claves de las dos mitades más la promocionada son las de antes, en orden.
	claves := append(append(clavesDe(izq), string(sep)), clavesDe(der)...)
	if len(claves) != len(clavesAntes) {
		t.Fatalf("habia %d separadoras y quedan %d", len(clavesAntes), len(claves))
	}
	for i := range clavesAntes {
		if claves[i] != clavesAntes[i] {
			t.Fatalf("la separadora %d era %q y ahora es %q", i, clavesAntes[i], claves[i])
		}
	}

	// La promocionada ya no está en ninguna de las dos mitades: si siguiera, la misma clave
	// separaría en dos niveles y el descenso funcionaría igual -- el fallo saldría mucho
	// después.
	for _, m := range []Node{izq, der} {
		if _, hay := m.Search(sep); hay {
			t.Errorf("la separadora %q sigue en una de las mitades", sep)
		}
	}

	// La secuencia de hijos se conserva: izquierda, su enlace, derecha, su enlace.
	hijos := make([]uint64, 0, len(hijosAntes))
	for _, m := range []Node{izq, der} {
		for i := 0; i < m.NCells(); i++ {
			c, err := m.Child(i)
			if err != nil {
				t.Fatalf("Child(%d): %v", i, err)
			}
			hijos = append(hijos, c)
		}
		hijos = append(hijos, m.Link())
	}
	if len(hijos) != len(hijosAntes) {
		t.Fatalf("habia %d hijos y quedan %d: %v", len(hijosAntes), len(hijos), hijos)
	}
	for i := range hijosAntes {
		if hijos[i] != hijosAntes[i] {
			t.Fatalf("el hijo %d era %d y ahora es %d", i, hijosAntes[i], hijos[i])
		}
	}
}

func TestDivisionRechazaLoQueNoPuedeRepartir(t *testing.T) {
	t.Run("tipos distintos", func(t *testing.T) {
		izq := hoja()
		llenaHoja(t, izq, 40)
		if _, err := Split(izq, interno()); !errors.Is(err, ErrWrongType) {
			t.Errorf("se esperaba ErrWrongType y se obtuvo %v", err)
		}
	})

	t.Run("el destino no esta vacio", func(t *testing.T) {
		izq := hoja()
		llenaHoja(t, izq, 40)
		der := hoja()
		if err := insertaOrdenado(t, der, clave(9999), valor(0, 4)); err != nil {
			t.Fatalf("preparando el destino: %v", err)
		}
		if _, err := Split(izq, der); !errors.Is(err, ErrCannotSplit) {
			t.Errorf("se esperaba ErrCannotSplit y se obtuvo %v", err)
		}
	})

	t.Run("una hoja con una sola celda", func(t *testing.T) {
		izq := hoja()
		if err := insertaOrdenado(t, izq, clave(0), valor(0, 4)); err != nil {
			t.Fatalf("InsertHoja: %v", err)
		}
		if _, err := Split(izq, hoja()); !errors.Is(err, ErrCannotSplit) {
			t.Errorf("se esperaba ErrCannotSplit y se obtuvo %v", err)
		}
	})

	t.Run("un nodo interno con dos celdas", func(t *testing.T) {
		izq := interno()
		for i := 0; i < 2; i++ {
			if err := izq.InsertInterno(i, clave(i), uint64(100+i)); err != nil {
				t.Fatalf("InsertInterno: %v", err)
			}
		}
		if _, err := Split(izq, interno()); !errors.Is(err, ErrCannotSplit) {
			t.Errorf("se esperaba ErrCannotSplit y se obtuvo %v", err)
		}
	})
}

// TestSetChild comprueba el setter que hace atómica la propagación de una división, y que no
// se sale de su sitio: cambiar un hijo no puede mover ni un byte de las claves.
func TestSetChild(t *testing.T) {
	n := interno()
	for i := 0; i < 5; i++ {
		if err := n.InsertInterno(i, clave(i), uint64(100+i)); err != nil {
			t.Fatalf("InsertInterno(%d): %v", i, err)
		}
	}
	antes := clavesDe(n)

	if err := n.SetChild(2, 777); err != nil {
		t.Fatalf("SetChild: %v", err)
	}
	if c, err := n.Child(2); err != nil || c != 777 {
		t.Fatalf("Child(2) = %d, %v; se esperaba 777", c, err)
	}
	for i := 0; i < 5; i++ {
		if i == 2 {
			continue
		}
		if c, _ := n.Child(i); c != uint64(100+i) {
			t.Errorf("SetChild(2) cambio tambien el hijo %d", i)
		}
	}
	if despues := clavesDe(n); !equalStrings(antes, despues) {
		t.Errorf("SetChild movio las claves: %v -> %v", antes, despues)
	}
	exigeCoherente(t, n)

	if err := n.SetChild(5, 1); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("SetChild fuera de rango: se esperaba ErrOutOfRange y se obtuvo %v", err)
	}
	if err := hoja().SetChild(0, 1); !errors.Is(err, ErrWrongType) {
		t.Errorf("SetChild sobre una hoja: se esperaba ErrWrongType y se obtuvo %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDivisionSobreviveALaPagina ata la división al formato en disco: las dos mitades tienen
// que pasar por EncodeTo/Decode y leerse igual, como hace TestRoundTripPorLaPagina con la
// inserción.
func TestDivisionSobreviveALaPagina(t *testing.T) {
	izq := hoja()
	llenaHoja(t, izq, 40)
	der := hoja()
	if _, err := Split(izq, der); err != nil {
		t.Fatalf("Split: %v", err)
	}

	for nombre, m := range map[string]Node{"izquierda": izq, "derecha": der} {
		buf := make([]byte, page.Size)
		if err := m.Page().EncodeTo(buf); err != nil {
			t.Fatalf("EncodeTo de la mitad %s: %v", nombre, err)
		}
		p, err := page.Decode(buf, m.Page().ID)
		if err != nil {
			t.Fatalf("Decode de la mitad %s: %v", nombre, err)
		}
		releida := Of(p)
		if err := releida.Check(); err != nil {
			t.Fatalf("la mitad %s no valida tras el viaje: %v", nombre, err)
		}
		if !equalStrings(clavesDe(m), clavesDe(releida)) {
			t.Errorf("la mitad %s cambio de claves al pasar por la pagina", nombre)
		}
		for i := 0; i < m.NCells(); i++ {
			if !bytes.Equal(valorDe(t, m, i), valorDe(t, releida, i)) {
				t.Errorf("la mitad %s cambio el valor %d al pasar por la pagina", nombre, i)
			}
		}
	}
}
