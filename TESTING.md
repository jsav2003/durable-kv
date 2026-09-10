# TESTING

Qué se prueba en este motor, con qué fuerza, y qué queda fuera.

Este documento tiene dos mitades, que son las dos que la sec. 12 del `DESIGN.md` pide: la
**tabla de inyección de fallos con sus resultados** (sec. 3) y el **argumento de correctitud
de la sec. 9.5** (sec. 4). Alrededor hay lo mínimo para que las dos se puedan reproducir y
para que se sepa qué significan sus verdes.

Es el complemento de `BUGS.md`, no un resumen suyo. `BUGS.md` cuenta los errores que
aparecieron, qué los causaba y cómo se detectaron, fase por fase. Aquí está lo que la suite
**afirma** cuando pasa, y —con el mismo detalle— lo que no afirma.

---

## 1. Cómo se corre

```
go test ./...                                     # la suite entera
go test -short ./...                              # salta los cuatro tests largos
go test -run TestQuinientosPuntosDeCaida -v .     # solo el barrido de la F4
go test ./internal/page -run=XXX -fuzz=FuzzDecode -fuzztime=60s   # un objetivo de fuzz
```

Sin dependencias externas: solo la librería estándar. No hace falta preparar nada.

Medido en la máquina de desarrollo (Windows 11, go1.26.7, amd64) con `-count=1`, la suite
completa tarda **23 s** de reloj. Los dos paquetes que la dominan son la raíz (19,7 s) e
`internal/recovery` (18,9 s), y los demás están entre 0,9 y 4,5 s; Go corre los paquetes en
paralelo, así que el reloj se parece al más lento y no a la suma.

`-short` salta cuatro tests: el barrido de 500 puntos de caída, las 100.000 claves de la F2 y
las dos corridas de propiedades. **Ahorra tres segundos** —20 s frente a 23 s— porque el
paquete que marca el paso es `internal/recovery`, que no salta nada. O sea: `-short` sirve
para el ciclo de edición sobre la raíz, y no hay ninguna razón de tiempo para usarlo en el CI
ni para dar una fase por terminada con él.

**`go test -race` no corre en esta máquina.** Necesita cgo y aquí no hay compilador de C
(`cgo: C compiler "gcc" not found`). Queda para el CI de Linux de la F6. Su valor aquí es
limitado —la sec. 2 declara un solo hilo escritor y `NO-GOALS.md` fija la concurrencia como
exclusión permanente— pero el CI lo tiene gratis y conviene tenerlo puesto.

Los objetivos de fuzz corren unos segundos con su corpus semilla en cada `go test`. Para que
signifiquen algo hay que darles tiempo explícito con `-fuzz`; las corridas largas que se han
hecho y lo que encontraron están en `BUGS.md` (718.939 ejecuciones de `FuzzArbol`, 168.757 de
`FuzzLeer`).

---

## 2. Las seis capas, y qué caza cada una

190 funciones de test y 5 objetivos de fuzz, repartidos así:

| Capa | Qué caza | Dónde vive | Qué no ve |
|---|---|---|---|
| Unidad por paquete | formato, codificación, límites, errores | `internal/*/…_test.go` | todo lo que se decide entre componentes |
| `Validate()` | los seis invariantes sobre el árbol entero, incluida la partición | `internal/tree/validate.go`; se llama tras cada recuperación y en cada punto de caída | nada sobre durabilidad: opera sobre lo que ya está en memoria y en disco |
| Fuzzing | pánicos y aceptación de basura en los deserializadores | `page`, `record`, `wal`, `node`, `tree` | lo que necesita una secuencia con sentido |
| `kill -9` real | la **frontera del proceso**: que nada de lo que sostiene la durabilidad viva en la memoria del motor | `crash_test.go`, en un subproceso | la **frontera del disco**: matar un proceso no vacía el caché del sistema operativo, así que no distingue si hubo `fsync` |
| Inyección de fallos | la frontera del disco: descarte, reordenamiento y desgarro de lo no sincronizado | `crash_injection_test.go` sobre `internal/fsx/fsxtest` | lo que el disco falso no modela (sec. 3.4) |
| Propiedades contra modelo | la lógica del motor compuesto contra un `map` de referencia | `propiedades_test.go` | durabilidad: en esas corridas el disco no falla |

Las dos últimas son las que dan valor al proyecto, y las dos hacen falta: la inyección de
fallos encuentra bugs de durabilidad, las propiedades encuentran bugs de lógica, y ninguna de
las dos ve la clase de la otra (sec. 9.3).

### 2.1 Los dientes de la corrida de propiedades

`TestPropiedadesContraElModelo` es el criterio de terminación de la F5: cinco semillas fijas,
3000 operaciones cada una, aplicadas a la vez al motor y a un `map` de referencia, con
comparación completa después de cada operación y `Scan` contra las claves del mapa ordenadas.
La secuencia intercala cierres y **reaperturas abandonadas sin `Close`**, que es lo que
obliga a la recuperación a reproducir log de verdad: la corrida falla si ninguna reapertura
abandonó la base o si el mayor log reproducido no llegó a 64 KiB. Un arnés en verde no dice
nada hasta que se sabe qué camino recorrió.

Sus mutaciones, con el mismo método de la sec. 3.3:

| Mutación | Qué se rompe | Resultado |
|---|---|---|
| **M-META** | quitar el desempate por época de `meta.Leer` | **rojo, 5 de 5 semillas** |
| **M-SUST** | que `Put` no borre la celda vieja: duplica la clave en vez de sustituirla | **rojo, 5 de 5** |
| **M-SCAN** | que el fin del rango pase a inclusivo | **rojo, 5 de 5** |
| **M-COPY** | que `Get` devuelva el slice interno en vez de una copia (D6) | **verde: sobrevive** |

M-COPY sobrevive y está bien que así sea: la secuencia lee, compara y suelta, así que nunca
conserva un valor a través de una escritura posterior. Es una propiedad sobre el **pasado del
llamador**, no sobre el contenido de la base, y una comparación contra un modelo no la alcanza
por construcción. La cobertura de esta capa no es un superconjunto de la de las anteriores.

M-META es el bug real que esta fase encontró (D12): con las dos metas empatadas en LSN, la
elección se quedaba con la ranura vieja. Está contado entero en `BUGS.md`.

---

## 3. La tabla de inyección de fallos

### 3.1 El barrido

`TestQuinientosPuntosDeCaida` es el ciclo de la sec. 9.2. Para cada `N` de 1 a 500:

- abrir la base sobre un disco falso configurado para caer en la escritura `N`;
- escribir un conjunto conocido de claves, anotando cuáles devolvió OK cada `Put`;
- la caída ocurre —puede ser durante la apertura, si `N` es muy bajo;
- reabrir sobre un disco que hereda **solo los bytes duraderos**;
- verificar (a), (b) y (c).

| Parámetro | Valor | Por qué |
|---|---|---|
| Puntos de caída | 500 | lo que pide la tabla de la sec. 10 |
| Escrituras de la carga limpia | **661** | el test las mide antes de empezar y falla si no llegan a 500 |
| Claves por corrida | 180, con valores de 213 bytes | pasa holgadamente de 500 escrituras sin inflar cada subtest |
| Umbral de checkpoint | 6000 bytes de WAL | fuerza checkpoints cada pocos `Put`, para que haya puntos de caída **dentro** de un checkpoint y de una rotación |
| Semilla | `0xF40000 + N` | cada fila es reproducible por separado, y la tabla entera desde una sola constante |

Los tres criterios:

- **(a)** `Validate()` pasa: los seis invariantes, incluida la partición.
- **(b)** toda clave cuyo `Put` devolvió OK está, y con su valor. **Es la afirmación central
  del proyecto entero.**
- **(c)** toda clave presente pertenece al conjunto de las que se intentaron escribir.

El criterio (c) está redactado así a propósito. "Ninguna clave no confirmada aparece" es
**falso y esperable**: una clave anexada al WAL cuyo `fsync` no llegó a retornar puede
sobrevivir perfectamente, porque el sistema pudo volcar esos bytes por su cuenta. Las
confirmadas **deben** estar; las no confirmadas **pueden** estar o no; lo que nunca puede
aparecer es una clave que jamás se intentó escribir.

**Resultado: 500 de 500 en verde, cero pérdidas.** El barrido tarda 2,9 s.

### 3.2 Qué inyecta el disco falso

`internal/fsx/fsxtest` con `Volatil` en true mantiene las escrituras sin sincronizar en una
cola **global** —una sola para `datos.db` y el WAL, etiquetada por archivo— y no las hace
duraderas hasta que un `Sync` del archivo correspondiente las vuelca. Al llegar al `WriteAt`
número `CaeEn`:

1. descarta al azar una parte de las pendientes;
2. reordena las que sí aplica;
3. parte una de ellas en una frontera de sector de 512 bytes: la escritura desgarrada.

La cola es global y no por archivo porque **el orden relativo entre `datos.db` y el WAL *es*
el write-ahead logging**. Con una cola por archivo, el bug de la sec. 7.5 —una página de
datos en disco antes que su registro de log— sería invisible para el arnés por construcción.

### 3.3 La tabla de mutaciones: dónde tiene dientes el barrido

Un verde de 500 filas no dice nada por sí solo. Lo que dice cuánto vale es qué pasa cuando se
rompe algo a propósito. Cada mutación se aplicó sola, se corrió el barrido entero, y se
restauró.

| Mutación | Qué se rompe | Resultado | Criterio |
|---|---|---|---|
| **M1** | `WAL.Commit` no hace `fsync` del registro de commit | **206 de 500 en rojo** | (b) |
| **M2** | el checkpoint no hace `fsync` de `datos.db` | **89 de 500 en rojo** | (b) |
| **M3** | la recuperación no aplica las imágenes de los grupos completos | **205 de 500 en rojo** | (b) |
| **H1** | `cae()` aplica todas las pendientes, intactas y en orden | **500 en verde** | — |
| **M-WA** | quitar la regla de desalojo `wal_flushed_lsn < page_lsn` | **500 en verde** | — |
| **D11-estricto** | `comprobarLibres` deja de aceptar la ranura enteramente a ceros | **500 en verde** | — |

Cómo se lee cada bloque:

**M1, M2 y M3 muerden, y ninguna llega a 500.** No tienen por qué: un punto de caída donde lo
pendiente relevante ya estaba sincronizado no distingue el motor sano del roto. M2 solo
muerde cuando la caída cae dentro de un checkpoint o justo después; M3, solo cuando hay
imágenes que reaplicar que no tapó ya un checkpoint anterior. Que las dos muerdan es la
prueba de que el barrido mete puntos de caída dentro de un checkpoint y de una rotación, y no
solo en el camino tranquilo de un `Put`. El criterio que se pone en rojo en los tres casos es
(b), que es el que había que ver fallar.

**H1 es una mutación del arnés, no del motor, y es la más importante de la tabla.** Con un
`cae()` que aplica todo intacto —una caída limpia— los 500 puntos pasan en verde *con M1, M2
y M3 puestas*. O sea: si `cae()` no rompe nada, el barrido no prueba nada más de lo que ya
probaba el `kill -9` de la F3. Todo el valor de esta sección descansa en la fidelidad de
`cae()`, y por eso `cae()` tiene sus propios tests (`TestElDesgarroEsEnFronteraDeSector`,
`TestLoSincronizadoSobreviveALaCaida`, `TestCaidaEsReproducibleConLaMismaSemilla`).

**M-WA en verde no significa que la regla de desalojo sobre.** Significa que **este camino no
la alcanza**: `writePage` solo se llama desde `FlushDirty`, es decir en un checkpoint, y para
entonces todo `Put` ya sincronizó su commit, así que `FlushedLSN` va siempre por delante de
cualquier `page_lsn`. El motor, tal como está construido, nunca crea la precondición. Quien
la crea a mano es el pager: con M-WA puesta, `TestReglaDeDesalojo` y `TestWriteAheadIrreparable`
se ponen en rojo de inmediato. La regla está viva y probada, y el árbitro de orden global
existirá para cazarla el día que un caché acotado abra ese camino
(`docs/DEUDA-DISENO.md`, D4).

**D11-estricto en verde es evidencia para una decisión abierta, no la decisión.** Dice que la
franja estrecha que D11 describe —una escritura desgarrada que deje una ranura libre entera a
ceros, indistinguible de una extensión legítima— no la produce este barrido. Acota lo que
está en juego; las tres salidas de D11 siguen sobre la mesa.

### 3.4 Lo que el disco falso no modela

- **La creación de un archivo, en el barrido.** Lo estuvo del todo hasta la F6: `Open` creaba
  el archivo de forma duradera en el acto y la ventana entre el `Open` de la generación nueva
  del WAL y el `dir.Sync()` de la rotación no existía. Ahora el disco falso sabe modelarla
  —`fsxtest.DirVolatil` deja la creación y el borrado de un archivo pendientes hasta el
  `fsync` del directorio, y `cae()` decide con la misma semilla cuáles llegaron al plato—,
  pero **el barrido de los 500 puntos corre con eso apagado**. Quien la ejercita es un test
  aparte, `TestCaidaEntreLaCreacionDelLogYElFsyncDelDirectorio`, con sus propias semillas.

  La separación es deliberada: encender el modo cambia lo que sobrevive a cada caída, y la
  tabla de la sec. 9.2 está calibrada sin él. Que el barrido siga dando exactamente el mismo
  resultado no es una suposición: la huella de los 500 estados duraderos es la misma antes y
  después del cambio (sha256 `8bb2983d…`), porque las entradas de directorio se deciden al
  final de `cae()` y no mueven el flujo del `rand` que consumen el descarte, el reordenamiento
  y el desgarro. Meterlas en el barrido es una decisión abierta, no una tarea pendiente.
- **El disco no corrompe un sector ya escrito.** Solo descarta, reordena y desgarra
  escrituras **sin sincronizar**. Un bit que se voltea en un sector que ya estaba en el plato
  es otra clase de fallo, y lo cubren los tests de corrupción de un bit de la F1 y la F3
  (`page.TestBitFlipCorruption`, `record.TestBitFlipCorruption`).
- **Puntos de caída más allá del 500.** La carga limpia emite 661 escrituras. Los 161
  restantes caen en la parte final de la carga, donde no hay ninguna estructura que los 500
  anteriores no hayan ejercido ya.

---

## 4. Argumento de correctitud (sec. 9.5)

Tres pruebas de escritorio. No las corre nadie: son el razonamiento que dice **por qué** los
tests que sí corren son los que hay que correr. Se revisan al terminar cada fase.

### 4.1 Prueba del corte en cualquier punto

El motor hace exactamente cinco `fsync` en su camino normal. Para cada uno, qué ve la
recuperación si la caída ocurre justo antes y justo después. Diez casos.

| # | Corte | Qué ve la recuperación | Qué lo ejercita | v1 |
|---|---|---|---|---|
| 1a | antes del `fsync` del WAL en `Put` (`wal.Commit`) | el grupo puede estar anexado sin ser duradero; sin su commit legible se descarta **entero**. `Put` no había devuelto: (b) no exige la clave y (c) la admite si sobrevivió | `wal.TestElUltimoGrupoSinCommitSeDescartaEntero`, `recovery.TestUnGrupoSinCommitSeDescartaEntero`, `recovery.TestUnCRCMaloCortaYLoPosteriorSeDescarta` | **fallaba** (H1) |
| 1b | después | el grupo entero es duradero y `Put` devolvió `nil`; la recuperación lo reproduce sobre `datos.db` | `recovery.TestLoConfirmadoSobreviveSinCheckpoint`, `recovery.TestSobreviveConDivisiones`, `TestCaidaYReapertura` (`kill -9`), **M1** (206 rojos) | correcto |
| 2a | antes del `fsync` de `datos.db` en el checkpoint (paso 2) | páginas sucias a medias en `datos.db`; ninguna meta se tocó todavía, así que vale la vieja, y `datos.wal.N` sigue existiendo porque el paso 5 no corrió. Se reproduce desde el LSN de la meta vieja y las imágenes sobrescriben enteras las páginas rotas | `recovery.TestSobreviveConCheckpointPorMedio`, **M2** (89 rojos) | correcto |
| 2b | después | `datos.db` al día; la meta vieja hace reproducir un log ya aplicado. Idempotente por construcción: la imagen sobrescribe la página entera | `recovery.TestIdempotenciaDeLaRecuperacion`, `recovery.TestDosRecuperacionesDelMismoEstadoCoinciden` | correcto |
| 3a | antes del `fsync` de la meta (paso 4) | la ranura **más antigua** puede quedar desgarrada: su CRC falla, `meta.Leer` la descarta, y gana la otra, que es la que venía valiendo. El log de la época N sigue ahí | `meta.TestUnaCorruptaYLaOtraSirve`, `meta.TestUnaMetaEnLaRanuraEquivocadaSeDescarta`, `pager.TestMetasFueraDelPager` | **fallaba** (H6) |
| 3b | después, antes de rotar | la meta dice época N+1 y en el directorio solo está `datos.wal.N`: no se reproduce nada, y es correcto, porque el paso 2 ya bajó todo lo que el log tenía que aportar. Si el LSN no avanzó entre los dos checkpoints, el desempate por época es lo único que impide elegir la meta vieja | `recovery.TestElPaso10DejaLaMetaAlDiaYElLogLimpio`, `meta.TestConElMismoLSNGanaLaEpocaMasAlta` (D12) | correcto |
| 4a | antes del `fsync` de la extensión de `datos.db` (sec. 7.6) | las páginas cero pueden no ser duraderas, pero el commit del grupo que usa la página nueva todavía no existe: nada confirmado depende de ellas. Y la recuperación calcula el `page_id` máximo de las imágenes y extiende explícitamente antes de aplicar (paso 5) | `pager.TestExtensionAntesDelCommit`, `recovery.TestElArchivoSeExtiendeHastaElTotalPagesConfirmado`, `recovery.TestLasPaginasDelHuecoSeEscribenAntesDeAplicarImagenes` | **fallaba** (H12) |
| 4b | después, antes del commit | el archivo mide lo que promete y esas ranuras quedan sin dueño; al reabrir, `ReconstruirLibres` las declara libres y `comprobarLibres` las acepta por estar enteramente a ceros | `recovery.TestUnaPaginaLibreACerosEsLegitima`, `recovery.TestUnaPaginaLibreIlegibleSeDetecta`, criterio (a) del barrido | correcto (D11) |
| 5a | antes del `fsync` del directorio en la rotación (paso 5) | `datos.wal.N+1` puede no existir de forma duradera. `Rotar` crea, sincroniza el directorio y **solo entonces** borra el viejo, así que lo peor es que falte la N+1 y siga la N: la meta apunta a una generación ausente y no se reproduce nada, lo que es seguro porque el paso 2 ya bajó todo | `wal.TestRotarCreaAntesDeBorrar`, `wal.TestRotarConCaidaAntesDelSyncDelDirectorio`, `TestCaidaEntreLaCreacionDelLogYElFsyncDelDirectorio`, `fsx.TestSyncDelDirectorio` | **fallaba** (H7) |
| 5b | después, tras borrar el viejo | puede quedar la generación **vieja** sin borrar. El estado a reproducir es "existen la N y la N+1, y la meta dice N+1": la huérfana es la vieja, y se barre | `recovery.TestSeBarreLaGeneracionHuerfanaDeUnaRotacionAMedias`, `wal.TestRotarConCaidaAntesDelSyncDelBorrado`, `recovery.TestNoSeAcumulanGeneracionesDelLog` | **fallaba** (H7) |

**Los cuatro que la versión 1 fallaba.** No es una cifra redonda por casualidad: son los
cuatro cortes que `docs/REVIEW-01.md` convirtió en hallazgos. El quinto `fsync` —el de
`datos.db` en el checkpoint— es el único que la v1 ya tenía bien, porque el orden 1→2→3 del
checkpoint estaba correcto desde el principio.

- **Corte 1 (H1).** La v1 aplicaba el log registro por registro. Una división genera cuatro
  imágenes; si la caída deja tres, el padre ya apunta a una hoja cuya imagen faltó. Si esa
  página había sido liberada y reciclada, contiene datos viejos con **CRC válido**:
  `Validate()` pasa en verde y el árbol devuelve claves de otra época. El registro de commit
  y el descarte del grupo incompleto son lo que convierte "todas o ninguna" en un mecanismo.
- **Corte 3 (H6).** En la v1 la meta era una página sucia como cualquier otra, así que el
  paso 1 del checkpoint escribía la **actual** y el paso 3 la **más antigua**: las dos ranuras
  acababan con estado de la misma época y la garantía de la sec. 5.2 dejaba de valer. Y con
  las dos inválidas, la v1 declaraba el archivo irrecuperable teniendo el WAL entero intacto
  en el disco. Hoy las metas están fuera del conjunto de sucias, y dos metas inválidas mandan
  reproducir el log desde cero (`recovery.TestLasDosMetasInvalidasReproducenElLogEntero`).
- **Corte 4 (H12).** Sin `total_pages` en el commit y sin extensión explícita, aplicar una
  imagen de la página 900 sobre un archivo de 800 dejaba un archivo disperso con un agujero
  que se lee como ceros y falla el CRC; y la alternativa —no aplicar más allá de
  `total_pages`— descartaba datos confirmados. Las dos salidas rompían algo.
- **Corte 5 (H7).** La v1 truncaba el WAL en sitio y sin `fsync`. Tras la caída el archivo
  podía conservar longitud y bytes viejos, y la recuperación, al terminar los registros
  nuevos, encontraba registros antiguos con CRC **perfectamente válido**: la regla del primer
  CRC inválido no disparaba nunca. La rotación, el LSN monótono con contigüidad y el campo
  `epoca` son tres defensas para lo mismo, y se usan las tres.

**La fila 5a, que fue el límite de esta prueba durante cinco fases.** Era la única de las diez
que ningún test ejercitaba con inyección de fallos, y lo que la sostenía era el **orden de las
llamadas** —que la traza compartida sí ve— más las defensas 1 y 2 de la sec. 7.4. No era un
límite de la plataforma sino del arnés: el disco falso hacía la creación de un archivo duradera
en el acto, así que la ventana no existía.

Desde la F6 existe. `TestCaidaEntreLaCreacionDelLogYElFsyncDelDirectorio` corta entre el `Open`
de la generación 50 y el `fsync` del directorio, con 90 claves confirmadas por corrida, y deja
el estado que ninguna otra prueba alcanza: **la meta dice época N+1 —se escribió y sincronizó en
los pasos 3 y 4, antes de rotar— y en el directorio solo está la N**. Al reabrir, la generación
que la meta nombra no existe, `Open` la crea vacía, no se reproduce nada, y eso es seguro porque
el paso 2 ya bajó a `datos.db` todo lo que el log tenía que aportar. Las dos salidas del azar
—la generación nueva sobrevive o se pierde— se recorren las dos: en veinte semillas, 12 y 8.
Cero errores del motor.

Lo que sigue sin ejercitarse aquí es el `dir.Sync()` **real**, que en Windows es un no-op
documentado (`docs/DEUDA-DISENO.md`, D10). Eso es de la plataforma, no del arnés, y su sitio es
el CI de Linux.

### 4.2 Prueba de la partición

Recorrer los escenarios que tocan el conjunto de páginas libres y comprobar que el invariante
6 los declara inválidos si algo salió mal.

| Escenario | Qué tendría que salir mal | Qué lo declara inválido | Test |
|---|---|---|---|
| Asignación desde el conjunto de libres | entregar una página que sigue alcanzable desde la raíz | doble pertenencia: `particion()` la ve en el árbol y en `FreePages()` | `pager.TestFreeYReasignacion`, `pager.TestAdoptFreeSet`, `tree.TestValidateDetectaCadaRotura` |
| Liberación de una página | soltarla sin sacarla del árbol, o al revés | las dos mitades de la partición | `tree.TestReconstruirLibres`, `tree.TestValidateDetectaCadaRotura` |
| Reasignación dentro del mismo intervalo de checkpoint | la ventana de la sec. 6.1: la página está en el árbol según el WAL y en la cabeza de libres según la meta | **no puede ocurrir**: el conjunto no se persiste, se reconstruye en cada `Open` | todas las de `recovery`: cada apertura lo reconstruye |
| Página que no está en ninguno de los dos conjuntos | fuga: el motor filtra espacio en silencio | pertenencia nula. La v1 solo prohibía la doble pertenencia, así que un motor que filtrara el 100% del archivo pasaba la validación | `tree.TestValidateDetectaCadaRotura` |
| Página libre ilegible | basura que un reciclado convertiría en datos | `comprobarLibres`, tras la recuperación | `recovery.TestUnaPaginaLibreIlegibleSeDetecta` |

**El resultado de esta prueba es en parte vacuo hoy, y hay que decirlo.** El borrado no
existe, así que no hay fusión, así que ninguna página se libera nunca en el camino normal: el
conjunto de libres solo se llena por extensiones del archivo y casi siempre está vacío. Los
500 `Validate()` del barrido comprueban la partición sobre poca cosa. Lo que la ejercita de
verdad son los tests del pager y de `tree`, que construyen el estado a mano, y las once
mutaciones de `Validate()` de la F2.

La tercera fila es la más importante de la tabla y no la prueba ningún test, porque no hay
nada que probar: la sec. 6.1 elimina esa ventana **por construcción** y no por vigilancia
(sec. 5 de este documento).

La contradicción conocida de este invariante —una ranura materializada por extensión está en
el conjunto de libres y su CRC de ceros es inválido, así que el invariante 6 tal como está
redactado no puede ser cierto para ella— es **D11**, sigue abierta, y `comprobarLibres` acepta
hoy la ranura a ceros con ese motivo escrito en una sola función.

### 4.3 Prueba del modo tombstone

Releer los seis invariantes suponiendo la regla de rescate de la sec. 11 —borrado por marca,
sin fusión— y confirmar que los seis siguen siendo satisfacibles. Si alguno no lo fuera, la
vía de escape del riesgo pondría en rojo la tabla de la F4, que es la fase que el plan declara
irrecortable.

| Inv. | Con tombstones sin fusión | |
|---|---|---|
| 1 · orden estricto | una marca es una celda con la misma clave: el orden no cambia | se sostiene |
| 2 · hojas a la misma profundidad | sin fusión, un borrado no quita niveles; lo único que puede cambiar la estructura es una división, que preserva el invariante | se sostiene |
| 3 · ninguna alcanzable vacía, y ningún par de hermanos cabe entero en una página | ver abajo | se sostiene **bajo condición** |
| 4 · separadoras | marcar no toca las separadoras del padre | se sostiene |
| 5 · cadena lateral | sin fusión no se reengancha ningún enlace: es el caso fácil, no el difícil | se sostiene |
| 6 · partición | sin fusión no se libera ninguna página: el conjunto de libres queda vacío y la partición es cierta trivialmente | se sostiene, y queda **más** vacuo que hoy |

**La condición del invariante 3, que es donde vive todo el interés de esta prueba.** La
segunda mitad —"ningún par de hermanos adyacentes cabe entero en una sola página"— es la
propiedad que la fusión persigue. Sin fusión solo puede seguir siendo cierta si **el borrado
por marca no reduce el espacio que ocupa la celda**: si la marca conservara la clave y
liberara el valor, dos hermanos medio vacíos acabarían cabiendo en una página, nadie los
fusionaría, y el invariante 3 se rompería sobre un árbol que hizo exactamente lo que se le
pidió.

Esa condición no es una restricción nueva: es **exactamente** la contrapartida que la sec. 11
ya declara como efecto colateral —*"con tombstones un `Delete` puede provocar una división de
página, así que deja de ser el caso fácil"*—. Un `Delete` solo puede necesitar espacio si la
marca ocupa espacio. Las dos afirmaciones del diseño se sostienen mutuamente, y la lectura
útil es la inversa: **quien un día "optimice" el tombstone para liberar el valor rompe el
invariante 3 sin tocar el invariante 3.** Conviene que quede escrito antes de que exista el
código de borrado, no después.

Con esa condición, los seis son satisfacibles y la regla de rescate sigue siendo compatible
con la tabla de la F4. Es lo que se ganó al bajar el 40% de ocupación de invariante a objetivo
de llenado (H10): en la v1, la vía de escape del riesgo ponía en rojo la fase irrecortable.

---

## 5. El conjunto de libres se reconstruye: es una decisión, no una omisión

La sec. 6.1 del `DESIGN.md` pide que este argumento quede aquí, y no en una nota al pie del
código.

**Decisión: el conjunto de páginas libres no se persiste. Se reconstruye en cada `Open`**
barriendo las páginas alcanzables desde la raíz y marcando el complemento.

Una lista enlazada de libres en disco introduce una ventana de inconsistencia entre
checkpoints que produce el peor bug del proyecto. El asignador saca la página 30 de la lista y
el WAL registra la 30 ya convertida en hoja con datos vivos; la meta —que solo se escribe en
el checkpoint— sigue diciendo `free_head = 30`. Al recuperar, la página 30 está a la vez en el
árbol y en la cabeza de la lista de libres. El siguiente `Put` que necesite espacio la asigna
otra vez y borra en silencio todas sus claves: árbol estructuralmente válido, datos perdidos,
y el fallo aparece miles de operaciones después de su causa.

Reconstruir es O(n) al abrir, pero **el costo ya está pagado**: el paso 9 de la recuperación
recorre el árbol entero con `Validate()` de todas formas. A cambio, la ventana desaparece por
construcción y el invariante 6 pasa a ser cierto por definición en vez de por vigilancia.

El campo `free_head` sigue existiendo en el formato de la meta y en el registro de commit, y
se escribe siempre a cero: se conserva por si algún día se persistiera el conjunto, y no se lee
en la recuperación. Está así en `checkpoint.Correr` y en `pager.CommitGroup`, en los dos sitios
con el motivo escrito.

Para un proyecto cuyo objetivo declarado es demostrar durabilidad, eliminar una clase entera de
bug de caída vale más que un `Open` rápido.

---

## 6. Lo que la suite no prueba

Reunido de las seis fases. Cada punto está desarrollado en la sección correspondiente de
`BUGS.md`.

- **El borrado, y con él la partición sobre un árbol que encoge.** No hay `Delete`. El conjunto
  de libres solo se llena por extensión del archivo, así que **todo lo que este motor demuestra
  es sobre un árbol que solo crece**. Es la limitación más grande de la lista y afecta por igual
  a la F2, la F3, la F4 y la F5.
- **La caída durante la creación de un archivo, dentro del barrido.** El disco falso ya sabe
  modelarla (`DirVolatil`), y la fila 5a tiene su test de inyección desde la F6, pero el barrido
  de los 500 puntos corre con el modo apagado para no mover la tabla de la sec. 9.2. Lo que
  cubre esa fila es un test aparte y sus veinte semillas, no las 500 filas.
- **La degradación del medio.** El disco falso no voltea bits en sectores ya escritos; eso lo
  cubren los tests de corrupción de un bit, que son otra cosa.
- **El `fsync` de directorio real.** No existe en Windows (D10). Queda para el CI de Linux.
- **La regla de desalojo bajo presión de caché.** El caché del pager no está acotado (D4), así que
  el motor nunca desaloja una página sucia con su WAL sin sincronizar. La regla está probada a
  mano, no por el barrido (M-WA).
- **Concurrencia.** `NO-GOALS.md` la fija como exclusión permanente: un solo hilo escritor. No hay
  nada que probar, pero conviene decir que todo lo de aquí es estrictamente secuencial.
- **Escala más allá de 100.000 claves**, que es `tree.TestCienMilClaves`. El barrido de la F4 usa
  180 claves, y las propiedades 3000 operaciones sobre unas 800 claves distintas por semilla:
  buscan variedad de secuencia y de punto de caída, no tamaño.
- **Las propiedades que hablan de la memoria del llamador.** Una comparación contra un modelo de
  referencia no las alcanza por construcción: la secuencia lee, compara y suelta. Que `Get`
  devuelva una copia (D6) lo prueba `TestGetDevuelveUnaCopia`, y hace falta forzar una compactación
  para verlo.
- **Un `Put` que falla a medias.** No hay camino de aborto: el grupo de commit queda abierto (D9,
  sigue abierta). Hoy lo tapa que el motor propague el error y la base se abandone; ningún test
  ejercita seguir operando después.
