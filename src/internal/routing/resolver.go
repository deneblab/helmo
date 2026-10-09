// Package routing maps incoming requests to managed apps and applies the
// per-request checks (identity, CSRF, audit) shared by every panel handler.
package routing

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"helmo/internal/config"
)

// Resolver finds the app a request belongs to. Helmo is reachable only
// through Traefik, which overwrites X-Forwarded-Port with the port of the
// entrypoint that accepted the connection, so that header identifies the
// app. The port in Host, when present, must agree with it.
type Resolver struct {
	byPort map[int]config.App
}

func NewResolver(apps []config.App) *Resolver {
	byPort := make(map[int]config.App)
	for _, a := range apps {
		for _, p := range a.Ports {
			byPort[p] = a
		}
	}
	return &Resolver{byPort: byPort}
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

	app, ok := r.byPort[port]
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
