# BUGS

Errores encontrados durante el desarrollo: qué los causaba y cómo se detectaron. Este
archivo se acumula fase a fase; no se reescribe, se añade.

## F0 · Registros de longitud variable

**Ningún error de código detectado.**

Los tests exhaustivos (`TestTruncation`, `TestTruncatedLastOfMultiple`,
`TestBitFlipCorruption`, `TestAbsurdLength`) cubren el espacio de fallo por enumeración
completa, no por muestreo: cada offset de truncamiento del último registro, cada bit de
la cabecera, la carga y el crc, y varios valores absurdos del campo de longitud. El
formato del marco se diseñó explícitamente contra estos casos antes de escribir el
código, y los cuatro pasaron a la primera ejecución.

### Nota sobre el fuzzer nativo (DESIGN.md sec. 9.4)

`go test ./internal/record -fuzz=FuzzReader` falla al arrancar en esta máquina con:

```
fork/exec C:\Users\...\record.test.exe: Una directiva de Control de aplicaciones bloqueó este archivo.
```

Esto ocurre **solo** en modo `-fuzz` (que re-ejecuta el binario de test como proceso
*worker*, con variables de entorno y argumentos internos distintos de una corrida
normal). La ejecución estándar de `go test` sobre el mismo binario -- incluidos los
casos `FuzzReader/seed#0`..`seed#5`, que corren el corpus semilla como tests ordinarios
-- funciona sin problema y está en verde.

Es una directiva de Control de aplicaciones de Windows (Smart App Control / WDAC)
bloqueando ese patrón de re-ejecución en esta máquina concreta, no un defecto del
código ni del diseño del marco. No se intentó modificar la directiva del sistema sin
autorización explícita del usuario.

Consecuencia: la corrida de fuzzing de 60 s que pide la sec. 9.4 no se pudo ejecutar
localmente. Queda pendiente correrla en CI (F6) o en una máquina sin esa restricción,
donde se espera que funcione sin cambios en el código.

## F1 · Página, pager y contrato

*(fase en curso; esta sección se cierra al terminarla)*

### El fuzzer se atasca en la minimización con páginas de tamaño fijo

**No es un error del motor: es un error del aparato de verificación.** Un fuzzer que
parece correr y no corre es peor que uno que falla al arrancar, porque el verde no
significa nada y nadie lo mira dos veces.

**Cómo se manifestó.** `go test ./internal/page -fuzz=FuzzDecode -fuzztime=30s` termina
en `PASS`, pero el contador se congela a los 3 segundos:

```
fuzz: elapsed:  3s, execs: 35729 (11903/sec), new interesting: 0 (total: 7)
fuzz: elapsed:  6s, execs: 35729 (0/sec),     new interesting: 0 (total: 7)
...
fuzz: elapsed: 30s, execs: 35729 (0/sec),     new interesting: 0 (total: 7)
```

Con `-parallel=1` la congelación llega a las **8 ejecuciones**. Treinta segundos de
fuzzing rindieron ocho casos.

**Cómo se detectó.** Comparando con `FuzzReader` de la F0 sobre la misma máquina, que
sostiene 200.000 ejecuciones por segundo sin despeinarse. Descartada la máquina, se
bisecó el objetivo con siete sondas desechables, cada una cambiando una sola cosa:

| Sonda | Qué cambiaba | Resultado |
|---|---|---|
| A | un solo argumento, semillas pequeñas | 37.000/sec |
| B | dos argumentos, semillas pequeñas | 38.000/sec |
| C | dos argumentos, semilla de 4096 bytes a cero | 36.000/sec |
| D | semilla = página **válida**, cuerpo mínimo | **se atasca (2 execs)** |
| E | semilla = página válida, cuerpo completo | **se atasca (2 execs)** |
| F | página válida leída desde otra ranura (nunca decodifica bien) | **se atasca (3 execs)** |
| G | 4096 bytes no-cero con CRC inválido | 26.000/sec |

A y B descartan la firma del objetivo; C descarta el tamaño de la entrada; D y E
descartan el cuerpo del test. F y G aíslan la causa hasta un único predicado: **el
objetivo se atasca si, y solo si, el corpus semilla contiene una página que pasa el
CRC.** En F, `Decode` ni siquiera devuelve una página -- falla después, en el `page_id`
-- y aun así se atasca. Lo que importa no es que decodifique bien, es que cruce el CRC.

**Qué lo causaba.** La minimización, y la pista estaba en que el contador se quedara
clavado en vez de avanzar despacio: **las ejecuciones del minimizador no cuentan como
`execs`**. El worker no estaba colgado, estaba minimizando sin descanso una entrada que
no se puede minimizar.

Cruzar el CRC es cobertura nueva, así que el fuzzer marca la entrada como interesante e
intenta reducirla conservando esa cobertura. Pero la página es de **tamaño fijo**:
cualquier byte que el minimizador quite deja el buffer en 4095 y `Decode` sale por
`ErrBadSize`, una rama distinta. Ninguna entrada reducida conserva jamás la cobertura de
la original, así que el minimizador agota su presupuesto -- por omisión, sin límite útil
-- en cada entrada interesante que encuentra. El formato hace la minimización fútil por
construcción.

`FuzzReader` de la F0 no lo sufre porque sus registros son de longitud variable: quitarle
bytes a un registro produce otro registro, no un error de tamaño.

**Cómo se arregló.** Acotando la minimización, que para este formato no aporta nada:

```
go test ./internal/page -run=XXX -fuzz=FuzzDecode -fuzztime=60s -fuzzminimizetime=2s
```

Con esa bandera, la misma sonda F pasó de 3 ejecuciones a 24.000/sec. La corrida de
verificación de la sec. 9.4 quedó en **7.207.495 ejecuciones en 60 s (~100.000/sec), sin
pánicos ni fallos**. La invocación está documentada en el comentario de `FuzzDecode`,
donde la va a leer quien la ejecute.

**Sin semilla que citar.** No es un fallo con una entrada culpable: es una propiedad del
formato frente a la estrategia de minimización, y se reproduce con cualquier página
válida en el corpus semilla.

### Nota: el bloqueo de App Control de la F0 ya no ocurre

El fallo de `fork/exec ... Una directiva de Control de aplicaciones bloqueó este
archivo` que la F0 documenta más arriba **no se reproduce**. La cadena de Go se
reinstaló desde cero (`winget install GoLang.Go`, go1.26.7 windows/amd64) al empezar la
F1, y desde entonces `-fuzz` levanta sus 8 workers sin problema en la misma máquina.
Queda como muy probable que la directiva afectara al binario de Go anterior y no al
patrón de re-ejecución en sí. La corrida de 60 s que la F0 dejó pendiente para CI se
puede hacer ya en local.
