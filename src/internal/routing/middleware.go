package routing

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"helmo/internal/config"
)

// Identity is who is calling. Until real authentication exists every caller
// is the tailnet.
type Identity struct{ Name string }

// Authenticator is the seam for future authentication.
type Authenticator func(*http.Request) (Identity, error)

// Tailnet accepts every request; network access is the only protection.
func Tailnet(*http.Request) (Identity, error) { return Identity{Name: "tailnet"}, nil }

// AuditEvent describes one state-changing request.
type AuditEvent struct {
	Time     time.Time
	AppID    string
	Identity string
	ClientIP string
	Method   string
	Path     string
	Status   int
}

// Options configures Middleware.
type Options struct {
	Resolver     *Resolver
	Authenticate Authenticator
	Audit        func(AuditEvent)
}

// Info is what Middleware stores in the request context.
type Info struct {
	App      config.App
	Identity Identity
}

type ctxKey struct{}

// FromContext returns the Info set by Middleware.
func FromContext(ctx context.Context) (Info, bool) {
	i, ok := ctx.Value(ctxKey{}).(Info)
	return i, ok
}

// Middleware resolves the app (404 when unknown), authenticates (401),
// rejects cross-site state-changing requests (403) and audits the latter.
func Middleware(o Options, next http.Handler) http.Handler {
	auth := o.Authenticate
	if auth == nil {
		auth = Tailnet
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app, ok := o.Resolver.Resolve(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		id, err := auth(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		safe := isSafe(r.Method)
		if !safe && !sameOrigin(r) {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), ctxKey{}, Info{App: app, Identity: id})))

		if !safe && o.Audit != nil {
			o.Audit(AuditEvent{
				Time:     time.Now().UTC(),
				AppID:    app.ID,
				Identity: id.Name,
				ClientIP: ClientIP(r),
				Method:   r.Method,
				Path:     r.URL.Path,
				Status:   sw.status,
			})
		}
	})
}

func isSafe(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

// sameOrigin allows a state-changing request only when the browser says it
// came from the same origin. Requests carrying none of the signals (for
// example curl) are rejected.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	return false
}

// ClientIP is the address Traefik saw: the last X-Forwarded-For entry (the
// one Traefik appended), else the peer address.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush for SSE.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
