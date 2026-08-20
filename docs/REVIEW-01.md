# REVIEW-01 · Revisión hostil de `DESIGN.md`

> **Revisión asistida por IA.** Generada con **Claude Code** (Anthropic), modelo Opus 5,
> el **20 de agosto de 2026**,
> a partir de la lectura completa del documento de diseño. Ningún hallazgo ha sido
> verificado contra código: en la fecha de esta revisión el proyecto no tiene
> implementación. Los escenarios de caída son razonamiento sobre el documento, no
> resultados de ejecución. Deben tratarse como hipótesis a confirmar durante las fases
> F3 y F4, no como defectos comprobados.
>
> Las referencias a secciones apuntan al DESIGN.md v1 (commit 362b724).
> La numeración cambió en la v2.
>
> Este documento **no** modifica `DESIGN.md`. Las secciones "Corrección" son propuestas.

---

## Alcance de la revisión

El documento de diseño se escribió antes del código, a propósito, y su tesis central es una
sola frase: *"si `Put` devuelve `nil`, ese dato sobrevive a cualquier caída posterior"*
(sec. 4). Todo lo demás existe para sostenerla.

Esta revisión no busca huecos ("falta especificar X"). Busca lugares donde el mecanismo
descrito, **implementado exactamente como está escrito**, produce pérdida de datos o
corrupción silenciosa. Trece hallazgos. Los cinco primeros rompen la tesis central.

---

## H1 · El WAL no tiene atomicidad de grupo. La sec. 6 nombra el problema y la sec. 7 no lo resuelve. (crítico)

La sec. 6 lo dice con todas las letras: *"una sola llamada a `Put` puede convertirse en
tres, cuatro o más escrituras físicas que deben aplicarse todas o ninguna"*. Después la
sec. 8 declara resuelto el problema con: *"Atomicidad del último registro: un registro
escrito a medias se descarta entero. El CRC es lo que lo garantiza."*

Eso es atomicidad **de registro**, no de transacción. Son cosas distintas y el documento
las confunde.

**Escenario.** Hoja `L` (pág. 40) se llena y se divide. `Put` genera cuatro imágenes de
página: `L`(40) reescrita, `R`(41) nueva, padre `P`(20) con el nuevo separador, y la meta
en memoria cambia. Se anexan cuatro registros al WAL: LSN 700→703. `fsync`. Caída durante
el `fsync`: el disco persiste 700, 701, 702 completos y 703 a medias.

Recuperación: se leen 700, 701, 702 (CRC válido), se detiene en 703. Se aplican tres
imágenes de cuatro. El padre `P` ya apunta a `R`, pero la imagen de `R`… depende de cuál
de los cuatro quedó fuera. Si el que faltó fue el de `R`, la página 41 en `datos.db` es lo
que hubiera antes: basura, ceros, o una página vieja liberada.

**Invariantes rotos:** el 6 (`P` alcanzable apunta a una página con CRC inválido), el 2
(profundidad de hojas si 41 nunca existió) y el 4 (el separador de `P` describe un
subárbol que no está ahí). Y si la página 41 contenía datos viejos con CRC *válido* —
porque fue una hoja liberada y reciclada antes— entonces `Validate()` **pasa** y el árbol
devuelve claves de otra época.

Nota adicional: el corte parcial en una **fusión** rompe el invariante 5 sin tocar ningún
otro. Fusionar `L` en su hermana izquierda `K` modifica `K` (absorbe celdas + `K.next` pasa
de `L` a `L.next`), el padre, y libera `L`. Si se aplica el padre pero no `K`, la cadena
lateral sigue pasando por una `L` que ya no está en el árbol: `Get` encuentra las claves
por el árbol y `Scan` devuelve un conjunto distinto. Dos caminos de lectura que discrepan
es el peor bug posible en un motor de almacenamiento.

**Corrección.** Un registro de *commit* al final de cada grupo, con el número de registros
del grupo (o el LSN del primero). El `fsync` de la sec. 7 paso 4 ocurre **después** del
registro de commit. La recuperación aplica solo grupos completos: un grupo sin commit se
descarta entero, no registro por registro. Esto convierte la promesa "todas o ninguna" de
la sec. 6 en un mecanismo real.

---

## H2 · El estado del asignador (raíz, cabeza de libres, total de páginas) no está en el WAL. Datos confirmados quedan inalcanzables. (crítico)

Sec. 5.2: la meta guarda `id de la página raíz`, `cabeza de la lista de libres`, `total de
páginas`. Sec. 7: la meta se escribe **solo en el checkpoint** (paso 3). Sec. 7 paso 3 del
camino de escritura: al WAL van *"las páginas modificadas"*.

El documento nunca dice si la página meta es una "página modificada" a efectos del log. Las
dos lecturas posibles llevan a dos sistemas distintos y **ambos están rotos**:

- **Si la meta NO se registra en el WAL** (lectura natural: si se registrara, el paso 3 del
  checkpoint no haría falta), entonces `root_id`, `free_head` y `total_pages` solo existen
  en memoria entre checkpoints. Se pierden en la caída.
- **Si SÍ se registra**, la recuperación (sec. 8 paso 1) lee la meta para saber desde qué
  LSN reproducir, y luego el paso 4 sobrescribe esa misma meta con una imagen del log. La
  circularidad no está resuelta en ninguna parte, y el mecanismo de alternancia de la
  sec. 5.2 pierde sentido: la reproducción escribe en la ranura que diga `page_id`, no en
  "la más antigua".

**Escenario para la primera lectura (raíz).** Checkpoint: meta dice `root=5`, LSN=500. Se
insertan claves hasta que la hoja-raíz 5 se divide: se asignan 6 y 7, la raíz nueva es la
8, `total_pages` pasa a 9. Al WAL van las imágenes de 5, 6, 7, 8. `fsync`. `Put` devuelve
`nil`. Caída.

Al reabrir: meta sigue diciendo `root=5`, LSN=500. Se reproduce el WAL: las páginas 6, 7 y
8 quedan escritas en `datos.db`, perfectas, con CRC válido. Y el motor arranca con
`root=5`, que ahora es la mitad izquierda de la división. **La mitad de las claves
confirmadas es inalcanzable.** Las páginas 6, 7 y 8 no están en el árbol ni en la lista de
libres: están huérfanas.

Lo grave es que `Validate()` **pasa en verde**. Una hoja como raíz es un B+tree
perfectamente legal. Los seis invariantes se sostienen sobre un árbol al que le falta la
mitad de los datos. El test 9.2(a) da OK y solo falla el 9.2(b) — y si el conjunto de
claves de prueba es pequeño y cabe en una hoja, ni siquiera eso.

**Corrección.** El registro de commit de H1 lleva `root_id`, `free_head` y `total_pages`.
Son 24 bytes por `Put`, no 4 KB. La recuperación los aplica al final. La meta pasa a ser
una optimización para no leer el WAL entero, no la única copia de la verdad.

---

## H3 · Lista de libres: una página puede quedar en el árbol y en la lista a la vez, y el invariante 6 está escrito al revés. (crítico)

Esta es la respuesta directa a *"¿qué pasa si el proceso muere entre el paso 3 y el 5?"* —
y la respuesta incómoda es que **esa ventana concreta es casi benigna**, mientras que el
daño real está en la ventana de la que el documento no habla: entre dos checkpoints.

Ventana 3→5 (meta escrita, WAL sin truncar): al reabrir se lee la meta nueva, se reproduce
un WAL cuyos registros ya están aplicados, y con imágenes de página completas eso es
idempotente. Sale bien. La doble meta cubre incluso la caída entre 3 y 4 (meta a medias →
CRC malo → se usa la vieja → se reproduce todo el WAL). Ese orden está bien pensado.

El problema es el otro:

**Escenario (asignación desde la lista de libres).**
- `t0` Checkpoint. Meta: `root=20`, `free_head=30`, `total=1000`, LSN=500. La página 30 está
  libre y en su cuerpo guarda `next=31`.
- `t1` Un `Put` divide una hoja. El asignador saca la 30 de la lista: en memoria
  `free_head := 31`.
- `t2` Al WAL van las imágenes de la pág. 30 (ahora hoja derecha, con datos reales) y de la
  pág. 20 (padre con el separador nuevo). `fsync`. `Put` devuelve `nil`.
- `t3` Caída.

Recuperación: la meta no se tocó desde `t0`, así que `free_head=30`. Se reproducen las
imágenes: la 20 ahora apunta a la 30, y la 30 contiene una hoja viva. Resultado:

> **La página 30 está simultáneamente en el árbol y en la cabeza de la lista de libres.**
> Invariante 6, violado literalmente.

Y no se detecta: el documento nunca dice que `Validate()` recorra la lista de libres, y sin
recorrerla no hay forma de notarlo. El siguiente `Put` que necesite una página asigna **otra
vez** la 30, la sobrescribe con una hoja nueva, y todas las claves de la hoja anterior
desaparecen. El árbol queda estructuralmente válido y silenciosamente incompleto: `Validate()`
en verde, datos perdidos. Es el bug que la sec. 11 describe como *"bugs que aparecen miles de
operaciones después de su causa"*, pero la mitigación propuesta (`Validate()` tras cada
operación) **no lo detecta** porque la comprobación necesaria no está en la lista.

**El caso espejo** (una fusión libera la pág. 30, la meta se queda con `free_head=10`):
la página no está en el árbol ni en la lista. Fuga permanente. Y esto **no viola el
invariante 6 tal como está escrito** — *"ninguna página está en la lista de libres y en el
árbol a la vez"* prohíbe la doble pertenencia, no la pertenencia nula. Tal como está
redactado, un motor que filtra el 100% del archivo pasa la validación.

**Corrección, dos partes.**

1. Reescribir el invariante 6 como una **partición**: toda página en `[2, total_pages)` está
   en exactamente uno de dos conjuntos — alcanzable desde la raíz, o en la lista de libres.
   Ni en los dos, ni en ninguno. `Validate()` recorre ambos y comprueba que la unión es
   completa y la intersección vacía. Esto convierte tanto la fuga como el doble uso en
   fallos detectables, que es lo que hace falta para que la tabla de la F4 signifique algo.
2. **Recomendación fuerte: no persistir la lista de libres.** Reconstruirla en el `Open`
   barriendo las páginas alcanzables y marcando el complemento. Es O(n) al abrir — pero la
   sec. 8 paso 6 ya recorre el árbol entero con `Validate()`, así que el costo ya está
   pagado. A cambio, la ventana de inconsistencia **desaparece por construcción** y el
   invariante 6 se vuelve cierto por definición en vez de por vigilancia. Para un proyecto
   cuyo objetivo declarado es demostrar durabilidad, eliminar una clase entera de bug de
   crash vale más que un `Open` rápido.

   Si aun así se persiste: las páginas libres necesitan CRC válido y verificado (hoy el
   invariante 6 solo exige CRC a las páginas *alcanzables*, y una libre no lo es). Sin eso,
   un torn write en una página libre da un `next` de basura y el asignador entrega la
   página `0x4141…` — o la 0, que es la meta.

---

## H4 · `valor hasta 1 MB` no cabe en una página de 4096 bytes. (crítico, y es una contradicción literal)

Sec. 4: *"clave hasta 512 bytes, valor hasta 1 MB. Ambos caben holgadamente en una página,
lo que evita tener que implementar páginas de desbordamiento."*

1 MB = 1.048.576 bytes. La página son 4096. No caben, ni holgada ni apretadamente.

No es una errata sin consecuencias: es la premisa que justifica **no implementar páginas de
desbordamiento**, y esa decisión propaga a todo el resto. El formato de registro del WAL
(sec. 7) es de ancho fijo — `| longitud (4) | 4096 bytes de página |` — así que un valor
grande tampoco tiene representación en el log. Y el propio layout está internamente
partido: `val_len` son 4 bytes (hasta 4 GB, coherente con valores grandes) mientras el
directorio de slots es `uint16` (offsets hasta 65535, incompatible con páginas grandes).

**Corrección.** Fijar el límite de valor en función del invariante 3, no al revés. Para que
una hoja siempre pueda dividirse en dos mitades que ambas superen el 40%, hace falta que
quepan al menos ~4 celdas por página: con 4096 − 32 de cabecera, eso da un tope realista
de **~1000 bytes por par clave+valor**. Documentar que valores mayores requieren páginas de
desbordamiento y que están explícitamente fuera de alcance (va a `NO-GOALS.md`).

---

## H5 · Al nodo interno le falta exactamente un puntero de hijo. (alto)

Sec. 5.1: `Formato de celda en un nodo interno: key_len (2) | hijo_izq (8) | clave`.

Un nodo con *n* claves separadoras tiene *n+1* hijos. Con una celda por clave y un puntero
por celda, se direccionan *n*. **El hijo más a la derecha no tiene dónde vivir.** O va en
los 20 bytes reservados de la cabecera (el documento no lo dice) o el subárbol derecho del
último separador es inalcanzable — lo que rompe el invariante 4 en cuanto haya más de una
clave, y el 2 en cuanto haya una división.

Lo mismo con las hojas: la sec. 6 dice que cada hoja guarda *"un puntero a la hoja
siguiente"*, y en el layout de 32 bytes de cabecera no hay campo para él. El invariante 5
depende de un puntero que el formato no declara.

**Corrección.** Usar los 20 bytes reservados: `hijo_derecho (8)` para tipo=1,
`hoja_siguiente (8)` para tipo=2. Dibujarlo en el diagrama.

---

## H6 · Las páginas meta: el CRC detecta corrupción, nunca detecta identidad. Y la alternancia no está protegida. (alto)

*"¿El CRC alcanza?"* — para lo que el documento le pide, sí; el problema es que le pide poco.
Un torn write deja mitad vieja + mitad nueva y el CRC32 falla con probabilidad
1 − 2⁻³². Como detector de desgarro, sobra. Tres cosas que **no** cubre:

**(a) Nada garantiza que la meta se escriba en la ranura correcta.** La meta contiene
`root_id` y `free_head`, que cambian en operación normal, así que la copia en caché de la
meta está *sucia* casi siempre. El paso 1 del checkpoint dice *"escribir al archivo de
datos todas las páginas sucias"*. Si la meta es una página del pager como cualquier otra
—y el documento no la excluye— entonces el paso 1 escribe la meta **actual** (la más
nueva) y el paso 3 escribe la **más antigua**. Las dos ranuras quedan con estado de la
misma época y la garantía de la sec. 5.2 ("si te caes escribiendo una meta, la otra sigue
intacta") deja de valer.

*Escenario en la ventana 3→5 que se preguntaba:* meta viva en pág. 0, meta vieja en pág. 1.
Paso 1 vuelca la pág. 0 sucia junto al resto. Paso 2 `fsync`. Paso 3 escribe la pág. 1.
Caída antes del `fsync` del paso 4, con la pág. 1 desgarrada. Al reabrir: pág. 1 con CRC
malo, y pág. 0 con un estado que puede ser también parcial (se escribió en el paso 1 sin
que nada garantice que fue atómica). **Las dos metas inválidas.** Sec. 8 paso 1 declara el
archivo irrecuperable — teniendo el WAL entero intacto en el disco.

**(b) Ese "irrecuperable" contradice la garantía de la sec. 4.** La durabilidad de `Put`
descansa exclusivamente en el `fsync` del WAL (paso 4 de la sec. 7). El WAL contiene todo lo
confirmado. Declarar muerto el archivo porque las dos metas fallaron el CRC convierte una
situación **totalmente recuperable** en pérdida permanente. Si ambas metas son inválidas, el
camino correcto es reproducir el WAL desde el LSN 0 y reconstruir la meta, no rendirse. Con
imágenes de página completas eso funciona: es el argumento que el propio documento da en la
sec. 7 para elegir imágenes en vez de log lógico.

**(c) El CRC valida contenido, no ubicación.** No hay `page_id` dentro de la página. Una
escritura mal dirigida —bug de aritmética de offsets, reproducción aplicando una imagen al
lugar equivocado, `total_pages` desalineado— produce una página con CRC perfectamente
válido en la ranura equivocada, y nada en el sistema lo nota. InnoDB y SQL Server guardan el
número de página dentro de la página exactamente por esto. Tampoco hay `page_lsn`, lo que
además impide (ver H8) hacer cumplir la regla del write-ahead.

**Corrección.** `page_id (8)` y `page_lsn (8)` en los 20 bytes reservados (junto con H5 no
caben los tres campos: subir la cabecera a 40 bytes). Al leer, verificar
`page_id == id_esperado` además del CRC. Excluir las páginas meta del conjunto de páginas
sucias del pager: se escriben **solo** en el paso 3 del checkpoint, por su propio camino.
Y cambiar la sec. 8 paso 1: dos metas inválidas ⇒ reproducir el WAL completo desde 0, no
abortar.

---

## H7 · Truncar el WAL en sitio hace que la regla "el primer CRC inválido marca la caída" sea falsa. (alto)

Sec. 8 paso 3 es enfática: *"El primer registro con CRC inválido marca el punto exacto donde
ocurrió la caída: ahí se detiene la lectura y se descarta todo lo que siga, **incluso si más
adelante hubiera registros aparentemente válidos**."*

Esa regla es sólida solo si el WAL nunca se reescribe sobre bytes ya usados. Pero el paso 5
del checkpoint lo trunca y después se vuelve a anexar desde el offset 0. Además, el
documento **nunca dice si el LSN se reinicia al truncar** o es monótono global.

**Escenario.** Checkpoint trunca el WAL, que tenía 900 registros. El `Truncate` es una
operación de metadatos del sistema de archivos y el documento **no hace `fsync` después**
(paso 5, ni del archivo ni del directorio). Caída. El sistema de archivos puede quedar con
la longitud vieja y los bytes viejos. Al reabrir se anexan registros nuevos que ocupan
menos que los viejos. La recuperación lee los nuevos y, al terminarlos, encuentra registros
antiguos **con CRC perfectamente válido** — porque son registros reales, solo que de antes
del checkpoint. La regla del "primer CRC inválido" no dispara: no hay ningún CRC inválido.
Se reproducen imágenes de página anteriores al checkpoint **encima** de datos posteriores.

Si además el LSN se reinicia, ni siquiera el orden de LSN permite distinguirlos.

**Corrección.** Tres medidas, cualquiera basta pero conviene el conjunto: (1) LSN
estrictamente monótono, que nunca se reinicia, y la recuperación exige contigüidad —
`lsn[i+1] == lsn[i]+1`, cualquier salto termina la lectura; (2) un campo `epoca` (id de
generación del WAL) en la cabecera de cada registro, verificado contra el de la meta;
(3) en vez de truncar en sitio, **rotar**: escribir en `datos.wal.N+1` y borrar el anterior
tras el `fsync` del directorio. La rotación elimina la clase entera de bug y cuesta veinte
líneas.

Y en cualquier caso: `fsync` del **directorio** después de crear, truncar o rotar. Sin él,
en ext4/XFS la creación de `datos.wal` puede no ser durable — es decir, `Put` devuelve
`nil` y tras la caída el archivo del log no existe.

---

## H8 · El caché de páginas puede escribir una página sucia a `datos.db` antes de que su registro esté en el WAL. (alto)

Sec. 7 enuncia la regla —*"nada se modifica en el archivo de datos antes de que la
descripción del cambio esté sincronizada en el log"*— y después describe un camino de
escritura que la aplica al orden de las llamadas (paso 2 memoria, paso 3 log), pero nunca
al **desalojo** del caché. Un caché de páginas tiene tamaño acotado; desaloja. Si desaloja
una página sucia escribiéndola a `datos.db` antes del `fsync` del paso 4, la regla del WAL
queda violada aunque el código la respete línea a línea.

**Escenario.** Caché de 64 páginas. Una división que toca 4 páginas provoca el desalojo de
la página `P` de un `Put` anterior, aún no sincronizado. `P` se escribe en el medio de
`datos.db`; la escritura se desgarra; caída antes del `fsync` del WAL. `datos.db` tiene `P`
a medias y el WAL no tiene con qué repararla, porque su registro se perdió. Invariante 6
roto sobre una página alcanzable, sin log de reparación.

**Corrección.** Regla explícita en la sec. 7: *una página sucia no puede escribirse a
`datos.db` mientras `wal_flushed_lsn < pagina.page_lsn`*. Requiere el `page_lsn` de H6 —
por eso los dos hallazgos se corrigen juntos. Si el desalojo encuentra una página en esa
situación, fuerza antes el `fsync` del WAL.

---

## H9 · La recuperación no termina en un checkpoint: el motor arranca sobre una meta que acaba de demostrarse obsoleta. (medio)

Sec. 8 aplica imágenes (paso 4), hace `fsync` (paso 5), valida (paso 6) — y ahí termina. No
escribe la meta ni trunca el WAL.

Como reproducir es idempotente, reabrir otra vez no rompe nada. Pero el motor queda
**operando** con el `root_id` y el `free_head` de la meta vieja, que es exactamente el
estado que H2 y H3 acaban de identificar como incorrecto. El primer `Put` tras la
recuperación desciende por el árbol viejo y escribe encima, divergiendo más. Además, el WAL
nunca se trunca en un ciclo caída-recuperación-caída, así que crece sin límite y cada
recuperación es más lenta que la anterior.

**Corrección.** La recuperación termina con un checkpoint completo (escribir meta con el
estado reconstruido, `fsync`, rotar el WAL). Esto hace también que la recuperación sea
**observable** en los tests: tras `Open`, la meta debe reflejar el último grupo confirmado.

---

## H10 · Invariante 3 (40%) y borrado con tombstones son mutuamente excluyentes, y el invariante 3 no es alcanzable con celdas de tamaño variable. (medio-alto, pero es el que hunde la F4)

Dos problemas distintos que se suman.

**(a) La regla de rescate se anula a sí misma.** La sec. 11 ofrece: *"se puede entregar con
borrado por marca (tombstone) sin fusión, documentando la limitación"*. Pero sin fusión, la
ocupación de una hoja puede caer a cero, y el invariante 3 dice *"todo nodo salvo la raíz
está ocupado al menos al 40%"*. La sec. 6 dice que los seis invariantes *se convierten* en
`Validate()`, y la sec. 9.2(a) exige `Validate()` en verde en los 500 puntos de caída.

Consecuencia concreta: **tomar la vía de escape del riesgo pone en rojo la tabla entera de
la F4**, que el propio plan (sec. 10, reglas de rescate) declara la parte que nunca se
recorta. La mitigación destruye la entrega. Y no es un ajuste trivial de último minuto:
requiere decidir qué significa "válido" en modo tombstone, en la semana 20, bajo presión.

Efectos colaterales que la tabla de riesgos no menciona: con tombstones, **`Delete` puede
provocar una división de página** (la marca ocupa espacio), así que `Delete` deja de ser el
caso fácil y pasa a ser tan peligroso como `Put` — un `Delete` que falla por "página llena"
es absurdo de cara al usuario. Y sin fusión no se libera nunca una página, así que la lista
de libres queda vacía y toda la maquinaria de H3 se vuelve código muerto **justo en el
camino que más probablemente se entregue**.

**(b) Con celdas de tamaño variable, el 40% no es un invariante.** Es una heurística de
llenado. Una hoja con una sola celda de 100 bytes está al 2,4%; su hermana tiene una celda
de 3000 bytes; fusionarlas no cabe y redistribuir no produce dos páginas ≥40%. **No existe
reparación legal**, y `Validate()` falla sobre un árbol perfectamente sano. Eso son falsos
positivos en la F4, es decir, semanas persiguiendo un bug que no existe. SQLite no garantiza
ocupación mínima; BoltDB usa un `FillPercent` que es una preferencia, no una regla.

**Corrección.** Bajar el 3 de "invariante" a "objetivo de llenado", y expresarlo de forma
comprobable y siempre satisfacible: *ninguna página alcanzable tiene 0 celdas* (salvo una
raíz vacía), y *ninguna pareja de hermanos adyacentes cabe entera en una sola página* —
esta segunda es la propiedad que la fusión realmente persigue y es cierta tanto con como
sin tombstones. Con tombstones activados, se relaja la segunda y se documenta. Así la vía
de escape de la sec. 11 deja de invalidar la F4.

---

## H11 · El disco falso de la sec. 9.1 no puede probar la propiedad que el diseño entero defiende. (medio-alto)

La interfaz `File` es por archivo, y hay dos archivos. Si cada instancia mantiene su propio
buffer de escrituras pendientes y su propio `Sync()`, entonces **el orden relativo entre
escrituras a `datos.wal` y escrituras a `datos.db` nunca se modela**. Y ese orden relativo
*es* el write-ahead logging. El bug de H8 —página de datos en disco antes que su registro
de log— es invisible para este arnés por construcción.

Es un error en el aparato de verificación, que la sec. 9 llama *"la parte que da valor al
proyecto"*.

Segundo problema, en la sec. 9.2(c): *"ninguna clave que nunca se escribió aparece"*. La
redacción es ambigua y la ambigüedad cuesta caro. Una clave cuyo `Put` fue **anexado al WAL
pero cuyo `fsync` no había retornado** puede sobrevivir perfectamente: el sistema operativo
puede haber volcado esos bytes por su cuenta. Eso es legal y esperado, pero si (c) se
implementa como "no aparece ninguna clave no confirmada", el test falla de forma
intermitente e irreproducible durante toda la F4.

**Corrección.** (1) Un único árbitro de orden global compartido por las dos instancias de
`File`: una sola cola de escrituras pendientes con el nombre de archivo como etiqueta, y el
`Sync()` de un archivo vacía solo sus entradas pero el descarte en la caída se decide sobre
la cola global. (2) Semilla explícita y registrada para que cada punto de caída sea
reproducible bit a bit — sin eso, `BUGS.md` no puede citar un caso. (3) Reformular (c):
*toda clave presente pertenece al conjunto de claves que se intentaron escribir*; las
confirmadas **deben** estar (b), las no confirmadas **pueden** estar o no.

---

## H12 · La extensión del archivo no está cubierta por el WAL: la reproducción puede crear agujeros. (medio)

`total_pages` vive solo en la meta (H2) y crecer el archivo es una operación de metadatos.

**Escenario.** El WAL contiene una imagen de la página 900. La meta dice `total=800` y
`datos.db` mide 800×4096. La recuperación (sec. 8 paso 4) escribe en el offset 900×4096.
Resultado: un archivo disperso con un agujero en las páginas 800-899, que se leen como
ceros y fallan el CRC. Si alguna de ellas era una página real cuyo registro se perdió,
pérdida estructural silenciosa. Y si en cambio la recuperación decide **no** aplicar
imágenes más allá de `total_pages`, descarta datos confirmados. Las dos salidas rompen algo.

**Corrección.** `total_pages` en el registro de commit (H2); la recuperación calcula el
máximo `page_id` visto, extiende el archivo hasta ahí con páginas cero explícitas antes de
aplicar, y hace `fsync` de la extensión. Y en operación normal, extender el archivo con un
`fsync` **antes** de que el WAL confirme un `Put` que use la página nueva.

---

## H13 · Secuenciación de fases: la F2 diseña la API del árbol sin conocer las restricciones del log. (bajo, pero caro)

La F2 (30 h) termina en la semana 12 con Put/Get/Scan y `Validate()` en verde. La F3 (WAL)
empieza en la 13. Pero las correcciones de H1, H2 y H8 imponen restricciones sobre **cómo
muta páginas el árbol**: cada mutación debe producir una imagen registrable, pertenecer a un
grupo de commit delimitado, y respetar `page_lsn` frente al desalojo. Una API de árbol
diseñada sin esas restricciones se reescribe en la F3. Son las 30 horas más caras del plan.

Segundo: la regla de rescate de la semana 14 (sustituir el B+tree por un índice hash en
memoria con log en disco) **elimina la actualización en sitio**, que es de donde vienen los
torn writes, las páginas meta y la lista de libres. La F4 sobre un log de solo-anexar es
una F4 mucho más fácil, así que la premisa *"el proyecto sigue siendo válido porque la parte
que importa es la F4"* no se sostiene: se conserva el nombre de la fase, no su dificultad.

**Corrección.** Definir el contrato del pager (imagen de página + `page_lsn` + límites de
grupo) al final de la F1, antes de escribir el árbol. Y en la regla de rescate, decir
explícitamente qué se pierde: que la F4 degradada prueba durabilidad de anexado, no de
actualización en sitio.
