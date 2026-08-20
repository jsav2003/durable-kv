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
