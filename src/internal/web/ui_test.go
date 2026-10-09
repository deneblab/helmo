package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/deploy"
	"helmo/internal/docker"
)

func uiHandler() http.Handler {
	s := &Server{Docker: &docker.Fake{}}
	return routingFor([]config.App{{ID: "cadastro", Dir: "/srv/apps/cadastro", Ports: []int{8600}}}, s)
}

func TestIndexPage(t *testing.T) {
	rec := get(uiHandler(), "/_helmo/", "8600")
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>Helmo · cadastro</title>") || !strings.Contains(body, `<h1 id="app-id">cadastro</h1>`) {
		t.Errorf("app id missing:\n%s", body)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("CSP must not allow inline code")
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", rec.Header())
	}
	// the CSP only works if the page really has no inline code
	if regexp.MustCompile(`(?i)<script[^>]*>[^<]`).MatchString(body) || strings.Contains(body, " style=") ||
		regexp.MustCompile(`(?i)\son[a-z]+=`).MatchString(body) {
		t.Error("inline script, style or event handler in index.html")
	}
}

func TestIndexEscapesAppID(t *testing.T) {
	s := &Server{Docker: &docker.Fake{}}
	h := routingFor([]config.App{{ID: `<script>alert(1)</script>`, Ports: []int{8600}}}, s)
	body := get(h, "/_helmo/", "8600").Body.String()
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("app id not escaped:\n%s", body)
	}
}

func TestStaticFiles(t *testing.T) {
	h := uiHandler()
	for path, ctype := range map[string]string{
		"/_helmo/static/app.css": "text/css",
		"/_helmo/static/app.js":  "text/javascript",
	} {
		rec := get(h, path, "8600")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), ctype) || rec.Body.Len() < 100 {
			t.Errorf("%s: %d %q (%d bytes)", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", path)
		}
	}
	for _, bad := range []string{"/_helmo/static/index.html", "/_helmo/static/..%2Fweb.go", "/_helmo/static/nope.js", "/_helmo/static/"} {
		if rec := get(h, bad, "8600"); rec.Code != 404 {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec := get(h, "/_helmo/static/app.js", "9999"); rec.Code != 404 {
		t.Errorf("static files must also require a known app: %d", rec.Code)
	}
}

func TestBareHelmoPathRedirects(t *testing.T) {
	rec := get(uiHandler(), "/_helmo", "8600")
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "/_helmo/" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get(uiHandler(), "/_helmo/", "9999"); rec.Code != 404 {
		t.Errorf("unknown port: %d", rec.Code)
	}
	if rec := post(uiHandler(), "/_helmo/", "8600"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /_helmo/: %d", rec.Code)
	}
}

// Every endpoint app.js calls must exist with the method it uses.
func TestScriptOnlyCallsExistingEndpoints(t *testing.T) {
	js, err := fs.ReadFile(uiFiles, "ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	cm := &compose.Manager{Runner: &fakeRunner{}}
	ops := &docker.Fake{}
	srv := &Server{
		Docker:   ops,
		Compose:  cm,
		Registry: &fakeTags{},
		Deployer: &deploy.Deployer{Compose: cm, Docker: ops, Registry: digestSource{}},
	}
	h := routingFor([]config.App{{ID: "cadastro", Dir: t.TempDir(), Ports: []int{8600}}}, srv)

	check := func(method, path string) {
		var rec *httptest.ResponseRecorder
		if method == "POST" {
			rec = post(h, "/_helmo/api"+path, "8600")
		} else {
			rec = get(h, "/_helmo/api"+path, "8600")
		}
		// a registered route may answer 4xx/5xx for its own reasons, but never
		// "no such route" (the mux answers 404 with its own body, or 405)
		if rec.Code == http.StatusMethodNotAllowed || strings.HasPrefix(rec.Body.String(), "404 page not found") {
			t.Errorf("%s /api%s: not routed (%d)", method, path, rec.Code)
		}
	}

	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bapi\('(/[a-z]+)`).FindAllStringSubmatch(src, -1) {
		check("GET", m[1])
		seen["GET "+m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\bpost\('(/[a-z]+)'`).FindAllStringSubmatch(src, -1) {
		check("POST", m[1])
		seen["POST "+m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\$\{API\}(/[a-z]+)`).FindAllStringSubmatch(src, -1) {
		check("GET", m[1])
		seen["GET "+m[1]] = true
	}
	for _, m := range regexp.MustCompile(`post\('/' \+ op\)`).FindAllString(src, -1) {
		_ = m
		for _, op := range []string{"start", "stop", "restart"} {
			check("POST", "/"+op)
			seen["POST /"+op] = true
		}
	}
	for _, want := range []string{"GET /status", "GET /versions", "GET /history", "GET /deploy", "GET /logs",
		"POST /deploy", "POST /rollback", "POST /start", "POST /restart", "POST /stop"} {
		if !seen[want] {
			t.Errorf("expected app.js to use %s", want)
		}
	}
}
