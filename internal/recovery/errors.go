package recovery

import "errors"

// ErrLibreIlegible indica que una página del conjunto de libres no es legible: ni una página
// íntegra con su page_id, ni una ranura a ceros.
//
// Es la mitad del invariante 6 que el barrido del árbol no puede comprobar (sec. 6: "toda
// página de ambos conjuntos tiene CRC y page_id válidos"), y se comprueba solo tras la
// recuperación, que es donde el archivo está materializado entero. Ver comprobarLibres.
var ErrLibreIlegible = errors.New("recovery: una pagina libre no es legible")
