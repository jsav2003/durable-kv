package fsx

import "errors"

// ErrNoEsDirectorio indica que la ruta existe pero no es un directorio. Es un error
// propio porque el fallo de os.Stat no lo distingue: la ruta se abrió sin problema y aun
// así no sirve.
var ErrNoEsDirectorio = errors.New("fsx: la ruta no es un directorio")
