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
| D3 | Los 4 bytes sin nombrar de la cabecera de página son relleno | sec. 5.1 | F1 | **pendiente de decisión** |
| D4 | El caché del pager no está acotado; la regla de desalojo vive en la escritura | sec. 7.5 | F3 | pendiente |

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

**A diferencia de D1 y D2, esta entrada no es una decisión tomada: es una discrepancia
del diseño consigo mismo que hay que resolver.** La implementación de la F1 avanza con
una interpretación provisional, marcada abajo, hasta que se decida.

**Qué no cuadra.** La sec. 5.1 declara una cabecera de **40 bytes**, pero los campos que
enumera su lista suman **36**:

```
crc32(4) + page_id(8) + page_lsn(8) + tipo(1) + flags(1) + nceldas(2)
  + libre_fin(2) + libre(2) + enlace(8) = 36
```

El diagrama de la misma sección marca los offsets `0, 4, 12, 20, 21, 22, 24, 26, 32, 40`.
Entre la marca 26 y la 32 hay un solo campo dibujado -- `libre` -- ocupando seis bytes,
cuando el texto dice que mide dos. Los 4 bytes que faltan viven ahí, entre el offset 28
y el 32, y el documento no los nombra.

**Interpretación provisional adoptada.** Los 4 bytes son **relleno reservado**:

```
libre_fin  24..26   uint16
libre      26..28   uint16
relleno    28..32   4 bytes, siempre a cero
enlace     32..40   uint64
```

**Por qué esta y no otra.** Es la única lectura que respeta a la vez las tres cosas que
el documento sí afirma sin ambigüedad -- cabecera de 40 bytes, `libre_fin` y `libre` de
2 bytes cada uno, y `enlace` en el offset 32 marcado en el diagrama -- y además deja
`enlace`, que es un `uint64`, alineado a 8 bytes dentro de la página. Las alternativas
las rompen: ensanchar `libre` a 6 bytes contradice el texto, y compactar los campos
contradice el 32 del diagrama y desalinea `enlace`.

`EncodeTo` escribe esos 4 bytes a cero siempre, en vez de dejar lo que hubiera en el
buffer reutilizado del pager. Es lo que hace que las páginas escritas hoy tengan un valor
conocido ahí el día que el relleno se convierta en un campo con significado -- de otro
modo, ese día habría páginas en disco con basura del marco anterior en un campo que ya
tendría lectores.

**Qué hay que decidir.** Si el relleno se queda como reservado, la sec. 5.1 debe
nombrarlo en la lista de campos y marcar el offset 28 en el diagrama. Si en realidad
faltaba un campo que se perdió al escribir el documento, hay que declararlo ahora: el
formato en disco de la página es de las cosas más caras de cambiar más adelante, porque
a partir de la F2 habrá páginas escritas con este layout.

**Fase.** F1, antes de cerrarla. Es la fase que fija el formato de la página.

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
