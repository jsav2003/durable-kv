# BUGS

Errores encontrados durante el desarrollo: qué los causaba y cómo se detectaron. Este
archivo se acumula fase a fase; no se reescribe, se añade.

## F0 · Registros de longitud variable

**Ningún error de código detectado.**

Los tests exhaustivos (`TestTruncation`, `TestTruncatedLastOfMultiple`,
`TestBitFlipCorruption`, `TestAbsurdLength`) cubren el espacio de fallo por enumeración
completa, no por muestreo: cada offset de truncamiento del último registro, cada bit de
la cabecera, la carga y el crc, y varios valores absurdos del campo de longitud. El
formato del marco se diseñó explícitamente contra estos casos antes de escribir el
código, y los cuatro pasaron a la primera ejecución.

### Nota sobre el fuzzer nativo (DESIGN.md sec. 9.4)

`go test ./internal/record -fuzz=FuzzReader` falla al arrancar en esta máquina con:

```
fork/exec C:\Users\...\record.test.exe: Una directiva de Control de aplicaciones bloqueó este archivo.
```

Esto ocurre **solo** en modo `-fuzz` (que re-ejecuta el binario de test como proceso
*worker*, con variables de entorno y argumentos internos distintos de una corrida
normal). La ejecución estándar de `go test` sobre el mismo binario -- incluidos los
casos `FuzzReader/seed#0`..`seed#5`, que corren el corpus semilla como tests ordinarios
-- funciona sin problema y está en verde.

Es una directiva de Control de aplicaciones de Windows (Smart App Control / WDAC)
bloqueando ese patrón de re-ejecución en esta máquina concreta, no un defecto del
código ni del diseño del marco. No se intentó modificar la directiva del sistema sin
autorización explícita del usuario.

Consecuencia: la corrida de fuzzing de 60 s que pide la sec. 9.4 no se pudo ejecutar
localmente. Queda pendiente correrla en CI (F6) o en una máquina sin esa restricción,
donde se espera que funcione sin cambios en el código.
