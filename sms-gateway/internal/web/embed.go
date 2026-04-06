package web

import (
	"embed"
	"io/fs"
)

//go:embed templates/*.html
var embedded embed.FS

//go:embed static/*
var staticFS embed.FS

func init() {
	// Ensure the embedded filesystem is valid
	_, err := fs.Sub(embedded, "templates")
	if err != nil {
		panic(err)
	}
}
