package deploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/registry"
)

const (
	envKey         = "APP_TAG"
	runTimeout     = 20 * time.Minute
	defaultPoll    = 2 * time.Second
	defaultHealthy = 60 * time.Second
)

var (
	ErrBadTag      = errors.New("not a version tag")
	ErrSameVersion = errors.New("the app already runs this version")
	ErrNoPrevious  = errors.New("there is no previous version to roll back to")
	ErrNoImage     = errors.New("cannot determine the service image: define APP_TAG in .helmo/env or start the app once")
	// ErrNotUsingAppTag: the image in the compose file does not take its tag
	// from APP_TAG, so writing APP_TAG would change nothing.
	ErrNotUsingAppTag = errors.New("the compose file does not take the image tag from APP_TAG")

	dockerTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// Registry is the part of the registry client deploy needs.
type Registry interface {
	Digest(ctx context.Context, ref registry.Ref, reference string) (string, error)
}

// Deployer runs one version change at a time per app, in the background.
type Deployer struct {
	Compose  *compose.Manager
	Docker   docker.DockerOps
	Registry Registry

	PollInterval time.Duration // health polling; default 2s

	mu   sync.Mutex
	jobs map[string]*Job
}

// Job is the progress of the latest version change of an app.
type Job struct {
	Action   string // deploy | rollback
	From     string
	To       string
	State    string // running | ok | failed | rolled_back
	Phase    string
	Error    string
	Started  time.Time
	Finished time.Time
}

// Plan describes what a deploy would change.
type Plan struct {
	Service string
	Ref     registry.Ref
	Current Version
	Target  Version
}

// Last returns the latest job of the app, if any ran since Helmo started.
func (d *Deployer) Last(appID string) (Job, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[appID]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Plan resolves the service and checks that tag exists in the registry,
// without changing anything.
func (d *Deployer) Plan(ctx context.Context, app config.App, tag string) (Plan, error) {
	if !registry.IsVersionTag(tag) {
		return Plan{}, fmt.Errorf("%w: %q (want X.Y.Z or vX.Y.Z)", ErrBadTag, tag)
	}
	service, ref, fromCompose, err := d.resolve(ctx, app)
	if err != nil {
		return Plan{}, err
	}
	if fromCompose {
		if err := checkUsesAppTag(app, ref); err != nil {
			return Plan{}, err
		}
	}
	digest, err := d.Registry.Digest(ctx, ref, tag)
	if err != nil {
		return Plan{}, fmt.Errorf("tag %s: %w", tag, err)
	}
	target, err := NewVersion(tag, digest)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Service: service, Ref: ref, Current: d.current(app, ref), Target: target}, nil
}

// Deploy starts changing the app to tag and returns at once; follow the
// progress with Last. It returns compose.ErrBusy when another operation is
// running for the app.
func (d *Deployer) Deploy(ctx context.Context, app config.App, tag, by string) (Job, error) {
	p, err := d.Plan(ctx, app, tag)
	if err != nil {
		return Job{}, err
	}
	if p.Current == p.Target {
		return Job{}, ErrSameVersion
	}
	return d.start(app, "deploy", p, p.Target, by)
}

// Rollback starts returning the app to the version it ran before the
// current one.
func (d *Deployer) Rollback(ctx context.Context, app config.App, by string) (Job, error) {
	entries, err := Recent(app.Dir, 200)
	if err != nil {
		return Job{}, err
	}
	target, ok := RollbackTarget(entries)
	if !ok {
		return Job{}, ErrNoPrevious
	}
	if !writable(target) {
		return Job{}, fmt.Errorf("previous version %q cannot be restored", target)
	}
	service, ref, fromCompose, err := d.resolve(ctx, app)
	if err != nil {
		return Job{}, err
	}
	if fromCompose {
		if err := checkUsesAppTag(app, ref); err != nil {
			return Job{}, err
		}
	}
	p := Plan{Service: service, Ref: ref, Current: d.current(app, ref), Target: target}
	if p.Current == p.Target {
		return Job{}, ErrNoPrevious
	}
	return d.start(app, "rollback", p, target, by)
}

func writable(v Version) bool {
	return dockerTag.MatchString(v.Tag) && (v.Digest == "" || digestPattern.MatchString(v.Digest))
}

func (d *Deployer) start(app config.App, action string, p Plan, target Version, by string) (Job, error) {
	op := compose.OpDeploy
	if action == "rollback" {
		op = compose.OpRollback
	}
	release, err := d.Compose.Acquire(app.ID, op)
	if err != nil {
		return Job{}, err
	}
	job := &Job{Action: action, From: p.Current.String(), To: target.String(), State: "running", Phase: "starting", Started: time.Now().UTC()}
	d.mu.Lock()
	if d.jobs == nil {
		d.jobs = map[string]*Job{}
	}
	d.jobs[app.ID] = job
	d.mu.Unlock()

	go d.run(app, p, target, by, job, release)
	return *job, nil
}

func (d *Deployer) update(job *Job, f func(*Job)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(job)
}

func (d *Deployer) phase(job *Job, phase string) {
	d.update(job, func(j *Job) { j.Phase = phase })
}

func (d *Deployer) finish(job *Job, state, errMsg string) {
	d.update(job, func(j *Job) {
		j.State, j.Phase, j.Error, j.Finished = state, "done", errMsg, time.Now().UTC()
	})
}

// run performs the change. A failed deploy is rolled back automatically
// when the previous version is known.
func (d *Deployer) run(app config.App, p Plan, target Version, by string, job *Job, release func()) {
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	env := filepath.Join(app.Dir, ".helmo", "env")
	prevRaw, hadPrev, _ := GetVar(env, envKey)
	prev := p.Current
	record := func(action string, from, to Version, result string, err error) {
		e := Entry{Time: time.Now().UTC(), Action: action, From: from.String(), To: to.String(), Result: result, By: by}
		if err != nil {
			e.Error = err.Error()
		}
		Append(app.Dir, e) // best effort: the app state matters more than its log
	}
	restoreEnv := func() {
		switch {
		case hadPrev:
			SetVar(env, envKey, prevRaw)
		default:
			UnsetVar(env, envKey)
		}
	}
	action := job.Action

	fail := func(err error) {
		restoreEnv()
		record(action, prev, target, "failed", err)
		d.finish(job, "failed", err.Error())
	}

	d.phase(job, "writing "+envKey)
	if err := SetVar(env, envKey, target.String()); err != nil {
		fail(err)
		return
	}
	// The compose file must now resolve to the target, or pull and up would
	// keep the old image and the deploy would wrongly report success.
	d.phase(job, "checking the compose file")
	if err := d.checkResolvesTo(ctx, app, p.Service, target); err != nil {
		fail(err)
		return
	}
	d.phase(job, "pulling image")
	if _, err := d.Compose.Exec(ctx, app, "pull", p.Service); err != nil {
		fail(err)
		return
	}
	d.phase(job, "starting service")
	err := func() error {
		if _, err := d.Compose.Exec(ctx, app, "up", "-d", p.Service); err != nil {
			return err
		}
		d.phase(job, "waiting for health")
		return d.waitHealthy(ctx, app, p.Service)
	}()
	if err == nil {
		record(action, prev, target, "ok", nil)
		d.finish(job, "ok", "")
		return
	}

	if action != "deploy" || prev.IsZero() || !writable(prev) {
		record(action, prev, target, "failed", err)
		d.finish(job, "failed", err.Error())
		return
	}

	d.phase(job, "rolling back")
	record(action, prev, target, "rolled_back", err)
	rbErr := func() error {
		if err := SetVar(env, envKey, prev.String()); err != nil {
			return err
		}
		if _, err := d.Compose.Exec(ctx, app, "up", "-d", p.Service); err != nil {
			return err
		}
		return d.waitHealthy(ctx, app, p.Service)
	}()
	if rbErr != nil {
		record("auto-rollback", target, prev, "failed", rbErr)
		d.finish(job, "failed", fmt.Sprintf("%v; automatic rollback also failed: %v", err, rbErr))
		return
	}
	record("auto-rollback", target, prev, "ok", nil)
	d.finish(job, "rolled_back", err.Error())
}

// waitHealthy waits until the service container is healthy. Without a
// healthcheck the container must stay running for the whole health timeout.
func (d *Deployer) waitHealthy(ctx context.Context, app config.App, service string) error {
	timeout := app.HealthTimeout
	if timeout <= 0 {
		timeout = defaultHealthy
	}
	poll := d.PollInterval
	if poll <= 0 {
		poll = defaultPoll
	}
	start := time.Now()
	for {
		cs, err := d.Docker.ProjectContainers(ctx, app.Dir)
		if err != nil {
			return fmt.Errorf("check service: %w", err)
		}
		var c *docker.Container
		for i := range cs {
			if cs[i].Service == service {
				c = &cs[i]
				break
			}
		}
		elapsed := time.Since(start)
		switch {
		case c == nil:
			return fmt.Errorf("service %q has no container", service)
		case c.Health == "healthy":
			return nil
		case c.Health == "unhealthy":
			return errors.New("service reported unhealthy")
		case c.Health == "" && !c.Running():
			return fmt.Errorf("service stopped (state %s)", c.State)
		case c.Health == "" && elapsed >= timeout:
			return nil // no healthcheck: it stayed up long enough
		case elapsed >= timeout:
			return fmt.Errorf("service did not become healthy within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// resolve finds the service to version and its image repository. Compose is
// asked first; when it cannot answer (APP_TAG not defined yet) the image of
// an existing container is used.
func (d *Deployer) resolve(ctx context.Context, app config.App) (service string, ref registry.Ref, fromCompose bool, err error) {
	if name, img, err := d.Compose.ResolveService(ctx, app, app.Service); err == nil {
		if ref, err := registry.ParseRef(img); err == nil {
			return name, ref, true, nil
		}
	}
	if app.Service != "" {
		cs, err := d.Docker.ProjectContainers(ctx, app.Dir)
		if err == nil {
			for _, c := range cs {
				if c.Service == app.Service && c.Image != "" && !strings.HasPrefix(c.Image, "sha256:") {
					if ref, err := registry.ParseRef(c.Image); err == nil {
						return app.Service, ref, false, nil
					}
				}
			}
		}
	}
	return "", registry.Ref{}, false, ErrNoImage
}

// checkUsesAppTag compares the image Compose resolves with APP_TAG in
// .helmo/env. When APP_TAG is set but the image does not carry it, the compose
// file takes its tag from elsewhere (a literal tag or another variable).
func checkUsesAppTag(app config.App, ref registry.Ref) error {
	raw, ok, _ := GetVar(filepath.Join(app.Dir, ".helmo", "env"), envKey)
	if !ok || raw == "" {
		return nil
	}
	return matches(ref, parseCurrent(raw))
}

// checkResolvesTo asks Compose for the image of service, after APP_TAG was
// written, and fails unless it is exactly v.
func (d *Deployer) checkResolvesTo(ctx context.Context, app config.App, service string, v Version) error {
	img, err := d.Compose.ServiceImage(ctx, app, service)
	if err != nil {
		return fmt.Errorf("check the compose file: %w", err)
	}
	ref, err := registry.ParseRef(img)
	if err != nil {
		return fmt.Errorf("check the compose file: %w", err)
	}
	return matches(ref, v)
}

func matches(ref registry.Ref, v Version) error {
	if ref.Tag == v.Tag && (v.Digest == "" || ref.Digest == v.Digest) {
		return nil
	}
	got := ref.Name()
	if ref.Tag != "" {
		got += ":" + ref.Tag
	}
	if ref.Digest != "" {
		got += "@" + ref.Digest
	}
	return fmt.Errorf("%w: APP_TAG is %s, but Compose resolves the image to %s; use ${APP_TAG} as the tag of the image",
		ErrNotUsingAppTag, v, got)
}

// current is the version the app is configured to run: APP_TAG in
// .helmo/env, else the tag in the image Compose resolves.
func (d *Deployer) current(app config.App, ref registry.Ref) Version {
	if raw, ok, _ := GetVar(filepath.Join(app.Dir, ".helmo", "env"), envKey); ok && raw != "" {
		return parseCurrent(raw)
	}
	return Version{Tag: ref.Tag, Digest: ref.Digest}
}
