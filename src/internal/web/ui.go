package web

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"time"

	"helmo/internal/routing"
)

//go:embed ui/index.html ui/app.css ui/app.js
var uiFiles embed.FS

var indexTemplate = template.Must(template.ParseFS(uiFiles, "ui/index.html"))

// The page and its assets are same-origin only: no inline script or style,
// no framing, no third-party requests.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

var staticTypes = map[string]string{
	"app.css": "text/css; charset=utf-8",
	"app.js":  "text/javascript; charset=utf-8",
}

func (s *Server) uiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /_helmo", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/_helmo/", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /_helmo/{$}", s.index)
	mux.HandleFunc("GET /_helmo/static/{file}", s.static)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	if err := indexTemplate.Execute(w, struct{ AppID string }{info.App.ID}); err != nil {
		log.Printf("render index: %v", err)
	}
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ctype, ok := staticTypes[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := uiFiles.Open("ui/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	rs, ok := f.(fs.File).(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, name, time.Time{}, rs)
}
