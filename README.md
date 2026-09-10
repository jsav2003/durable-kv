# motor-almacenamiento

Un motor de almacenamiento clave-valor embebido, escrito en Go sin dependencias externas.
Es la capa más baja de una base de datos: un B+tree sobre páginas de 4 KiB, con
write-ahead log y recuperación ante caídas.

**La garantía, y a ella sirve todo lo demás:** si `Put` devuelve `nil`, ese dato sobrevive
a cualquier caída posterior. No porque esté en su sitio definitivo —probablemente no lo
esté todavía— sino porque está en el log y ese log ya pasó por `fsync`.

Lo que hace interesante al proyecto no es el árbol, que es un ejercicio de estructuras de
datos: es **demostrar esa garantía**. Un barrido de 500 puntos de caída sobre un disco
falso que descarta, reordena y desgarra las escrituras sin sincronizar; un `kill -9` real
sobre un subproceso; y miles de operaciones aleatorias contra un modelo de referencia.

---

## Uso

```go
import motor "github.com/jsav2003/motor-almacenamiento"

db, err := motor.Open("ruta/a/la/base")   // el directorio se crea si no existe
if err != nil {
    return err
}
defer db.Close()

if err := db.Put([]byte("clave"), []byte("valor")); err != nil {
    return err   // solo aquí; si devuelve nil, el dato ya es duradero
}

v, err := db.Get([]byte("clave"))         // ErrNotFound si no está
if err != nil {
    return err
}

err = db.Scan(nil, nil, func(k, v []byte) bool {
    fmt.Printf("%s = %s\n", k, v)
    return true                            // devolver false corta el recorrido
})
```

El contrato de la sec. 4 del `DESIGN.md` son seis funciones y hay **cinco**: `Open`, `Put`,
`Get`, `Scan` y `Close`. La sexta es `Delete`, y no existe todavía a propósito: la tabla de
fases no se lo asigna a ninguna, y un método público que devuelve "no implementado" es peor
que uno que no está, porque compila en el código del llamador. Sin `Delete` no se libera
ninguna página, así que **todo lo que este motor demuestra es sobre un árbol que solo
crece** — el límite más grande de la lista, y está dicho también en `TESTING.md`.

Fuera del contrato hay una función más, `Validate`, que comprueba los seis invariantes del
árbol entero. Es cara y está para el modo depuración y para el arnés de inyección de fallos,
donde es literalmente el criterio (a) de cada punto de caída.

Los errores —`ErrNotFound`, `ErrEntryTooLarge`, `ErrKeyTooLarge`, `ErrCerrada`— están
reexportados en el paquete raíz: los de `internal/` no son importables desde fuera del
módulo, así que sin eso un llamador no podría distinguir una clave ausente de un fallo de
disco.

Un detalle de memoria que conviene leer antes de usarlo: el valor que devuelve `Get` es
**una copia** y se puede conservar; los pares que recibe el `fn` de `Scan` **no**, valen lo
que dura la llamada. Quien los conserve, copia.

## Límites

Son exclusiones **permanentes**, no tareas pendientes; están en [`NO-GOALS.md`](NO-GOALS.md)
para no discutirlas dos veces:

- Sin SQL, sin red, sin índices secundarios, sin compresión ni cifrado.
- **Sin concurrencia:** un solo hilo escritor, sin transacciones multi-operación. `DB` no
  es segura para uso concurrente.
- Sin páginas de desbordamiento: una clave llega a 512 bytes, y `clave + valor + 6` no
  puede pasar de 1000.
- No busca medirse en rendimiento contra motores reales.

## Correr la suite

```sh
go test ./...              # todo: 12 paquetes, medio minuto
go test -short ./...       # salta los cuatro tests caros
go test -race ./...        # necesita cgo
```

`-short` salta justamente los cuatro por los que existe el proyecto: el barrido de 500
puntos de caída, las 100.000 claves del árbol, las dos corridas de propiedades y el
`kill -9`. En CI se corre **sin** `-short`.

```sh
go test -run TestQuinientosPuntosDeCaida -v .   # la tabla de inyección de fallos
go test -run TestCaidaYReapertura -v .          # el kill -9, en un subproceso de verdad
go test -fuzz FuzzArbol ./internal/tree         # uno de los cinco objetivos de fuzz
```

Cada punto de caída lleva su semilla registrada: el mismo número de escritura con la misma
semilla reproduce el estado del disco bit a bit, y por eso `BUGS.md` puede citar casos
concretos.

## Cómo está organizado

```
db.go                    la interfaz pública, cinco de las seis funciones
internal/page            la página de 4 KiB: cabecera, CRC, codificación
internal/node            celdas, directorio de slots y la división por bytes
internal/tree            el B+tree: descenso, Put/Get/Scan, y Validate()
internal/record          registros de longitud variable sobre un archivo
internal/wal             el write-ahead log y su rotación
internal/meta            las dos páginas meta alternadas
internal/pager           el caché de páginas y el contrato con el log
internal/checkpoint      los cinco pasos del checkpoint
internal/recovery        los diez pasos de la recuperación
internal/fsx             la capa de sistema de archivos (y su doble, fsxtest)
```

`internal/fsx/fsxtest` es el disco falso: mantiene las escrituras sin sincronizar en una
cola **compartida por todos los archivos**, para que el orden relativo entre `datos.db` y
el log —que *es* el write-ahead logging— se pueda observar y afirmar.

## Los documentos

| Archivo | Qué es |
|---|---|
| [`DESIGN.md`](DESIGN.md) | La especificación. Se escribió **antes** del código y no se edita sobre la marcha. |
| [`NO-GOALS.md`](NO-GOALS.md) | Los límites, explícitos y permanentes. |
| [`TESTING.md`](TESTING.md) | Qué prueba la suite y qué no: la tabla de inyección de fallos y el argumento de correctitud. |
| [`BUGS.md`](BUGS.md) | Los errores por fase: qué los causaba y cómo se detectaron, con la semilla que los reproduce. |
| [`docs/DEUDA-DISENO.md`](docs/DEUDA-DISENO.md) | Las doce decisiones de diseño y su estado. |
| [`docs/REVIEW-01.md`](docs/REVIEW-01.md) | La revisión adversarial del diseño v1, previa al código. Asistida por IA, identificada como tal. |

**Si vas a leer solo dos:** `TESTING.md` para saber qué se demuestra y con qué, y `BUGS.md`
para ver qué se rompió por el camino. El segundo es el que más dice de un proyecto así.

## Estado

En la **F6**, la última de las seis fases de la sec. 10 del `DESIGN.md`. Las fases de
código están hechas y las diez filas del argumento de correctitud tienen cobertura por
inyección de fallos. `Delete` sigue sin fase asignada, con la consecuencia dicha arriba.

La fase **no está cerrada**: su criterio incluye "CI en verde", y el workflow
—[`.github/workflows/ci.yml`](.github/workflows/ci.yml), matriz Linux + Windows— está
escrito y empujado pero todavía no ha llegado a ejecutarse. Con él está pendiente también
la primera corrida del detector de carreras, que necesita cgo. Un workflow que nunca corrió
es exactamente igual de fiable que un test que nunca corrió, así que queda dicho aquí y no
en una nota al pie.
