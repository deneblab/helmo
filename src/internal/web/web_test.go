package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/routing"
)

type fakeRunner struct {
	out    string
	err    error
	called [][]string
}

func (f *fakeRunner) Run(_ context.Context, dir string, args ...string) (string, error) {
	f.called = append(f.called, append([]string{dir}, args...))
	return f.out, f.err
}

func handler(ops docker.DockerOps) http.Handler {
	return handlerWith(ops, &fakeRunner{})
}

func handlerWith(ops docker.DockerOps, r compose.Runner) http.Handler {
	apps := []config.App{{ID: "cadastro", Dir: "/srv/apps/cadastro", Ports: []int{8600}}}
	return routing.Middleware(routing.Options{Resolver: routing.NewResolver(apps)},
		(&Server{Docker: ops, Compose: &compose.Manager{Runner: r}}).Handler())
}

func post(h http.Handler, path, port string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, nil)
	r.Host = "node:" + port
	r.Header.Set("X-Forwarded-Port", port)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
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

func TestOperate(t *testing.T) {
	f := &fakeRunner{out: "done"}
	rec := post(handlerWith(&docker.Fake{}, f), "/_helmo/api/restart", "8600")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got opJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Op != "restart" || got.Output != "done" {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if len(f.called) != 1 || f.called[0][0] != "/srv/apps/cadastro" || f.called[0][1] != "restart" {
		t.Fatalf("runner calls: %v", f.called)
	}
}

func TestOperateFailureHidesInternalError(t *testing.T) {
	f := &fakeRunner{out: "no such service", err: errors.New("exit status 1")}
	rec := post(handlerWith(&docker.Fake{}, f), "/_helmo/api/start", "8600")
	if rec.Code != 500 {
		t.Fatalf("status %d", rec.Code)
	}
	var got opJSON
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Error != "start failed" || got.Output != "no such service" {
		t.Fatalf("got %+v", got)
	}
}

func TestOperateUnknownOpAndCrossSite(t *testing.T) {
	h := handlerWith(&docker.Fake{}, &fakeRunner{})
	if rec := post(h, "/_helmo/api/down", "8600"); rec.Code != 404 {
		t.Errorf("unknown op: %d", rec.Code)
	}
	if rec := post(h, "/_helmo/api/stop", "9999"); rec.Code != 404 {
		t.Errorf("unknown port: %d", rec.Code)
	}
	r := httptest.NewRequest("POST", "/_helmo/api/stop", nil)
	r.Host = "node:8600"
	r.Header.Set("X-Forwarded-Port", "8600")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Errorf("no CSRF signal: %d", rec.Code)
	}
}

func TestTidyOutput(t *testing.T) {
	in := "Container serpsearch  Restarting \r\n Container serpsearch  Started\n\n"
	if got, want := tidyOutput(in), "Container serpsearch Restarting\nContainer serpsearch Started"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
