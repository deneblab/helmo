package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/registry"
)

var (
	d123 = "sha256:" + strings.Repeat("1", 64)
	d130 = "sha256:" + strings.Repeat("3", 64)
)

// env simulates one app: a Compose runner, a Docker daemon and a registry
// that react to what Helmo writes into .helmo/env.
type env struct {
	t   *testing.T
	app config.App

	mu         sync.Mutex
	calls      []string
	pullErr    error
	containers []docker.Container
	// health decides what the service reports after "up", from APP_TAG.
	health  func(appTag string, upCount int) (state, health string)
	ups     int
	digests map[string]string
}

func newEnv(t *testing.T, initialTag string) *env {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(dir, ".helmo"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &env{
		t:       t,
		app:     config.App{ID: "cadastro", Dir: dir, Service: "web", HealthTimeout: 40 * time.Millisecond},
		digests: map[string]string{"v1.3.0": d130, "v1.2.3": d123},
		health:  func(string, int) (string, string) { return "running", "healthy" },
	}
	if initialTag != "" {
		os.WriteFile(e.envPath(), []byte("OTHER=keep\nAPP_TAG="+initialTag+"\n"), 0o644)
	}
	e.containers = []docker.Container{{ID: "c0", Service: "web", State: "running", Health: "healthy"}}
	return e
}

func (e *env) envPath() string { return filepath.Join(e.app.Dir, ".helmo", "env") }

func (e *env) appTag() string {
	v, _, _ := GetVar(e.envPath(), "APP_TAG")
	return v
}

func (e *env) Run(_ context.Context, dir string, args ...string) (string, error) {
	e.mu.Lock()
	e.calls = append(e.calls, strings.Join(args, " "))
	pullErr := e.pullErr
	e.mu.Unlock()
	switch args[0] {
	case "pull":
		return "pull output", pullErr
	case "up":
		e.mu.Lock()
		e.ups++
		n := e.ups
		e.mu.Unlock()
		state, health := e.health(e.appTag(), n) // may block; must not hold the lock
		e.mu.Lock()
		e.containers = []docker.Container{{ID: fmt.Sprintf("c%d", n), Service: "web", State: state, Health: health}}
		e.mu.Unlock()
	}
	return "", nil
}

func (e *env) Output(context.Context, string, ...string) (string, error) {
	tag := e.appTag()
	if tag == "" {
		tag = "v1.2.3"
	}
	return fmt.Sprintf(`{"services":{"web":{"image":"ghcr.io/org/app:%s"},"db":{"image":"postgres:16"}}}`, tag), nil
}

func (e *env) Ping(context.Context) error { return nil }
func (e *env) ProjectContainers(context.Context, string) ([]docker.Container, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]docker.Container(nil), e.containers...), nil
}
func (e *env) StreamLogs(context.Context, string, docker.LogOptions, func(docker.LogLine) error) error {
	return nil
}

func (e *env) Digest(_ context.Context, _ registry.Ref, reference string) (string, error) {
	if d, ok := e.digests[reference]; ok {
		return d, nil
	}
	return "", fmt.Errorf("ghcr.io/org/app: not found")
}

func (e *env) deployer() *Deployer {
	return &Deployer{
		Compose:      &compose.Manager{Runner: e},
		Docker:       e,
		Registry:     e,
		PollInterval: time.Millisecond,
	}
}

func (e *env) callList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func wait(t *testing.T, d *Deployer, id string) Job {
	t.Helper()
	for i := 0; i < 500; i++ {
		if j, ok := d.Last(id); ok && j.State != "running" {
			return j
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

func history(t *testing.T, e *env) []Entry {
	t.Helper()
	h, err := Recent(e.app.Dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestDeploySuccess(t *testing.T) {
	e := newEnv(t, "v1.2.3@"+d123)
	d := e.deployer()

	job, err := d.Deploy(context.Background(), e.app, "v1.3.0", "tailnet")
	if err != nil || job.State != "running" || job.To != "v1.3.0@"+d130 || job.From != "v1.2.3@"+d123 {
		t.Fatalf("job %+v err=%v", job, err)
	}
	got := wait(t, d, "cadastro")
	if got.State != "ok" || got.Error != "" {
		t.Fatalf("final %+v", got)
	}
	if e.appTag() != "v1.3.0@"+d130 {
		t.Errorf("APP_TAG = %q", e.appTag())
	}
	if b, _ := os.ReadFile(e.envPath()); !strings.Contains(string(b), "OTHER=keep") {
		t.Errorf("other variables must survive:\n%s", b)
	}
	if want := []string{"pull web", "up -d web"}; !reflect.DeepEqual(e.callList(), want) {
		t.Errorf("calls %v, want %v", e.callList(), want)
	}
	h := history(t, e)
	if len(h) != 1 || h[0].Action != "deploy" || h[0].Result != "ok" || h[0].By != "tailnet" ||
		h[0].From != "v1.2.3@"+d123 || h[0].To != "v1.3.0@"+d130 {
		t.Errorf("history %+v", h)
	}
}

func TestDeployUnhealthyRollsBackAutomatically(t *testing.T) {
	e := newEnv(t, "v1.2.3@"+d123)
	e.health = func(tag string, _ int) (string, string) {
		if strings.HasPrefix(tag, "v1.3.0") {
			return "running", "unhealthy"
		}
		return "running", "healthy"
	}
	d := e.deployer()
	if _, err := d.Deploy(context.Background(), e.app, "v1.3.0", ""); err != nil {
		t.Fatal(err)
	}
	got := wait(t, d, "cadastro")
	if got.State != "rolled_back" || !strings.Contains(got.Error, "unhealthy") {
		t.Fatalf("final %+v", got)
	}
	if e.appTag() != "v1.2.3@"+d123 {
		t.Errorf("APP_TAG after rollback = %q", e.appTag())
	}
	if want := []string{"pull web", "up -d web", "up -d web"}; !reflect.DeepEqual(e.callList(), want) {
		t.Errorf("calls %v", e.callList())
	}
	h := history(t, e)
	if len(h) != 2 || h[0].Action != "deploy" || h[0].Result != "rolled_back" ||
		h[1].Action != "auto-rollback" || h[1].Result != "ok" || h[1].To != "v1.2.3@"+d123 {
		t.Errorf("history %+v", h)
	}
}

func TestDeployRollbackAlsoFails(t *testing.T) {
	e := newEnv(t, "v1.2.3")
	e.health = func(string, int) (string, string) { return "running", "unhealthy" }
	d := e.deployer()
	d.Deploy(context.Background(), e.app, "v1.3.0", "")
	got := wait(t, d, "cadastro")
	if got.State != "failed" || !strings.Contains(got.Error, "automatic rollback also failed") {
		t.Fatalf("final %+v", got)
	}
	h := history(t, e)
	if len(h) != 2 || h[1].Action != "auto-rollback" || h[1].Result != "failed" {
		t.Errorf("history %+v", h)
	}
}

func TestDeployPullFailureRestoresEnvAndSkipsUp(t *testing.T) {
	for _, initial := range []string{"v1.2.3", ""} {
		e := newEnv(t, initial)
		e.pullErr = errors.New("pull access denied")
		d := e.deployer()
		d.Deploy(context.Background(), e.app, "v1.3.0", "")
		got := wait(t, d, "cadastro")
		if got.State != "failed" || !strings.Contains(got.Error, "pull") {
			t.Fatalf("initial %q: %+v", initial, got)
		}
		if e.appTag() != initial {
			t.Errorf("initial %q: APP_TAG left as %q", initial, e.appTag())
		}
		if want := []string{"pull web"}; !reflect.DeepEqual(e.callList(), want) {
			t.Errorf("calls %v", e.callList())
		}
		if h := history(t, e); len(h) != 1 || h[0].Result != "failed" {
			t.Errorf("history %+v", h)
		}
	}
}

func TestDeployWithoutHealthcheck(t *testing.T) {
	e := newEnv(t, "v1.2.3")
	e.health = func(string, int) (string, string) { return "running", "" }
	d := e.deployer()
	d.Deploy(context.Background(), e.app, "v1.3.0", "")
	if got := wait(t, d, "cadastro"); got.State != "ok" {
		t.Fatalf("stayed up: %+v", got)
	}

	e = newEnv(t, "v1.2.3")
	e.health = func(tag string, _ int) (string, string) {
		if strings.HasPrefix(tag, "v1.3.0") {
			return "restarting", ""
		}
		return "running", ""
	}
	d = e.deployer()
	d.Deploy(context.Background(), e.app, "v1.3.0", "")
	if got := wait(t, d, "cadastro"); got.State != "rolled_back" || !strings.Contains(got.Error, "stopped") {
		t.Fatalf("crash loop: %+v", got)
	}
}

func TestDeployNoPreviousVersionCannotRollBack(t *testing.T) {
	e := newEnv(t, "")
	e.health = func(string, int) (string, string) { return "exited", "" }
	// Compose resolves the image without APP_TAG, but pretend no tag is known
	d := e.deployer()
	d.Compose = &compose.Manager{Runner: &noTagRunner{env: e}}
	d.Deploy(context.Background(), e.app, "v1.3.0", "")
	got := wait(t, d, "cadastro")
	if got.State != "failed" {
		t.Fatalf("final %+v", got)
	}
	if e.appTag() != "v1.3.0@"+d130 {
		t.Logf("APP_TAG = %q", e.appTag()) // left at the attempted version: nothing to go back to
	}
}

// noTagRunner reports an image without a tag, so the current version is unknown.
type noTagRunner struct{ env *env }

func (n *noTagRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	return n.env.Run(ctx, dir, args...)
}
func (n *noTagRunner) Output(context.Context, string, ...string) (string, error) {
	return `{"services":{"web":{"image":"ghcr.io/org/app"}}}`, nil
}

func TestDeployRefusals(t *testing.T) {
	e := newEnv(t, "v1.2.3@"+d123)
	d := e.deployer()

	for _, bad := range []string{"latest", "v1.2", "1.2.3; rm -rf /", ""} {
		if _, err := d.Deploy(context.Background(), e.app, bad, ""); err == nil {
			t.Errorf("tag %q must be refused", bad)
		}
	}
	if _, err := d.Deploy(context.Background(), e.app, "v9.9.9", ""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown tag: %v", err)
	}
	if _, err := d.Deploy(context.Background(), e.app, "v1.2.3", ""); !errors.Is(err, ErrSameVersion) {
		t.Errorf("same version: %v", err)
	}
	if len(e.callList()) != 0 {
		t.Errorf("nothing may run for refused deploys: %v", e.callList())
	}

	release, err := d.Compose.Acquire("cadastro", compose.OpRestart)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Deploy(context.Background(), e.app, "v1.3.0", ""); !errors.Is(err, compose.ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	release()
}

func TestDeployBlocksOtherOperationsWhileRunning(t *testing.T) {
	e := newEnv(t, "v1.2.3")
	gate := make(chan struct{})
	e.health = func(string, int) (string, string) { <-gate; return "running", "healthy" }
	d := e.deployer()
	if _, err := d.Deploy(context.Background(), e.app, "v1.3.0", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200 && len(e.callList()) < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := d.Compose.Do(context.Background(), e.app, compose.OpRestart); !errors.Is(err, compose.ErrBusy) {
		t.Errorf("restart during deploy: %v", err)
	}
	if j, _ := d.Last("cadastro"); j.State != "running" {
		t.Errorf("state %s", j.State)
	}
	close(gate)
	wait(t, d, "cadastro")
	if _, err := d.Compose.Do(context.Background(), e.app, compose.OpRestart); err != nil {
		t.Errorf("lock not released: %v", err)
	}
}

func TestRollback(t *testing.T) {
	e := newEnv(t, "v1.3.0@"+d130)
	d := e.deployer()
	if _, err := d.Rollback(context.Background(), e.app, ""); !errors.Is(err, ErrNoPrevious) {
		t.Fatalf("empty history: %v", err)
	}

	Append(e.app.Dir, Entry{Action: "deploy", From: "v1.1.0", To: "v1.2.3@" + d123, Result: "ok"})
	Append(e.app.Dir, Entry{Action: "deploy", From: "v1.2.3@" + d123, To: "v1.3.0@" + d130, Result: "ok"})
	Append(e.app.Dir, Entry{Action: "deploy", From: "v1.3.0@" + d130, To: "v1.4.0@sha256:" + strings.Repeat("4", 64), Result: "rolled_back"})
	Append(e.app.Dir, Entry{Action: "auto-rollback", From: "v1.4.0", To: "v1.3.0@" + d130, Result: "ok"})

	job, err := d.Rollback(context.Background(), e.app, "tailnet")
	if err != nil || job.Action != "rollback" || job.To != "v1.2.3@"+d123 {
		t.Fatalf("job %+v err=%v", job, err)
	}
	if got := wait(t, d, "cadastro"); got.State != "ok" {
		t.Fatalf("final %+v", got)
	}
	if e.appTag() != "v1.2.3@"+d123 {
		t.Errorf("APP_TAG = %q", e.appTag())
	}
	h := history(t, e)
	if last := h[len(h)-1]; last.Action != "rollback" || last.Result != "ok" || last.By != "tailnet" {
		t.Errorf("last entry %+v", last)
	}
}

func TestRollbackFailureIsNotRolledBackAgain(t *testing.T) {
	e := newEnv(t, "v1.3.0@"+d130)
	e.health = func(string, int) (string, string) { return "running", "unhealthy" }
	Append(e.app.Dir, Entry{Action: "deploy", From: "v1.2.3", To: "v1.3.0@" + d130, Result: "ok"})
	d := e.deployer()
	if _, err := d.Rollback(context.Background(), e.app, ""); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, d, "cadastro"); got.State != "failed" {
		t.Fatalf("final %+v", got)
	}
	if n := len(e.callList()); n != 2 { // pull + one up, no flip-flop
		t.Errorf("calls %v", e.callList())
	}
}

func TestPlanFallsBackToContainerImage(t *testing.T) {
	e := newEnv(t, "")
	e.containers = []docker.Container{{ID: "c", Service: "web", Image: "ghcr.io/org/app:v1.2.3", State: "running"}}
	d := e.deployer()
	d.Compose = &compose.Manager{Runner: &failingConfig{env: e}}
	p, err := d.Plan(context.Background(), e.app, "v1.3.0")
	if err != nil || p.Service != "web" || p.Ref.Name() != "ghcr.io/org/app" || p.Current.Tag != "v1.2.3" || p.Target.Digest != d130 {
		t.Fatalf("plan %+v err=%v", p, err)
	}

	e.containers = []docker.Container{{ID: "c", Service: "web", Image: "sha256:deadbeef", State: "running"}}
	if _, err := d.Plan(context.Background(), e.app, "v1.3.0"); !errors.Is(err, ErrNoImage) {
		t.Fatalf("image id only: %v", err)
	}
}

type failingConfig struct{ env *env }

func (f *failingConfig) Run(ctx context.Context, dir string, args ...string) (string, error) {
	return f.env.Run(ctx, dir, args...)
}
func (f *failingConfig) Output(context.Context, string, ...string) (string, error) {
	return "", errors.New("APP_TAG is not set")
}
