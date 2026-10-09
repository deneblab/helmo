package docker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	up := Container{State: "running"}
	upBad := Container{State: "running", Health: "unhealthy"}
	down := Container{State: "exited"}
	tests := []struct {
		name string
		in   []Container
		want State
	}{
		{"no containers", nil, StateDown},
		{"all running", []Container{up, up}, StateRunning},
		{"running but unhealthy", []Container{up, upBad}, StateUnhealthy},
		{"some running", []Container{up, down}, StatePartial},
		{"none running", []Container{down, {State: "created"}}, StateStopped},
		{"restarting is not up", []Container{{State: "restarting"}}, StateStopped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Summarize(tt.in); got != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHealthFromStatus(t *testing.T) {
	tests := map[string]string{
		"Up 2 hours (healthy)":            "healthy",
		"Up 3 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 2 hours":                      "",
		"Exited (0) 3 hours ago":          "",
	}
	for in, want := range tests {
		if got := healthFromStatus(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

const listFixture = `[
 {"Id":"0123456789abcdef0123","Names":["/cadastro-web-1"],"Image":"ghcr.io/org/app:v1.2.3",
  "State":"running","Status":"Up 2 hours (healthy)",
  "Labels":{"com.docker.compose.service":"web","com.docker.compose.project.working_dir":"/srv/apps/cadastro"}},
 {"Id":"fedcba9876543210fedc","Names":["/cadastro-db-1"],"Image":"postgres:16",
  "State":"exited","Status":"Exited (0) 1 hour ago",
  "Labels":{"com.docker.compose.service":"db"}}
]`

func fakeDaemon(t *testing.T, check func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_ping":
			w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			if check != nil {
				check(r)
			}
			w.Write([]byte(listFixture))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
}

func TestClientOverUnixSocket(t *testing.T) {
	var gotQuery map[string]string
	srv := fakeDaemon(t, func(r *http.Request) {
		var f map[string][]string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &f); err != nil {
			t.Errorf("filters: %v", err)
		}
		gotQuery = map[string]string{"all": r.URL.Query().Get("all"), "label": strings.Join(f["label"], ",")}
	})
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	defer srv.Close()

	c, err := NewClient("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	got, err := c.ProjectContainers(ctx, "/srv/apps/cadastro")
	if err != nil {
		t.Fatal(err)
	}
	want := []Container{
		{ID: "0123456789ab", Name: "cadastro-web-1", Service: "web", Image: "ghcr.io/org/app:v1.2.3", State: "running", Health: "healthy"},
		{ID: "fedcba987654", Name: "cadastro-db-1", Service: "db", Image: "postgres:16", State: "exited"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if gotQuery["all"] != "1" || gotQuery["label"] != "com.docker.compose.project.working_dir=/srv/apps/cadastro" {
		t.Errorf("query: %v", gotQuery)
	}
}

func TestClientOverTCPAndErrors(t *testing.T) {
	srv := fakeDaemon(t, nil)
	srv.Start()
	defer srv.Close()

	c, err := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = c.get(context.Background(), "/nope", nil)
	if err == nil || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want 500 error with body, got %v", err)
	}
}

func TestNewClientRejectsScheme(t *testing.T) {
	if _, err := NewClient("ftp://x"); err == nil {
		t.Fatal("want error")
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Projects: map[string][]Container{"/a": {{State: "running"}}}}
	cs, _ := f.ProjectContainers(context.Background(), "/a")
	if Summarize(cs) != StateRunning {
		t.Fatal("fake lookup")
	}
}

func TestNegotiate(t *testing.T) {
	tests := []struct {
		daemon, min, want string
		ok                bool
	}{
		{"1.51", "1.44", maxAPIVersion, true}, // newer daemon: ours
		{"1.41", "1.12", "1.41", true},        // older daemon: its own
		{"1.60", "1.50", "1.50", true},        // our version already dropped
		{"1.47", "", "1.47", true},
		{"", "1.24", "", false},
		{"x", "1.24", "", false},
	}
	for _, tt := range tests {
		got, ok := negotiate(tt.daemon, tt.min)
		if got != tt.want || ok != tt.ok {
			t.Errorf("negotiate(%q, %q) = %q, %v; want %q, %v", tt.daemon, tt.min, got, ok, tt.want, tt.ok)
		}
	}
}

// A daemon like Docker 29 refuses API versions below 1.44. The client must
// ask /version (without a prefix) and use a version the daemon accepts.
func TestClientNegotiatesAPIVersion(t *testing.T) {
	var versionCalls int
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			versionCalls++
			w.Write([]byte(`{"Version":"29.0.4","ApiVersion":"1.44","MinAPIVersion":"1.44"}`))
			return
		}
		paths = append(paths, r.URL.Path)
		if !strings.HasPrefix(r.URL.Path, "/v1.44/") {
			http.Error(w, `{"message":"client version is too old"}`, http.StatusBadRequest)
			return
		}
		w.Write([]byte(listFixture))
	}))
	defer srv.Close()

	c, _ := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	for i := 0; i < 2; i++ {
		if _, err := c.ProjectContainers(context.Background(), "/srv/apps/cadastro"); err != nil {
			t.Fatal(err)
		}
	}
	if versionCalls != 1 {
		t.Errorf("/version asked %d times, want once", versionCalls)
	}
	if len(paths) != 2 || paths[0] != "/v1.44/containers/json" {
		t.Errorf("paths: %v", paths)
	}
}
