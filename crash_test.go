package motor_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	motor "github.com/jsav2003/motor-almacenamiento"
)

// Este archivo es el criterio de terminación de la F3, tal como lo pide la tabla de la sec.
// 10: un kill -9 y, al reabrir, todas las claves confirmadas.
//
// # Por qué un proceso de verdad
//
// Los tests de internal/recovery simulan la caída soltando el estado sin cerrar, sobre un
// disco en memoria. Eso prueba la lógica de la recuperación, pero no prueba la afirmación
// del proyecto, porque hay una parte que no está bajo el control del programa: si el fsync
// llegó de verdad al disco. Un motor que llamara a fsync sobre el archivo equivocado, o que
// se olvidara de llamarlo, pasaría todos aquellos tests y perdería datos aquí.
//
// Matar un proceso de verdad, sobre archivos de verdad, es lo único que cierra ese hueco. El
// hijo no tiene ocasión de ejecutar nada al morir -- ni defer, ni Close, ni flush -- que es
// exactamente lo que hace un kill -9.
//
// # Lo que se afirma, y lo que no
//
// (b) de la sec. 9.2: **toda clave cuyo Put devolvió OK está presente y con su valor.** Esa
// es la afirmación central del proyecto entero.
//
// (c): toda clave presente pertenece al conjunto de las que se intentaron escribir. Está
// redactado así a propósito y no como "ninguna clave no confirmada aparece", que es falso y
// esperable: una clave cuyo Put se anexó al WAL pero cuyo fsync no había retornado puede
// sobrevivir perfectamente, porque el sistema operativo pudo volcar esos bytes por su
// cuenta. Las confirmadas **deben** estar; las no confirmadas **pueden** estar o no.

// guardaHijo es la variable de entorno que convierte una corrida del binario de tests en el
// proceso que va a morir. Sin ella, TestHijoQueEscribe no hace nada.
const guardaHijo = "MOTOR_TEST_RUTA_DEL_HIJO"

// prefijoOK marca en la salida del hijo cada Put confirmado. Se imprime **después** de que
// Put haya devuelto nil, así que toda línea que el padre llegue a leer corresponde a un dato
// cuyo registro de commit ya pasó por fsync.
const prefijoOK = "ok "

// TestHijoQueEscribe no es un test: es el proceso hijo. Solo hace algo cuando el padre lo
// lanza con la variable de entorno puesta, y entonces no termina nunca -- lo termina el
// padre, matándolo.
func TestHijoQueEscribe(t *testing.T) {
	ruta := os.Getenv(guardaHijo)
	if ruta == "" {
		t.Skip("no es el proceso hijo: lo lanza TestCaidaYReapertura")
	}

	db, err := motor.Open(ruta)
	if err != nil {
		fmt.Println("error al abrir:", err)
		os.Exit(1)
	}
	// Sin defer db.Close(): la gracia es morir sin cerrar. Y aunque lo hubiera, un kill no
	// le daría ocasión de correr.

	for i := 0; ; i++ {
		if err := db.Put(clave(i), valor(i)); err != nil {
			fmt.Println("error en Put:", err)
			os.Exit(1)
		}
		// os.Stdout no está tamponado en Go, así que este Println es una escritura al pipe
		// que ya ocurrió cuando el padre lo lee. Si estuviera tamponado, el padre podría
		// perder confirmaciones al matar y el test sería más débil sin que se notara.
		fmt.Printf("%s%d\n", prefijoOK, i)
	}
}

func TestCaidaYReapertura(t *testing.T) {
	if os.Getenv(guardaHijo) != "" {
		t.Skip("este proceso es el hijo")
	}

	// El directorio no puede ser t.TempDir(): su limpieza corre al terminar el test y en
	// Windows puede chocar con los handles que el hijo aún tuviera abiertos.
	ruta, err := os.MkdirTemp("", "motor-caida-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ruta) })
	base := filepath.Join(ruta, "base")

	// Suficientes confirmaciones para que el árbol se divida varias veces y para cruzar al
	// menos un checkpoint automático: así la mitad vieja de las claves está en datos.db y la
	// nueva solo en el log, y la recuperación tiene que juntar las dos.
	const objetivo = 1500

	cmd := exec.Command(os.Args[0], "-test.run=^TestHijoQueEscribe$")
	cmd.Env = append(os.Environ(), guardaHijo+"="+base)
	salida, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("no se pudo lanzar el hijo: %v", err)
	}

	confirmadas := make(map[int]bool)
	ultima := -1
	sc := bufio.NewScanner(salida)
	for sc.Scan() {
		i, ok := confirmacion(sc.Text())
		if !ok {
			continue
		}
		confirmadas[i] = true
		ultima = i
		if len(confirmadas) >= objetivo {
			break
		}
	}
	if len(confirmadas) < objetivo {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("el hijo solo confirmo %d claves de %d antes de terminar", len(confirmadas), objetivo)
	}

	// El kill. En Windows es TerminateProcess y en Unix un SIGKILL: en los dos casos el
	// proceso muere sin ejecutar nada.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Drenar el pipe **antes** de Wait, y no después. El hijo no se detuvo cuando el padre
	// dejó de leer: siguió confirmando claves hasta el kill, y esas confirmaciones están en
	// el pipe. Y Wait cierra el pipe al ver terminar al proceso, así que drenar después de
	// llamarlo no lee nada -- el padre creería que la última clave confirmada es la 1499 y
	// daría por no intentadas unas cuantas que sí lo fueron.
	for sc.Scan() {
		if i, ok := confirmacion(sc.Text()); ok {
			confirmadas[i] = true
			ultima = max(ultima, i)
		}
	}
	cmd.Wait()
	t.Logf("el hijo confirmo %d claves, la ultima la %d, antes del kill", len(confirmadas), ultima)

	// --- La reapertura ---
	db, err := motor.Open(base)
	if err != nil {
		t.Fatalf("Open tras el kill: %v", err)
	}
	defer db.Close()

	// (a) Los seis invariantes se sostienen.
	if err := db.Validate(); err != nil {
		t.Fatalf("Validate tras la caida: %v", err)
	}

	// (b) La afirmación central: toda clave confirmada está, y con su valor.
	faltan := 0
	for i := range confirmadas {
		got, err := db.Get(clave(i))
		if err != nil {
			if faltan < 5 {
				t.Errorf("la clave %d estaba confirmada y no esta: %v", i, err)
			}
			faltan++
			continue
		}
		if !bytes.Equal(got, valor(i)) {
			t.Errorf("la clave %d confirmada vale %q, quiero %q", i, got, valor(i))
		}
	}
	if faltan > 0 {
		t.Fatalf("%d claves confirmadas se perdieron en la caida", faltan)
	}

	// (c) Ninguna clave que jamás se intentó escribir. El hijo escribe 0, 1, 2... en orden,
	// así que como mucho intentó la siguiente a la última confirmada.
	presentes := 0
	err = db.Scan(nil, nil, func(k, _ []byte) bool {
		presentes++
		i, ok := confirmacion(prefijoOK + strings.TrimPrefix(string(k), "clave-"))
		if !ok || i > ultima+1 {
			t.Errorf("aparecio la clave %q, que nunca se intento escribir", k)
			return false
		}
		return true
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if presentes < len(confirmadas) {
		t.Errorf("el Scan devolvio %d claves y hay %d confirmadas", presentes, len(confirmadas))
	}

	// Y la base sigue viva: se puede seguir escribiendo sobre lo recuperado.
	if err := db.Put(clave(ultima+100), valor(ultima+100)); err != nil {
		t.Errorf("Put tras la recuperacion: %v", err)
	}
}

// confirmacion extrae el número de una línea "ok N" del hijo.
func confirmacion(linea string) (int, bool) {
	resto, ok := strings.CutPrefix(linea, prefijoOK)
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(strings.TrimSpace(resto))
	if err != nil {
		return 0, false
	}
	return i, true
}
