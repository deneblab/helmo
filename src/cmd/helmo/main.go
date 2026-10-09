package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/routing"
	"helmo/internal/web"
)

func main() {
	appsDir := env("HELMO_APPS_DIR", "/srv/apps")
	listen := env("HELMO_LISTEN", ":8080")

	apps, err := config.Load(appsDir)
	if err != nil {
		log.Fatalf("load apps from %s: %v", appsDir, err)
	}
	log.Printf("loaded %d app(s) from %s", len(apps), appsDir)

	dockerHost := env("DOCKER_HOST", "unix:///var/run/docker.sock")
	dc, err := docker.NewClient(dockerHost)
	if err != nil {
		log.Fatal(err)
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := dc.Ping(pingCtx); err != nil {
		log.Printf("warning: docker at %s is not reachable: %v", dockerHost, err)
	}
	cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	panel := routing.Middleware(routing.Options{
		Resolver: routing.NewResolver(apps),
		Audit: func(e routing.AuditEvent) {
			log.Printf("audit app=%s identity=%s ip=%s %s %s status=%d",
				e.AppID, e.Identity, e.ClientIP, e.Method, e.Path, e.Status)
		},
	}, (&web.Server{Docker: dc}).Handler())
	mux.Handle("/_helmo/", panel)

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
