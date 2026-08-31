package tree

import (
	"errors"
	"testing"
)

// recoge acumula lo que Scan entrega, copiando: k y v son subsectores de la página y solo
// valen mientras dura la llamada.
func recoge(t *testing.T, arbol *Tree, ini, fin []byte) []string {
	t.Helper()
	var salida []string
	if err := arbol.Scan(ini, fin, func(k, v []byte) bool {
		salida = append(salida, string(k)+"="+string(v))
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return salida
}

func TestGetEncuentraTodasLasClaves(t *testing.T) {
	k := arbolCanonico(t)
	for _, clave := range clavesCanonicas {
		v, err := k.arbol.Get([]byte(clave))
		if err != nil {
			t.Fatalf("Get(%q): %v", clave, err)
		}
		if string(v) != valorDe(clave) {
			t.Errorf("Get(%q) = %q, se esperaba %q", clave, v, valorDe(clave))
		}
	}
}

// TestGetSobreLaSeparadora es el caso que el invariante 4 decide: una clave igual a la
// separadora vive en el subárbol derecho. Las cuatro separadoras del árbol canónico son
// además claves reales, así que un descenso que bajara por la izquierda ante la igualdad
// devolvería ErrNotFound sobre las cuatro.
func TestGetSobreLaSeparadora(t *testing.T) {
	k := arbolCanonico(t)
	for _, sep := range []string{"k030", "k050", "k070", "k090"} {
		v, err := k.arbol.Get([]byte(sep))
		if err != nil {
			t.Fatalf("Get(%q) sobre una separadora: %v", sep, err)
		}
		if string(v) != valorDe(sep) {
			t.Errorf("Get(%q) = %q, se esperaba %q", sep, v, valorDe(sep))
		}
	}
}

func TestGetClaveAusente(t *testing.T) {
	k := arbolCanonico(t)
	// Una por zona: antes de la primera, entre dos hojas, dentro de una hoja, después de la
	// última, y la clave vacía.
	for _, clave := range []string{"", "k005", "k045", "k065", "k110"} {
		if _, err := k.arbol.Get([]byte(clave)); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q): se esperaba ErrNotFound y se obtuvo %v", clave, err)
		}
	}
}

// TestGetDevuelveCopia comprueba D6 desde el lado del llamador: si Get entregara el
// subsector de la página, escribir en el resultado corrompería el árbol en silencio.
func TestGetDevuelveCopia(t *testing.T) {
	k := arbolCanonico(t)
	v, err := k.arbol.Get([]byte("k010"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for i := range v {
		v[i] = 'X'
	}
	otra, err := k.arbol.Get([]byte("k010"))
	if err != nil {
		t.Fatalf("Get de nuevo: %v", err)
	}
	if string(otra) != valorDe("k010") {
		t.Errorf("escribir en el valor devuelto cambio el arbol: ahora vale %q", otra)
	}
}

func TestScanRecorreTodoEnOrden(t *testing.T) {
	k := arbolCanonico(t)
	visto := recoge(t, k.arbol, nil, nil)
	if len(visto) != len(clavesCanonicas) {
		t.Fatalf("Scan dio %d entradas y el arbol tiene %d: %v",
			len(visto), len(clavesCanonicas), visto)
	}
	for i, clave := range clavesCanonicas {
		if quiere := clave + "=" + valorDe(clave); visto[i] != quiere {
			t.Errorf("entrada %d: %q, se esperaba %q", i, visto[i], quiere)
		}
	}
}

// TestScanRango comprueba que el rango es semiabierto y que las cotas no tienen por qué ser
// claves existentes.
func TestScanRango(t *testing.T) {
	k := arbolCanonico(t)
	casos := []struct {
		nombre   string
		ini, fin string
		quiere   []string
	}{
		{"dentro de una hoja", "k030", "k040", []string{"k030"}},
		{"cruza dos hojas", "k030", "k070", []string{"k030", "k040", "k050", "k060"}},
		{"cotas que no existen", "k035", "k075", []string{"k040", "k050", "k060", "k070"}},
		{"sin cota superior", "k090", "", []string{"k090", "k100"}},
		{"por encima de la ultima", "k101", "", nil},
		{"rango vacio", "k050", "k050", nil},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			var fin []byte
			if c.fin != "" {
				fin = []byte(c.fin)
			}
			visto := recoge(t, k.arbol, []byte(c.ini), fin)
			if len(visto) != len(c.quiere) {
				t.Fatalf("Scan(%q, %q) dio %v, se esperaban %v", c.ini, c.fin, visto, c.quiere)
			}
			for i, clave := range c.quiere {
				if quiere := clave + "=" + valorDe(clave); visto[i] != quiere {
					t.Errorf("entrada %d: %q, se esperaba %q", i, visto[i], quiere)
				}
			}
		})
	}
}

// TestScanParaCuandoLoPideFn: devolver false es fin de trabajo, no fallo.
func TestScanParaCuandoLoPideFn(t *testing.T) {
	k := arbolCanonico(t)
	var visto []string
	err := k.arbol.Scan(nil, nil, func(kk, _ []byte) bool {
		visto = append(visto, string(kk))
		return len(visto) < 3
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(visto) != 3 {
		t.Fatalf("Scan siguio tras el false: %v", visto)
	}
}

// TestScanCruzaLasCincoHojas comprueba que el recorrido usa el enlace lateral y no vuelve a
// subir: las diez claves viven en cinco páginas distintas, así que verlas todas seguidas
// exige que la cadena funcione.
func TestScanCruzaLasCincoHojas(t *testing.T) {
	k := arbolCanonico(t)
	// La segunda mitad empieza en L4, a la que solo se llega por el enlace de L3 si se
	// arranca antes de k070.
	visto := recoge(t, k.arbol, []byte("k060"), nil)
	quiere := []string{"k060", "k070", "k080", "k090", "k100"}
	if len(visto) != len(quiere) {
		t.Fatalf("Scan dio %v, se esperaban %v", visto, quiere)
	}
}

// TestScanNoSeCuelgaConLaCadenaEnCiclo comprueba el cortafuegos: un enlace hacia atrás no
// puede convertir el recorrido en infinito, porque un test colgado no dice qué pasó.
func TestScanNoSeCuelgaConLaCadenaEnCiclo(t *testing.T) {
	k := arbolCanonico(t)
	k.c.nodoDe(k.l5).SetLink(k.l1)

	err := k.arbol.Scan(nil, nil, func(_, _ []byte) bool { return true })
	if err == nil {
		t.Fatal("Scan deberia haber detectado que la cadena no termina")
	}
	if !errors.Is(err, ErrBadTree) {
		t.Errorf("se esperaba ErrBadTree y se obtuvo %v", err)
	}
}

// TestDescensoNoSeCuelgaConUnCiclo es el mismo cortafuegos en el camino del descenso.
func TestDescensoNoSeCuelgaConUnCiclo(t *testing.T) {
	k := arbolCanonico(t)
	// La raíz baja por su primer hijo hacia sí misma.
	n := k.c.nodoDe(k.raiz)
	if err := n.Delete(0); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := n.InsertInterno(0, []byte("k070"), k.raiz); err != nil {
		t.Fatalf("InsertInterno: %v", err)
	}

	if _, err := k.arbol.Get([]byte("k010")); !errors.Is(err, ErrBadTree) {
		t.Errorf("se esperaba ErrBadTree por el ciclo y se obtuvo %v", err)
	}
}
