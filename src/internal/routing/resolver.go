// Package routing maps incoming requests to managed apps and applies the
// per-request checks (identity, CSRF, audit) shared by every panel handler.
package routing

import (
	"context"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"helmo/internal/config"
)

// Resolver finds the app a request belongs to. Helmo is reachable only
// through Traefik, which overwrites X-Forwarded-Port with the port of the
// entrypoint that accepted the connection, so that header identifies the
// app. The port in Host, when present, must agree with it.
type Resolver struct {
	mu     sync.RWMutex
	apps   []config.App
	byPort map[int]config.App
}

func NewResolver(apps []config.App) *Resolver {
	r := &Resolver{}
	r.Reload(apps)
	return r
}

// Reload replaces the known apps; requests in flight keep the app they
// already resolved.
func (r *Resolver) Reload(apps []config.App) {
	byPort := make(map[int]config.App)
	for _, a := range apps {
		for _, p := range a.Ports {
			byPort[p] = a
		}
	}
	r.mu.Lock()
	r.apps, r.byPort = apps, byPort
	r.mu.Unlock()
}

// Watch calls load every interval until ctx ends and reloads the resolver
// when the apps changed, so a new .helmo/app.yaml needs no restart of Helmo.
// A failing load (for example a half-written file) keeps the apps that work
// and is reported once per distinct error. changed, when not nil, is called
// with the new apps after each reload.
func (r *Resolver) Watch(ctx context.Context, interval time.Duration, load func() ([]config.App, error),
	changed func([]config.App), logf func(format string, args ...any)) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	lastErr := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		apps, err := load()
		if err != nil {
			if err.Error() != lastErr {
				logf("reload apps: %v (keeping the previous configuration)", err)
			}
			lastErr = err.Error()
			continue
		}
		lastErr = ""
		r.mu.RLock()
		same := reflect.DeepEqual(apps, r.apps)
		r.mu.RUnlock()
		if same {
			continue
		}
		r.Reload(apps)
		logf("reloaded %d app(s)", len(apps))
		if changed != nil {
			changed(apps)
		}
	}
}

// Resolve returns the app for req, or false when the request must be
// answered with 404.
func (r *Resolver) Resolve(req *http.Request) (config.App, bool) {
	vals := req.Header.Values("X-Forwarded-Port")
	if len(vals) != 1 {
		return config.App{}, false
	}
	port, err := strconv.Atoi(strings.TrimSpace(vals[0]))
	if err != nil {
		return config.App{}, false
	}

	host, hostPort := splitHost(req.Host)
	if hostPort != "" && hostPort != strconv.Itoa(port) {
		return config.App{}, false
	}

	r.mu.RLock()
	app, ok := r.byPort[port]
	r.mu.RUnlock()
	if !ok {
		return config.App{}, false
	}
	if len(app.Hosts) > 0 && !contains(app.Hosts, host) {
		return config.App{}, false
	}
	return app, true
}

// splitHost returns the lower-cased host name and the port ("" when absent).
func splitHost(hostport string) (host, port string) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		h, p = strings.Trim(hostport, "[]"), ""
	}
	return strings.ToLower(h), p
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
