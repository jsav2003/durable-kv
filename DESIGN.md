# Motor de almacenamiento clave-valor con durabilidad ante caídas

Documento de diseño. Escrito antes del código, a propósito.

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

Todo el contrato externo son cinco funciones:

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

Límites: clave hasta 512 bytes, valor hasta 1 MB. Ambos caben holgadamente en una
página, lo que evita tener que implementar páginas de desbordamiento.

---

## 5. Estructura en disco

Dos archivos:

| Archivo | Contenido | Patrón de escritura |
|---|---|---|
| `datos.db` | El árbol, en páginas de 4096 bytes | Escritura en el medio, en sitio |
| `datos.wal` | El log de cambios | Solo se agrega al final |

La distinción importa: agregar al final de un archivo es una operación mucho más
fácil de hacer segura que modificar algo en el medio. Todo el diseño se apoya en eso.

### 5.1 La página

4096 bytes, igual al tamaño de bloque del sistema de archivos. Es la unidad mínima de
lectura y de escritura: nunca se lee ni se escribe menos que una página completa.

Layout interno:

```
byte 0        4        5      6        8         10        12          32
     +--------+--------+------+--------+---------+---------+-----------+
     | crc32  | tipo   |flags | nceldas| libre_fin| libre  | reservado |
     +--------+--------+------+--------+---------+---------+-----------+
     |  slot0 | slot1  | slot2 | ...                                   |  ← crece →
     +-----------------------------------------------------------------+
     |                     espacio libre                                |
     +-----------------------------------------------------------------+
     |                              ... | celda2 | celda1 | celda0      |  ← crece ←
     +-----------------------------------------------------------------+
                                                                   byte 4096
```

- **crc32 (4 bytes):** checksum de los 4092 bytes restantes. Es lo que permite
  distinguir "página válida" de "página escrita a medias". Sin esto leerías basura y
  le creerías.
- **tipo (1 byte):** 1 = nodo interno, 2 = hoja, 3 = meta.
- **nceldas (2 bytes):** cuántas entradas contiene.
- **directorio de slots:** un `uint16` por celda, con el offset donde empieza esa
  celda. Los slots están ordenados por clave; las celdas físicas no tienen por qué
  estarlo. Insertar en el medio mueve solo 2 bytes por slot, no la celda entera.
- **celdas:** crecen desde el final hacia el centro. Cuando el área de slots y el área
  de celdas se tocan, la página está llena.

Formato de celda en una **hoja**: `key_len (2) | val_len (4) | clave | valor`
Formato de celda en un **nodo interno**: `key_len (2) | hijo_izq (8) | clave`

### 5.2 Las páginas meta

Las páginas 0 y 1 guardan el estado global: número mágico, versión del formato, id de
la página raíz, cabeza de la lista de páginas libres, total de páginas, y el LSN del
último checkpoint.

**Se usan dos, alternadas.** Al hacer checkpoint se escribe siempre sobre la más
antigua. Al abrir, se leen ambas, se descartan las que fallen el CRC y se elige la
válida con el número de transacción más alto. Esto resuelve el problema de que
actualizar el estado global no es atómico: si te caes escribiendo una meta, la otra
sigue intacta. Es la misma técnica que usa BoltDB.

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

- **Nodos internos:** solo claves separadoras y punteros a páginas hijas. No guardan
  valores. Su único trabajo es dirigir la búsqueda.
- **Hojas:** guardan las claves con sus valores, y un puntero a la hoja siguiente.
  Ese enlace es lo que hace barato el `Scan` de rango: se llega a la primera hoja por
  el árbol y de ahí se sigue el enlace lateral sin volver a subir.

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

**Borrado:** se quita la entrada de la hoja. Si la ocupación cae por debajo del 40%,
se intenta redistribuir con un hermano; si tampoco alcanza, se fusionan las dos y se
quita el separador del padre. La página liberada va a la lista de páginas libres para
reutilizarse.

### Invariantes que deben sostenerse siempre

1. Las claves de cada nodo están estrictamente ordenadas.
2. Todas las hojas están a la misma profundidad.
3. Todo nodo salvo la raíz está ocupado al menos al 40%.
4. La clave separadora de un nodo interno es mayor que toda clave del subárbol
   izquierdo y menor o igual que toda clave del derecho.
5. La cadena de enlaces entre hojas recorre todas las claves en orden.
6. Toda página alcanzable tiene CRC válido; ninguna página está en la lista de libres
   y en el árbol a la vez.

Estos seis puntos se convierten en una función `Validate()` que recorre el árbol
completo. Es la herramienta de depuración más importante del proyecto y se llama
después de cada recuperación en los tests.

---

## 7. El write-ahead log

### La regla

**Nada se modifica en el archivo de datos antes de que la descripción del cambio esté
sincronizada en el log.** De ahí el nombre: escritura por adelantado.

### El camino de una escritura

1. Llega el `Put`.
2. El cambio se aplica a las páginas en memoria (caché de páginas).
3. Las páginas modificadas se serializan como registros de log y se agregan al final
   del WAL.
4. `fsync` sobre el WAL. **Hasta aquí, nada es seguro; después de aquí, todo lo es.**
5. `Put` devuelve `nil`. El usuario recibe la confirmación.
6. Más tarde, en un checkpoint, las páginas sucias se escriben al archivo de datos.

El paso 5 ocurre antes del 6 a propósito. El dato está a salvo porque está en el log,
no porque esté en su lugar definitivo. Eso permite agrupar las escrituras al árbol y
evitar que cada operación cueste un `fsync` en medio del archivo.

### Qué se guarda en cada registro

Decisión de diseño: **el log guarda imágenes completas de página**, no descripciones
lógicas de la operación.

```
| lsn (8) | tipo (1) | page_id (8) | longitud (4) | 4096 bytes de página | crc32 (4) |
```

**Por qué la imagen completa y no "insertar clave X con valor Y":** el log lógico es
mucho más compacto, pero para reaplicarlo necesitas que el árbol de partida esté en un
estado consistente — y si el sistema se cayó escribiendo una página, no lo está. La
imagen completa se puede reaplicar sobre cualquier estado, incluso sobre una página
corrupta, porque la sobrescribe entera. Esto es exactamente lo que hace Postgres con
su opción `full_page_writes`, y por la misma razón.

El costo es volumen de log. Se acepta: la simplicidad de la recuperación vale más aquí
que el ahorro de espacio.

### Checkpoint

Cada N registros o cada N bytes de log:

1. Escribir al archivo de datos todas las páginas sucias.
2. `fsync` sobre el archivo de datos.
3. Escribir la página meta (la más antigua de las dos) con el nuevo LSN.
4. `fsync` sobre la meta.
5. Recién ahora, truncar el WAL.

El orden es todo. Truncar el log antes del paso 2 sería perder la única copia buena.

---

## 8. Recuperación

Al abrir el archivo:

1. Leer las dos páginas meta, descartar las de CRC inválido, quedarse con la de LSN
   más alto. Si ninguna es válida, el archivo es irrecuperable — se reporta el error,
   no se adivina.
2. Leer el WAL desde ese LSN, registro por registro.
3. Validar el CRC de cada registro. **El primer registro con CRC inválido marca el
   punto exacto donde ocurrió la caída:** ahí se detiene la lectura y se descarta todo
   lo que siga, incluso si más adelante hubiera registros aparentemente válidos.
4. Aplicar las imágenes de página leídas al archivo de datos.
5. `fsync`.
6. Ejecutar `Validate()` sobre el árbol resultante.

Dos propiedades que deben cumplirse:

- **Idempotencia:** reaplicar el mismo log dos veces produce el mismo resultado. Es
  obligatorio porque el sistema puede caerse *durante* la recuperación. Con imágenes
  de página esto es automático.
- **Atomicidad del último registro:** un registro escrito a medias se descarta entero.
  El CRC es lo que lo garantiza.

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

### 9.2 El ciclo de prueba

```
para N en 1..500:
    abrir DB con disco falso configurado para caer en la escritura N
    escribir un conjunto conocido de claves, registrando cuáles devolvieron OK
    la caída ocurre
    reabrir DB
    verificar:
      (a) Validate() pasa: los seis invariantes del árbol se sostienen
      (b) toda clave cuyo Put devolvió OK está presente y con el valor correcto
      (c) ninguna clave que nunca se escribió aparece
```

El punto (b) es la afirmación central del proyecto entero.

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

---

## 10. Plan por fases

Presupuesto: 4-5 horas semanales durante 26 semanas ≈ 103 horas.

| Fase | Semanas | Horas | Terminada cuando |
|---|---|---|---|
| F0 · Go base | 1-2 | 9 | Lees y escribes registros de longitud variable en un archivo, con tests |
| F1 · Páginas y pager | 3-5 | 13 | Serializas una página, la relees, y detectas corrupción al alterar un byte |
| F2 · B+tree | 6-12 | 30 | Put/Get/Scan con 100.000 claves y `Validate()` en verde |
| F3 · WAL y recuperación | 13-17 | 22 | `kill -9` manual y al reabrir están todas las claves confirmadas |
| F4 · Inyección de fallos | 18-21 | 18 | Tabla con 500 puntos de caída y cero pérdidas |
| F5 · Property testing | 22-23 | 9 | Miles de operaciones aleatorias coinciden con el modelo de referencia |
| F6 · Documentación | 24-26 | 12 | DESIGN, TESTING y BUGS escritos, CI en verde |

### Reglas de rescate

- Si en la semana 14 la F2 no está terminada: se reemplaza el B+tree por un índice
  hash en memoria con log en disco. Se pierde el `Scan` de rango y se ganan seis
  semanas. El proyecto sigue siendo válido porque la parte que importa es la F4.
- Si en la semana 20 hay retraso: se recorta la F5, nunca la F4.
- La F6 no se recorta bajo ninguna circunstancia. Un proyecto difícil sin documentar
  no comunica nada.

---

## 11. Riesgos

| Riesgo | Mitigación |
|---|---|
| El alcance se expande (SQL, concurrencia, red) | Lista de no-objetivos escrita y fija desde el día uno |
| El borrado con fusión de nodos resulta más complejo de lo previsto | Se puede entregar con borrado por marca (tumbstone) sin fusión, documentando la limitación |
| Bugs que aparecen miles de operaciones después de su causa | `Validate()` invocable tras cada operación en modo depuración |
| Aprender Go toma más de lo previsto | La F0 es evaluable: si a las 9 horas el lenguaje no fluye, se reevalúa el proyecto antes de invertir 90 horas más |

---

## 12. Qué queda en el repositorio

- El código, sin dependencias externas.
- `DESIGN.md` — este documento.
- `NO-GOALS.md` — los límites, explícitos.
- `TESTING.md` — la tabla de inyección de fallos con los resultados.
- `BUGS.md` — los errores encontrados durante el desarrollo, qué los causaba y cómo se
  detectaron. Esta es la sección que más comunica a un lector técnico.
- CI que ejecuta la suite completa, incluida la inyección de fallos.

---

## 13. Glosario

- **Página:** bloque de tamaño fijo (4096 bytes), unidad mínima de E/S.
- **B+tree:** árbol de búsqueda de aridad alta donde los valores viven solo en las
  hojas, y las hojas están enlazadas entre sí.
- **WAL (write-ahead log):** archivo al que solo se agrega, donde se registra un
  cambio antes de aplicarlo al archivo de datos.
- **LSN (log sequence number):** número creciente que identifica cada registro del log.
- **fsync:** llamada al sistema que obliga a que los datos escritos lleguen al disco
  físico. Sin ella, `write()` solo llega al caché del sistema operativo.
- **Escritura desgarrada (torn write):** escritura interrumpida que deja una página
  medio vieja y medio nueva.
- **Checkpoint:** momento en que las páginas modificadas se llevan al archivo de datos
  y el log puede truncarse.
- **Idempotencia:** aplicar una operación varias veces produce el mismo resultado que
  aplicarla una vez.
