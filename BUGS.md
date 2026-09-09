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

## F3 · WAL y recuperación

Cero errores de código del motor detectados en esta fase, y como en la F1 eso solo significa
algo si se dice cómo se comprobó. Lo que sí apareció fueron **cuatro errores en el aparato
de verificación**: tres tests que pasaban sin probar lo que decían y un bug real en el arnés
de la prueba de caída. La sec. 11 lo dice con todas las letras —"el arnés de pruebas tiene
bugs y los verdes no significan nada"— y en esta fase esa fila del riesgo se cobró todo lo
que se cobró.

El método fue el mismo de la F2: **verificación por mutación**. Se rompe el motor a propósito
de una forma concreta y se exige que un test se ponga en rojo. Un test que sigue en verde con
el motor roto no es un test.

### Las ocho mutaciones, y las cuatro que no se cazaron a la primera

| # | Mutación | ¿La cazó un test? |
|---|---|---|
| 1 | El checkpoint rota el log antes de bajar las páginas sucias | sí, orden de la traza |
| 2 | La meta no alterna de ranura entre checkpoints | sí |
| 3 | La meta guarda la época actual en vez de la N+1 | sí |
| 4 | La recuperación toma el `root_id` de la meta y no del último commit | sí, cinco tests |
| 5 | La recuperación se salta el checkpoint del paso 10 | sí, cinco tests |
| 6 | La recuperación no extiende el archivo (paso 5) | **no** |
| 7 | La recuperación no barre las generaciones huérfanas | **no** |
| 8 | `Get` no copia el valor antes de devolverlo (D6) | **no** |

Y una novena, aparte, que se comenta en su propia sección: quitarle el `fsync` al commit del
WAL.

### El test de la extensión (mutación 6): el disco falso no puede tener agujeros

El test truncaba `datos.db` a dos páginas y exigía
que tras recuperar el archivo midiera lo que `total_pages` promete. Pasaba con y sin el paso
5, por dos razones que se suman:

1. El `WriteAt` del disco en memoria **rellena de ceros** al escribir más allá del final. No
   puede producir un archivo disperso, así que el contenido resultante es el mismo con
   extensión y sin ella. Un disco real no se comporta así, y ese es justo el fallo que el
   paso 5 existe para evitar.
2. Sin checkpoint por medio, **toda** página asignada tiene su imagen en el log, así que el
   paso 6 acaba escribiendo exactamente las mismas páginas que habría escrito el paso 5.

Se reconstruyó sobre el caso que el paso 5 existe para cubrir, y que es el de la sec. 7.6:
un registro de commit que sube `total_pages` **sin traer ninguna imagen**, porque crecer el
archivo es una operación de metadatos que ninguna imagen describe. Ahí sí hay páginas que
solo el paso 5 puede materializar, y sin él el archivo se queda corto de forma observable.
Es `TestElArchivoSeExtiendeHastaElTotalPagesConfirmado`. Con la mutación puesta:

```
datos.db mide 12288 bytes y total_pages es 13: quedan 10 paginas sin materializar,
que en disco real son un agujero disperso
```

El segundo test que quedó, `TestLasPaginasDelHuecoSeEscribenAntesDeAplicarImagenes`, afirma
lo otro que el paso 5 promete y que sí se puede observar en memoria: que las páginas del
hueco se escriben **explícitamente** y se sincronizan antes de aplicar ninguna imagen.

### El test de la generación huérfana (mutación 7): la huérfana es la vieja, no la nueva

El test creaba una generación `N+1` y comprobaba que la recuperación la limpiaba. No limpiaba
nada, porque esa generación no sobra: es a la que la recuperación va a rotar.

Una caída en mitad de una rotación deja siempre la **anterior** sin borrar. `Rotar` crea la
`N+1`, hace `fsync` del directorio y solo entonces borra la `N`; la meta, escrita antes de
rotar, ya apunta a la `N+1`. Así que el estado a reproducir es "existen la `N` y la `N+1`, y
la meta dice `N+1`". Con la huérfana puesta en el sitio correcto, quitar el barrido pone el
test en rojo.

### El test de D6 (mutación 8): hace falta forzar una compactación

`TestGetDevuelveUnaCopia` insertaba claves nuevas después del `Get` y comprobaba que el valor
devuelto no cambiaba. Pasaba igual sin la copia, porque **ninguna de esas inserciones toca los
bytes de una celda ya escrita**: las claves nuevas caían en otras hojas, y una sustitución
tampoco reescribe la celda en su sitio —`internal/tree` la borra y la vuelve a insertar—.

Lo que sí mueve esos bytes es `node.Compactar`, que reordena todas las celdas hacia el final
del cuerpo y pone a cero lo que queda libre. Se fuerza sustituyendo **la misma clave** una y
otra vez sobre una hoja con poco espacio contiguo: cada sustitución deja una celda muerta,
hasta que la siguiente inserción tiene que compactar para hacer sitio. Con el test así y sin
la copia:

```
el valor devuelto por Get cambio bajo los pies del llamador: "tttttttttttttttt"...,
era "bbbbbbbbbbbbbbbb"...
```

El valor del llamador no se corrompió: se convirtió en el de **otra clave**, que es
exactamente el modo de fallo que D6 describe. La página es coherente, el CRC es correcto, y
el error está en el pasado del llamador.

### El bug del arnés: drenar el pipe después de `cmd.Wait()`

Este no es un test flojo, es un error de verdad en el arnés, y del tipo que la sec. 11 llama
más grave que un error en el motor.

`TestCaidaYReapertura` lanza un hijo que inserta claves e imprime una línea por cada `Put`
confirmado; el padre lee 1500 confirmaciones, mata al hijo, y exige que todas estén al
reabrir. El padre drenaba lo que quedara en el pipe **después** de `cmd.Wait()`.

`Wait` cierra el pipe de salida al ver terminar al proceso. Así que el drenaje no leía nada,
y el padre creía que la última clave confirmada era la 1499 — cuando el hijo, que no se
detuvo al dejar el padre de leer, había seguido confirmando hasta el `kill`. El efecto:
claves realmente confirmadas quedaban fuera del conjunto que el criterio (b) comprueba, y
—peor— caían fuera del rango que el criterio (c) considera "intentadas", así que su presencia
legítima se contaba como el fallo *"apareció una clave que nunca se intentó escribir"*.

Se detectó por accidente, y ese accidente es el interesante: la mutación de quitarle el
`fsync` al commit puso el test en rojo, pero **por el motivo equivocado** —falló (c), no (b)—.
Sin el `fsync` el hijo corría mucho más rápido y confirmaba muchas más claves entre el
`break` y el `kill`, lo que hacía el desajuste grande y visible. Con el `fsync` puesto el
desajuste era de una o dos claves y no llegaba a fallar nunca.

Arreglado drenando antes de `Wait`. La consecuencia se ve en la salida: el padre pasa de
registrar 1500 confirmaciones a registrar entre 1507 y 1510.

### `kill -9` no puede distinguir si hubo `fsync`

Con el arnés ya arreglado, la mutación de quitarle el `fsync` al `Commit` del WAL **pasa la
prueba de caída**, cinco corridas de cinco:

```
=== M: Commit sin fsync, 5 corridas ===
--- PASS: TestCaidaYReapertura (0.58s)
--- PASS: TestCaidaYReapertura (0.15s)
...
```

No es un fallo del test: es el límite de lo que un `kill -9` puede probar, y conviene tenerlo
escrito porque el criterio de terminación de la F3 es literalmente ese `kill -9`.

Matar un proceso no vacía ni invalida el caché de páginas del sistema operativo. Los bytes
que el motor escribió con `write()` siguen ahí y el sistema los lleva al disco por su cuenta,
haya habido `fsync` o no. Lo que un `kill -9` prueba es la **frontera del proceso**: que el
motor no depende de nada que viva en su memoria, que no hay `defer` ni `Close` ni buffer
propio del que dependa la durabilidad. Eso no es poco, y ningún test con disco en memoria lo
prueba. Pero la **frontera del disco** —que el `fsync` sirva de algo, que el disco no
reordene ni descarte ni desgarre— no la toca.

Distinguirlas hace falta un disco falso con árbitro de orden global, que es la sec. 9.1 y por
tanto la F4. Entretanto, lo que sí se puede afirmar en la F3 es el **orden de las llamadas**,
y eso lo ve la traza compartida: `TestPutNoDevuelveSinElFsyncDelLog` exige que ningún `Put`
devuelva `nil` sin que su registro de commit haya pasado por el `fsync` del log, y que nada
haya bajado a `datos.db` por el camino. Es la mitad de la sec. 7.2 que se puede probar sin
inyección de fallos.

### La contradicción del invariante 6 con la sec. 7.6

Al cerrar D8 —comprobar CRC y `page_id` también en las páginas **libres**, que es la mitad
del invariante 6 que la F2 dejó abierta— la recuperación se puso en rojo sobre una base
perfectamente sana:

```
Recuperar: tree: el arbol no cumple sus invariantes: la pagina libre 3 no es legible:
page: crc invalido
```

No es un bug del motor. El invariante 6 de la sec. 6 dice que *toda* página de ambos
conjuntos tiene CRC y `page_id` válidos, y la sec. 7.6 con el paso 5 de la sec. 8 mandan
materializar la extensión del archivo con páginas cero **explícitas**. Una ranura así está en
el conjunto de libres y un CRC de ceros es inválido: el diseño manda crear páginas que su
propio invariante declara imposibles.

Está anotado como **D11** en `docs/DEUDA-DISENO.md`, con las tres salidas y sin decidir: es
una decisión del diseño, no de la implementación. Mientras tanto `comprobarLibres` acepta una
página libre que sea íntegra **o** enteramente cero, y esa decisión está aislada en una sola
función.

### `FuzzLeer`: 168.757 ejecuciones, y el mismo contador a cero que en la F2

El fuzzer del lector del WAL cubre lo que la sec. 9.4 pide para "el lector de registros del
WAL". No lo cubría `FuzzRecord`, que existe desde la F0: aquel ejercita el **marco**
—longitud, CRC, delimitación— y este lo que se hace con una carga que el marco ya dio por
buena. Un registro con CRC correcto puede llevar dentro una página con un tipo imposible, un
`page_id` que no es el suyo, o una longitud que no corresponde a su tipo, y esos caminos solo
se alcanzan desde aquí.

El corpus se siembra con logs bien formados además de con basura, porque un fuzzer que
arrancara solo de bytes aleatorios casi nunca produciría una cabecera de registro válida y
nunca llegaría a la parte que el fuzzer existe para probar.

45 s, 168.757 ejecuciones, 3 entradas nuevas, sin fallos. **Y el mismo comportamiento que
BUGS.md ya anotó para `FuzzArbol` en la F2:** el ritmo cayó a 0 ejecuciones/s a los 6
segundos y se quedó ahí el resto de la corrida. Se comprobó lo mismo que entonces y con el
mismo resultado: el corpus completo se replica en menos de un segundo, así que no hay ninguna
entrada patológica en la que el motor se atascara. Que ocurra ahora en un fuzzer distinto,
sobre código distinto, refuerza la atribución al arnés en esta máquina y descarta que fuera
algo del árbol.

### `-race` no se pudo correr

`go test -race` necesita cgo y en esta máquina no hay compilador de C:

```
cgo: C compiler "gcc" not found: exec: "gcc": executable file not found in %PATH%
```

El valor que tendría aquí es limitado —la sec. 2 declara un solo hilo escritor y
`NO-GOALS.md` fija la concurrencia como exclusión permanente—, pero queda anotado para el CI
de la F6, que corre en Linux y sí lo tiene.

### Lo que esta fase no prueba

- **Que el `fsync` sirva de algo.** Ver más arriba. La F3 prueba que se llama y en qué orden;
  que el disco lo respete es la F4.
- **Escrituras desgarradas.** Ninguna. Todas las corrupciones que se prueban son de un bit o
  de una página entera, provocadas a mano y en un punto elegido. El desgarro real —media
  página vieja y media nueva— es de la F4.
- **Caídas en puntos arbitrarios.** Los tests de recuperación construyen estados de caída
  concretos, elegidos por lo que se quiere probar. Las 500 caídas sistemáticas con semilla
  reproducible son la F4.
- **Borrado.** Sigue sin existir, así que el conjunto de páginas libres solo se llena por
  extensiones del archivo y nunca por una fusión. Todo lo que la F3 demuestra sigue siendo
  sobre **un árbol que solo crece**.
- **Dos generaciones del WAL con contenido.** La recuperación sabe encadenarlas y hay un test
  de que las barre, pero el único camino que crea dos generaciones a la vez es una caída en
  mitad de una rotación, y ese punto de caída exacto no se provoca hasta la F4.

## F4 · Inyección de fallos

Cero errores de código del motor detectados en esta fase. Como en la F1 y la F3, eso solo
significa algo si se dice cómo se comprobó, y la comprobación aquí tiene una capa más: antes
de creerle nada al barrido de 500 puntos hay que creerle al **disco falso** que lo sostiene,
que es código nuevo escrito en esta fase. La sec. 9.1 lo dice sin rodeos —*"un error en el
aparato de verificación es más grave que un error en el motor, porque hace que los verdes no
signifiquen nada"*— así que la mayor parte del trabajo de la F4 fue sobre el aparato.

El método es el de siempre: **verificación por mutación**. Se rompe algo a propósito y se
exige un rojo. Aquí las mutaciones son de dos clases: las del motor, que el barrido debe
cazar, y las del propio disco falso, que dicen si el barrido tiene dientes.

### Qué modela el disco falso, y qué no

`fsxtest` con `Volatil` en true mantiene las escrituras sin sincronizar en una cola
**global** —una sola para `datos.db` y el WAL, etiquetada por archivo— y no las hace
duraderas hasta que un `Sync` del archivo correspondiente las vuelca. Al llegar al `WriteAt`
número `CaeEn`:

1. descarta al azar una parte de las pendientes (las que el sistema aún no había llevado al
   plato);
2. reordena las que sí aplica (el disco no promete orden sin un `fsync` de por medio, y esos
   ya no están en la cola);
3. parte una de ellas en una frontera de sector de 512 bytes —la escritura desgarrada.

Todo el azar sale de un `rand` sembrado con `Semilla`. El punto de caída N usa
`semillaBase+N`, así que cada fila de la tabla es reproducible por separado y la tabla entera
desde una sola constante. Un rojo cita `caída en escritura N, semilla semillaBase+N`.

**Lo que el disco falso no modela**, y que por tanto la F4 tampoco prueba:

- **La creación de un archivo no es una operación con caché.** `Open` crea el archivo en el
  acto y de forma duradera; no hay un estado "el archivo existe pero el `fsync` del
  directorio aún no". Una caída entre el `Open` de la generación nueva del WAL y el
  `dir.Sync()` de la rotación no se provoca. El código de `Rotar` ordena las dos cosas y hay
  un test de la F3 de que las ordena, pero el barrido no lo ejercita.
- **El disco no corrompe un sector ya escrito.** Solo descarta, reordena y desgarra
  escrituras **sin sincronizar**. Un bit que se voltea en un sector que ya estaba en el plato
  —degradación del medio— es otra clase de fallo, y esa la cubren los tests de corrupción de
  un bit de la F1 y la F3, no esta.

### Las mutaciones del motor que el barrido caza

Cada una se aplicó sola, se corrió `TestQuinientosPuntosDeCaida`, y se restauró.

| Mutación | Qué se rompió | Puntos en rojo (de 500) | Criterio |
|---|---|---|---|
| M1 | `WAL.Commit` no hace `fsync` del registro de commit | 206 | (b) |
| M2 | El checkpoint no hace `fsync` de `datos.db` (paso 4 de la sec. 7.4) | 89 | (b) |
| M3 | La recuperación no aplica las imágenes de los grupos completos (paso 6 de la sec. 8) | 205 | (b) |

Ninguna llega a 500, y el motivo es el mismo que la F3 anotó para el `kill -9`: **un punto
de caída donde lo pendiente relevante ya estaba sincronizado no distingue el motor sano del
roto.** M2 solo muerde cuando la caída cae dentro de un checkpoint o justo después; M3, solo
cuando hay imágenes que reaplicar y no las tapó ya un checkpoint anterior. Que M2 y M3
muerdan a la vez es la prueba de que el barrido **sí** mete puntos de caída dentro de un
checkpoint y de una rotación del WAL, y no solo en el camino tranquilo de un `Put`.

El criterio (b) —*toda clave cuyo `Put` devolvió OK está y con su valor*— es el que se pone
en rojo en los tres casos. Es la afirmación central del proyecto, y es la que había que ver
fallar.

### La fuerza del barrido vive entera en `cae()`

Mutación **H1**: `cae()` aplica **todas** las pendientes, intactas y en orden —una caída
limpia, sin descarte ni reordenamiento ni desgarro. Resultado: **los 500 puntos en verde**,
con M1, M2 y M3 puestas o sin poner.

O sea: un barrido de 500 puntos sobre un `cae()` que no rompe nada no prueba absolutamente
nada más que lo que ya probaba el `kill -9` de la F3. El verde de `TestQuinientosPuntosDeCaida`
solo vale lo que valga la fidelidad de `cae()`, y por eso `cae()` tiene sus propios tests
—`TestElDesgarroEsEnFronteraDeSector`, `TestLoSincronizadoSobreviveALaCaida`,
`TestCaidaEsReproducibleConLaMismaSemilla`— que comprueban que descarta, que desgarra en
frontera de sector, que respeta lo que pasó por `fsync`, y que la semilla lo hace
reproducible.

### La regla de desalojo (sec. 7.5): el barrido no la puede ejercer

Mutación **M-WA**: quitar de `pager.writePage` la comprobación `wal_flushed_lsn < page_lsn`
que impide bajar una página sucia a `datos.db` antes de sincronizar su registro de log.
Resultado: **los 500 puntos en verde.**

No es que la mutación sea equivalente —lo era la fila K de la F2—, es que **este camino no
la alcanza**. `writePage` solo se llama desde `FlushDirty`, es decir en un checkpoint, y para
entonces todo `Put` ya sincronizó su commit: `FlushedLSN` va siempre por delante de
cualquier `page_lsn`. El motor, tal como está construido, nunca desaloja una página sucia con
su WAL sin sincronizar, así que el barrido no tiene forma de crear la precondición.

Quien la crea a mano es el pager: `TestReglaDeDesalojo` y `TestWriteAheadIrreparable`
construyen el estado —página sucia, LSN sin sincronizar— y exigen `ErrWriteAhead`. Con M-WA
puesta, esos dos tests se ponen en rojo de inmediato. La regla está viva y probada; lo que
la F4 añade es la constancia de que el árbitro de orden global del disco falso **existiría**
para cazarla el día que un caché acotado abra ese camino (docs/DEUDA-DISENO.md, D4).

### D11: el barrido con desgarro real no toca la franja estrecha

`comprobarLibres` acepta hoy una página libre que sea íntegra **o** enteramente cero, porque
la extensión del archivo (sec. 7.6) materializa ranuras a ceros que ningún CRC valida —la
contradicción del invariante 6 consigo mismo que quedó anotada como **D11**, sin decidir.

Mutación **D11-estricto**: quitar la aceptación de la ranura a ceros, dejando solo
`page.Decode`. Resultado con el desgarro real de la F4 en marcha: **los 500 puntos en
verde.**

Es una pieza de evidencia para la decisión que sigue siendo tuya, no la decisión. Dice que
la "franja estrecha" que D11 describe —una escritura desgarrada que deje una ranura libre
entera a ceros, indistinguible de una extensión legítima— **no la produce este barrido**: en
140 claves con siete checkpoints, ningún punto de caída deja una página libre a ceros que
llegue a `comprobarLibres`. La franja es estrecha de verdad. Las tres salidas de D11 siguen
sobre la mesa con el mismo coste que antes; esto solo acota lo que está en juego.

### Lo que esta fase no prueba

- **Borrado.** Sigue sin existir. El conjunto de páginas libres solo se llena por extensiones
  del archivo, nunca por una fusión, así que —igual que en la F2 y la F3— todo lo que la F4
  demuestra es sobre **un árbol que solo crece**. La partición del invariante 6 se comprueba
  en cada `Validate()` del criterio (a), pero sobre un conjunto de libres que casi siempre
  está vacío.
- **La caída durante la creación de un archivo.** Ver arriba: el disco falso hace la creación
  duradera en el acto.
- **La degradación del medio.** El disco falso no voltea bits en sectores ya escritos.
- **Cargas concurrentes.** `NO-GOALS.md` fija un solo hilo escritor como exclusión
  permanente; no hay nada que probar aquí, pero conviene decir que el barrido es
  estrictamente secuencial.
- **Puntos de caída más allá del 500.** La carga limpia emite 661 escrituras; se prueban los
  primeros 500 puntos, que es lo que pide la tabla de la sec. 10. Los 161 restantes caen en
  la parte final de la carga, donde no hay ninguna estructura que los 500 anteriores no
  hayan ejercido ya.

---

## F5 · Pruebas basadas en propiedades

Criterio de la sec. 10 cumplido: `TestPropiedadesContraElModelo` corre **cinco semillas de
3000 operaciones** contra un `map[string]string`, y `TestPropiedadesSobreDiscoReal` repite
una de ellas sobre un directorio de verdad. Salen unas 1500 inserciones y 700 sustituciones
por corrida, 60 reaperturas, y un árbol de 800 claves al final.

La fase encontró **un error, y de los caros**: pérdida silenciosa de datos confirmados.

### El error: el empate de LSN entre las dos ranuras meta

Reducido a siete operaciones y ninguna caída (`TestElCicloVacioNoPierdeLoConfirmado`):

```
1. base nueva          gana ranura=0   epoca=1   archivos=[datos.db datos.wal.1]
2. tras Close          gana ranura=1   epoca=2   archivos=[datos.db datos.wal.3]
3. tras Close vacío    gana ranura=0   epoca=3   archivos=[datos.db datos.wal.4]  <- miente
4. Put("confirmada") -> nil
5. reabrir             gana ranura=0   epoca=3   grupos=0
   -> "tree: la clave no esta en el arbol"
```

**Qué lo causaba.** La sec. 5.2 dice *"se elige la válida con el LSN más alto"*, y `meta.Leer`
lo implementaba con `m.LSN > mejor.LSN`. Pero **dos metas pueden tener el mismo LSN**: el de
la meta es el del último checkpoint, así que dos checkpoints sin ningún commit por medio
escriben el mismo número en las dos ranuras. Basta con abrir y cerrar una base sin tocarla; y
también lo produce el checkpoint del paso 10 justo después de una recuperación que no aplicó
ningún grupo, que es el caso corriente de reabrir una base bien cerrada.

Con el empate y una comparación estricta ganaba la ranura 0 por ser la primera que se lee, y
la mitad de las veces esa es la vieja. Lo que convierte el fallo en pérdida de datos, y no en
un `root_id` desactualizado, es la **generación del WAL**: una meta vieja nombra una que el
paso 5 de la sec. 7.4 ya borró. La recuperación abre `datos.wal.3`, `dir.Open` lo crea vacío
—es lo que hace con un archivo que no existe—, lee cero grupos, no ve nada anómalo, y arranca
una base a la que le faltan todos los `Put` confirmados desde ese checkpoint. Rompe la única
frase que el proyecto entero existe para sostener (sec. 4).

**Cómo se detectó.** Por la comparación completa tras una reapertura abandonada, en las cinco
semillas, entre las operaciones 148 y 733. El síntoma era siempre un valor **viejo** de una
clave, a veces del mismo tamaño que el correcto y distinto solo en los bytes. De ahí que el
generador de valores marque cada escritura con el número de operación: dos escrituras
sucesivas de la misma clave caliente no producen nunca los mismos bytes, y sin eso una
sustitución perdida sería indistinguible de una aplicada.

**El arreglo** (`ccd084c`) desempata por **época**. No hace falta inventar un contador: el
paso 5 de la sec. 7.4 rota el WAL en todo checkpoint, sin condición, así que la época crece
una vez por cada escritura de meta y la más nueva es siempre la de época más alta. El LSN
sigue mandando cuando difieren. **Queda pendiente reflejar la regla de lectura en la sec. 5.2
del DESIGN.md** (docs/DEUDA-DISENO.md, D12).

### Por qué ninguna fase anterior podía verlo

Ni la F3 ni la F4 abren la base más de dos veces: cargan, caen, reabren y comprueban. El
empate necesita **dos checkpoints sin ningún commit entre medias**, y eso no ocurre en un
arnés que solo abre para cargar y solo reabre para verificar. `TestCerrarYReabrir` tampoco lo
alcanza: la primera apertura de una base nueva no tiene meta (`sinMeta`), así que la primera
escritura de meta no compite con nada, y su único ciclo de cierre lleva 800 `Put` dentro que
suben el LSN.

Lo que lo destapa es que la secuencia **reabra a voluntad, en cualquier punto**, incluidos los
puntos en los que no hay nada que registrar. Es la clase de estado que un test escrito a mano
no visita porque no parece interesante.

### El arnés pasaba en verde sin ejercer la recuperación

La primera versión de este archivo reabría siempre con `Close`. Pasó en verde a la primera,
con 69 reaperturas por corrida, y sin reproducir un solo registro del log. `Close` hace un
checkpoint completo (sec. 7.4): baja todo a `datos.db`, pone la meta al día y rota. La
apertura que viene detrás encuentra el WAL vacío y los diez pasos de la sec. 8 no tienen nada
que hacer. Se estaba midiendo el camino barato y llamándolo recuperación.

Con la mitad de las reaperturas **abandonando** la base sin `Close`, las cinco semillas se
pusieron en rojo en la primera ejecución. El umbral de checkpoint tuvo que subir por la misma
razón: el WAL transporta imágenes de página completas (`wal.CargaImagen`, 4104 bytes), así
que con el umbral de 64 KiB de la primera versión hay un checkpoint cada ocho `Put` y el log
nunca acumula nada. Con 1 MiB son entre 100 y 250 `Put`, y la corrida ahora **exige** que
alguna reapertura haya tenido al menos 64 KiB de log por delante: salen entre 26 y 36
reaperturas abandonadas por semilla, con hasta 635 KiB reproducidos.

La lección es la misma que la F4 anotó sobre `cae()`: un arnés en verde no dice nada hasta que
se sabe qué camino recorrió. Las cuentas del final de la corrida —sustituciones, rechazos,
reaperturas abandonadas, mayor log reproducido, generación del log— están para eso, y tres de
ellas son condiciones de fallo del test, no solo trazas.

### Verificación por mutación

| Mutación | Resultado | Dónde salta |
|---|---|---|
| **M-META** · quitar el desempate por época de `meta.Leer` | **rojo, 5/5** | la comparación completa tras reabrir abandonada, op. 148 a 733 |
| **M-SUST** · que `Put` no borre la celda vieja: duplica la clave en vez de sustituirla | **rojo, 5/5** | la poscondición de `put`, op. 34 a 52 |
| **M-SCAN** · que el fin del rango pase a inclusivo (`>=` por `>`) | **rojo, 5/5** | `scanRango`, op. 23 a 114 |
| **M-COPY** · que `Get` devuelva el slice interno en vez de una copia (D6) | **verde: sobrevive** | — |

**M-COPY sobrevive, y está bien que así sea.** La secuencia nunca conserva un valor de `Get` a
través de una escritura posterior: lee, compara y lo suelta. Para verlo hay que retener el
slice y forzar después una compactación de esa hoja, que es exactamente lo que hace
`TestGetDevuelveUnaCopia` en `db_test.go` desde la F3. Es una propiedad sobre el **pasado del
llamador**, no sobre el contenido de la base, y una comparación contra un modelo no la alcanza
por construcción. Anotarlo importa: la cobertura de esta fase no es un superconjunto de la de
las anteriores.

### Por qué la reapertura abandonada va sobre el disco falso

`TestPropiedadesSobreDiscoReal` no abandona nunca: siempre cierra. Abandonar deja los
descriptores abiertos, y en Windows un archivo abierto no se puede borrar, así que la
siguiente rotación del WAL falla con `The process cannot access the file because it is being
used by another process`, y el borrado de `t.TempDir()` al terminar el test también. Se
comprobó al escribir la primera versión de `TestElCicloVacioNoPierdeLoConfirmado` sobre un
directorio real.

No es un fallo del motor: un proceso que muere de verdad no conserva descriptores, y por eso
el `kill -9` de la F3 corre en un subproceso (`crash_test.go`). Sí explica por qué
`TestReaperturaSinCerrar` funciona sobre disco real: sus 400 `Put` no llegan a cruzar el
umbral de 4 MiB, así que no hay rotación que intente borrar nada. Es una propiedad frágil de
ese test y conviene tenerla escrita.

### Lo que esta fase no prueba

- **Durabilidad.** El disco no falla en ninguna de estas corridas. Lo que se comprueba es la
  lógica del motor compuesto —árbol, log y checkpoint— contra el mapa. La sec. 9.3 separa las
  dos clases a propósito, y la de durabilidad es la F4.
- **Borrado.** Sigue sin existir, así que el modelo solo crece. Igual que en la F2, la F3 y la
  F4, el conjunto de páginas libres casi siempre está vacío y la partición del invariante 6 se
  comprueba sobre poca cosa.
- **Escala.** 800 claves por corrida, un árbol de dos o tres niveles. Las 100.000 claves siguen
  siendo de `TestCienMilClaves` (F2): esta fase busca variedad de secuencia, no tamaño.
- **Concurrencia.** `NO-GOALS.md` fija un solo hilo escritor. La secuencia es estrictamente
  secuencial.
- **Lo que dice M-COPY:** las propiedades que hablan de la memoria que el llamador retiene, y
  no del contenido de la base.

---

## F6 · Documentación y CI

**La fase no está cerrada.** El criterio de la sec. 10 son cuatro cosas —DESIGN, TESTING y
BUGS escritos, y CI en verde— y la última no se ha podido comprobar: los jobs de GitHub
Actions no llegan a arrancar (ver más abajo). Lo que sí se hizo fue correr por primera vez la
suite completa sobre Linux, que era la mitad del valor que se le pedía al CI.

Cero errores del motor. Un error de análisis, que es lo que esta sección tiene que contar.

### La primera corrida sobre Linux: verde, y sin nada que anotar del motor

Todo el desarrollo ocurrió en Windows, así que hasta aquí solo se había probado
`internal/fsx/os_windows.go`. La corrida se hizo sobre Ubuntu en WSL2 (kernel
6.6.87.2-microsoft-standard-WSL2) con go1.26.7 linux/amd64, y sobre **ext4**, no sobre
`/mnt/c`: drvfs conserva parte de la semántica de Windows y habría probado poco.

`go build`, `go vet`, `gofmt -l` y `go test -count=1 ./...` en verde, los doce paquetes, a la
primera. Los dos caminos que se esperaba que dieran problemas no dieron ninguno:

- el `fsync` de directorio, que en Windows es un no-op (D10) y en Linux existe de verdad;
- el borrado de un archivo abierto, que Windows prohíbe y Linux permite. Esa prohibición es la
  que obliga a `TestReaperturaSinCerrar` a no cruzar el umbral de rotación y la que manda la
  reapertura abandonada de la F5 al disco falso. Donde la restricción no existe, los dos
  siguen pasando.

### El error de análisis: el `fsync` de directorio no era el culpable

Lo único llamativo de la corrida fueron los tiempos:

| Test | Windows | Linux | |
|---|---|---|---|
| `TestElCheckpointSeDisparaSoloYNoAcumulaLogs` | 2,66 s | 20,44 s | 7,7× |
| `TestCaidaYReapertura` (`kill -9`) | 1,97 s | 7,06 s | 3,6× |
| `TestSobreviveConDivisiones` | 11,08 s | 18,94 s | 1,7× |
| `TestReaperturaSinCerrar` | 0,34 s | 1,93 s | 5,7× |
| `TestQuinientosPuntosDeCaida` (disco falso) | 2,87 s | **1,69 s** | 0,6× |

La última fila es la que orienta: el barrido, que no toca el disco de verdad, es *más rápido*
en Linux. Todo lo que va a disco real es de 2× a 8× más lento. La diferencia está en la E/S
sincronizada y no en la CPU ni en el compilador.

**La hipótesis fue que la causa era el `fsync` de directorio**, por ser justamente el camino
que en Windows no se ejecuta (D10). Es falsa, y la desmiente un programa de cuarenta líneas
—fuera del motor, para no medir el motor— que cronometra las tres operaciones en las dos
plataformas:

| | Windows / NTFS | Linux / ext4 en WSL2 | |
|---|---|---|---|
| `fsync` de archivo | 683 µs | 2,32 ms | 3,4× |
| `fsync` de directorio, sin nada sucio | **no existe** | 0,56 µs | — |
| una rotación entera (crear + `fsync` + 2 × `fsync` de directorio + borrar) | 1,18 ms | 4,56 ms | 3,9× |

Lo que decide es **la frecuencia de cada uno, no su coste**. El `fsync` de archivo ocurre una
vez por `Put` confirmado —es el paso 5 de la sec. 7.2—; el de directorio, una vez por
rotación. En `TestElCheckpointSeDisparaSoloYNoAcumulaLogs`, que son 4000 `Put` con el umbral
de 4 MiB, eso es **4000 `fsync` de archivo contra unas 4 rotaciones**: 4000 × 1,64 ms de
diferencia son unos 6,6 s de los 17,8 s que separan las dos plataformas, y las 4 rotaciones
enteras suman 18 ms. El `fsync` de directorio no puede explicar nada aquí, ni siquiera si
costara mil veces más.

El resto de la diferencia —los otros 11 s— no está aislado y no se va a fingir que sí: viene
del volumen de E/S del checkpoint, que baja todas las páginas sucias antes de su `fsync`.
Tampoco tiene sentido perseguirlo en WSL2, que corre sobre un disco virtual: los ratios
orientan, los números absolutos son de esta máquina y de nadie más.

La lección es la de la F4 sobre `cae()`, otra vez y desde el otro lado: una explicación
plausible sobre por qué un número es como es no vale nada hasta que se mide. Aquí la
explicación plausible señalaba precisamente al mecanismo más interesante del documento, que
es lo que la hacía atractiva y lo que debería haberla hecho sospechosa.

### D10, confirmado desde fuera del motor

El mismo programa dio, sin buscarlo, la mejor evidencia que hay de D10. En Windows,
`os.Open` sobre un directorio seguido de `Sync()` no falla por cómo esté escrito el motor:
falla siempre.

```
sync C:\Users\sebas\AppData\Local\Temp\costefsync1223039689: Access is denied.
```

Cuarenta líneas de librería estándar, sin `internal/fsx` por medio. El no-op de
`os_windows.go` no es una decisión de implementación que pudiera revisarse con más esfuerzo:
es que Windows no entrega un handle de directorio que acepte `FlushFileBuffers`. D10 estaba
anotada como limitación permanente y ahora tiene una comprobación independiente.

### El CI no ha llegado a correr

El workflow está escrito y empujado, y su primer disparo terminó en 2 segundos con los dos
jobs en rojo **sin ejecutar un solo paso**:

```
Test (ubuntu-latest)  -> failure
Test (windows-latest) -> failure
"The job was not started because your account is locked due to a billing issue."
```

(run 34374907440). No es un fallo del workflow ni del código: es la cuenta de GitHub. El
workflow queda sin ejercitar, y **el criterio "CI en verde" de la sec. 10 sigue sin
cumplirse**. Conviene que quede escrito así y no como un detalle administrativo: un workflow
que nunca corrió es exactamente igual de fiable que un test que nunca corrió.

### Lo que esta fase no prueba, todavía

- **El detector de carreras.** `go test -race` sigue sin haberse ejecutado ni una vez sobre
  este código. Necesita cgo, y ni la máquina de desarrollo ni el Ubuntu de WSL2 tienen
  compilador de C (`go: -race requires cgo`). Instalarlo en WSL2 pide `sudo`; el runner de
  Actions lo trae de fábrica, así que espera al CI. Lo que se espera de él es confirmación
  —`NO-GOALS.md` fija un solo hilo escritor y no hay una sola goroutine en el motor— pero
  "se espera" no es "se comprobó".
- **Linux de verdad.** WSL2 no es un runner de Actions: kernel propio, disco virtual, y un
  `fsync` que cuesta lo que cuesta ahí. Lo que la corrida demuestra es que el código pasa por
  el camino POSIX de `internal/fsx` sin romperse, no que lo haga con estos tiempos en otra
  parte.
- **La fila 5a del argumento de correctitud.** Sigue sin cobertura por inyección de fallos, y
  correr en Linux no la añade: lo que falta es que el disco falso pueda modelar la creación
  de un archivo como una operación con caché, y eso es del arnés, no de la plataforma
  (`TESTING.md`, sec. 3.4).
