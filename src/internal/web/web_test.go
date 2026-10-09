package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/routing"
)

func handler(ops docker.DockerOps) http.Handler {
	apps := []config.App{{ID: "cadastro", Dir: "/srv/apps/cadastro", Ports: []int{8600}}}
	return routing.Middleware(routing.Options{Resolver: routing.NewResolver(apps)},
		(&Server{Docker: ops}).Handler())
}

func get(h http.Handler, path, port string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "node:" + port
	r.Header.Set("X-Forwarded-Port", port)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestStatus(t *testing.T) {
	ops := &docker.Fake{Projects: map[string][]docker.Container{
		"/srv/apps/cadastro": {
			{ID: "a1", Name: "cadastro-web-1", Service: "web", Image: "app:v1", State: "running", Health: "healthy"},
			{ID: "b2", Name: "cadastro-db-1", Service: "db", Image: "pg", State: "exited"},
		},
		"/srv/apps/other": {{State: "running"}},
	}}
	rec := get(handler(ops), "/_helmo/api/status", "8600")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var got statusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.App != "cadastro" || got.State != docker.StatePartial || len(got.Containers) != 2 ||
		got.Containers[0].Health != "healthy" {
		t.Fatalf("got %+v", got)
	}
}

func TestStatusStoppedAppHasEmptyList(t *testing.T) {
	rec := get(handler(&docker.Fake{}), "/_helmo/api/status", "8600")
	var got statusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != docker.StateDown || got.Containers == nil || len(got.Containers) != 0 {
		t.Fatalf("got %+v (%s)", got, rec.Body)
	}
}

func TestStatusDockerError(t *testing.T) {
	rec := get(handler(&docker.Fake{Err: errors.New("socket gone")}), "/_helmo/api/status", "8600")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if body := rec.Body.String(); body != "docker unavailable\n" {
		t.Fatalf("error detail leaked: %q", body)
	}
}

func TestStatusUnknownPort(t *testing.T) {
	if rec := get(handler(&docker.Fake{}), "/_helmo/api/status", "9999"); rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
}
