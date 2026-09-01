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

**Un error, y no en el motor sino en el aparato de verificación.** El formato de página
y el pager no dieron ningún error de código; el fuzzer sí.

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

### El pager: ningún error de código detectado, y cómo se comprobó que eso significa algo

El pager (`internal/pager`) pasó su suite en verde en la primera ejecución. Ese es
justamente el resultado del que hay que desconfiar: un verde a la primera puede querer
decir que el código está bien o que los tests no comprueban nada, y desde fuera se ven
igual. Es el riesgo de la sec. 11 -- *"el arnés de pruebas tiene bugs y los verdes no
significan nada"*.

Se comprobó por mutación: romper a mano cada una de las tres reglas que el pager existe
para imponer, y verificar que el rojo aparece, que aparece en el test que corresponde, y
que no aparece en los demás.

| Mutación | Qué se quitó | Resultado |
|---|---|---|
| A | El `fsync` de extensión antes del commit (sec. 7.6) | `TestExtensionAntesDelCommit` en rojo, con la traza `[wal:imagen, wal:commit, datos:write, datos:sync]` -- el commit confirmando un grupo que usa una página que el archivo aún no cubre |
| B | La comprobación `wal_flushed_lsn < page_lsn` de `writePage` (sec. 7.5) | `TestReglaDeDesalojo` y `TestWriteAheadIrreparable` en rojo; la página bajó a `datos.db` sin sincronizar antes el WAL |
| C | La detección de página obsoleta en `MarkDirty` | `TestPaginaObsoleta` en rojo; ensuciar una copia de una página ya liberada y reciclada pasó sin error |

Cada mutación puso en rojo su test y solo el suyo, y el original quedó restaurado byte a
byte antes de seguir.

**Lo que hizo posible la mutación A** es que los dobles de prueba -- el `File` en memoria y
el `Log` instrumentado -- escriben en una **traza compartida**. Con una traza por
componente, el orden relativo entre `datos.db` y el WAL no se observa, y ese orden
relativo *es* el write-ahead logging: la mutación A habría pasado en verde. Es la misma
razón por la que la sec. 9.1 exige un árbitro de orden global entre los dos archivos en la
F4, aplicada aquí en miniatura.

**Sin semilla que citar:** no hay ningún fallo con una entrada culpable, solo la
comprobación de que los tests pueden ponerse rojos.

## F2 · El B+tree

**Un hueco real, y no en un test que fallara sino en uno que no existía.** El árbol pasó su
suite en verde a la primera, tres veces seguidas: la capa de nodo, el descenso con `Get` y
`Scan`, y `Put` con la división propagada. Como en la F1, ese es el resultado del que hay que
desconfiar. Esta vez la desconfianza encontró algo.

### El hueco: la rama que ninguna inserción alcanzaba

`insertaEnPadre` reparte la separadora que sube entre las dos mitades del padre recién
dividido, y tenía tres casos: el hijo que se partió está en la mitad izquierda, es el de la
celda que `Split` promociona, o está en la mitad derecha. La suite entera —500 inserciones
validando el árbol tras cada una, 800 en orden aleatorio, 600 de tamaños mezclados, 1000 que
hacen crecer la raíz dos veces— pasaba en verde.

`go tool cover` decía que **el caso de en medio no se ejecutaba ni una sola vez**.

No es casualidad. Insertar en orden creciente parte siempre por el extremo derecho, así que
el hijo que se divide es siempre el último. Y el orden aleatorio tampoco basta: que el hijo
que se parte sea justo el de la celda promocionada exige que la posición del corte por bytes
coincida con la posición de ese hijo, y con ocho hijos eso es una de cada ocho divisiones de
padre —que a su vez son una fracción pequeña de las divisiones—. Con 800 claves el suceso
simplemente no cayó.

**Cómo se detectó.** Cobertura, no un test rojo. Es la única de las tres fases en que la
comprobación por mutación no habría bastado por sí sola: una mutación sobre código que ningún
test ejecuta no pone nada en rojo, así que se habría archivado como "mutación equivalente".

**Qué se hizo.** `TestPropagacionEnCadaPosicionDelPadre` monta un nodo interno lleno de
separadoras del tamaño máximo —que se llena con ocho hijos y no con doscientos, y por eso el
test cabe— y parte una hoja en **cada** posición posible, incluida la del enlace derecho.
Recorrer todas las posiciones alcanza las tres ramas sin tener que saber de antemano por
dónde va a cortar `puntoDeCorte`.

**Y con el test delante se vio que la rama era un no-op:** `q` ya valía `p`. El caso de en
medio existía aparentando calcular algo que no calculaba. El `switch` de tres casos se quedó
en un `if`, con el argumento escrito de por qué `p <= m` agrupa dos situaciones que parecen
distintas y no lo son.

**Sin semilla que citar:** no hubo entrada culpable. La entrada que lo revela es el propio
test nuevo.

### Verificación por mutación

Ocho mutaciones sobre el código del árbol, cada una restaurada byte a byte antes de la
siguiente. Las once de `Validate` van aparte, más abajo.

| Mutación | Qué se rompió | Resultado |
|---|---|---|
| D | El `i++` del descenso ante una clave igual a la separadora | `TestGetSobreLaSeparadora` y `TestGetEncuentraTodasLasClaves` en rojo |
| E | `Scan` deja de seguir el enlace lateral | los cinco tests de `Scan` en rojo, ninguno de `Get` |
| F | `particion` sin la detección de fuga (el invariante 6 de la v1 del diseño) | `TestReconstruirLibres` y la fila de la fuga, y solo esas |
| G | El hijo de la celda promocionada se manda a la mitad derecha (`p >= m`) | **solo** la posición 4 de `TestPropagacionEnCadaPosicionDelPadre` |
| H | El desplazamiento a la mitad derecha no descuenta la celda promocionada (`p-m`) | cuatro tests, entre ellos las posiciones 5 a 7 del anterior |
| I | `coloca` no reapunta la celda vieja a la mitad derecha (sin `SetChild`) | el orden aleatorio y la sustitución; **no** el orden creciente |
| J | La hoja nueva no hereda el enlace lateral de la vieja | el orden aleatorio y la sustitución |
| K | `>=` por `>` al elegir la mitad donde va la celda nueva | **nadie**, y es correcto |

La fila G es la que justifica el test nuevo: esa mutación no la ve **ningún otro test de la
suite**. Sin `TestPropagacionEnCadaPosicionDelPadre`, mandar un subárbol entero al padre
equivocado habría pasado en verde.

La fila I explica por qué el orden de inserción es una dimensión del test y no un detalle: en
orden creciente nunca se inserta una separadora en medio del directorio, así que la mitad de
`coloca` no se ejerce nunca.

**La fila K no es un hueco: la mutación es equivalente.** `sep` es una clave que sigue en la
hoja y `key` acaba de salir de ella si es que estaba, así que `key == sep` no puede darse y
las dos comparaciones son la misma función. Se conserva el `>=` porque es la regla del
invariante 4 y no una consecuencia de por dónde se llegó a esa línea, y queda dicho en el
comentario para que nadie lo lea como una rama sin probar.

### Las once mutaciones de `Validate()`

`Validate()` se escribió **antes** que `Put`, a propósito: un oráculo escrito después de la
división hereda sus suposiciones, porque quien acaba de escribirla mira su propio código para
decidir qué comprobar. Para que ese orden valga algo hay que demostrar que el oráculo se pone
rojo.

`TestValidateDetectaCadaRotura` parte del árbol canónico y rompe una cosa por fila: claves
desordenadas dentro de una hoja, una hoja a distinta profundidad, una hoja alcanzable sin
celdas, una clave fuera de la cota de su hoja, la cadena lateral saltándose una hoja, la
cadena acabando antes de tiempo, una página en el árbol y en el conjunto de libres a la vez,
una página en ninguno de los dos, un ciclo hacia un ancestro, una ranura meta colgando del
árbol, y un nodo interno sin hijo derecho.

Cada fila exige **su propio fragmento de mensaje**, distinto del de las demás. Es la mitad
que importa: un `Validate` que devolviera siempre el mismo error genérico pasaría un test que
solo comprobara "devuelve error", y desde fuera se vería igual que uno que funciona.

### Observación medida: el orden de inserción cambia la ocupación, no la corrección

`TestCienMilClaves` mete las mismas 100.000 claves en los dos ordenes y mide el archivo:

| Orden | Páginas | Niveles bajo la raíz |
|---|---|---|
| Creciente | 12.714 | 2 |
| Aleatorio | 9.947 | 2 |

Un 28% más de páginas en el orden creciente. **No es un error.** Es el comportamiento
conocido de un B+tree que parte siempre por la mitad: en orden creciente la división cae
siempre en la hoja de la derecha y la mitad izquierda queda al 50% para siempre, mientras que
en orden aleatorio la ocupación converge hacia el 69% habitual. Se anota porque una cifra así
invita a leerse como un fallo la próxima vez que alguien la vea, y porque la salida conocida
—partir 100/0 cuando la inserción va al extremo derecho— es una optimización de llenado que
la sec. 2 no pide y que este proyecto no va a hacer.

Los dos ordenes dan tres niveles con 100.000 claves, que es lo que la sec. 6 promete de la
aridad.

### `FuzzArbol`: 718.939 ejecuciones, y el contador a cero al final

El fuzzer del árbol no corrompe páginas —eso es `FuzzNode`, un nivel más abajo— sino que
construye árboles con secuencias de `Put` sacadas de la entrada y los contrasta contra un
mapa ordenado: mismos pares por `Get`, mismas claves y en el mismo orden por `Scan`, y
`Validate()` en verde. Es la propiedad de la sec. 9.3 en miniatura, un ciclo antes de la F5.
60 s, 718.939 ejecuciones, 34 entradas nuevas en el corpus, sin fallos.

El valor se genera repitiendo la clave en vez de leerse del flujo, y es deliberado: así un
valor de 800 bytes cuesta **un byte** de entrada y el fuzzer llega a las divisiones con
entradas cortas, que son las que sabe minimizar. Es la lección de la F1 sobre el minimizador
aplicada al revés — en vez de sufrirla, se diseña la entrada para no provocarla.

**Lo que no se pudo explicar:** el ritmo cayó a **0 ejecuciones/s durante los últimos 15
segundos** de la corrida. No es una entrada patológica: el corpus completo de 34 entradas
corre en ~1 s (`go test -run FuzzArbol`), así que no hay ningún caso lento que el motor
tuviera atascado. Queda como comportamiento del arnés en esta máquina, sin más atribución, y
de la misma familia que la nota de la F1 sobre el minimizador en Windows. No afecta al verde:
las 718.939 ejecuciones anteriores sí ocurrieron.

### Deuda de diseño abierta en esta fase

- **D8** — `Validate()` no lee las páginas libres, así que la mitad del invariante 6 que pide
  CRC y `page_id` válidos en **ambos** conjuntos queda para la F3. No es un olvido: en la F2
  una página puede asignarse y liberarse sin llegar nunca a `datos.db`, con su ranura a ceros
  y su CRC inválido sin que nada esté mal. Comprobarlo ahora sería un falso positivo sobre un
  árbol sano, que es la clase de fallo que la sec. 6 declara inaceptable.
- **D9** — un `Put` que falla a medias deja el grupo de commit abierto y envenena el pager.
  No hay `AbortGroup` y no se añade uno: el contrato del pager lo cerró la F1 con cinco
  operaciones, y dentro del grupo ya solo pueden fallar páginas que no son lo que dicen ser.
  La atomicidad **en disco** no depende de ese camino sino del grupo de commit; lo que falta
  es la recuperación en memoria, y esa necesita el WAL.

### Lo que esta fase no prueba

El borrado con redistribución y fusión queda fuera de la F2, que es lo que la tabla de la
sec. 10 pide literalmente. El invariante 3 se comprueba en cada `Validate()`, pero la fusión
no se ejercita nunca, y el conjunto de páginas libres se recorre siempre vacío salvo en
`TestReconstruirLibres`. Todo lo que la F2 demuestra es sobre **un árbol que solo crece**.
