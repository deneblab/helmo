package routing

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"helmo/internal/config"
)

var testApps = []config.App{
	{ID: "cadastro", Ports: []int{8600}},
	{ID: "blog", Ports: []int{8601, 8602}, Hosts: []string{"node.ts.net"}},
}

func req(method, host, xfp string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(method, "/_helmo/", nil)
	r.Host = host
	if xfp != "" {
		r.Header.Set("X-Forwarded-Port", xfp)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestResolve(t *testing.T) {
	res := NewResolver(testApps)
	tests := []struct {
		name string
		r    *http.Request
		want string // app ID, "" = not found
	}{
		{"port from forwarded header", req("GET", "node:8600", "8600", nil), "cadastro"},
		{"second port of an app", req("GET", "node.ts.net:8602", "8602", nil), "blog"},
		{"host without port (default port)", req("GET", "node.ts.net", "8601", nil), "blog"},
		{"host narrowing, case-insensitive", req("GET", "NODE.ts.net:8601", "8601", nil), "blog"},
		{"host not allowed for app", req("GET", "evil.example:8601", "8601", nil), ""},
		{"unknown port", req("GET", "node:9999", "9999", nil), ""},
		{"missing forwarded port", req("GET", "node:8600", "", nil), ""},
		{"garbage forwarded port", req("GET", "node:8600", "abc", nil), ""},
		{"host port disagrees", req("GET", "node:8600", "8601", nil), ""},
		{"ipv6 host", req("GET", "[::1]:8600", "8600", nil), "cadastro"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, ok := res.Resolve(tt.r)
			if tt.want == "" {
				if ok {
					t.Fatalf("resolved %q, want not found", app.ID)
				}
				return
			}
			if !ok || app.ID != tt.want {
				t.Fatalf("got %q (%v), want %q", app.ID, ok, tt.want)
			}
		})
	}
}

func TestResolveRejectsRepeatedForwardedPort(t *testing.T) {
	r := req("GET", "node:8600", "8600", nil)
	r.Header.Add("X-Forwarded-Port", "8601")
	if _, ok := NewResolver(testApps).Resolve(r); ok {
		t.Fatal("repeated X-Forwarded-Port must be rejected")
	}
}

func TestMiddleware(t *testing.T) {
	var events []AuditEvent
	var seen Info
	h := Middleware(Options{
		Resolver: NewResolver(testApps),
		Audit:    func(e AuditEvent) { events = append(events, e) },
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusAccepted)
	}))

	sameSite := map[string]string{"Sec-Fetch-Site": "same-origin"}
	tests := []struct {
		name       string
		r          *http.Request
		wantStatus int
		wantAudit  bool
	}{
		{"unknown app", req("GET", "node:9999", "9999", nil), 404, false},
		{"GET passes without audit", req("GET", "node:8600", "8600", nil), 202, false},
		{"POST same-origin via fetch metadata", req("POST", "node:8600", "8600", sameSite), 202, true},
		{"POST same-origin via Origin", req("POST", "node:8600", "8600", map[string]string{"Origin": "http://node:8600"}), 202, true},
		{"POST cross-site fetch metadata", req("POST", "node:8600", "8600", map[string]string{"Sec-Fetch-Site": "cross-site"}), 403, false},
		{"POST foreign Origin", req("POST", "node:8600", "8600", map[string]string{"Origin": "http://evil.example"}), 403, false},
		{"POST without browser signals", req("POST", "node:8600", "8600", nil), 403, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events = nil
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tt.r)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := len(events) == 1; got != tt.wantAudit {
				t.Fatalf("audit events %d, want audit=%v", len(events), tt.wantAudit)
			}
		})
	}

	events = nil
	r := req("POST", "node:8600", "8600", map[string]string{"Sec-Fetch-Site": "same-origin", "X-Forwarded-For": "1.2.3.4, 100.64.0.7"})
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen.App.ID != "cadastro" || seen.Identity.Name != "tailnet" {
		t.Errorf("context info: %+v", seen)
	}
	e := events[0]
	if e.AppID != "cadastro" || e.Identity != "tailnet" || e.ClientIP != "100.64.0.7" ||
		e.Method != "POST" || e.Status != 202 {
		t.Errorf("audit event: %+v", e)
	}
}

func TestAuthenticateFailure(t *testing.T) {
	h := Middleware(Options{
		Resolver:     NewResolver(testApps),
		Authenticate: func(*http.Request) (Identity, error) { return Identity{}, errors.New("no") },
	}, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("must not run") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("GET", "node:8600", "8600", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestClientIPFallsBackToPeer(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "100.64.0.9:5555"
	if got := ClientIP(r); got != "100.64.0.9" {
		t.Fatalf("got %q", got)
	}
}
