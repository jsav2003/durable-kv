package tree

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/jsav2003/durable-kv/internal/node"
	"github.com/jsav2003/durable-kv/internal/pager"
)

// arbolNuevo crea un árbol vacío sobre un archivo en memoria.
func arbolNuevo(t testing.TB) (*Tree, *pager.Pager) {
	t.Helper()
	pg, err := pager.Create(&memFile{}, &pager.NopLog{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	arbol, err := Crear(pg)
	if err != nil {
		t.Fatalf("Crear: %v", err)
	}
	return arbol, pg
}

// claveN produce claves de longitud fija cuyo orden lexicográfico coincide con el numérico.
// Con "10" < "9", media suite de un B+tree pasa por accidente.
func claveN(i int) []byte { return []byte(fmt.Sprintf("k%09d", i)) }

// valorN es un valor de n bytes derivado de i, para poder comprobar que cada clave conserva
// el suyo y no el de la vecina.
func valorN(i, n int) []byte {
	v := make([]byte, n)
	for j := range v {
		v[j] = byte(i*7 + j)
	}
	return v
}

// valorMaximo es el valor más grande que la sec. 4 admite junto a una clave de claveN.
const valorMaximo = node.MaxEntrySize - node.LeafCellOverhead - 10

// profundidad es la del árbol, medida por el mismo barrido que usa Validate.
func profundidad(t *testing.T, arbol *Tree) int {
	t.Helper()
	r, err := arbol.barrido()
	if err != nil {
		t.Fatalf("barrido: %v", err)
	}
	return r.profundidadHoja
}

// exigeTodas comprueba que las n claves están con su valor.
func exigeTodas(t *testing.T, arbol *Tree, n, tam int) {
	t.Helper()
	for i := 0; i < n; i++ {
		v, err := arbol.Get(claveN(i))
		if err != nil {
			t.Fatalf("Get(%q): %v", claveN(i), err)
		}
		if string(v) != string(valorN(i, tam)) {
			t.Fatalf("Get(%q) devolvio el valor equivocado", claveN(i))
		}
	}
}

func TestPutYGet(t *testing.T) {
	arbol, _ := arbolNuevo(t)
	if err := arbol.Put([]byte("clave"), []byte("valor")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, err := arbol.Get([]byte("clave"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(v) != "valor" {
		t.Errorf("Get devolvio %q", v)
	}
	if err := arbol.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestPutValidaTrasCadaInsercion es la razón de haber escrito Validate antes que Put.
//
// Con entradas del tamaño máximo la hoja se parte cada cuatro inserciones, así que estas 500
// llamadas son más de cien divisiones y varias propagaciones al nivel de arriba. Validar tras
// cada una localiza la primera que rompe algo; validar solo al final diría que el árbol está
// mal sin decir desde cuándo.
func TestPutValidaTrasCadaInsercion(t *testing.T) {
	arbol, _ := arbolNuevo(t)
	const n = 500
	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i, valorMaximo)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		if err := arbol.Validate(); err != nil {
			t.Fatalf("Validate tras el Put %d: %v", i, err)
		}
	}
	exigeTodas(t, arbol, n, valorMaximo)
}

// TestPutHaceCrecerElArbol comprueba que la raíz se divide y el árbol gana niveles: es la
// única operación que cambia la altura, y por tanto la única que puede romper el invariante
// 2 de un golpe.
func TestPutHaceCrecerElArbol(t *testing.T) {
	arbol, _ := arbolNuevo(t)
	raizInicial := arbol.Root()

	// Con entradas del tamaño máximo caben cuatro por hoja y unas doscientas separadoras por
	// nodo interno, así que mil claves bastan para tres niveles.
	const n = 1000
	alturas := map[int]bool{}
	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i, valorMaximo)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
		alturas[profundidad(t, arbol)] = true
	}

	if err := arbol.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if arbol.Root() == raizInicial {
		t.Error("la raiz no cambio: el arbol nunca crecio")
	}
	if h := profundidad(t, arbol); h < 2 {
		t.Errorf("el arbol acabo con altura %d y se esperaban al menos 3 niveles", h)
	}
	if len(alturas) < 3 {
		t.Errorf("el arbol solo paso por las alturas %v", alturas)
	}
	exigeTodas(t, arbol, n, valorMaximo)
}

// TestPutEnOrdenAleatorio: insertar en orden creciente solo ejercita la división por el
// extremo derecho. Con orden aleatorio las divisiones caen en cualquier posición del padre,
// que es donde vive la aritmética de coloca.
func TestPutEnOrdenAleatorio(t *testing.T) {
	arbol, _ := arbolNuevo(t)

	const n = 800
	const semilla = 20260831 // fija: un fallo aqui tiene que poder repetirse
	r := rand.New(rand.NewPCG(semilla, semilla))
	orden := r.Perm(n)

	for j, i := range orden {
		if err := arbol.Put(claveN(i), valorN(i, valorMaximo)); err != nil {
			t.Fatalf("Put(%d) en la posicion %d (semilla %d): %v", i, j, semilla, err)
		}
		if err := arbol.Validate(); err != nil {
			t.Fatalf("Validate tras insertar %d en la posicion %d (semilla %d): %v",
				i, j, semilla, err)
		}
	}
	exigeTodas(t, arbol, n, valorMaximo)

	// Y el recorrido lateral las devuelve ordenadas, que es el invariante 5 visto desde
	// fuera.
	visto := 0
	anterior := ""
	if err := arbol.Scan(nil, nil, func(k, _ []byte) bool {
		if visto > 0 && string(k) <= anterior {
			t.Fatalf("Scan devolvio %q despues de %q", k, anterior)
		}
		anterior = string(k)
		visto++
		return true
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if visto != n {
		t.Errorf("Scan devolvio %d claves de %d", visto, n)
	}
}

// TestPutTamanosVariables mezcla entradas diminutas con entradas casi máximas, que es lo que
// hace que el corte por bytes importe.
func TestPutTamanosVariables(t *testing.T) {
	arbol, _ := arbolNuevo(t)

	const n = 600
	const semilla = 7
	r := rand.New(rand.NewPCG(semilla, semilla))
	tams := make([]int, n)
	for i := range tams {
		tams[i] = r.IntN(valorMaximo + 1)
	}

	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i, tams[i])); err != nil {
			t.Fatalf("Put(%d) con valor de %d bytes: %v", i, tams[i], err)
		}
		if err := arbol.Validate(); err != nil {
			t.Fatalf("Validate tras el Put %d (valor de %d bytes, semilla %d): %v",
				i, tams[i], semilla, err)
		}
	}
	for i := 0; i < n; i++ {
		v, err := arbol.Get(claveN(i))
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if string(v) != string(valorN(i, tams[i])) {
			t.Fatalf("la clave %d perdio su valor", i)
		}
	}
}

// TestPutSustituye: la segunda escritura de una clave reemplaza a la primera y no duplica la
// celda. Se prueba con un valor más grande a propósito, que es el caso en el que sustituir
// puede provocar una división.
func TestPutSustituye(t *testing.T) {
	arbol, _ := arbolNuevo(t)

	const n = 200
	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i, 8)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i+1000, valorMaximo)); err != nil {
			t.Fatalf("Put(%d) de sustitucion: %v", i, err)
		}
		if err := arbol.Validate(); err != nil {
			t.Fatalf("Validate tras sustituir %d: %v", i, err)
		}
	}

	for i := 0; i < n; i++ {
		v, err := arbol.Get(claveN(i))
		if err != nil {
			t.Fatalf("Get(%d): %v", i, err)
		}
		if string(v) != string(valorN(i+1000, valorMaximo)) {
			t.Fatalf("la clave %d conservo el valor viejo", i)
		}
	}

	// Y siguen siendo n claves, no 2n.
	contadas := 0
	if err := arbol.Scan(nil, nil, func(_, _ []byte) bool { contadas++; return true }); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if contadas != n {
		t.Errorf("el arbol tiene %d claves y deberia tener %d", contadas, n)
	}
}

// TestPutRechazaTamanosSinEnvenenarElPager: los límites de la sec. 4 se comprueban antes de
// abrir el grupo de commit. Si se comprobaran dentro, una clave demasiado larga dejaría el
// grupo abierto y el Put siguiente fallaría con ErrGroupOpen sin que el llamador hubiera
// hecho nada mal.
func TestPutRechazaTamanosSinEnvenenarElPager(t *testing.T) {
	arbol, _ := arbolNuevo(t)

	grande := make([]byte, node.MaxKeySize+1)
	if err := arbol.Put(grande, []byte("v")); !errors.Is(err, node.ErrKeyTooLarge) {
		t.Errorf("clave larga: se esperaba ErrKeyTooLarge y se obtuvo %v", err)
	}
	if err := arbol.Put([]byte("k"), make([]byte, node.MaxEntrySize)); !errors.Is(err, node.ErrEntryTooLarge) {
		t.Errorf("entrada larga: se esperaba ErrEntryTooLarge y se obtuvo %v", err)
	}

	if err := arbol.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("el Put valido de despues fallo, el pager quedo envenenado: %v", err)
	}
	if err := arbol.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestPutSobreviveAlArchivo: el árbol construido con Put tiene que bajar a datos.db y volver
// a leerse igual. Sin esto, los demás tests validan páginas que nunca pasaron por el CRC.
func TestPutSobreviveAlArchivo(t *testing.T) {
	f := &memFile{}
	pg, err := pager.Create(f, &pager.NopLog{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	arbol, err := Crear(pg)
	if err != nil {
		t.Fatalf("Crear: %v", err)
	}

	const n = 400
	for i := 0; i < n; i++ {
		if err := arbol.Put(claveN(i), valorN(i, valorMaximo)); err != nil {
			t.Fatalf("Put(%d): %v", i, err)
		}
	}
	if err := pg.FlushDirty(); err != nil {
		t.Fatalf("FlushDirty: %v", err)
	}

	pg2, err := pager.New(f, &pager.NopLog{}, pg.TotalPages())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	relectura := New(pg2, arbol.Root())
	if err := relectura.ReconstruirLibres(); err != nil {
		t.Fatalf("ReconstruirLibres: %v", err)
	}
	if err := relectura.Validate(); err != nil {
		t.Fatalf("el arbol releido no valida: %v", err)
	}
	exigeTodas(t, relectura, n, valorMaximo)
}
