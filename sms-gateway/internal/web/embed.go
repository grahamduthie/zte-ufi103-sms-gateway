package web

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var embedded embed.FS

//go:embed static/*
// StaticFS holds the embedded static files (CSS, logo). Exported for setup mode.
var StaticFS embed.FS

func init() {
	// Ensure the embedded filesystem is valid
	_, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(err)
	}
}
