package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/registry"
	"helmo/internal/routing"
)

type fakeTags struct {
	tags []string
	err  error
	ref  registry.Ref
}

func (f *fakeTags) Tags(_ context.Context, ref registry.Ref, keep func(string) bool) ([]string, error) {
	f.ref = ref
	var out []string
	for _, t := range f.tags {
		if keep == nil || keep(t) {
			out = append(out, t)
		}
	}
	return out, f.err
}

// outRunner is a compose runner that can also return `config` output.
type outRunner struct {
	fakeRunner
	config string
	cfgErr error
}

func (o *outRunner) Output(context.Context, string, ...string) (string, error) {
	return o.config, o.cfgErr
}

func versionsHandler(ops docker.DockerOps, run compose.Runner, tags TagSource, service string) http.Handler {
	apps := []config.App{{ID: "cadastro", Dir: "/srv/apps/cadastro", Ports: []int{8600}, Service: service}}
	s := &Server{Docker: ops, Compose: &compose.Manager{Runner: run}, Registry: tags}
	return routing.Middleware(routing.Options{Resolver: routing.NewResolver(apps)}, s.Handler())
}

const cfgJSON = `{"name":"cadastro","services":{"web":{"image":"ghcr.io/org/app:v1.2.3"},"db":{"image":"postgres:16"}}}`

func TestVersions(t *testing.T) {
	tags := &fakeTags{tags: []string{"latest", "v1.2.3", "v1.10.0", "sha-abc", "v1.9.0", "v2.0.0-rc1"}}
	h := versionsHandler(&docker.Fake{}, &outRunner{config: cfgJSON}, tags, "web")

	rec := get(h, "/_helmo/api/versions", "8600")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var got versionsJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Image != "ghcr.io/org/app" || got.Current != "v1.2.3" || got.Total != 3 ||
		strings.Join(got.Tags, ",") != "v1.10.0,v1.9.0,v1.2.3" || got.Truncated {
		t.Fatalf("got %+v", got)
	}
	if tags.ref.Registry != "ghcr.io" || tags.ref.Repository != "org/app" {
		t.Errorf("registry queried with %+v", tags.ref)
	}
}

func TestVersionsLimit(t *testing.T) {
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, fmt.Sprintf("v1.%d.0", i))
	}
	h := versionsHandler(&docker.Fake{}, &outRunner{config: cfgJSON}, &fakeTags{tags: many}, "web")

	var got versionsJSON
	json.Unmarshal(get(h, "/_helmo/api/versions", "8600").Body.Bytes(), &got)
	if len(got.Tags) != 20 || got.Total != 30 || !got.Truncated || got.Tags[0] != "v1.29.0" {
		t.Fatalf("default limit: %d tags total=%d trunc=%v first=%s", len(got.Tags), got.Total, got.Truncated, got.Tags[0])
	}
	got = versionsJSON{}
	json.Unmarshal(get(h, "/_helmo/api/versions?limit=3", "8600").Body.Bytes(), &got)
	if len(got.Tags) != 3 {
		t.Fatalf("limit=3: %v", got.Tags)
	}
	for _, bad := range []string{"0", "101", "x"} {
		if rec := get(h, "/_helmo/api/versions?limit="+bad, "8600"); rec.Code != 400 {
			t.Errorf("limit=%s: %d", bad, rec.Code)
		}
	}
}

func TestVersionsFallsBackToContainerImage(t *testing.T) {
	ops := &docker.Fake{Projects: map[string][]docker.Container{"/srv/apps/cadastro": {
		{ID: "w", Service: "web", Image: "ghcr.io/org/app:v1.1.0", State: "running"},
	}}}
	tags := &fakeTags{tags: []string{"v1.1.0", "v1.2.0"}}
	run := &outRunner{cfgErr: errors.New("APP_TAG missing")}
	h := versionsHandler(ops, run, tags, "web")
	rec := get(h, "/_helmo/api/versions", "8600")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"current":"v1.1.0"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestVersionsImageUnknown(t *testing.T) {
	// compose cannot resolve, container image is only an ID
	ops := &docker.Fake{Projects: map[string][]docker.Container{"/srv/apps/cadastro": {
		{ID: "w", Service: "web", Image: "sha256:deadbeef", State: "running"},
	}}}
	h := versionsHandler(ops, &outRunner{cfgErr: errors.New("boom")}, &fakeTags{}, "web")
	rec := get(h, "/_helmo/api/versions", "8600")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "APP_TAG") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// runner without Output support behaves the same, and nothing is down
	h = versionsHandler(&docker.Fake{}, &fakeRunner{}, &fakeTags{}, "web")
	if rec := get(h, "/_helmo/api/versions", "8600"); rec.Code != http.StatusConflict {
		t.Fatalf("no Output: %d", rec.Code)
	}
}

func TestVersionsRegistryErrors(t *testing.T) {
	run := &outRunner{config: cfgJSON}
	h := versionsHandler(&docker.Fake{}, run, &fakeTags{err: errors.New("dial tcp 10.0.0.5: secret-internal-detail")}, "web")
	rec := get(h, "/_helmo/api/versions", "8600")
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "secret-internal-detail") {
		t.Fatalf("generic error must hide details: %d %q", rec.Code, rec.Body)
	}

	auth := fmt.Errorf("%w for ghcr.io: no credentials in config.json (run docker login on the host)", registry.ErrUnauthorized)
	h = versionsHandler(&docker.Fake{}, run, &fakeTags{err: auth}, "web")
	rec = get(h, "/_helmo/api/versions", "8600")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "docker login") {
		t.Fatalf("auth error should explain: %d %q", rec.Code, rec.Body)
	}
}

func TestVersionsUnknownPort(t *testing.T) {
	h := versionsHandler(&docker.Fake{}, &outRunner{config: cfgJSON}, &fakeTags{}, "web")
	if rec := get(h, "/_helmo/api/versions", "9999"); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}
