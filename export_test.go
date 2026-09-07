package motor

import "github.com/jsav2003/motor-almacenamiento/internal/fsx"

// AbrirCon expone abrirCon al arnés de inyección de fallos de la F4 (crash_injection_test.go),
// que necesita abrir la base sobre un fsx.Dir en memoria en vez de un directorio de disco.
// No forma parte del contrato de la sec. 4: es un punto de enganche de pruebas y por eso
// vive en un archivo _test.go, fuera del binario que se compila para un consumidor.
func AbrirCon(dir fsx.Dir, umbral int64) (*DB, error) {
	return abrirCon(dir, umbral)
}
