# Deuda de diseño

Decisiones de implementación que el `DESIGN.md` todavía no refleja.

`DESIGN.md` es la especificación y no se edita sobre la marcha (ver `CLAUDE.md`). Cuando
la implementación necesita apartarse del diseño, o precisar algo que el diseño deja
abierto, la decisión se anota aquí con la fase en la que hay que llevarla al documento.
Este archivo se actualiza al cerrar cada fase.

| # | Decisión | Refleja en | Fase | Estado |
|---|---|---|---|---|
| D1 | Campo de longitud explícito en el marco de registro | sec. 7.3 | F3 | pendiente |
| D2 | CRC32 con polinomio Castagnoli | sec. 5.1 y 7.3 | F1 / F3 | pendiente |
| D3 | Los 4 bytes sin nombrar de la cabecera de página son relleno | sec. 5.1 | F1 | **decidida**, pendiente de reflejar |
| D4 | El caché del pager no está acotado; la regla de desalojo vive en la escritura | sec. 7.5 | F3 | **cerrada** en la F3 |
| D5 | El campo `libre` de la cabecera es derivable de `nceldas`: se escribe, no se lee | sec. 5.1 | F2 | **decidida**, pendiente de reflejar |
| D6 | `Key` y `Value` devuelven subsectores de la página, no copias | — | F3 | **cerrada** en la F3 |
| D7 | La derivación del tope de 1000 bytes no descuenta el directorio de slots | sec. 4 y `NO-GOALS.md` | F2 | **decidida**, pendiente de reflejar |
| D8 | `Validate()` no comprueba el CRC de las páginas libres | sec. 6 | F3 | **cerrada** en la F3, con el matiz de D11 |
| D9 | Un `Put` que falla a medias deja el grupo abierto: no hay camino de aborto | sec. 7.3 | sin asignar | **sigue abierta**, análisis revisado |
| D10 | El `fsync` de directorio no existe en Windows: no-op documentado | sec. 7.4 | F3 | **decidida**, limitación permanente |
| D11 | El invariante 6 no puede ser cierto para una ranura materializada por extensión | sec. 6 y 7.6 | **la decides tú** | contradicción del diseño consigo mismo |

## D1 · El marco de registro lleva un campo de longitud explícito

**Qué se decidió.** El marco de `internal/record` es
`| lsn (8) | tipo (1) | epoca (4) | long (4) | carga | crc32 (4) |`. La sec. 7.3 del
`DESIGN.md` dibuja los dos registros del WAL (imagen y commit) sin campo de longitud:
la deducen del tipo, porque los dos son de tamaño fijo dado el tipo.

**Por qué.** Los dos registros de la sec. 7.3 son de tamaño fijo dado el tipo, así que
el WAL del diseño, tal como está dibujado, no tiene registros de longitud variable --
pero el criterio de terminación de la F0 en la sec. 10 pide exactamente eso ("lees y
escribes registros de longitud variable en un archivo"). Con el campo de longitud
explícito, el lector delimita y valida un registro sin conocer su tipo ni su semántica,
lo que hace significativo el fuzzing que exige la sec. 9.4 (un fuzzer que solo probara
dos tamaños fijos apenas ejercitaría el formato) y evita que un tipo de registro nuevo,
el día que aparezca, corte una recuperación en seco por no saber cuánto leer. Coste: 4
bytes sobre 4121 en una imagen de página, un 0,1%.

**Dónde reflejarlo.** Los dos diagramas de la sec. 7.3, añadiendo el campo `long (4)`
entre `epoca` y la carga en ambos.

**Fase.** F3, al implementar el WAL sobre este paquete. No antes: modificar la sec. 7.3
tres fases antes de tocarla sería adelantar trabajo sobre un diseño que aún puede
cambiar mientras tanto.

## D2 · CRC32 con polinomio Castagnoli en vez de IEEE

**Qué se decidió.** `crc32.Castagnoli` (`hash/crc32` de la librería estándar) en todos
los checksums del proyecto: registros de log (sec. 7.3) y, cuando llegue la F1,
páginas (sec. 5.1).

**Por qué.** El diseño dice "crc32" y nombra `hash/crc32` en la sec. 3, pero no
especifica polinomio. Esto no es una contradicción con el diseño sino una precisión que
le falta, y conviene fijarla en un solo sitio antes de que aparezcan dos
implementaciones distintas en fases distintas. Castagnoli tiene mejor distancia mínima
que IEEE para bloques de este tamaño y cuenta con aceleración por hardware (SSE4.2) en
amd64. Sigue siendo librería estándar, así que no toca el compromiso de cero
dependencias de la sec. 3.

**Dónde reflejarlo.** Sec. 5.1 (campo `crc32` de la página) y sec. 7.3 (campo `crc32`
del registro), nombrando el polinomio en ambas.

**Fase.** F1 para la página, F3 para el registro del WAL. Se puede cerrar de una vez en
la F1 si se prefiere no arrastrar la anotación.

## D3 · Los 4 bytes sin nombrar de la cabecera de página

**Estado: decidida.** Son **relleno reservado, siempre a cero**. Queda pendiente
reflejarlo en la sec. 5.1 del `DESIGN.md`, con el texto que se propone al final de esta
entrada.

A diferencia de D1 y D2, esta entrada no nació como una decisión de implementación sino
como una discrepancia del diseño consigo mismo. La F1 avanzó con la interpretación de
abajo como provisional y se cerró confirmándola.

**Qué no cuadraba.** La sec. 5.1 declara una cabecera de **40 bytes**, pero los campos que
enumera su lista suman **36**:

```
crc32(4) + page_id(8) + page_lsn(8) + tipo(1) + flags(1) + nceldas(2)
  + libre_fin(2) + libre(2) + enlace(8) = 36
```

El diagrama de la misma sección marca los offsets `0, 4, 12, 20, 21, 22, 24, 26, 32, 40`.
Entre la marca 26 y la 32 hay un solo campo dibujado -- `libre` -- ocupando seis bytes,
cuando el texto dice que mide dos. Los 4 bytes que faltan viven ahí, entre el offset 28
y el 32, y el documento no los nombra.

**Layout que se fija:**

```
libre_fin  24..26   uint16
libre      26..28   uint16
relleno    28..32   4 bytes, siempre a cero
enlace     32..40   uint64
```

**Por qué esta y no otra.** Es la única lectura que respeta a la vez las tres cosas que
el documento sí afirma sin ambigüedad -- cabecera de 40 bytes, `libre_fin` y `libre` de
2 bytes cada uno, y `enlace` en el offset 32 marcado en el diagrama -- y además deja
`enlace`, que es un `uint64`, alineado a 8 bytes dentro de la página.

Se consideraron y se descartaron dos alternativas:

- **Que los 4 bytes fueran un campo que se perdió al escribir el documento.** El único
  candidato serio era un contador de *bytes muertos* dejados por las celdas borradas --
  el `fragmented free bytes` que SQLite sí tiene y esta cabecera no --, para decidir si
  una página se compacta en vez de dividirse. Se descarta porque ese dato **es
  derivable**: recorrer el directorio de slots y sumar el tamaño de las celdas da el
  espacio libre incluida la fragmentación, en ≤200 iteraciones sobre memoria que ya está
  en el caché, en un proyecto cuya sec. 2 declara que no compite en rendimiento.
  Persistirlo no cuesta 2 bytes: cuesta **estado redundante que puede discrepar de la
  verdad**. Un contador desactualizado tras un borrado produce una página que se cree
  llena estando vacía, y el fallo aparece miles de operaciones después de su causa --
  el riesgo que la sec. 11 nombra explícitamente.

  Es el mismo razonamiento que la sec. 6.1 ya hizo para no persistir el conjunto de
  libres: reconstruir es O(n), pero a cambio la ventana de inconsistencia desaparece por
  construcción. Persistir aquí lo que allí se decidió derivar sería contradecir esa
  decisión en la misma cabecera.

- **Compactar la cabecera a 36 bytes**, con `enlace` en el offset 28. Gana 4 bytes de
  cuerpo por página, un 0,1%. Cuesta contradecir el offset 32 del diagrama, desalinear un
  `uint64` dentro de la página, y reescribir la derivación de la sec. 4 (4056/4 pasa a
  4060/4; el límite sigue dando 1000, pero el texto dejaría de cuadrar). Mal cambio.

**Qué se gana dejándolo reservado.** No es aplazar la decisión. Un campo reservado, a cero
y **cubierto por el CRC**, es un punto de extensión gratuito: el día que haga falta un
campo nuevo, todas las páginas escritas hasta entonces tienen ahí un valor conocido. Eso
es exactamente lo que no se tendría si `EncodeTo` dejara en esos bytes lo que hubiera en
el marco reutilizado del pager. Lo comprueba `TestRellenoACero`.

**Dónde reflejarlo.** Sec. 5.1: añadir a la lista de campos

> **relleno (4):** reservado, siempre a cero. Está cubierto por el CRC, así que su valor
> es conocido en toda página escrita con esta versión del formato: el día que se convierta
> en un campo con significado, no hay páginas antiguas con basura en él.

y marcar el offset 28 en el diagrama, entre `libre` y `enlace`.

**Fase.** F1. Decidida al cerrarla.

## D4 · El caché del pager no está acotado

**Qué se decidió.** El caché de páginas del pager no tiene tamaño máximo: una página
entra al leerse o al asignarse y se queda residente. Lo que la libera es el checkpoint,
no un desalojo por presión de memoria.

La sec. 7.5 habla de "un caché de tamaño acotado" que desaloja, y la regla que enuncia
--no bajar una página sucia mientras `wal_flushed_lsn < page_lsn`-- está pensada para ese
desalojo. La regla **sí está implementada**, en `writePage`, que es el único camino por el
que una página llega a `datos.db`.

**Por qué.** Un caché acotado necesita saber qué páginas están en uso para no desalojar
una que el árbol tiene en la mano; eso es un contador de fijación (`pin`/`unpin`) en cada
`Get`, y una API que el `DESIGN.md` no pide y que la F2 puede filtrar en cualquier
descenso. A cambio no compra nada todavía: sin concurrencia y con un checkpoint que vacía
el conjunto de sucias, la memoria residente entre checkpoints está acotada por el propio
intervalo de checkpoint.

Lo que sí importaba era que la regla de la sec. 7.5 no quedara como código muerto hasta el
día que el caché se acote. Poniéndola en `writePage`, el checkpoint pasa por ella en cada
corrida --el paso 1 de la sec. 7.4 baja páginas sucias, y si alguna tuviera su `page_lsn`
por delante de lo sincronizado, esa escritura sería exactamente la que el log no puede
reparar--. Está probada en `TestReglaDeDesalojo` y `TestWriteAheadIrreparable`.

**Dónde reflejarlo.** Sec. 7.5, precisando que la regla se aplica en toda bajada a
`datos.db` --checkpoint incluido-- y no solo en el desalojo, y que el caché acotado es una
posibilidad futura y no una premisa.

**Fase.** F3, junto con el resto del checkpoint. Si en la F4 el arnés necesita forzar
desalojos para provocar el escenario de la sec. 7.5, es ahí donde el caché se acota y
aparece la fijación.

## D5 · El campo `libre` de la cabecera es derivable de `nceldas`

**Qué se decidió.** El área de slots empieza en el offset 40 y ocupa 2 bytes por celda, así
que `libre` es **siempre** `HeaderSize + 2·nceldas`. `internal/node` lo escribe porque el
formato de la sec. 5.1 lo declara, pero no lo lee nunca como fuente de verdad: lo deriva al
mutar (`syncFree`) y contrasta las dos cuentas al validar (`Check`). Una página cuyo `libre`
discrepe de su `nceldas` es un nodo mal formado, no una página con otra disposición.

**Por qué.** Es el razonamiento de D3 aplicado al campo de al lado. Un segundo lugar donde
vive el mismo dato es un lugar donde el dato puede discrepar, y una discrepancia aquí no da
un error: da una página que se cree más llena o más vacía de lo que está, y el fallo aparece
miles de operaciones después de su causa —el riesgo que la sec. 11 nombra—. Derivarlo cuesta
una suma; leerlo cuesta tener que mantenerlo correcto en cada uno de los caminos de mutación
para siempre.

No se elimina del formato, y esto es deliberado: quitarlo obligaría a mover `libre_fin`, a
contradecir el diagrama de la sec. 5.1 y a reescribir la aritmética de la sec. 4, que es
exactamente el mal cambio que D3 ya descartó para los 4 bytes de relleno. Un campo redundante
pero verificado es barato; un cambio de layout no lo es.

Lo comprueban `TestLibreEsDerivable` y el caso `libre desincronizado de nceldas` de
`TestCheckDetectaCorrupcion`.

**Dónde reflejarlo.** Sec. 5.1, en la descripción de `libre_fin (2), libre (2)`: precisar que
`libre` es el final del directorio de slots, que se deriva de `nceldas`, y que su
discrepancia es una comprobación de integridad y no una variante válida del formato.

**Fase.** F2. Decidida al escribir la capa de celdas.

## D6 · `Key` y `Value` devuelven subsectores de la página, no copias

**Qué se decidió.** Los accesores de `internal/node` devuelven slices que apuntan al cuerpo
de la página. Son válidos hasta la siguiente mutación de ese nodo. Quien necesite conservar
una clave o un valor más allá de ese punto, copia.

**Por qué.** Cada nivel de cada descenso hace del orden de ocho comparaciones de clave, y
copiar en el accesor pondría una asignación en cada una: una asignación por comparación, no
por operación. La página ya paga exactamente una copia, al decodificarse
(`internal/page/page.go`, `Decode`), y esa es la que garantiza que el nodo no aliase el buffer
de E/S reutilizado del pager. Pagarla otra vez aquí no compra ninguna garantía nueva.

**Qué obliga.** El `Get` público de la sec. 4 devuelve `[]byte` al llamador, que lo conservará
todo lo que quiera. **Ese `Get` tiene que copiar antes de devolver.** Si no lo hace, un `Put`
posterior sobre la misma página le cambia el valor bajo los pies a quien ya lo tenía, y el
motor devuelve datos que nadie escribió sin que ninguna comprobación de integridad se entere:
la página es coherente, el CRC es correcto, y el error está en el pasado del llamador.

Esta entrada existe por eso. No hay nada que corregir en el `DESIGN.md` —es una decisión de
implementación que el documento no contradice—, pero es una obligación que cruza de fase y
que en la F3 no tendría de dónde deducirse.

**Dónde reflejarlo.** En ningún sitio del `DESIGN.md`. Se cierra cuando el `Get` de la F3
copie y su test lo compruebe: mutar la página después de un `Get` y exigir que el valor
devuelto no cambie.

**Fase.** F3.

## D7 · La derivación del tope de 1000 bytes no descuenta el directorio de slots

**Qué se decidió.** El tope de la sec. 4 —`clave + valor + 6 ≤ 1000`— **se mantiene tal cual**.
Lo que hay que corregir es la aritmética con la que el documento lo justifica, no el número.

**Qué no cuadra.** La sec. 4 (y `NO-GOALS.md`, que la repite) razonan así: hacen falta al
menos cuatro celdas por página para que una hoja llena se pueda dividir en dos mitades
razonables; 4096 − 40 = 4056 bytes útiles; 4056 / 4 ≈ 1014; de ahí el tope de 1000.

El paso de los 4056 bytes útiles ignora el directorio de slots, que son 2 bytes por celda
(sec. 5.1). Con cuatro celdas hay cuatro slots, así que el espacio real para las celdas es
4056 − 8 = 4048, y 4048 / 4 = **1012**, no 1014.

**Por qué el número no cambia.** El tope de 1000 sigue por debajo de 1012 con 12 bytes de
holgura por celda, así que la propiedad que el número persigue —que cuatro celdas del tamaño
máximo quepan en una página— se sostiene. Lo comprueba `TestCuatroCeldasDeMilBytes`, que mete
las cuatro y registra cuánto sobra. Corregir la aritmética a la baja tampoco cambiaría el
tope: 1000 es un número redondo elegido por debajo de la cota, y sigue estándolo.

Se anota igualmente porque una derivación que no cuadra invita a que alguien la rehaga mal.
Si un día se sube el tope apoyándose en el 1014 del documento, el resultado es una página
donde la cuarta celda no entra —y el fallo aparecería como una división que no puede
progresar, no como un error de tamaño.

**Dónde reflejarlo.** Sec. 4, en el párrafo de la derivación: descontar los 2 bytes de slot
por celda y dar 4048 / 4 = 1012 como cota. Y el mismo párrafo repetido en `NO-GOALS.md`.

**Fase.** F2. Detectada al implementar el límite.

## D8 · `Validate()` no comprueba el CRC de las páginas libres

**Qué se decidió.** La comprobación del invariante 6 en `internal/tree/validate.go`
(`particion`) verifica la partición —que toda página de `[2, total_pages)` está en
exactamente uno de los dos conjuntos— pero **no lee las páginas libres**. La otra mitad de
lo que pide el invariante queda para la F3.

**Qué dice el invariante.** La sec. 6, invariante 6: *"Toda página de ambos conjuntos tiene
CRC y `page_id` válidos."* Las alcanzables lo cumplen por construcción, porque el barrido las
lee con `pager.Get`, que las decodifica con `page.Decode` y verifica las dos cosas. Las
libres no las lee nadie.

**Por qué no se puede comprobar todavía.** En la F2 una página puede asignarse y liberarse
sin llegar nunca a `datos.db`: `pager.Alloc` la crea en memoria y `pager.Free` la saca del
caché y del conjunto de sucias, así que su ranura en el archivo sigue a ceros. Un CRC de
ceros es inválido. Leer las libres en `Validate()` hoy pondría en rojo un árbol perfectamente
sano, y esa es la clase de falso positivo que la sec. 6 declara inaceptable para la F4 al
argumentar por qué el 40% de ocupación deja de ser invariante.

Esa mitad del invariante solo tiene sentido cuando el checkpoint ha materializado el archivo
entero, que es la sec. 7.4 y por tanto la F3.

**Qué falta hacer en la F3.** Extender `particion` para leer cada página libre y exigir CRC y
`page_id` válidos, y llamarla desde la recuperación (sec. 8) donde la precondición sí se
cumple. El comentario de `particion` ya nombra esta deuda.

**Fase.** F3. Detectada al escribir `Validate()` en la F2.

## D9 · Un `Put` que falla a medias deja el grupo de commit abierto

**Qué se decidió.** `tree.Put` abre el grupo con `pager.BeginGroup` y lo cierra con
`CommitGroup`. Si algo falla entre medias, **el grupo se queda abierto** y toda operación
posterior falla con `ErrGroupOpen` o `ErrNoGroup`. No hay `AbortGroup`.

**Por qué no lo hay.** El contrato del pager que la sec. 10 manda cerrar en la F1 enumera
cinco operaciones —`alloc`, `get`, `markDirty`, `beginGroup`, `commitGroup`— y ninguna es un
aborto. Añadirlo ahora sería abrir un contrato que el diseño declara cerrado, y hacerlo para
resolver un caso que en la F2 no puede alcanzarse por un error del llamador: los límites de
tamaño de la sec. 4 se comprueban **antes** de abrir el grupo, precisamente para eso, y lo
fija `TestPutRechazaTamanosSinEnvenenarElPager`.

Lo que queda como fuente de error dentro del grupo es una página que no es lo que dice ser:
CRC malo, `page_id` que no corresponde, un cuerpo que `node.Check` rechazaría. Con esa clase
de fallo el árbol en memoria ya está a medias, y sin log no hay forma de deshacerlo: las
páginas mutadas están en el caché del pager y el estado anterior no existe en ninguna parte.
Un pager que se niega a seguir es entonces más honesto que uno que continúa sobre una
estructura rota.

**Por qué esto no es el mecanismo de atomicidad.** La atomicidad del `Put` no depende de este
camino sino del grupo de commit de la sec. 7.3: un grupo sin registro de commit no se
reaplica en la recuperación, así que en disco el `Put` a medias no existe. Lo que falta es
solo la recuperación **en memoria**, y esa necesita el WAL para volver a leer las páginas
buenas.

**Qué falta hacer en la F3.** Cuando el WAL exista, el aborto es releer del log las páginas
del grupo o descartarlas del caché para que el siguiente `Get` las traiga de `datos.db`.
Entonces sí tiene sentido añadir la operación al pager, con el estado anterior recuperable
detrás.

**Fase.** F3. Detectada al implementar `Put` en la F2.

## D10 · El `fsync` de directorio es un no-op en Windows

**Qué se decidió.** El sync de directorio pasa por `fsx.Dir.Sync()`. En Unix hace el
`fsync` de verdad sobre el directorio; en **Windows no hace nada y devuelve `nil`**.

**Por qué.** La sec. 7.4 exige `fsync` de directorio tras crear, rotar o borrar un
archivo, y da la razón: sin él, en ext4/XFS la creación del WAL puede no ser duradera —
`Put` devolvería `nil` y tras la caída el archivo de log no existiría.

Windows no ofrece esa operación. `FlushFileBuffers` exige un handle con permiso de
escritura y un handle de directorio no lo admite: la llamada devuelve
`ERROR_ACCESS_DENIED`. No hay equivalente en la API Win32, así que no es cuestión de dar
con la llamada correcta — la garantía no está disponible en la plataforma.

Se descartó *intentarlo y tragarse el error*, que da el mismo efecto práctico pero deja el
camino de fallo indistinguible de un error de E/S real: el día que el `fsync` de
directorio falle en Linux por una razón de verdad, ese código lo ignoraría igual.

**Qué se pierde, concretamente.** En Windows, una caída inmediatamente posterior a una
rotación puede dejar sin materializar la entrada de directorio del WAL nuevo. Lo
confirmado sigue a salvo: el checkpoint que provocó la rotación ya hizo `fsync` de
`datos.db` (paso 2) y de la meta (paso 4), así que lo que puede faltar es un archivo de
log **vacío**, que la reapertura vuelve a crear. La pérdida sería real en el caso inverso
—un WAL viejo que se creía borrado y sigue ahí—, y contra eso las defensas 1 y 2 de la
sec. 7.4 (LSN monótono con contigüidad, y `epoca` verificada) siguen operativas en las dos
plataformas. Son tres defensas precisamente para que ninguna sea la única.

**Dónde se ejercita el camino real.** En el CI de Linux de la F6. El disco falso de la F4
modela la operación igual en las dos plataformas, así que los tests de orden no dependen
de en cuál se corran.

**Dónde reflejarlo.** Sec. 7.4, en el párrafo del `fsync` de directorio: nombrar que la
operación no existe en Windows y que allí las defensas 1 y 2 son las que sostienen la
regla del primer CRC inválido.

**Fase.** F3. Detectada al escribir `internal/fsx`.

---

## Cierres de la F3

### D4 · cerrada

El caché sigue sin acotar, que es lo que la entrada decidía, y la regla de desalojo de la
sec. 7.5 dejó de ser código que solo ejercitaba su propio test: ahora la atraviesa **cada
corrida del checkpoint**, porque el paso 1 de la sec. 7.4 baja páginas sucias por
`writePage`. Con el WAL real detrás, `FlushedLSN` puede quedarse de verdad por detrás de lo
asignado entre commits, cosa que `NopLog` no podía modelar.

Queda pendiente de reflejar en la sec. 7.5 que la regla se aplica en toda bajada a
`datos.db` y no solo en el desalojo, y que el caché acotado es una posibilidad futura y no
una premisa. Si la F4 necesita forzar desalojos, es ahí donde el caché se acota y aparece la
fijación.

### D6 · cerrada

`tree.Get` copia con `bytes.Clone` y el `Get` público la propaga sin deshacerla.

Lo que hay que anotar es que el primer test que lo comprobaba **no probaba nada**: quitar la
copia no lo ponía en rojo. Insertar en otras hojas no toca los bytes de una celda ya
escrita, y una sustitución tampoco la reescribe en su sitio. Lo que sí mueve esos bytes es la
**compactación** (`node.Compactar`), que reordena todas las celdas hacia el final del cuerpo.
Se fuerza sustituyendo la misma clave una y otra vez, porque cada sustitución es un borrado
más una inserción y deja una celda muerta que consume el espacio contiguo. Con el test así,
quitar la copia hace que el valor devuelto se convierta en el de otra clave.

### D8 · cerrada, con el matiz de D11

La comprobación existe y corre en la recuperación (`internal/recovery`, `comprobarLibres`),
que es donde se cumple su precondición: los pasos 5 y 6 de la sec. 8 acaban de materializar
el archivo entero.

No vive en `internal/tree` como anticipaba la entrada, y por dos razones que solo se ven al
escribirla. La precondición es un hecho de la recuperación y no del árbol, así que un
`Validate` que la exigiera sería un `Validate` que no se puede llamar en cualquier momento —
y llamarlo en cualquier momento es justo para lo que existe. Y leer la ranura en crudo, sin
pasar por `pager.Get`, evita meter en el caché del pager páginas que nadie va a usar.

Lo que la comprobación acepta además de una página íntegra es una ranura **a ceros**. El
porqué es D11.

### D9 · sigue abierta, y es más grande de lo que parecía

La entrada decía que el aborto, con el WAL existiendo, es "releer del log las páginas del
grupo **o** descartarlas del caché para que el siguiente `Get` las traiga de `datos.db`".
Escribiéndolo se ve que la segunda mitad de esa disyunción es incorrecta y que la primera es
bastante más trabajo del que la frase sugiere:

- **Descartar del caché no basta.** Vale solo si la página estaba limpia al entrar en el
  grupo. Una página que ya estaba sucia por un grupo anterior tiene en `datos.db` una
  versión **vieja** —sus cambios están en el log y no han pasado por un checkpoint—, así que
  el siguiente `Get` la traería obsoleta y en silencio.
- **Un snapshot al ensuciar tampoco.** El árbol muta la página y *después* llama a
  `MarkDirty`, así que para cuando el pager se entera ya no hay estado anterior que copiar.
  Capturarlo obligaría a mover la copia al `Get`, y eso es una copia de 4 KiB por página de
  cada descenso.
- **Hay que rebobinar el WAL.** Las imágenes del grupo abortado ya están anexadas. Si no se
  retrocede el LSN y el desplazamiento hasta el último commit, el grupo siguiente las
  hereda: la recuperación agrupa "las imágenes desde el commit anterior", así que su
  `n_registros` no cuadraría y el log entero a partir de ahí se descartaría con
  `ErrGrupoIncompleto`.

La forma correcta es la primera de la disyunción, completa: rebobinar el log, y para cada
página del grupo restaurar su última imagen **confirmada** de la generación actual, o
descartarla del caché si no tiene ninguna —en ese caso su último estado sí está en
`datos.db`, porque la rotación solo ocurre en un checkpoint y el checkpoint baja todas las
sucias—.

**Por qué no se hace ahora.** No es solo el tamaño. El único fallo que puede llegar hasta
dentro de un grupo es una página que no es lo que dice ser: CRC malo, `page_id` que no
corresponde, un cuerpo que `node.Check` rechazaría. Es decir, corrupción del disco. En esa
situación no está claro que recuperar el estado *en memoria* sea lo que se quiere: un pager
que se niega a seguir es información, y un motor que continúa sobre un disco que acaba de
devolver basura es un motor que va a producir más basura. La F4, que es la que va a generar
esos casos a propósito, es también la que dirá qué comportamiento quiere de ellos.

**Fase: sin asignar.** La tabla de la sec. 10 no tiene una casilla para esto, y forzarla en
la F3 sería adelantar trabajo sobre una decisión que la F4 informa. Se decide al abrir la F4.

## D11 · El invariante 6 no puede ser cierto para una ranura materializada por extensión

**Esto no es una decisión de implementación: es una contradicción del diseño consigo mismo,
y la decides tú.** Se anota aquí porque es donde va lo que el `DESIGN.md` todavía no refleja,
pero a diferencia del resto de entradas no viene con una decisión tomada.

**Qué no cuadra.** La sec. 6, invariante 6, dice:

> Toda página de ambos conjuntos tiene CRC y `page_id` válidos.

Y la sec. 7.6, más el paso 5 de la sec. 8, dicen que crecer `datos.db` es una operación de
metadatos que **ninguna imagen de página describe**, y que la extensión se materializa con
páginas cero **explícitas**.

Una ranura así está en el conjunto de libres —no es alcanzable desde la raíz— y es todo
ceros. Un CRC de ceros es inválido. Así que hay páginas que el propio diseño manda crear y
que el invariante 6 declara imposibles.

**No es hipotético.** El caso lo produce cualquier registro de commit que suba `total_pages`
sin traer imágenes, que es exactamente la forma que la sec. 7.6 le da a una extensión. Lo
reproduce `TestUnaPaginaLibreACerosEsLegitima`, y con la comprobación estricta la
recuperación falla ahí con `page: crc invalido` sobre una base perfectamente sana.

**Lo que hace la implementación mientras tanto.** `comprobarLibres` acepta una página libre
que sea íntegra **o** enteramente cero. Se conserva lo que el invariante persigue —que una
página libre no sea basura ilegible que un día se recicle y pase por datos— porque una
ranura a ceros es un valor conocido y deliberado, no basura; es el mismo argumento que D3
hace para los 4 bytes de relleno. Y al reciclarse, `pager.Alloc` entrega una página nueva a
ceros de todas formas, así que su contenido anterior no se lee nunca.

**Lo que se pierde.** Una escritura desgarrada que dejara una ranura libre entera a ceros
sería indistinguible de una extensión legítima. Es una franja estrecha, y es el precio de que
el formato de la sec. 5.1 no defina un tipo de página para las libres.

**Las tres salidas, para que elijas.**

1. **Reformular el invariante 6** en la sec. 6: *toda página de ambos conjuntos es legible —
   una página íntegra con su `page_id`, o una ranura a ceros aún sin usar—*. Es lo que el
   código hace hoy. Coste: la franja estrecha de arriba.
2. **Un tipo de página para las libres** en la sec. 5.1, escrito por la extensión con su CRC
   válido. El invariante 6 se sostiene literalmente y el desgarro deja de ser
   indistinguible. Coste: un valor nuevo en el campo `tipo`, y que extender el archivo pase
   de escribir ceros a codificar páginas — más caro, y toca un formato que ya está en disco.
3. **Dejarlo como está y no comprobar las libres**, volviendo a D8 sin cerrar. Coste: la
   mitad del invariante 6 no se comprueba nunca, que es donde estaba la F2.

**Fase.** La decides tú. La implementación actual es la opción 1 y está aislada en una sola
función, así que cambiar a la 2 o a la 3 no toca nada más.
