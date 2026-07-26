package webassets

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist/*
var assets embed.FS

func Handler() http.Handler {
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		cleaned := strings.TrimPrefix(path.Clean(request.URL.Path), "/")
		if cleaned == "." || cleaned == "" {
			cleaned = "index.html"
		}
		if _, err := fs.Stat(dist, cleaned); err != nil {
			clone := request.Clone(request.Context())
			clone.URL.Path = "/"
			files.ServeHTTP(w, clone)
			return
		}
		files.ServeHTTP(w, request)
	})
}
