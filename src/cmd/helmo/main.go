package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/registry"
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

	for _, a := range apps {
		if err := compose.EnsureWrapper(a.Dir); err != nil {
			log.Printf("warning: app %s: %v", a.ID, err)
		}
	}

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
	}, (&web.Server{
		Docker:   dc,
		Compose:  &compose.Manager{Runner: compose.ExecRunner{}},
		Registry: registry.Source{ConfigPath: dockerConfigPath()},
	}).Handler())
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

// dockerConfigPath is where "docker login" keeps registry credentials.
func dockerConfigPath() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}
