// Package compose runs docker compose commands for one app at a time.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"helmo/internal/config"
)

// Op is an operation on a whole Compose project.
type Op string

const (
	OpStart   Op = "start"   // up -d: also (re)creates missing containers
	OpStop    Op = "stop"    // stop, containers are kept
	OpRestart Op = "restart" // restart existing containers

	// Held by the deploy package while it changes versions; Do rejects them.
	OpDeploy   Op = "deploy"
	OpRollback Op = "rollback"
)

const (
	opTimeout  = 5 * time.Minute
	maxOutput  = 4096
	defaultBin = "docker"
)

// ErrBusy is returned when another operation is running for the same app.
var ErrBusy = errors.New("another operation is in progress for this app")

// ErrUnknownOp is returned for an Op Helmo does not support.
var ErrUnknownOp = errors.New("unknown operation")

// Runner runs "docker compose <args>" in the project directory dir and
// returns the combined output.
type Runner interface {
	Run(ctx context.Context, dir string, args ...string) (string, error)
}

// ExecRunner runs the docker CLI as a child process. The environment is
// inherited, so DOCKER_HOST and DOCKER_CONFIG apply.
type ExecRunner struct {
	Bin string // defaults to "docker"
}

func (e ExecRunner) Run(ctx context.Context, dir string, args ...string) (string, error) {
	bin := e.Bin
	if bin == "" {
		bin = defaultBin
	}
	full := append([]string{"compose", "--project-directory", dir}, envFileArgs(dir)...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	// Compose looks for compose.yaml and .env in the working directory.
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Output is like Run but returns stdout only (stderr, where Compose prints
// warnings, is dropped), for commands whose output is parsed.
func (e ExecRunner) Output(ctx context.Context, dir string, args ...string) (string, error) {
	bin := e.Bin
	if bin == "" {
		bin = defaultBin
	}
	full := append([]string{"compose", "--project-directory", dir}, envFileArgs(dir)...)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, bin, full...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, tail(stderr.String()))
	}
	return stdout.String(), nil
}

// Outputter is implemented by runners that can return clean stdout.
type Outputter interface {
	Output(ctx context.Context, dir string, args ...string) (string, error)
}

// ErrNoImage means Compose could not tell which image a service uses (for
// example APP_TAG is not defined yet).
var ErrNoImage = errors.New("cannot determine the service image")

// ServiceImages returns service name -> image as Compose resolves them
// (APP_TAG included). Services without an image are omitted.
func (m *Manager) ServiceImages(ctx context.Context, app config.App) (map[string]string, error) {
	o, ok := m.Runner.(Outputter)
	if !ok {
		return nil, ErrNoImage
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := o.Output(ctx, app.Dir, "config", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoImage, err)
	}
	var cfg struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return nil, fmt.Errorf("%w: decode compose config: %v", ErrNoImage, err)
	}
	images := map[string]string{}
	for name, svc := range cfg.Services {
		if svc.Image != "" {
			images[name] = svc.Image
		}
	}
	return images, nil
}

// ServiceImage returns the image reference Compose resolves for service in
// the app's project. An empty service is accepted when the project has
// exactly one.
func (m *Manager) ServiceImage(ctx context.Context, app config.App, service string) (string, error) {
	_, img, err := m.ResolveService(ctx, app, service)
	return img, err
}

// ResolveService returns the name and image of the service to version. An
// empty service is accepted when the project has exactly one.
func (m *Manager) ResolveService(ctx context.Context, app config.App, service string) (name, image string, err error) {
	images, err := m.ServiceImages(ctx, app)
	if err != nil {
		return "", "", err
	}
	if service == "" {
		if len(images) != 1 {
			return "", "", fmt.Errorf("%w: several services, set service in app.yaml", ErrNoImage)
		}
		for n, img := range images {
			return n, img, nil
		}
	}
	img, ok := images[service]
	if !ok {
		return "", "", fmt.Errorf("%w: service %q has no image", ErrNoImage, service)
	}
	return service, img, nil
}

// envFileArgs makes Compose read the app's .env and .helmo/env explicitly.
// COMPOSE_ENV_FILES written inside .env is ignored by Compose (it is only
// honoured in the process environment), and passing any --env-file turns
// off the implicit .env, so both files are listed. The later file wins.
func envFileArgs(dir string) []string {
	var args []string
	for _, rel := range []string{".env", filepath.Join(".helmo", "env")} {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			args = append(args, "--env-file", p)
		}
	}
	return args
}

// Manager serializes operations per app.
type Manager struct {
	Runner Runner

	mu   sync.Mutex
	busy map[string]Op
}

// Result is the outcome of one operation.
type Result struct {
	Op     Op
	Output string // tail of the compose output
}

// Do runs op on the whole project of app. The command is not cancelled when
// the caller's context ends, so a dropped connection cannot leave a project
// half-restarted; it is bounded by its own timeout instead.
func (m *Manager) Do(ctx context.Context, app config.App, op Op) (Result, error) {
	args, err := argsFor(op)
	if err != nil {
		return Result{Op: op}, err
	}
	if !m.acquire(app.ID, op) {
		return Result{Op: op}, ErrBusy
	}
	defer m.release(app.ID)

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	out, err := m.Runner.Run(runCtx, app.Dir, args...)
	res := Result{Op: op, Output: tail(out)}
	if err != nil {
		return res, fmt.Errorf("%s %s: %w", op, app.ID, err)
	}
	return res, nil
}

// Acquire reserves the app for a long operation run by another package
// (deploy, rollback). The caller must call release exactly once.
func (m *Manager) Acquire(appID string, op Op) (release func(), err error) {
	if !m.acquire(appID, op) {
		return nil, ErrBusy
	}
	return func() { m.release(appID) }, nil
}

// Exec runs docker compose in the app directory without taking the lock;
// callers hold it through Acquire.
func (m *Manager) Exec(ctx context.Context, app config.App, args ...string) (string, error) {
	out, err := m.Runner.Run(ctx, app.Dir, args...)
	if err != nil {
		return tail(out), fmt.Errorf("compose %s: %w", args[0], err)
	}
	return tail(out), nil
}

// Busy reports the operation currently running for the app, if any.
func (m *Manager) Busy(appID string) (Op, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.busy[appID]
	return op, ok
}

func (m *Manager) acquire(id string, op Op) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy == nil {
		m.busy = map[string]Op{}
	}
	if _, taken := m.busy[id]; taken {
		return false
	}
	m.busy[id] = op
	return true
}

func (m *Manager) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.busy, id)
}

func argsFor(op Op) ([]string, error) {
	switch op {
	case OpStart:
		return []string{"up", "-d"}, nil
	case OpStop:
		return []string{"stop"}, nil
	case OpRestart:
		return []string{"restart"}, nil
	}
	return nil, ErrUnknownOp
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxOutput {
		s = "..." + s[len(s)-maxOutput:]
	}
	return s
}
