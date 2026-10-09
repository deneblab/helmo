package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/deploy"
	"helmo/internal/docker"
	"helmo/internal/registry"
	"helmo/internal/routing"
	"helmo/internal/web"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheck := flag.Bool("healthcheck", false, "query the running server and exit 0 when it is healthy")
	flag.Parse()

	listen := env("HELMO_LISTEN", ":8080")
	switch {
	case *showVersion:
		fmt.Println(version)
		return
	case *healthcheck:
		os.Exit(runHealthcheck(listen))
	}

	appsDir := env("HELMO_APPS_DIR", "/srv/apps")
	log.Printf("helmo %s", version)

	apps, err := config.Load(appsDir)
	if err != nil {
		log.Fatalf("load apps from %s: %v", appsDir, err)
	}
	log.Printf("loaded %d app(s) from %s", len(apps), appsDir)
	for _, a := range apps {
		if err := compose.EnsureWrapper(a.Dir); err != nil {
			log.Printf("warning: app %s: cannot write .helmo/dc: %v (is %s writable for this user?)", a.ID, err, a.Dir)
		}
	}

	dockerHost := env("DOCKER_HOST", "unix:///var/run/docker.sock")
	dc, err := docker.NewClient(dockerHost)
	if err != nil {
		log.Fatal(err)
	}
	cm := &compose.Manager{Runner: compose.ExecRunner{}} // one Manager: deploys and start/stop share the per-app lock
	reg := registry.Source{ConfigPath: dockerConfigPath()}
	startupChecks(dc, dockerHost, cm, reg.ConfigPath)

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
		Compose:  cm,
		Registry: reg,
		Deployer: &deploy.Deployer{Compose: cm, Docker: dc, Registry: reg},
		Version:  version,
	}).Handler())
	mux.Handle("/_helmo", panel)
	mux.Handle("/_helmo/", panel)

	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			srv.Close() // open log streams do not end by themselves
		}
	}()

	log.Printf("listening on %s", listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// startupChecks only warns: a missing piece should be visible in the log
// but must not stop the panel from starting.
func startupChecks(dc docker.DockerOps, dockerHost string, cm *compose.Manager, configPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := dc.Ping(ctx); err != nil {
		log.Printf("warning: docker at %s is not reachable: %v", dockerHost, err)
	}
	if v, err := cm.Version(ctx); err != nil {
		log.Printf("warning: docker compose is not usable: %v", err)
	} else {
		log.Printf("docker compose %s", v)
	}
	if _, err := os.Stat(configPath); err != nil {
		log.Printf("note: no registry credentials at %s; only public images can be listed and pulled", configPath)
	}
}

func runHealthcheck(listen string) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad HELMO_LISTEN:", err)
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "unhealthy:", resp.Status)
		return 1
	}
	return 0
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
