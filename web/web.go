// Package web embute o painel (HTML/CSS/JS estáticos) no binário do servidor.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var embedded embed.FS

// Files é a raiz servida em "/".
var Files, _ = fs.Sub(embedded, "static")
