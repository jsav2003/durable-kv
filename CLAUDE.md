# Motor de almacenamiento clave-valor en Go

## Documentos de referencia
- DESIGN.md es la especificación. No lo edites; si algo del diseño
  está mal, dímelo y lo decido yo.
- NO-GOALS.md define los límites. No implementes nada que esté ahí.
- docs/REVIEW-01.md es histórico, con numeración de secciones del v1.

## Reglas de trabajo
- Una fase a la vez, según la tabla de la sec. 10 del DESIGN.md.
  No adelantes fases.
- Al terminar cada fase, escribe en BUGS.md los errores encontrados,
  qué los causaba y cómo se detectaron. Con semilla si aplica.
- Tests junto con el código, no después.
- Sin dependencias externas. Solo librería estándar.
- Commits atómicos, formato convencional, con tag de fase: feat(f0): ...
