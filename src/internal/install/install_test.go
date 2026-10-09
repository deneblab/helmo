//go:build unix

// Package install tests scripts/install.sh, the script users download to set
// up Helmo and the apps it manages. The script runs for real, in a temporary
// directory, with stub docker and curl commands first on PATH.
package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

const repoRoot = "../../.."

const dockerStub = `#!/bin/sh
echo "docker $*" >> "$STUBLOG"
case "$1 $2" in
	"network inspect") [ "$3" = traefik ] || exit 1 ;;
	"ps --format") exit 1 ;;
esac
exit 0
`

const curlStub = `#!/bin/sh
case "$*" in
	*token*) echo '{"token":"t","access_token":"t"}' ;;
	*tags/list*) echo '{"name":"deneblab/helmo","tags":["0.1.2","0.1.10","0.1.9","latest","sha-abc"]}' ;;
	*) exit 1 ;;
esac
`

type env struct {
	t    *testing.T
	root string // temporary directory holding everything
	bin  string
	log  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, root: root, bin: filepath.Join(root, "bin"), log: filepath.Join(root, "docker.log")}
	if err := os.MkdirAll(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	e.stub("docker", dockerStub)
	e.stub("curl", curlStub)
	return e
}

func (e *env) stub(name, body string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.bin, name), []byte(body), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) read(path string) string {
	e.t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// run executes the script in dir and returns its output and exit error.
func (e *env) run(dir string, args ...string) (string, error) {
	e.t.Helper()
	script, err := filepath.Abs(filepath.Join(repoRoot, "scripts", "install.sh"))
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + e.bin + ":" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(e.root, "home"),
		"STUBLOG=" + e.log,
	}
	// No controlling terminal: a script that wrongly asks a question fails
	// instead of waiting for the keyboard of whoever runs the tests.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *env) mustRun(dir string, args ...string) string {
	e.t.Helper()
	out, err := e.run(dir, args...)
	if err != nil {
		e.t.Fatalf("install.sh %v: %v\n%s", args, err, out)
	}
	return out
}

func (e *env) mustFail(dir, want string, args ...string) {
	e.t.Helper()
	out, err := e.run(dir, args...)
	if err == nil {
		e.t.Fatalf("install.sh %v succeeded, want an error containing %q\n%s", args, want, out)
	}
	if !strings.Contains(out, want) {
		e.t.Fatalf("install.sh %v: output does not contain %q\n%s", args, want, out)
	}
}

func (e *env) dir(name string) string {
	e.t.Helper()
	d := filepath.Join(e.root, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return d
}

// ---- helmo ---------------------------------------------------------------

func TestHelmoWritesFilesAndStarts(t *testing.T) {
	e := newEnv(t)
	work := e.dir("helmo")
	apps := filepath.Join(e.root, "apps")

	out := e.mustRun(work, "helmo", "--yes", "--version", "1.2.3", "--apps-dir", apps)

	envFile := e.read(filepath.Join(work, ".env"))
	for _, want := range []string{"HELMO_TAG=1.2.3\n", "TRAEFIK_NETWORK=traefik\n", "DOCKER_GID="} {
		if !strings.Contains(envFile, want) {
			t.Errorf(".env lacks %q:\n%s", want, envFile)
		}
	}
	compose := e.read(filepath.Join(work, "compose.yaml"))
	if !strings.Contains(compose, "- "+apps+":"+apps) || !strings.Contains(compose, "HELMO_APPS_DIR: "+apps) {
		t.Errorf("compose.yaml does not use the apps directory:\n%s", compose)
	}
	if st, err := os.Stat(apps); err != nil || !st.IsDir() {
		t.Errorf("apps directory not created: %v", err)
	}
	if !strings.Contains(e.read(e.log), "docker compose up -d") {
		t.Errorf("docker compose up -d was not run:\n%s", e.read(e.log))
	}
	if !strings.Contains(out, "PathPrefix(`/_helmo`)") {
		t.Errorf("Traefik hint missing:\n%s", out)
	}
	// With no ~/.docker/config.json an empty file is used; Compose would
	// create a directory in its place.
	if got := e.read(filepath.Join(work, "docker-config.json")); got != "{}\n" {
		t.Errorf("docker-config.json = %q", got)
	}
}

func TestHelmoPicksNewestVersion(t *testing.T) {
	e := newEnv(t)
	work := e.dir("helmo")
	e.mustRun(work, "helmo", "--yes", "--apps-dir", filepath.Join(e.root, "apps"))
	// 0.1.10 beats 0.1.9 (numeric order), and "latest" is ignored.
	if got := e.read(filepath.Join(work, ".env")); !strings.Contains(got, "HELMO_TAG=0.1.10\n") {
		t.Errorf(".env:\n%s", got)
	}
}

func TestHelmoKeepsExistingFilesWithYes(t *testing.T) {
	e := newEnv(t)
	work := e.dir("helmo")
	e.write(filepath.Join(work, ".env"), "HELMO_TAG=0.0.1\n")
	out := e.mustRun(work, "helmo", "--yes", "--version", "1.2.3", "--apps-dir", filepath.Join(e.root, "apps"))
	if got := e.read(filepath.Join(work, ".env")); got != "HELMO_TAG=0.0.1\n" {
		t.Errorf("existing .env was overwritten:\n%s", got)
	}
	if !strings.Contains(out, ".env: kept as it is") {
		t.Errorf("output does not say the file was kept:\n%s", out)
	}
	// A second run with the same settings changes nothing.
	work2 := e.dir("helmo2")
	args := []string{"helmo", "--yes", "--version", "1.2.3", "--apps-dir", filepath.Join(e.root, "apps")}
	e.mustRun(work2, args...)
	if out := e.mustRun(work2, args...); !strings.Contains(out, "compose.yaml: up to date") || !strings.Contains(out, ".env: up to date") {
		t.Errorf("rerun is not idempotent:\n%s", out)
	}
}

func TestHelmoErrors(t *testing.T) {
	e := newEnv(t)
	work := e.dir("helmo")
	apps := filepath.Join(e.root, "apps")

	e.mustFail(work, "version must look like 1.2.3", "helmo", "--yes", "--version", "v1", "--apps-dir", apps)
	e.mustFail(work, "'nope' does not exist", "helmo", "--yes", "--version", "1.2.3", "--apps-dir", apps, "--network", "nope")
	e.mustFail(work, "must be an absolute path", "helmo", "--yes", "--version", "1.2.3", "--apps-dir", "relative")
	e.mustFail(work, "unknown option", "helmo", "--bogus")
	// Without --yes and without a terminal the script must not hang.
	e.mustFail(work, "no terminal", "helmo", "--version", "1.2.3")

	e.stub("curl", "#!/bin/sh\nexit 22\n")
	e.mustFail(work, "pass --version", "helmo", "--yes", "--apps-dir", apps)
	e.stub("docker", "#!/bin/sh\nexit 1\n")
	e.mustFail(work, "cannot talk to Docker", "helmo", "--yes", "--version", "1.2.3", "--apps-dir", apps)

	for _, f := range []string{".env", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(work, f)); err == nil {
			t.Errorf("%s written although the script failed", f)
		}
	}
}

// The compose file the script writes must stay the same as the example in the
// repository, apart from comments and the messages after ":?".
func TestComposeMatchesExample(t *testing.T) {
	e := newEnv(t)
	work := e.dir("helmo")
	apps := filepath.Join(e.root, "apps")
	e.mustRun(work, "helmo", "--yes", "--version", "1.2.3", "--apps-dir", apps)

	// The example uses /srv/apps; the script writes the directory it was given.
	got := normalize(strings.ReplaceAll(e.read(filepath.Join(work, "compose.yaml")), apps, "/srv/apps"))
	want := normalize(e.read(filepath.Join(repoRoot, "compose.example.yaml")))
	if got != want {
		t.Errorf("scripts/install.sh and compose.example.yaml have drifted apart\n--- script\n%s\n--- example\n%s", got, want)
	}
}

var (
	trailingComment = regexp.MustCompile(`\s+#.*$`)
	requiredMessage = regexp.MustCompile(`:\?[^}]*\}`)
)

func normalize(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(trailingComment.ReplaceAllString(line, ""), " \t")
		line = requiredMessage.ReplaceAllString(line, ":?}")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// ---- app -----------------------------------------------------------------

// app creates /apps/<name>/compose.yaml with the given image lines.
func (e *env) app(name, compose string) string {
	e.t.Helper()
	d := e.dir(filepath.Join("apps", name))
	e.write(filepath.Join(d, "compose.yaml"), compose)
	return d
}

const literalTag = "services:\n  web:\n    image: ghcr.io/org/blog:v2.0.0\n"

func TestAppWritesConfigAndStartingVersion(t *testing.T) {
	e := newEnv(t)
	d := e.app("blog", literalTag)

	out := e.mustRun(d, "app", "--port", "8600", "--port", "8601", "--host", "node.ts.net", "--service", "web")

	want := "enabled: true\nports: [8600, 8601]\nhosts: [node.ts.net]\nservice: web\n"
	if got := e.read(filepath.Join(d, ".helmo", "app.yaml")); got != want {
		t.Errorf("app.yaml:\n%s\nwant:\n%s", got, want)
	}
	if got := e.read(filepath.Join(d, ".helmo", "env")); got != "APP_TAG=v2.0.0\n" {
		t.Errorf(".helmo/env = %q", got)
	}
	// The app's own file is never edited, only explained.
	if got := e.read(filepath.Join(d, "compose.yaml")); got != literalTag {
		t.Errorf("compose.yaml was modified:\n%s", got)
	}
	for _, s := range []string{"${APP_TAG:?use .helmo/dc", "image: ghcr.io/org/blog:v2.0.0", "http://<host>:8600/_helmo/"} {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestAppAlreadyUsingAppTag(t *testing.T) {
	e := newEnv(t)
	d := e.app("plain", "services:\n  web:\n    image: ghcr.io/org/plain:${APP_TAG:?x}\n")
	out := e.mustRun(d, "app", "--port", "8800")
	if !strings.Contains(out, "the image already uses APP_TAG") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(d, ".helmo", "env")); err == nil {
		t.Error(".helmo/env written although the running version is unknown")
	}
	if !strings.Contains(out, "Set the version that runs now") {
		t.Errorf("no hint about the starting version:\n%s", out)
	}
}

func TestAppSeveralImagesDoesNotGuessVersion(t *testing.T) {
	e := newEnv(t)
	d := e.app("multi", "services:\n  web:\n    image: ghcr.io/org/app:v1.2.3\n  db:\n    image: postgres:16\n")
	e.mustRun(d, "app", "--port", "8700")
	if _, err := os.Stat(filepath.Join(d, ".helmo", "env")); err == nil {
		t.Error(".helmo/env guessed from one of several images")
	}
}

func TestAppRerunIsIdempotent(t *testing.T) {
	e := newEnv(t)
	d := e.app("blog", literalTag)
	e.mustRun(d, "app", "--port", "8600")
	out := e.mustRun(d, "app", "--port", "8600")
	if !strings.Contains(out, ".helmo/app.yaml: up to date") {
		t.Errorf("output:\n%s", out)
	}
	// A different port does not silently replace the existing file with --yes.
	out = e.mustRun(d, "app", "--port", "8601", "--yes")
	if !strings.Contains(out, "kept as it is") || !strings.Contains(e.read(filepath.Join(d, ".helmo", "app.yaml")), "[8600]") {
		t.Errorf("app.yaml was replaced:\n%s", out)
	}
}

func TestAppRefusesBadInput(t *testing.T) {
	e := newEnv(t)
	e.app("other", literalTag)
	e.write(filepath.Join(e.root, "apps", "other", ".helmo", "app.yaml"), "enabled: true\nports: [8601, 8602]\n")
	blog := e.app("blog", literalTag)

	e.mustFail(blog, "port already used by another app: 8602 (app other)", "app", "--port", "8602")
	e.mustFail(blog, "given twice", "app", "--port", "8603", "--port", "8603")
	e.mustFail(blog, "out of range", "app", "--port", "99999")
	e.mustFail(blog, "not a number", "app", "--port", "abc")
	e.mustFail(blog, "give the Traefik port", "app", "--yes")
	e.mustFail(blog, "no terminal", "app")
	e.mustFail(blog, "unknown option", "app", "--bogus")

	bad := e.app("Bad_Id", literalTag)
	e.mustFail(bad, "must match", "app", "--port", "8700")

	empty := e.dir(filepath.Join("apps", "nocompose"))
	e.mustFail(empty, "no Compose file", "app", "--port", "8701")

	for _, d := range []string{blog, bad, empty} {
		if _, err := os.Stat(filepath.Join(d, ".helmo")); err == nil {
			t.Errorf("%s: .helmo created although the script failed", d)
		}
	}
}

func TestAppWarnsWhenOutsideAppsDirectory(t *testing.T) {
	e := newEnv(t)
	d := e.app("blog", literalTag)
	out := e.mustRun(d, "app", "--port", "8600", "--apps-dir", "/somewhere/else")
	if !strings.Contains(out, "Helmo will not see it") {
		t.Errorf("no warning:\n%s", out)
	}
	out = e.mustRun(d, "app", "--port", "8600", "--apps-dir", filepath.Dir(d))
	if strings.Contains(out, "Helmo will not see it") {
		t.Errorf("unexpected warning:\n%s", out)
	}
}

func TestUsage(t *testing.T) {
	e := newEnv(t)
	e.mustFail(e.dir("x"), "usage:")
	e.mustFail(e.dir("x"), "unknown command", "bogus")
}

func TestHelmoDetectsTraefikNetwork(t *testing.T) {
	e := newEnv(t)
	e.stub("docker", `#!/bin/sh
echo "docker $*" >> "$STUBLOG"
case "$1" in
	ps) echo "abc123 traefik:v3.1" ;;
	inspect) printf 'bridge\nproxy\n' ;;
	network) [ "$3" = proxy ] || exit 1 ;;
esac
exit 0
`)
	work := e.dir("helmo")
	out := e.mustRun(work, "helmo", "--yes", "--version", "1.2.3", "--apps-dir", filepath.Join(e.root, "apps"))
	if !strings.Contains(out, "Traefik is running on the network(s): proxy") {
		t.Errorf("no detection message:\n%s", out)
	}
	if got := e.read(filepath.Join(work, ".env")); !strings.Contains(got, "TRAEFIK_NETWORK=proxy\n") {
		t.Errorf(".env:\n%s", got)
	}
}

const traefikPortsStub = `#!/bin/sh
echo "docker $*" >> "$STUBLOG"
case "$1" in
	ps) echo "abc123 traefik:v3.1 traefik" ;;
	port) cat <<PORTS
80/tcp -> 0.0.0.0:80
443/tcp -> 0.0.0.0:443
8080/tcp -> 0.0.0.0:8081
8085/tcp -> 0.0.0.0:8085
8085/tcp -> [::]:8085
8086/tcp -> 0.0.0.0:8086
8087/tcp -> 0.0.0.0:8087
PORTS
	;;
esac
exit 0
`

func TestAppOffersFreeTraefikPorts(t *testing.T) {
	e := newEnv(t)
	e.stub("docker", traefikPortsStub)
	other := e.app("other", literalTag)
	e.write(filepath.Join(other, ".helmo", "app.yaml"), "enabled: true\nports: [8085, 8086]\n")
	d := e.app("blog", literalTag)

	// Only 8087 is left (80, 443, the dashboard 8081->8080 and the ports of
	// "other" do not count), so --yes takes it.
	out := e.mustRun(d, "app", "--yes")
	if !strings.Contains(out, "Traefik ports not used by another app: 8087") {
		t.Errorf("output:\n%s", out)
	}
	if got := e.read(filepath.Join(d, ".helmo", "app.yaml")); !strings.Contains(got, "ports: [8087]") {
		t.Errorf("app.yaml:\n%s", got)
	}

	// Two free ports: there is no sensible default, so --yes needs --port.
	if err := os.RemoveAll(filepath.Join(d, ".helmo")); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(other, ".helmo", "app.yaml"), "enabled: true\nports: [8085]\n")
	second := e.app("second", literalTag)
	e.mustFail(second, "give the Traefik port", "app", "--yes")
	out, _ = e.run(second, "app", "--yes")
	if !strings.Contains(out, "not used by another app: 8086 8087") {
		t.Errorf("output:\n%s", out)
	}
}

func TestAppSuggestsTheExactImageLine(t *testing.T) {
	e := newEnv(t)
	d := e.app("extraction", "services:\n  web:\n    image: ${IMG:-registry.example:5000/app}:${IMG_TAG:-latest}\n")
	out := e.mustRun(d, "app", "--port", "8102")
	want := "change:     image: ${IMG:-registry.example:5000/app}:${APP_TAG:?use .helmo/dc instead of docker compose}"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}

// The app runs (with a tag taken from its own variable, here "latest"): the
// script pins the starting version to the digest of the running image, and
// warns about a registry reachable only over plain HTTP.
func TestAppReadsRunningVersion(t *testing.T) {
	e := newEnv(t)
	e.stub("docker", `#!/bin/sh
case "$1 $2" in
	"ps --filter") echo "c1 web" ;;
	"inspect -f")
		case "$3" in
			*Config.Image*) echo "registry.example:5000/app:latest" ;;
			*) echo "sha256:img" ;;
		esac ;;
	"image inspect") printf 'registry.example:5000/app@sha256:abc123\n' ;;
esac
exit 0
`)
	e.stub("curl", `#!/bin/sh
case "$*" in
	*https://registry.example:5000/*) exit 35 ;;
	*http://registry.example:5000/*) exit 0 ;;
	*) exit 1 ;;
esac
`)
	d := e.app("extraction", "services:\n  web:\n    image: ${IMG:-registry.example:5000/app}:${IMG_TAG:-latest}\n")
	out := e.mustRun(d, "app", "--port", "8102")
	if got := e.read(filepath.Join(d, ".helmo", "env")); got != "APP_TAG=latest@sha256:abc123\n" {
		t.Errorf(".helmo/env = %q\n%s", got, out)
	}
	if !strings.Contains(out, "answers only over plain HTTP") {
		t.Errorf("no plain-HTTP warning:\n%s", out)
	}
}
