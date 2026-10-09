package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"helmo/internal/config"
)

func main() {
	appsDir := env("HELMO_APPS_DIR", "/srv/apps")
	listen := env("HELMO_LISTEN", ":8080")

	apps, err := config.Load(appsDir)
	if err != nil {
		log.Fatalf("load apps from %s: %v", appsDir, err)
	}
	log.Printf("loaded %d app(s) from %s", len(apps), appsDir)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("listening on %s", listen)
	log.Fatal(srv.ListenAndServe())
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
