package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseRef(t *testing.T) {
	tests := []struct {
		in   string
		want Ref
	}{
		{"ghcr.io/org/app:v1.2.3", Ref{Registry: "ghcr.io", Repository: "org/app", Tag: "v1.2.3"}},
		{"ghcr.io/org/app", Ref{Registry: "ghcr.io", Repository: "org/app", Tag: "latest"}},
		{"nginx", Ref{Registry: "docker.io", Repository: "library/nginx", Tag: "latest"}},
		{"nginx:1.27.0", Ref{Registry: "docker.io", Repository: "library/nginx", Tag: "1.27.0"}},
		{"org/app:2.0.0", Ref{Registry: "docker.io", Repository: "org/app", Tag: "2.0.0"}},
		{"localhost:5000/app:1.0.0", Ref{Registry: "localhost:5000", Repository: "app", Tag: "1.0.0"}},
		{"registry.example.com:8443/a/b/c:v1", Ref{Registry: "registry.example.com:8443", Repository: "a/b/c", Tag: "v1"}},
		{"ghcr.io/org/app@sha256:abc", Ref{Registry: "ghcr.io", Repository: "org/app", Digest: "sha256:abc"}},
		{"GHCR.io/org/app:v1", Ref{Registry: "ghcr.io", Repository: "org/app", Tag: "v1"}},
	}
	for _, tt := range tests {
		got, err := ParseRef(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("%q: got %+v err=%v, want %+v", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "  ", "ghcr.io/"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestSortVersions(t *testing.T) {
	in := []string{"latest", "v1.2.3", "1.10.0", "v1.9.0", "sha-3f9a1c2", "v2.0.0-rc1", "main", "v0.0.1", "1.2.3"}
	want := []string{"1.10.0", "v1.9.0", "v1.2.3", "1.2.3", "v0.0.1"}
	if got := SortVersions(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if IsVersionTag("v1.2") || !IsVersionTag("v1.2.3") {
		t.Fatal("IsVersionTag")
	}
}

func writeConfig(t *testing.T, json string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(json), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLoadAuth(t *testing.T) {
	p := writeConfig(t, fmt.Sprintf(`{
	  "auths": {
	    "ghcr.io": {"auth": %q},
	    "https://index.docker.io/v1/": {"username": "hubuser", "password": "hubpass"},
	    "broken.example": {"auth": "!!!"}
	  },
	  "credHelpers": {"ecr.example.com": "ecr-login"}
	}`, b64("alice:s3cret:with:colons")))
	a, err := LoadAuth(p)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := a.For("GHCR.io"); !ok || c.User != "alice" || c.Pass != "s3cret:with:colons" {
		t.Errorf("ghcr: %+v %v", c, ok)
	}
	if c, ok := a.For("docker.io"); !ok || c.User != "hubuser" {
		t.Errorf("hub: %+v %v", c, ok)
	}
	if _, ok := a.For("broken.example"); ok {
		t.Error("undecodable entry must be ignored")
	}
	if !a.NeedsHelper("ecr.example.com") || a.NeedsHelper("ghcr.io") {
		t.Error("NeedsHelper")
	}

	missing, err := LoadAuth(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := missing.For("ghcr.io"); ok {
		t.Error("missing file means no credentials")
	}
	if _, err := LoadAuth(writeConfig(t, "{not json")); err == nil {
		t.Error("want parse error")
	}
}

// fakeRegistry serves one repository. With private=true it demands a bearer
// token that the /token endpoint only gives for the right Basic credentials.
type fakeRegistry struct {
	srv        *httptest.Server
	private    bool
	tokenCalls atomic.Int32
	tokenAuth  atomic.Value // last Authorization header seen by /token
}

func newFakeRegistry(t *testing.T, private bool) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{private: private}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		f.tokenAuth.Store(r.Header.Get("Authorization"))
		if f.private && r.Header.Get("Authorization") != "Basic "+b64("alice:pw") {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		if got := r.URL.Query().Get("scope"); got != "repository:org/app:pull" {
			http.Error(w, "bad scope "+got, http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"token":"tok123"}`)
	})
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.private && r.Header.Get("Authorization") != "Bearer tok123" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake",scope="repository:org/app:pull"`, f.srv.URL))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/v2/org/app/tags/list", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("n") != "1000" {
			t.Errorf("n = %q", r.URL.Query().Get("n"))
		}
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/org/app/tags/list?n=1000&last=v1.1.0>; rel="next"`)
			fmt.Fprint(w, `{"name":"org/app","tags":["latest","v1.0.0","v1.1.0"]}`)
			return
		}
		fmt.Fprint(w, `{"name":"org/app","tags":["sha-abc","v1.2.0","v2.0.0"]}`)
	}))
	mux.HandleFunc("/v2/org/app/manifests/", guard(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || !strings.Contains(r.Header.Get("Accept"), "oci.image.index") {
			t.Errorf("manifest request: %s accept=%q", r.Method, r.Header.Get("Accept"))
		}
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) ref() Ref {
	return Ref{Registry: strings.TrimPrefix(f.srv.URL, "http://"), Repository: "org/app", Tag: "v1.0.0"}
}

func TestTagsPublicWithPaginationAndFilter(t *testing.T) {
	f := newFakeRegistry(t, false)
	got, err := (&Client{}).Tags(context.Background(), f.ref(), IsVersionTag)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"v1.0.0", "v1.1.0", "v1.2.0", "v2.0.0"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if f.tokenCalls.Load() != 0 {
		t.Error("public repository needs no token")
	}
	all, _ := (&Client{}).Tags(context.Background(), f.ref(), nil)
	if len(all) != 6 {
		t.Errorf("nil filter keeps all: %v", all)
	}
}

func TestTagsPrivateUsesConfigCredentials(t *testing.T) {
	f := newFakeRegistry(t, true)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	auth, _ := LoadAuth(writeConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64("alice:pw"))))

	got, err := (&Client{Auth: auth}).Tags(context.Background(), f.ref(), IsVersionTag)
	if err != nil || len(got) != 4 {
		t.Fatalf("got %v err=%v", got, err)
	}
}

func TestTagsPrivateErrors(t *testing.T) {
	f := newFakeRegistry(t, true)
	host := strings.TrimPrefix(f.srv.URL, "http://")

	_, err := (&Client{}).Tags(context.Background(), f.ref(), nil)
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("no creds: %v", err)
	}

	wrong, _ := LoadAuth(writeConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64("alice:WRONG"))))
	_, err = (&Client{Auth: wrong}).Tags(context.Background(), f.ref(), nil)
	if !errors.Is(err, ErrUnauthorized) || !strings.Contains(err.Error(), "rejected") {
		t.Errorf("wrong creds: %v", err)
	}
	if strings.Contains(err.Error(), "WRONG") || strings.Contains(err.Error(), b64("alice:WRONG")) {
		t.Errorf("error leaks the secret: %v", err)
	}

	helper, _ := LoadAuth(writeConfig(t, fmt.Sprintf(`{"credsStore":"desktop","auths":{}}`)))
	_, err = (&Client{Auth: helper}).Tags(context.Background(), f.ref(), nil)
	if err == nil || !strings.Contains(err.Error(), "credential helper") {
		t.Errorf("helper: %v", err)
	}
}

func TestNotFoundAndServerError(t *testing.T) {
	f := newFakeRegistry(t, false)
	ref := f.ref()
	ref.Repository = "org/missing"
	if _, err := (&Client{}).Tags(context.Background(), ref, nil); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("404: %v", err)
	}
}

func TestDigest(t *testing.T) {
	f := newFakeRegistry(t, true)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	auth, _ := LoadAuth(writeConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64("alice:pw"))))
	d, err := (&Client{Auth: auth}).Digest(context.Background(), f.ref(), "v1.0.0")
	if err != nil || d != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("digest %q err=%v", d, err)
	}
}

func TestBasicChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+b64("bob:pw") {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"tags":["1.0.0"]}`)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	auth, _ := LoadAuth(writeConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64("bob:pw"))))
	got, err := (&Client{Auth: auth}).Tags(context.Background(), Ref{Registry: host, Repository: "any"}, nil)
	if err != nil || !reflect.DeepEqual(got, []string{"1.0.0"}) {
		t.Fatalf("got %v err=%v", got, err)
	}
}

func TestCredentialsNotSentToForeignRealm(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("untouched")
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer evil.Close()
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="x"`, strings.Replace(evil.URL, "127.0.0.1", "localhost", 1)))
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer reg.Close()

	host := strings.TrimPrefix(reg.URL, "http://") // 127.0.0.1:port
	auth, _ := LoadAuth(writeConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, b64("alice:pw"))))
	_, err := (&Client{Auth: auth}).Tags(context.Background(), Ref{Registry: host, Repository: "org/app"}, nil)
	if err == nil {
		t.Fatal("want failure")
	}
	if got := gotAuth.Load().(string); got != "" {
		t.Fatalf("credentials were sent to a foreign realm: %q", got)
	}
}

func TestSameSite(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"auth.docker.io", "registry-1.docker.io", true},
		{"ghcr.io", "ghcr.io", true},
		{"ghcr.io", "evil.io", false},
		{"127.0.0.1", "10.0.0.1", false},
		{"127.0.0.1", "127.0.0.1", true},
		{"localhost", "otherhost", false},
	}
	for _, tt := range tests {
		if got := sameSite(tt.a, tt.b); got != tt.want {
			t.Errorf("sameSite(%q,%q) = %v", tt.a, tt.b, got)
		}
	}
}

func TestSchemeAndChallenge(t *testing.T) {
	if scheme("localhost:5000") != "http" || scheme("127.0.0.1:1") != "http" || scheme("ghcr.io") != "https" {
		t.Error("scheme")
	}
	s, p := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:o/a:pull"`)
	if s != "Bearer" || p["realm"] != "https://ghcr.io/token" || p["service"] != "ghcr.io" || p["scope"] != "repository:o/a:pull" {
		t.Errorf("%s %v", s, p)
	}
}
