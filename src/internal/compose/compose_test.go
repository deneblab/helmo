package compose

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"helmo/internal/config"
)

type call struct {
	dir  string
	args []string
}

type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	out     string
	err     error
	started chan struct{} // closed on first Run when non-nil
	release chan struct{} // Run blocks until closed when non-nil
	ctxErr  error
}

func (f *fakeRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{dir, args})
	f.mu.Unlock()
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	f.ctxErr = ctx.Err()
	return f.out, f.err
}

var app = config.App{ID: "cadastro", Dir: "/srv/apps/cadastro"}

func TestDoBuildsCommands(t *testing.T) {
	tests := []struct {
		op   Op
		want []string
	}{
		{OpStart, []string{"up", "-d"}},
		{OpStop, []string{"stop"}},
		{OpRestart, []string{"restart"}},
	}
	for _, tt := range tests {
		f := &fakeRunner{}
		m := &Manager{Runner: f}
		if _, err := m.Do(context.Background(), app, tt.op); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 || f.calls[0].dir != app.Dir || !reflect.DeepEqual(f.calls[0].args, tt.want) {
			t.Errorf("%s: calls %+v", tt.op, f.calls)
		}
	}
}

func TestDoUnknownOp(t *testing.T) {
	f := &fakeRunner{}
	if _, err := (&Manager{Runner: f}).Do(context.Background(), app, "down"); !errors.Is(err, ErrUnknownOp) {
		t.Fatalf("got %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("must not run")
	}
}

func TestDoBusyThenReleased(t *testing.T) {
	f := &fakeRunner{started: make(chan struct{}), release: make(chan struct{})}
	m := &Manager{Runner: f}

	done := make(chan error, 1)
	go func() { _, err := m.Do(context.Background(), app, OpRestart); done <- err }()
	<-f.started

	if op, busy := m.Busy(app.ID); !busy || op != OpRestart {
		t.Fatalf("busy = %v %v", op, busy)
	}
	if _, err := m.Do(context.Background(), app, OpStop); !errors.Is(err, ErrBusy) {
		t.Fatalf("second op: %v", err)
	}
	other := config.App{ID: "blog", Dir: "/srv/apps/blog"}
	if !m.acquire(other.ID, OpStop) {
		t.Fatal("different app must not be blocked")
	}
	m.release(other.ID)

	close(f.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, busy := m.Busy(app.ID); busy {
		t.Fatal("lock not released")
	}
}

func TestDoErrorKeepsOutputAndReleases(t *testing.T) {
	f := &fakeRunner{out: "boom\n", err: errors.New("exit status 1")}
	m := &Manager{Runner: f}
	res, err := m.Do(context.Background(), app, OpStart)
	if err == nil || !strings.Contains(err.Error(), "start cadastro") || res.Output != "boom" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, busy := m.Busy(app.ID); busy {
		t.Fatal("lock not released after error")
	}
}

func TestDoSurvivesCallerCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeRunner{}
	if _, err := (&Manager{Runner: f}).Do(ctx, app, OpRestart); err != nil {
		t.Fatal(err)
	}
	if f.ctxErr != nil {
		t.Fatalf("command context was cancelled: %v", f.ctxErr)
	}
}

func TestTailTruncates(t *testing.T) {
	got := tail(strings.Repeat("x", maxOutput+10))
	if len(got) != maxOutput+3 || !strings.HasPrefix(got, "...") {
		t.Fatalf("len %d", len(got))
	}
}

func stubDocker(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\necho \"args=$*\"\necho \"cwd=$(pwd -P)\"\necho oops >&2\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestExecRunnerArgsAndDir(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	out, err := ExecRunner{Bin: stubDocker(t)}.Run(context.Background(), dir, "up", "-d")
	if err == nil {
		t.Fatal("want exit error")
	}
	for _, want := range []string{"args=compose --project-directory " + dir + " up -d", "cwd=" + dir, "oops"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--env-file") {
		t.Errorf("no env files exist, none must be passed:\n%s", out)
	}
}

func TestExecRunnerPassesEnvFiles(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(dir, ".helmo"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".env", ".helmo/env"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("A=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := ExecRunner{Bin: stubDocker(t)}.Run(context.Background(), dir, "restart")
	want := "args=compose --project-directory " + dir + " --env-file " + dir + "/.env --env-file " + dir + "/.helmo/env restart"
	if !strings.Contains(out, want) {
		t.Fatalf("want %q in:\n%s", want, out)
	}
}

func TestExecRunnerWithRealComposeCLI(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI not installed")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose plugin not available")
	}
	dir := t.TempDir()
	must := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("compose.yaml", "services:\n  web:\n    image: ghcr.io/org/app:${APP_TAG:?no tag}\n    environment:\n      S: ${SECRET}\n")
	must(".env", "SECRET=x\n")
	must(".helmo/env", "APP_TAG=v1.2.3\n")

	out, err := ExecRunner{}.Run(context.Background(), dir, "config", "--images")
	if err != nil || strings.TrimSpace(out) != "ghcr.io/org/app:v1.2.3" {
		t.Fatalf("err=%v out=%q", err, out)
	}
}

type outFake struct {
	fakeRunner
	out string
	err error
}

func (o *outFake) Output(context.Context, string, ...string) (string, error) { return o.out, o.err }

func TestServiceImage(t *testing.T) {
	cfg := `{"services":{"web":{"image":"ghcr.io/org/app:v1.2.3"},"db":{"image":"postgres:16"},"job":{}}}`
	one := `{"services":{"web":{"image":"ghcr.io/org/app:v1"}}}`
	tests := []struct {
		name, out, service, want string
		err                      error
		wantErr                  bool
	}{
		{"named service", cfg, "web", "ghcr.io/org/app:v1.2.3", nil, false},
		{"single service without name", one, "", "ghcr.io/org/app:v1", nil, false},
		{"several services without name", cfg, "", "", nil, true},
		{"unknown service", cfg, "nope", "", nil, true},
		{"service without image", cfg, "job", "", nil, true},
		{"compose fails", "", "web", "", errors.New("exit 1"), true},
		{"bad json", "not json", "web", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{Runner: &outFake{out: tt.out, err: tt.err}}
			got, err := m.ServiceImage(context.Background(), app, tt.service)
			if tt.wantErr {
				if !errors.Is(err, ErrNoImage) {
					t.Fatalf("got %q err=%v", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q err=%v", got, err)
			}
		})
	}
	if _, err := (&Manager{Runner: &fakeRunner{}}).ServiceImage(context.Background(), app, "web"); !errors.Is(err, ErrNoImage) {
		t.Fatalf("runner without Output: %v", err)
	}
}

func TestExecRunnerOutputIgnoresStderr(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\necho '{\"ok\":true}'\necho 'warning: noise' >&2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := ExecRunner{Bin: bin}.Output(context.Background(), t.TempDir(), "config")
	if err != nil || strings.TrimSpace(out) != `{"ok":true}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	fail := filepath.Join(t.TempDir(), "docker")
	os.WriteFile(fail, []byte("#!/bin/sh\necho 'bad thing' >&2\nexit 2\n"), 0o755)
	if _, err := (ExecRunner{Bin: fail}).Output(context.Background(), t.TempDir(), "config"); err == nil || !strings.Contains(err.Error(), "bad thing") {
		t.Fatalf("want stderr in error, got %v", err)
	}
}
