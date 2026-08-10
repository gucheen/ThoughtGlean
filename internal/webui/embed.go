package webui

import (
	"embed"
	"io/fs"
)

//go:embed assets/*
var embedded embed.FS

func Assets() fs.FS {
	assets, err := fs.Sub(embedded, "assets")
	if err != nil {
		panic(err)
	}
	return assets
}
