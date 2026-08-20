# Motor de almacenamiento clave-valor con durabilidad ante caídas

Documento de diseño, versión 2. Escrito antes del código, a propósito.

> **Versión 2.** Incorpora las correcciones de `docs/REVIEW-01.md`, una revisión
> adversarial realizada antes de escribir la primera línea de código. El changelog al
> final indica qué cambió y por qué. La versión 1 quedaba con al menos cinco caminos que
> producían pérdida de datos confirmados.

---

## 1. Qué es este proyecto

Una librería en Go que guarda pares clave-valor en un archivo en disco y garantiza
que **ningún dato confirmado se pierde ante una caída del proceso o del sistema**.

Es la capa más baja de una base de datos: la que no sabe nada de SQL, tablas ni
columnas, y solo sabe guardar bytes ordenados por clave, recuperarlos rápido, y no
perderlos nunca.

Equivalentes reales: InnoDB (dentro de MySQL), el pager de SQLite, BoltDB (Go).

## 2. Qué NO es

Estas exclusiones son deliberadas y permanentes. Cada una es un proyecto aparte.

- No hay SQL, ni parser, ni planificador de consultas.
- No hay concurrencia: un solo hilo escritor, sin transacciones multi-operación.
- No hay red, ni cliente, ni servidor. Es una librería embebida.
- No hay índices secundarios: el único orden es el de la clave primaria.
- No hay compresión, replicación ni cifrado.
- **No hay páginas de desbordamiento.** Un par clave+valor debe caber holgadamente en
  una página. El límite concreto está en la sec. 4 y su justificación en la sec. 5.1.
- No busca competir en rendimiento con motores reales.

El objetivo no es un producto. Es demostrar que se entiende el problema de la
durabilidad y que se sabe probar que la solución funciona.

## 3. Lenguaje: Go

**Por qué Go y no Python o TypeScript:** el proyecto trata de bytes en disco y de
control sobre cuándo esos bytes llegan realmente al hardware. En un lenguaje de alto
nivel esa capa está escondida detrás de abstracciones, y esconderla es justamente lo
que esos lenguajes hacen bien. Aquí el detalle escondido *es* el proyecto.

**Por qué Go y no Rust o C:** la dificultad debe estar en la durabilidad, no en pelear
con el compilador. Rust exigiría resolver problemas de propiedad sobre un buffer
compartido y mutable, que es fricción que no enseña nada sobre el problema real. C
permitiría errores de memoria que arruinarían el argumento de correctitud. Go da
acceso directo a `os.File`, `fsync` y manejo de bytes, con un tiempo de aprendizaje de
semanas y no de meses.

**Qué se usa de la librería estándar:** `os`, `encoding/binary`, `hash/crc32`,
`sort`, `testing`. Sin dependencias externas. Es una decisión de diseño: cero
dependencias hace que el repo sea auditable de principio a fin.

## 4. Interfaz pública

Todo el contrato externo son seis funciones:

```go
func Open(path string) (*DB, error)
func (db *DB) Put(key, value []byte) error
func (db *DB) Get(key []byte) ([]byte, error)
func (db *DB) Delete(key []byte) error
func (db *DB) Scan(start, end []byte, fn func(k, v []byte) bool) error
func (db *DB) Close() error
```

Garantía que ofrece: **si `Put` devuelve `nil`, ese dato sobrevive a cualquier caída
posterior.** Todo el resto del documento existe para sostener esa frase.

**Límites de tamaño.** Clave hasta 512 bytes. La suma `clave + valor + 6 bytes de
cabecera de celda` no puede pasar de **1000 bytes**. `Put` devuelve
`ErrEntryTooLarge` si se excede.

El número no es arbitrario y no se elige por comodidad: se deriva del objetivo de
llenado de la sec. 6. Para que una hoja llena siempre pueda dividirse en dos mitades
razonablemente ocupadas hacen falta al menos cuatro celdas por página. Con 4096 bytes
menos 40 de cabecera quedan 4056 útiles, y 4056 / 4 ≈ 1014. De ahí el tope.

> La versión 1 de este documento decía "valor hasta 1 MB" y afirmaba que cabía
> holgadamente en una página de 4096 bytes. No es una errata menor: era la premisa que
> justificaba no implementar páginas de desbordamiento, y además contradecía el propio
> layout (un directorio de slots `uint16` no puede direccionar más de 65535 bytes).

---

## 5. Estructura en disco

Dos archivos lógicos:

| Archivo | Contenido | Patrón de escritura |
|---|---|---|
| `datos.db` | El árbol, en páginas de 4096 bytes | Escritura en el medio, en sitio |
| `datos.wal.N` | El log de cambios, generación N | Solo se agrega al final |

La distinción importa: agregar al final de un archivo es una operación mucho más
fácil de hacer segura que modificar algo en el medio. Todo el diseño se apoya en eso.

El WAL lleva número de generación en el nombre porque **no se trunca en sitio, se
rota** (ver sec. 7.4). Escribir registros nuevos encima de bytes de registros viejos es
la causa de una clase entera de bugs de recuperación.

### 5.1 La página

4096 bytes, igual al tamaño de bloque del sistema de archivos. Es la unidad mínima de
lectura y de escritura: nunca se lee ni se escribe menos que una página completa.

Cabecera de **40 bytes**:

```
byte 0     4          12         20     21      22        24        26      32      40
    +------+----------+----------+------+-------+---------+---------+-------+-------+
    | crc32| page_id  | page_lsn | tipo | flags | nceldas | libre_f | libre |enlace |
    +------+----------+----------+------+-------+---------+---------+-------+-------+
    | slot0 | slot1 | slot2 | ...                                          | ← crece →
    +---------------------------------------------------------------------+
    |                       espacio libre                                  |
    +---------------------------------------------------------------------+
    |                        ... | celda2 | celda1 | celda0                |  ← crece ←
    +---------------------------------------------------------------------+
                                                                     byte 4096
```

- **crc32 (4):** checksum de los 4092 bytes restantes. Es lo que permite distinguir
  "página válida" de "página escrita a medias". Sin esto leerías basura y le creerías.
- **page_id (8):** el número de página que esta página *cree* ser. Al leer la página N
  se verifica `page_id == N` además del CRC. **El CRC valida contenido, no ubicación:**
  una escritura dirigida al offset equivocado —por un error de aritmética, o por una
  reproducción del log que aplica una imagen al lugar incorrecto— produce una página con
  CRC perfectamente válido en la ranura equivocada, y sin este campo nada lo detecta.
  InnoDB y SQL Server guardan el número de página dentro de la página por esta razón.
- **page_lsn (8):** el LSN del último registro de log que modificó esta página. Es lo
  que hace cumplible la regla del write-ahead frente al desalojo del caché (sec. 7.5).
- **tipo (1):** 1 = nodo interno, 2 = hoja, 3 = meta.
- **flags (1):** reservado.
- **nceldas (2):** cuántas entradas contiene.
- **libre_fin (2), libre (2):** frontera del espacio libre.
- **enlace (8):** para `tipo=1`, el puntero al **hijo más a la derecha**. Para `tipo=2`,
  el puntero a la **hoja siguiente**. Ambos son obligatorios y en la versión 1 no
  existían en el layout: un nodo con *n* claves separadoras tiene *n+1* hijos, y con un
  puntero por celda solo se direccionan *n*. El hijo derecho no tenía dónde vivir, y la
  cadena lateral entre hojas —de la que depende el invariante 5— dependía de un campo
  que el formato no declaraba.
- **directorio de slots:** un `uint16` por celda con el offset donde empieza esa celda.
  Los slots están ordenados por clave; las celdas físicas no tienen por qué estarlo.
  Insertar en el medio mueve solo 2 bytes por slot, no la celda entera.
- **celdas:** crecen desde el final hacia el centro. Cuando el área de slots y el área
  de celdas se tocan, la página está llena.

Formato de celda en una **hoja**: `key_len (2) | val_len (4) | clave | valor`
Formato de celda en un **nodo interno**: `key_len (2) | hijo_izq (8) | clave`

### 5.2 Las páginas meta

Las páginas 0 y 1 guardan: número mágico, versión del formato, id de la página raíz,
cabeza de la lista de páginas libres, total de páginas, generación actual del WAL, y el
LSN del último checkpoint.

**Se usan dos, alternadas.** Al hacer checkpoint se escribe siempre sobre la más
antigua. Al abrir, se leen ambas, se descartan las que fallen el CRC o el `page_id`, y
se elige la válida con el LSN más alto. Si te caes escribiendo una meta, la otra sigue
intacta. Es la misma técnica que usa BoltDB.

Dos reglas que la versión 1 no tenía y sin las cuales la alternancia no sirve de nada:

1. **Las páginas meta no pertenecen al conjunto de páginas sucias del pager.** El paso 1
   del checkpoint vuelca "todas las páginas sucias" a disco; si la meta viva estuviera
   ahí, el paso 1 escribiría la ranura nueva y el paso 3 la vieja, dejando las dos
   ranuras con estado de la misma época. La garantía "si una se corrompe, la otra sirve"
   se evaporaría. Las metas se escriben **solo** en el paso 3 del checkpoint, por su
   propio camino.
2. **La meta es una optimización, no la única copia de la verdad.** El estado global
   auténtico viaja en los registros de commit del WAL (sec. 7.3). La meta solo existe
   para no tener que leer el log entero en cada `Open`. De ahí se sigue que dos metas
   inválidas **no** son un archivo irrecuperable (sec. 8).

---

## 6. El B+tree

### Por qué

Sin índice, buscar una clave obliga a recorrer el archivo entero. Con un B+tree, con
un millón de claves el árbol tiene 3 niveles, así que cualquier búsqueda cuesta 3
lecturas de disco.

La razón de que sea tan plano es la aridad: con páginas de 4 KB y claves cortas, cada
nodo interno apunta a unos 150-200 hijos. Un árbol binario con un millón de claves
tendría 20 niveles; este tiene 3.

### Estructura

- **Nodos internos:** solo claves separadoras, punteros a páginas hijas y el puntero al
  hijo derecho en la cabecera. No guardan valores. Su único trabajo es dirigir la
  búsqueda.
- **Hojas:** guardan las claves con sus valores, y en la cabecera el puntero a la hoja
  siguiente. Ese enlace es lo que hace barato el `Scan` de rango: se llega a la primera
  hoja por el árbol y de ahí se sigue el enlace lateral sin volver a subir.

### Operaciones

**Búsqueda:** desde la raíz, en cada nodo interno se hace búsqueda binaria sobre las
claves separadoras para elegir el hijo. Al llegar a la hoja, búsqueda binaria de nuevo.

**Inserción:** se localiza la hoja y se inserta ordenado. Si no cabe, la hoja se
divide en dos y la clave del medio sube al padre. Si el padre tampoco tiene espacio,
se divide también, y así hacia arriba. Si se divide la raíz, el árbol crece un nivel.

**Este es el momento peligroso del proyecto:** una sola llamada a `Put` puede
convertirse en tres, cuatro o más escrituras físicas que deben aplicarse todas o
ninguna. Si el sistema se cae en el medio, el padre queda apuntando a una página que
no existe o que está a medias, y el árbol queda corrupto **sin ningún aviso**.

La sec. 7.3 es el mecanismo que convierte ese "todas o ninguna" en algo real. Enunciar
el problema aquí y no resolverlo allí fue el fallo central de la versión 1.

**Borrado:** se quita la entrada de la hoja. Si la ocupación cae por debajo del objetivo
de llenado, se intenta redistribuir con un hermano; si tampoco alcanza, se fusionan las
dos y se quita el separador del padre. La página liberada pasa al conjunto de libres.

### Invariantes que deben sostenerse siempre

1. Las claves de cada nodo están estrictamente ordenadas.
2. Todas las hojas están a la misma profundidad.
3. **Ninguna página alcanzable tiene 0 celdas**, salvo una raíz vacía. Y **ningún par
   de hermanos adyacentes cabe entero en una sola página** — esta segunda es la
   propiedad que la fusión realmente persigue.
4. La clave separadora de un nodo interno es mayor que toda clave del subárbol
   izquierdo y menor o igual que toda clave del derecho.
5. La cadena de enlaces entre hojas recorre todas las claves en orden.
6. **Partición.** Toda página en `[2, total_pages)` está en **exactamente uno** de dos
   conjuntos: alcanzable desde la raíz, o libre. Ni en los dos, ni en ninguno. Toda
   página de ambos conjuntos tiene CRC y `page_id` válidos.

> **Sobre el invariante 3.** La versión 1 exigía ocupación mínima del 40%. Con celdas de
> tamaño variable eso no es un invariante sino una heurística de llenado, y no siempre es
> alcanzable: una hoja con una celda de 100 bytes junto a una hermana con una celda de
> 3000 no se puede fusionar ni redistribuir para que ambas superen el 40%. `Validate()`
> fallaría sobre un árbol sano — falsos positivos en la F4, que es exactamente donde no
> se los puede permitir. SQLite no garantiza ocupación mínima; BoltDB usa un
> `FillPercent` que es una preferencia, no una regla. El 40% baja a objetivo de llenado
> del algoritmo de borrado y sale de la lista de invariantes.
>
> Consecuencia deliberada: la regla de rescate de la sec. 11 (borrado por marca sin
> fusión) ahora es compatible con los seis invariantes. En la versión 1 la vía de escape
> del riesgo ponía en rojo la tabla entera de la F4, que es la fase que el plan declara
> irrecortable.

> **Sobre el invariante 6.** La versión 1 decía "ninguna página está en la lista de
> libres y en el árbol a la vez". Eso prohíbe la doble pertenencia pero **no la
> pertenencia nula**: un motor que filtrara el 100% del archivo pasaba la validación.
> Como partición, tanto el doble uso como la fuga se vuelven detectables.

`Validate()` recorre el árbol completo **y el conjunto de páginas libres**, y comprueba
que la unión cubre todo el archivo y la intersección es vacía. Es la herramienta de
depuración más importante del proyecto y se llama después de cada recuperación en los
tests.

### 6.1 El conjunto de páginas libres

**Decisión: no se persiste. Se reconstruye en cada `Open`** barriendo las páginas
alcanzables desde la raíz y marcando el complemento.

El razonamiento: una lista enlazada de libres en disco introduce una ventana de
inconsistencia entre checkpoints que produce el peor bug posible del proyecto. Si el
asignador saca la página 30 de la lista y el WAL registra la 30 ya convertida en hoja
con datos vivos, pero la meta —que solo se escribe en el checkpoint— sigue diciendo
`free_head = 30`, al recuperar la página 30 está simultáneamente en el árbol y en la
cabeza de la lista de libres. El siguiente `Put` que necesite espacio la asigna otra
vez y borra silenciosamente todas sus claves. Árbol estructuralmente válido, datos
perdidos, y el fallo aparece miles de operaciones después de su causa.

Reconstruir es O(n) al abrir, pero **el costo ya está pagado**: el paso final de la
recuperación (sec. 8) recorre el árbol entero con `Validate()` de todas formas. A
cambio, la ventana desaparece por construcción y el invariante 6 se vuelve cierto por
definición en vez de por vigilancia.

Para un proyecto cuyo objetivo declarado es demostrar durabilidad, eliminar una clase
entera de bug de caída vale más que un `Open` rápido. Esto va a `TESTING.md` como
decisión argumentada, no como omisión.

---

## 7. El write-ahead log

### 7.1 La regla

**Nada se modifica en el archivo de datos antes de que la descripción del cambio esté
sincronizada en el log.** De ahí el nombre: escritura por adelantado.

### 7.2 El camino de una escritura

1. Llega el `Put`.
2. El cambio se aplica a las páginas en memoria (caché de páginas). Cada página tocada
   recibe el `page_lsn` del registro que la va a describir.
3. Las páginas modificadas se serializan como registros de log y se agregan al final
   del WAL.
4. **Se agrega el registro de commit del grupo.**
5. `fsync` sobre el WAL. **Hasta aquí, nada es seguro; después de aquí, todo lo es.**
6. `Put` devuelve `nil`. El usuario recibe la confirmación.
7. Más tarde, en un checkpoint, las páginas sucias se escriben al archivo de datos.

El paso 6 ocurre antes del 7 a propósito. El dato está a salvo porque está en el log,
no porque esté en su lugar definitivo. Eso permite agrupar las escrituras al árbol y
evitar que cada operación cueste un `fsync` en medio del archivo.

### 7.3 Formato de registro y atomicidad de grupo

El log guarda **imágenes completas de página**, no descripciones lógicas de la
operación:

```
imagen:  | lsn (8) | tipo=1 (1) | epoca (4) | page_id (8) | 4096 bytes | crc32 (4) |
commit:  | lsn (8) | tipo=2 (1) | epoca (4) | n_registros (4)
                   | root_id (8) | free_head (8) | total_pages (8) | crc32 (4) |
```

**Por qué la imagen completa y no "insertar clave X con valor Y":** el log lógico es
mucho más compacto, pero para reaplicarlo necesitas que el árbol de partida esté en un
estado consistente — y si el sistema se cayó escribiendo una página, no lo está. La
imagen completa se puede reaplicar sobre cualquier estado, incluso sobre una página
corrupta, porque la sobrescribe entera. Esto es exactamente lo que hace Postgres con
su opción `full_page_writes`, y por la misma razón. El costo es volumen de log, y se
acepta.

**Por qué el registro de commit.** El CRC por registro da *atomicidad de registro*. Lo
que hace falta es *atomicidad de grupo*, y son cosas distintas — confundirlas fue el
hallazgo H1.

Escenario: la hoja 40 se divide. El `Put` genera cuatro imágenes: la 40 reescrita, la
41 nueva, el padre 20 con el separador nuevo, y el estado del asignador. Se anexan con
LSN 700→703 y se cae durante el `fsync`. El disco persiste 700, 701 y 702 completos y
703 a medias. Aplicando registro por registro, la recuperación escribe tres de cuatro:
el padre ya apunta a la 41, pero la imagen de la 41 puede ser justamente la que faltó.
Si la 41 era una página liberada y reciclada, contiene datos viejos con CRC válido —
`Validate()` pasa en verde y el árbol devuelve claves de otra época.

Peor todavía en una fusión: si se aplica el padre pero no la absorción en la hermana,
la cadena lateral sigue pasando por una hoja que ya no está en el árbol. `Get` la
encuentra por el árbol y `Scan` devuelve un conjunto distinto. **Dos caminos de lectura
que discrepan es el peor bug posible en un motor de almacenamiento.**

Con el registro de commit, la recuperación aplica **solo grupos completos**. Un grupo
sin su commit se descarta entero. Eso es lo que convierte la promesa "todas o ninguna"
de la sec. 6 en un mecanismo.

**Por qué el estado del asignador viaja en el commit.** `root_id`, `free_head` y
`total_pages` cambian con cada división y solo vivían en la meta, que únicamente se
escribe en el checkpoint. Sin ellos en el log: la raíz 5 se divide, las páginas 6, 7 y 8
quedan escritas y perfectas en disco tras la recuperación, y el motor arranca con
`root=5`, que ahora es solo la mitad izquierda. La mitad de las claves confirmadas es
inalcanzable y las tres páginas están huérfanas — **y `Validate()` pasa**, porque una
hoja como raíz es un B+tree perfectamente legal. Son 24 bytes por `Put`, no 4 KB.

(`free_head` se conserva en el formato por si algún día se persistiera el conjunto de
libres; con la decisión de la sec. 6.1 se escribe pero no se usa en la recuperación.)

### 7.4 Checkpoint y rotación del WAL

Cada N registros o cada N bytes de log:

1. Escribir al archivo de datos todas las páginas sucias (las metas **no** están en ese
   conjunto).
2. `fsync` sobre el archivo de datos.
3. Escribir la página meta más antigua de las dos, con el nuevo LSN y la generación
   `N+1` del WAL.
4. `fsync` sobre la meta.
5. Recién ahora, **crear `datos.wal.N+1`, hacer `fsync` del directorio, y borrar
   `datos.wal.N`.**

El orden es todo. Liberar el log antes del paso 2 sería perder la única copia buena.

**Por qué rotar y no truncar.** La regla de la sec. 8 —"el primer registro con CRC
inválido marca el punto exacto de la caída"— solo es cierta si el WAL nunca se reescribe
sobre bytes ya usados. Truncando en sitio: el `Truncate` es una operación de metadatos
del sistema de archivos, y sin `fsync` de directorio puede no ser durable. Tras la caída
el archivo puede conservar longitud y bytes viejos; los registros nuevos ocupan menos
que los viejos; y al terminar de leerlos la recuperación encuentra registros antiguos
con **CRC perfectamente válido**, porque son registros reales de antes del checkpoint.
La regla del primer CRC inválido nunca dispara. Se reproducen imágenes viejas encima de
datos nuevos.

Tres defensas, y se usan las tres:

1. **LSN estrictamente monótono, nunca se reinicia.** La recuperación exige contigüidad:
   `lsn[i+1] == lsn[i] + 1`; cualquier salto termina la lectura.
2. **Campo `epoca`** (generación del WAL) en cada registro, verificado contra el de la
   meta. Un registro de otra generación termina la lectura.
3. **Rotación en vez de truncado**, con `fsync` de directorio.

`fsync` de directorio después de crear, rotar o extender cualquier archivo. Sin él, en
ext4/XFS la creación del WAL puede no ser durable: `Put` devolvería `nil` y tras la
caída el archivo de log no existiría.

### 7.5 Regla de desalojo del caché

**Una página sucia no puede escribirse a `datos.db` mientras
`wal_flushed_lsn < pagina.page_lsn`.** Si el desalojo encuentra una página en esa
situación, fuerza antes el `fsync` del WAL.

Sin esta regla, la sec. 7.1 se cumple en el orden de las llamadas pero se viola en el
desalojo: un caché de tamaño acotado desaloja, y si desaloja una página sucia a
`datos.db` antes del `fsync` de su registro, la escritura puede desgarrarse y el log no
tiene con qué repararla porque su registro se perdió. Página alcanzable rota, sin
reparación posible. Es lo que hace necesario el `page_lsn` de la sec. 5.1: los dos
hallazgos se corrigen juntos.

### 7.6 Extensión del archivo

Crecer `datos.db` es una operación de metadatos y no está descrita por ninguna imagen de
página. En operación normal, el archivo se extiende y se hace `fsync` **antes** de que
un commit confirme un `Put` que use la página nueva.

---

## 8. Recuperación

Al abrir el archivo:

1. Leer las dos páginas meta, descartar las que fallen CRC o `page_id`, quedarse con la
   de LSN más alto. **Si ninguna es válida, reproducir el WAL completo desde el LSN 0 y
   reconstruir la meta** — no se declara el archivo irrecuperable.
2. Localizar el WAL de la generación indicada y leerlo desde ese LSN, registro por
   registro.
3. Validar de cada registro: CRC, contigüidad del LSN, y `epoca`. **El primer registro
   que falle cualquiera de las tres marca el punto exacto donde ocurrió la caída:** ahí
   se detiene la lectura y se descarta todo lo que siga, incluso si más adelante hubiera
   registros aparentemente válidos.
4. Agrupar los registros leídos por commit. **Descartar entero el último grupo si no
   tiene su registro de commit.**
5. Calcular el `page_id` máximo entre las imágenes que se van a aplicar y extender
   `datos.db` hasta ahí con páginas cero explícitas. `fsync`.
6. Aplicar las imágenes de los grupos completos al archivo de datos. `fsync`.
7. Tomar `root_id` y `total_pages` del último commit aplicado.
8. Reconstruir el conjunto de páginas libres (sec. 6.1).
9. Ejecutar `Validate()` sobre el árbol resultante.
10. **Terminar con un checkpoint completo:** escribir la meta con el estado
    reconstruido, `fsync`, rotar el WAL.

**Por qué el paso 1 no aborta.** La durabilidad de `Put` descansa exclusivamente en el
`fsync` del WAL. El log contiene todo lo confirmado. Declarar muerto el archivo porque
las dos metas fallaron el CRC convierte una situación totalmente recuperable en pérdida
permanente — teniendo el WAL entero intacto en el disco. Con imágenes de página
completas, reproducir desde cero funciona; es el mismo argumento de la sec. 7.3.

**Por qué el paso 5.** Si el WAL contiene una imagen de la página 900 y `datos.db` mide
800 páginas, escribir en el offset `900×4096` deja un archivo disperso con un agujero en
las páginas 800-899 que se leen como ceros y fallan el CRC. Y la alternativa —no aplicar
imágenes más allá de `total_pages`— descarta datos confirmados. Las dos salidas rompen
algo; la extensión explícita es la única que no.

**Por qué el paso 10.** Sin él, el motor arranca operando con el `root_id` de la meta
vieja, que es exactamente el estado que los pasos anteriores acaban de demostrar
obsoleto. El primer `Put` desciende por el árbol viejo y diverge más. Además, el WAL
nunca se liberaría en un ciclo caída-recuperación-caída y cada recuperación sería más
lenta que la anterior. Como efecto secundario útil, hace la recuperación **observable**
en los tests: tras `Open`, la meta debe reflejar el último grupo confirmado.

Dos propiedades que deben cumplirse:

- **Idempotencia:** reaplicar el mismo log dos veces produce el mismo resultado. Es
  obligatorio porque el sistema puede caerse *durante* la recuperación. Con imágenes
  de página esto es automático.
- **Atomicidad de grupo:** un grupo sin commit se descarta entero. El registro de commit
  es lo que lo garantiza; el CRC solo cubre el registro individual.

---

## 9. Verificación: la parte que da valor al proyecto

Un B+tree con WAL que pasa tests normales es un ejercicio de estructuras de datos. Lo
que lo convierte en otra cosa es demostrar la garantía de durabilidad bajo fallo real.

### 9.1 Capa de disco con inyección de fallos

Toda la E/S pasa por una interfaz:

```go
type File interface {
    ReadAt(p []byte, off int64) (int, error)
    WriteAt(p []byte, off int64) (int, error)
    Sync() error
    Truncate(size int64) error
}
```

La implementación de producción envuelve `os.File`. La de pruebas simula el
comportamiento real del hardware en su peor día:

- Mantiene las escrituras no sincronizadas en un buffer, igual que el caché del
  sistema operativo.
- `Sync()` las vuelca de verdad.
- Al simular la caída en la escritura número N: descarta una parte de las escrituras
  pendientes, altera el orden de las que sí aplica, y a una la aplica solo a la mitad
  (escritura desgarrada, que es lo que ocurre cuando el disco solo garantiza
  atomicidad a nivel de sector).

**Árbitro de orden global.** Las dos instancias de `File` (datos y WAL) comparten **una
sola cola de escrituras pendientes**, con el nombre de archivo como etiqueta. `Sync()`
de un archivo vacía solo sus entradas, pero el descarte y el reordenamiento en la caída
se deciden sobre la cola global.

Sin esto, el orden relativo entre escrituras a `datos.db` y a `datos.wal` nunca se
modela — y ese orden relativo *es* el write-ahead logging. El bug de la sec. 7.5 (página
de datos en disco antes que su registro de log) sería invisible para el arnés por
construcción. Un error en el aparato de verificación es más grave que un error en el
motor, porque hace que los verdes no signifiquen nada.

**Semilla explícita y registrada** en cada corrida, para que cada punto de caída sea
reproducible bit a bit. Sin eso, `BUGS.md` no puede citar un caso concreto.

### 9.2 El ciclo de prueba

```
para N en 1..500:
    abrir DB con disco falso configurado para caer en la escritura N (semilla fija)
    escribir un conjunto conocido de claves, registrando cuáles devolvieron OK
    la caída ocurre
    reabrir DB
    verificar:
      (a) Validate() pasa: los seis invariantes se sostienen, incluida la partición
      (b) toda clave cuyo Put devolvió OK está presente y con el valor correcto
      (c) toda clave presente pertenece al conjunto de claves que se intentaron escribir
```

El punto (b) es la afirmación central del proyecto entero.

El punto (c) está redactado así a propósito. La versión 1 decía "ninguna clave que nunca
se escribió aparece", lo que se lee fácilmente como "ninguna clave no confirmada
aparece" — y eso es **falso y esperable**: una clave cuyo `Put` se anexó al WAL pero cuyo
`fsync` no había retornado puede sobrevivir perfectamente, porque el sistema operativo
pudo volcar esos bytes por su cuenta. Implementado con la lectura estricta, el test
falla de forma intermitente e irreproducible durante toda la F4. Las confirmadas **deben**
estar; las no confirmadas **pueden** estar o no; lo que nunca puede aparecer es una clave
que jamás se intentó escribir.

### 9.3 Pruebas basadas en propiedades

Se genera una secuencia aleatoria de miles de operaciones y se aplican en paralelo al
motor y a un `map[string]string` en memoria que sirve de modelo de referencia. Después
de cada operación, ambos deben coincidir. `Scan` se compara contra las claves del mapa
ordenadas.

Esto encuentra los errores de la lógica del árbol; la inyección de fallos encuentra los
de durabilidad. Son dos clases de bug distintas y hacen falta las dos.

### 9.4 Fuzzing

El deserializador de páginas y el lector de registros del WAL se someten al fuzzer
nativo de Go alimentado con bytes arbitrarios. Requisito: nunca deben provocar pánico.
Deben devolver un error de corrupción y nada más.

### 9.5 Argumento de correctitud

Tres pruebas de escritorio, escritas antes del código y revisadas al terminar cada fase.
Van a `TESTING.md`; es lo que un lector técnico busca antes de mirar el código.

1. **Prueba del corte en cualquier punto.** Para cada uno de los `fsync` del documento
   (WAL en `Put`, datos en checkpoint, meta en checkpoint, extensión de archivo,
   directorio en rotación), enumerar por escrito qué ve la recuperación si la caída
   ocurre justo antes y justo después. Diez casos. La versión 1 fallaba cuatro.
2. **Prueba de la partición.** Recorrer los escenarios de asignación desde libres,
   liberación por fusión y reasignación dentro del mismo intervalo de checkpoint, y
   comprobar que el invariante 6 los declara inválidos si algo salió mal.
3. **Prueba del modo tombstone.** Releer los seis invariantes suponiendo borrado por
   marca sin fusión y confirmar que los seis siguen siendo satisfacibles. Si alguno no
   lo es, la regla de rescate de la sec. 11 está rota.

---

## 10. Plan por fases

Presupuesto: 4-5 horas semanales durante 26 semanas ≈ 103 horas.

| Fase | Semanas | Horas | Terminada cuando |
|---|---|---|---|
| F0 · Go base | 1-2 | 9 | Lees y escribes registros de longitud variable en un archivo, con tests |
| F1 · Páginas, pager y **contrato** | 3-5 | 13 | Serializas una página, la relees, detectas corrupción al alterar un byte, y el contrato del pager está escrito |
| F2 · B+tree | 6-12 | 30 | Put/Get/Scan con 100.000 claves y `Validate()` en verde |
| F3 · WAL y recuperación | 13-17 | 22 | `kill -9` manual y al reabrir están todas las claves confirmadas |
| F4 · Inyección de fallos | 18-21 | 18 | Tabla con 500 puntos de caída y cero pérdidas |
| F5 · Property testing | 22-23 | 9 | Miles de operaciones aleatorias coinciden con el modelo de referencia |
| F6 · Documentación | 24-26 | 12 | DESIGN, TESTING y BUGS escritos, CI en verde |

### El contrato del pager se cierra en la F1, no en la F3

La F2 son las 30 horas más caras del plan y en la versión 1 se diseñaba sin conocer las
restricciones que impone el log. Pero las secciones 7.3 y 7.5 imponen tres condiciones
sobre cómo el árbol muta páginas: cada mutación debe producir una **imagen registrable**,
debe pertenecer a un **grupo de commit delimitado**, y debe respetar el **`page_lsn`**
frente al desalojo. Una API de árbol diseñada sin esas tres restricciones se reescribe
entera en la F3.

Por eso el contrato del pager —firma de `alloc`, `get`, `markDirty`, `beginGroup`,
`commitGroup`— se escribe al final de la F1, aunque el WAL detrás de él no exista todavía
y `commitGroup` sea un no-op durante toda la F2.

### Reglas de rescate

- **Semana 14, F2 sin terminar:** se reemplaza el B+tree por un índice hash en memoria
  con log en disco. Se pierde el `Scan` de rango y se ganan seis semanas.
  **Costo declarado:** esa variante elimina la actualización en sitio, que es de donde
  vienen los torn writes, las páginas meta y el conjunto de libres. La F4 resultante
  prueba **durabilidad de anexado, no de actualización en sitio** — se conserva el nombre
  de la fase, no su dificultad. Si se toma esta salida, se documenta en `NO-GOALS.md` con
  esas palabras. Decir "el proyecto sigue siendo válido porque la parte que importa es la
  F4" sin esta aclaración sería falso.
- **Semana 20 con retraso:** se recorta la F5, nunca la F4.
- **La F6 no se recorta bajo ninguna circunstancia.** Un proyecto difícil sin documentar
  no comunica nada.

---

## 11. Riesgos

| Riesgo | Mitigación |
|---|---|
| El alcance se expande (SQL, concurrencia, red) | Lista de no-objetivos escrita y fija desde el día uno |
| El borrado con fusión de nodos resulta más complejo de lo previsto | Se entrega con borrado por marca (tombstone) sin fusión, documentando la limitación. Compatible con los seis invariantes tras la corrección del inv. 3. **Efecto colateral a asumir:** con tombstones un `Delete` puede provocar una división de página, así que deja de ser el caso fácil; y sin fusión no se libera ninguna página, así que el conjunto de libres queda vacío |
| Bugs que aparecen miles de operaciones después de su causa | `Validate()` invocable tras cada operación en modo depuración, **incluida la comprobación de partición del inv. 6** — sin ella, el bug de la sec. 6.1 es indetectable |
| Aprender Go toma más de lo previsto | La F0 es evaluable: si a las 9 horas el lenguaje no fluye, se reevalúa el proyecto antes de invertir 90 horas más |
| El arnés de pruebas tiene bugs y los verdes no significan nada | El árbitro de orden global de la sec. 9.1 se prueba con un caso que debe fallar: un motor que escribe a datos antes del `fsync` del WAL tiene que ser detectado |

---

## 12. Qué queda en el repositorio

- El código, sin dependencias externas.
- `DESIGN.md` — este documento.
- `NO-GOALS.md` — los límites, explícitos.
- `docs/REVIEW-01.md` — la revisión adversarial del diseño v1, con sus trece hallazgos,
  identificada como asistida por IA y fechada.
- `TESTING.md` — la tabla de inyección de fallos con los resultados, y el argumento de
  correctitud de la sec. 9.5.
- `BUGS.md` — los errores encontrados durante el desarrollo, qué los causaba y cómo se
  detectaron, con la semilla que los reproduce. **Se escribe desde el día uno, no en la
  F6.** Esta es la sección que más comunica a un lector técnico.
- CI que ejecuta la suite completa, incluida la inyección de fallos.

---

## 13. Glosario

- **Página:** bloque de tamaño fijo (4096 bytes), unidad mínima de E/S.
- **B+tree:** árbol de búsqueda de aridad alta donde los valores viven solo en las
  hojas, y las hojas están enlazadas entre sí.
- **WAL (write-ahead log):** archivo al que solo se agrega, donde se registra un
  cambio antes de aplicarlo al archivo de datos.
- **LSN (log sequence number):** número creciente que identifica cada registro del log.
  Aquí es monótono global y nunca se reinicia.
- **`page_lsn`:** LSN del último registro que modificó una página. Es lo que permite
  saber si una página sucia puede bajar a disco.
- **Registro de commit:** marca el final de un grupo de imágenes que deben aplicarse
  todas o ninguna, y transporta el estado del asignador.
- **Época:** número de generación del WAL, incrementado en cada rotación.
- **fsync:** llamada al sistema que obliga a que los datos escritos lleguen al disco
  físico. Sin ella, `write()` solo llega al caché del sistema operativo.
- **Escritura desgarrada (torn write):** escritura interrumpida que deja una página
  medio vieja y medio nueva.
- **Checkpoint:** momento en que las páginas modificadas se llevan al archivo de datos
  y el log puede rotarse.
- **Idempotencia:** aplicar una operación varias veces produce el mismo resultado que
  aplicarla una vez.

---

## Changelog v1 → v2

| # | Sección | Cambio |
|---|---|---|
| H1 | 7.3, 8 | Registro de commit por grupo; `fsync` después del commit; la recuperación aplica solo grupos completos |
| H2 | 5.2, 7.3, 8 | `root_id`, `free_head` y `total_pages` viajan en el commit; la meta pasa a ser optimización |
| H3 | 6, 6.1 | Invariante 6 reescrito como partición; `Validate()` recorre los libres; el conjunto de libres se reconstruye al abrir en vez de persistirse |
| H4 | 2, 4 | Límite de par clave+valor a 1000 bytes, derivado del llenado; overflow explícitamente fuera de alcance |
| H5 | 5.1 | Cabecera a 40 bytes; campo `enlace` para hijo derecho / hoja siguiente |
| H6 | 5.1, 5.2, 8 | `page_id` y `page_lsn` en cabecera, verificados al leer; metas fuera del conjunto de sucias; dos metas inválidas ⇒ reproducir el WAL desde 0 |
| H7 | 5, 7.4, 8 | LSN monótono con contigüidad verificada; campo `epoca`; rotación del WAL en vez de truncado; `fsync` de directorio |
| H8 | 7.5 | Regla de desalojo: no bajar una página sucia si `wal_flushed_lsn < page_lsn` |
| H9 | 8 | La recuperación termina con checkpoint completo |
| H10 | 6, 11 | El 40% baja de invariante a objetivo de llenado; invariante 3 reformulado como comprobable y siempre satisfacible; compatibilidad con tombstones y sus efectos colaterales |
| H11 | 9.1, 9.2 | Árbitro de orden global entre los dos archivos; semilla reproducible; criterio (c) reformulado |
| H12 | 7.6, 8 | Extensión explícita del archivo antes de aplicar imágenes; `total_pages` en el commit |
| H13 | 10 | Contrato del pager al final de la F1; regla de rescate con su costo declarado |