package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/deploy"
	"helmo/internal/docker"
	"helmo/internal/registry"
)

var dig = "sha256:" + strings.Repeat("7", 64)

type digestSource struct{ err error }

func (d digestSource) Digest(_ context.Context, _ registry.Ref, ref string) (string, error) {
	if d.err != nil {
		return "", d.err
	}
	if ref == "v9.9.9" {
		return "", fmt.Errorf("ghcr.io/org/app: %w", registry.ErrNotFound)
	}
	return dig, nil
}

// appTagRunner resolves the image like a compose file with
// ghcr.io/org/app:${APP_TAG:-v1.2.3}, reading APP_TAG from .helmo/env.
type appTagRunner struct{ fakeRunner }

func (a *appTagRunner) Output(_ context.Context, dir string, _ ...string) (string, error) {
	tag, ok, _ := deploy.GetVar(filepath.Join(dir, ".helmo", "env"), "APP_TAG")
	if !ok || tag == "" {
		tag = "v1.2.3"
	}
	return fmt.Sprintf(`{"services":{"web":{"image":"ghcr.io/org/app:%s"}}}`, tag), nil
}

func deployHandler(t *testing.T, reg digestSource) (http.Handler, config.App, *compose.Manager) {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(dir, ".helmo"), 0o755)
	app := config.App{ID: "cadastro", Dir: dir, Ports: []int{8600}, Service: "web", HealthTimeout: 20 * time.Millisecond}

	ops := &docker.Fake{Projects: map[string][]docker.Container{dir: {{ID: "c", Service: "web", State: "running", Health: "healthy"}}}}
	cm := &compose.Manager{Runner: &appTagRunner{}}
	s := &Server{
		Docker:   ops,
		Compose:  cm,
		Deployer: &deploy.Deployer{Compose: cm, Docker: ops, Registry: reg, PollInterval: time.Millisecond},
	}
	apps := []config.App{app}
	return routingFor(apps, s), app, cm
}

func waitDeploy(t *testing.T, h http.Handler) jobJSON {
	t.Helper()
	for i := 0; i < 400; i++ {
		rec := get(h, "/_helmo/api/deploy", "8600")
		var j jobJSON
		if rec.Code == 200 && json.Unmarshal(rec.Body.Bytes(), &j) == nil && j.State != "running" {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("deploy did not finish")
	return jobJSON{}
}

func TestDeployEndpointsHappyPath(t *testing.T) {
	h, app, _ := deployHandler(t, digestSource{})

	if rec := get(h, "/_helmo/api/deploy", "8600"); rec.Code != 404 {
		t.Fatalf("status before any deploy: %d", rec.Code)
	}

	rec := post(h, "/_helmo/api/deploy?tag=v1.3.0&dry_run=1", "8600")
	var plan map[string]any
	json.Unmarshal(rec.Body.Bytes(), &plan)
	if rec.Code != 200 || plan["dry_run"] != true || plan["current"] != "v1.2.3" ||
		plan["target"] != "v1.3.0@"+dig || plan["image"] != "ghcr.io/org/app" || plan["changes"] != true {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(app.Dir, ".helmo", "env")); err == nil {
		t.Fatal("dry run must not write anything")
	}

	rec = post(h, "/_helmo/api/deploy?tag=v1.3.0", "8600")
	var job jobJSON
	json.Unmarshal(rec.Body.Bytes(), &job)
	if rec.Code != http.StatusAccepted || job.Action != "deploy" || job.To != "v1.3.0@"+dig {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	done := waitDeploy(t, h)
	if done.State != "ok" || done.Finished == "" {
		t.Fatalf("final %+v", done)
	}
	if v, _, _ := deploy.GetVar(filepath.Join(app.Dir, ".helmo", "env"), "APP_TAG"); v != "v1.3.0@"+dig {
		t.Errorf("APP_TAG = %q", v)
	}

	rec = get(h, "/_helmo/api/history", "8600")
	var hist []deploy.Entry
	json.Unmarshal(rec.Body.Bytes(), &hist)
	if rec.Code != 200 || len(hist) != 1 || hist[0].Result != "ok" || hist[0].By != "tailnet" {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
}

func TestDeployEndpointErrors(t *testing.T) {
	h, app, cm := deployHandler(t, digestSource{})
	tests := []struct {
		path string
		code int
		want string
	}{
		{"/_helmo/api/deploy?tag=latest", 400, "not a version tag"},
		{"/_helmo/api/deploy?tag=", 400, "not a version tag"},
		{"/_helmo/api/deploy?tag=v9.9.9", 404, "not found"},
		{"/_helmo/api/rollback", 409, "no previous"},
	}
	for _, tt := range tests {
		rec := post(h, tt.path, "8600")
		if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("%s: %d %q", tt.path, rec.Code, rec.Body)
		}
	}

	// the same tag is refused once the app is pinned to its digest
	deploy.SetVar(filepath.Join(app.Dir, ".helmo", "env"), "APP_TAG", "v1.2.3@"+dig)
	if rec := post(h, "/_helmo/api/deploy?tag=v1.2.3", "8600"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "already runs") {
		t.Errorf("same version: %d %q", rec.Code, rec.Body)
	}

	release, err := cm.Acquire("cadastro", compose.OpRestart)
	if err != nil {
		t.Fatal(err)
	}
	if rec := post(h, "/_helmo/api/deploy?tag=v1.3.0", "8600"); rec.Code != 409 || !strings.Contains(rec.Body.String(), "in progress") {
		t.Errorf("busy: %d %q", rec.Code, rec.Body)
	}
	release()

	if rec := post(h, "/_helmo/api/deploy?tag=v1.3.0", "9999"); rec.Code != 404 {
		t.Errorf("unknown port: %d", rec.Code)
	}
	if rec := get(h, "/_helmo/api/history?limit=0", "8600"); rec.Code != 400 {
		t.Errorf("history limit: %d", rec.Code)
	}
}

func TestDeployRegistryFailures(t *testing.T) {
	denied := fmt.Errorf("%w for ghcr.io: no credentials in config.json (run docker login on the host)", registry.ErrUnauthorized)
	h, _, _ := deployHandler(t, digestSource{err: denied})
	rec := post(h, "/_helmo/api/deploy?tag=v1.3.0", "8600")
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "docker login") {
		t.Errorf("auth: %d %q", rec.Code, rec.Body)
	}

	h, _, _ = deployHandler(t, digestSource{err: errors.New("dial tcp 10.1.2.3:443: refused")})
	rec = post(h, "/_helmo/api/deploy?tag=v1.3.0", "8600")
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "10.1.2.3") {
		t.Errorf("internal detail leaked: %d %q", rec.Code, rec.Body)
	}
}

func TestDeployNeedsSameOriginPost(t *testing.T) {
	h, _, _ := deployHandler(t, digestSource{})
	r, _ := http.NewRequest("POST", "/_helmo/api/deploy?tag=v1.3.0", nil)
	r.Host = "node:8600"
	r.Header.Set("X-Forwarded-Port", "8600")
	rec := newRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestDeployEndpointRefusesComposeWithoutAppTag(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(dir, ".helmo"), 0o755)
	os.WriteFile(filepath.Join(dir, ".helmo", "env"), []byte("APP_TAG=v1.2.3\n"), 0o644)
	app := config.App{ID: "cadastro", Dir: dir, Ports: []int{8600}, Service: "web"}
	cm := &compose.Manager{Runner: &outRunner{config: `{"services":{"web":{"image":"ghcr.io/org/app:latest"}}}`}}
	s := &Server{Docker: &docker.Fake{}, Compose: cm, Deployer: &deploy.Deployer{Compose: cm, Docker: &docker.Fake{}, Registry: digestSource{}}}
	rec := post(routingFor([]config.App{app}, s), "/_helmo/api/deploy?tag=v1.3.0&dry_run=1", "8600")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "does not take the image tag from APP_TAG") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
