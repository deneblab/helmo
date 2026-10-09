package traefik

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The API of a Traefik like the one on the first real server: File provider
// only, one TLS entrypoint per app, plus web/websecure and the dashboard.
func fakeTraefik(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/overview":
			w.Write([]byte(`{"http":{},"providers":["File"]}`))
		case "/api/entrypoints":
			w.Write([]byte(`[
				{"name":"web","address":":80"},
				{"name":"traefik","address":":8080"},
				{"name":"extraction-manager-develop","address":":8102/tcp","http":{"tls":{}}},
				{"name":"plain-app","address":"0.0.0.0:8085"},
				{"name":"other-tls","address":"[::]:8086"}
			]`))
		case "/api/http/routers":
			w.Write([]byte(`[
				{"name":"extraction-manager-develop@file","provider":"file","entryPoints":["extraction-manager-develop"],
				 "rule":"PathPrefix(` + "`/`" + `)","tls":{"certResolver":"","domains":[{"main":"node.example.ts.net"}]}},
				{"name":"plain-app@file","provider":"file","entryPoints":["plain-app"],"rule":"PathPrefix(` + "`/`" + `)"},
				{"name":"other@file","provider":"file","entryPoints":["other-tls"],"tls":{"certResolver":"le"}},
				{"name":"helmo-tls@file","provider":"file","entryPoints":["plain-app"],"tls":{"certResolver":"bogus"}},
				{"name":"dashboard@internal","provider":"internal","entryPoints":["traefik"],"tls":{}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestConfig(t *testing.T) {
	srv := fakeTraefik(t)
	defer srv.Close()
	c := Client{API: srv.URL}

	got, err := c.Config(context.Background(), []int{8102, 8085, 8086, 9999}, DefaultServiceURL)
	if err != nil {
		t.Fatal(err)
	}
	want := `# Helmo: add to the dynamic configuration of Traefik (File provider).
# No Traefik entrypoint listens on port 9999; add one, or check the port.
http:
  routers:
    helmo:
      rule: PathPrefix(` + "`/_helmo`" + `)
      priority: 10000
      entryPoints: ["plain-app"]
      service: helmo
    helmo-tls:
      rule: PathPrefix(` + "`/_helmo`" + `)
      priority: 10000
      entryPoints: ["other-tls"]
      service: helmo
      tls:
        certResolver: "le"
    helmo-tls-2:
      rule: PathPrefix(` + "`/_helmo`" + `)
      priority: 10000
      entryPoints: ["extraction-manager-develop"]
      service: helmo
      tls:
        domains:
          - main: "node.example.ts.net"
  services:
    helmo:
      loadBalancer:
        servers:
          - url: "http://helmo:8080"
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestConfigWithoutApps(t *testing.T) {
	srv := fakeTraefik(t)
	defer srv.Close()
	got, err := Client{API: srv.URL}.Config(context.Background(), nil, DefaultServiceURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "no app ports yet") || !strings.Contains(got, `url: "http://helmo:8080"`) {
		t.Errorf("got:\n%s", got)
	}
}

func TestProviders(t *testing.T) {
	srv := fakeTraefik(t)
	defer srv.Close()
	got, err := Client{API: srv.URL}.Providers(context.Background())
	if err != nil || !reflect.DeepEqual(got, []string{"File"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestAPIErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := (Client{API: srv.URL}).Config(context.Background(), []int{1}, DefaultServiceURL); err == nil {
		t.Fatal("want error")
	}
}

func TestPort(t *testing.T) {
	for addr, want := range map[string]int{":8102": 8102, "0.0.0.0:80": 80, "[::]:443": 443, ":8102/tcp": 8102, "nope": 0, ":x": 0} {
		got, ok := port(addr)
		if (want == 0) == ok || (ok && got != want) {
			t.Errorf("port(%q) = %d, %v", addr, got, ok)
		}
	}
}
