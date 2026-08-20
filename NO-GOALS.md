# NO-GOALS

Los límites del proyecto, explícitos. Estas exclusiones son deliberadas y permanentes:
cada una es un proyecto aparte, no una tarea pendiente. Este archivo existe para que no
haga falta discutirlas dos veces.

Fuente: sec. 2 de `DESIGN.md` (v2). Los dos últimos apartados se derivan de
`docs/REVIEW-01.md`.

## Fuera de alcance, permanentemente

- **SQL.** No hay parser, ni planificador de consultas, ni álgebra relacional.
- **Concurrencia.** Un solo hilo escritor. Sin transacciones multi-operación.
- **Red.** No hay cliente ni servidor. Es una librería embebida.
- **Índices secundarios.** El único orden es el de la clave primaria.
- **Compresión, replicación y cifrado.**
- **Rendimiento competitivo.** No busca medirse contra motores reales.

## Páginas de desbordamiento

**Fuera de alcance.** Un par clave+valor debe caber holgadamente en una página de 4096
bytes.

Límite concreto (sec. 4 de `DESIGN.md`): clave hasta 512 bytes, y la suma
`clave + valor + 6 bytes de cabecera de celda` **no puede pasar de 1000 bytes**. `Put`
devuelve `ErrEntryTooLarge` si se excede.

El número se deriva del objetivo de llenado, no de la comodidad: para que una hoja llena
siempre pueda dividirse en dos mitades razonablemente ocupadas hacen falta al menos cuatro
celdas por página; 4096 − 40 de cabecera = 4056 útiles, y 4056 / 4 ≈ 1014.

La versión 1 del diseño afirmaba "valor hasta 1 MB … cabe holgadamente en una página", lo
cual es falso por un factor de 256, y sobre esa afirmación se justificaba precisamente no
implementar desbordamiento. Almacenar valores grandes es un proyecto distinto: requiere
páginas de desbordamiento, encadenado, y un formato de registro de WAL que deje de ser de
ancho fijo.

## Regla de rescate de la semana 14, y qué deja de probar la F4

**Estado: no tomada.** Este apartado se activa solo si en la semana 14 la F2 no está
terminada y se aplica la regla de rescate de la sec. 10 de `DESIGN.md` —sustituir el
B+tree por un índice hash en memoria con log en disco—.

Si se toma esa salida, el costo declarado es:

> Esa variante elimina la actualización en sitio, que es de donde vienen los torn writes,
> las páginas meta y el conjunto de libres. **La F4 resultante prueba durabilidad de
> anexado, no de actualización en sitio.** Se conserva el nombre de la fase, no su
> dificultad.

Decir "el proyecto sigue siendo válido porque la parte que importa es la F4" sin esta
aclaración sería falso. También se pierde el `Scan` de rango.
