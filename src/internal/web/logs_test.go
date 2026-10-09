package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"helmo/internal/compose"
	"helmo/internal/config"
	"helmo/internal/docker"
	"helmo/internal/routing"
)

func logsServer(ops docker.DockerOps, service string) (*Server, http.Handler) {
	apps := []config.App{{ID: "cadastro", Dir: "/srv/apps/cadastro", Ports: []int{8600}, Service: service}}
	s := &Server{Docker: ops, Compose: &compose.Manager{Runner: &fakeRunner{}}}
	return s, routing.Middleware(routing.Options{Resolver: routing.NewResolver(apps)}, s.Handler())
}

func twoServices() *docker.Fake {
	return &docker.Fake{
		Projects: map[string][]docker.Container{"/srv/apps/cadastro": {
			{ID: "web1", Service: "web", State: "running"},
			{ID: "db1", Service: "db", State: "running"},
		}},
		Logs: map[string][]docker.LogLine{
			"web1": {
				{Time: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC), Stream: "stdout", Text: "one"},
				{Stream: "stderr", Text: "two <b>&"},
				{Stream: "stdout", Text: "three"},
			},
			"db1": {{Stream: "stdout", Text: "database line"}},
		},
	}
}

func TestLogsTailAndEnd(t *testing.T) {
	_, h := logsServer(twoServices(), "web")
	rec := get(h, "/_helmo/api/logs?follow=0&tail=2", "8600")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	for _, want := range []string{
		"data: {\"stream\":\"stderr\",\"text\":\"two \\u003cb\\u003e\\u0026\"}",
		`data: {"stream":"stdout","text":"three"}`,
		"event: end\ndata: {\"stream\":\"\",\"text\":\"\"}\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"text":"one"`) {
		t.Errorf("tail=2 returned the first line:\n%s", body)
	}
}

func TestLogsTimestampField(t *testing.T) {
	_, h := logsServer(twoServices(), "web")
	body := get(h, "/_helmo/api/logs?follow=0", "8600").Body.String()
	if !strings.Contains(body, `"ts":"2026-10-09T08:00:00Z"`) {
		t.Fatalf("no timestamp in:\n%s", body)
	}
}

func TestLogsServiceSelection(t *testing.T) {
	_, h := logsServer(twoServices(), "") // no default service
	tests := []struct {
		query string
		code  int
		want  string
	}{
		{"?follow=0", 400, "several services"},
		{"?follow=0&service=db", 200, "database line"},
		{"?follow=0&service=nope", 404, "no container for service"},
		{"?follow=0&service=db1", 404, "no container for service"}, // container IDs are not accepted
		{"?tail=-1", 400, "tail must be"},
		{"?tail=x", 400, "tail must be"},
		{"?tail=5000", 400, "tail must be"},
	}
	for _, tt := range tests {
		rec := get(h, "/_helmo/api/logs"+tt.query, "8600")
		if rec.Code != tt.code || !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("%s: %d %q", tt.query, rec.Code, rec.Body)
		}
	}

	_, hDefault := logsServer(twoServices(), "web")
	if rec := get(hDefault, "/_helmo/api/logs?follow=0", "8600"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"text":"one"`) {
		t.Errorf("default service from app.yaml: %d %s", rec.Code, rec.Body)
	}
}

func TestLogsStoppedAndDownApps(t *testing.T) {
	stopped := &docker.Fake{
		Projects: map[string][]docker.Container{"/srv/apps/cadastro": {{ID: "w", Service: "web", State: "exited"}}},
		Logs:     map[string][]docker.LogLine{"w": {{Stream: "stdout", Text: "last words"}}},
	}
	_, h := logsServer(stopped, "")
	if rec := get(h, "/_helmo/api/logs?follow=0", "8600"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "last words") {
		t.Errorf("stopped: %d %s", rec.Code, rec.Body)
	}
	_, hDown := logsServer(&docker.Fake{}, "")
	if rec := get(hDown, "/_helmo/api/logs", "8600"); rec.Code != 404 {
		t.Errorf("down: %d", rec.Code)
	}
}

func TestLogsErrors(t *testing.T) {
	_, h := logsServer(&docker.Fake{Err: context.DeadlineExceeded}, "")
	if rec := get(h, "/_helmo/api/logs", "8600"); rec.Code != 502 || strings.Contains(rec.Body.String(), "deadline") {
		t.Errorf("docker down: %d %q", rec.Code, rec.Body)
	}
	_, h2 := logsServer(twoServices(), "web")
	if rec := get(h2, "/_helmo/api/logs", "9999"); rec.Code != 404 {
		t.Errorf("unknown port: %d", rec.Code)
	}
}

func openStream(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Forwarded-Port", "8600")
	req.Host = "node:8600"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLogsFollowStreamsAndReleasesOnDisconnect(t *testing.T) {
	ops := twoServices()
	ops.HoldOpen = true
	s, h := logsServer(ops, "web")
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp := openStream(t, srv.URL+"/_helmo/api/logs?tail=1")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	var first string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data:") {
			first = sc.Text()
			break
		}
	}
	if !strings.Contains(first, `"text":"three"`) {
		t.Fatalf("first event %q", first)
	}
	open := func() int { s.mu.Lock(); defer s.mu.Unlock(); return s.streams["cadastro"] }
	if open() != 1 {
		t.Fatalf("streams = %d", open())
	}

	resp.Body.Close() // client goes away
	waitFor(t, "stream slot to be released", func() bool { return open() == 0 })
}

func TestLogsStreamLimit(t *testing.T) {
	ops := twoServices()
	ops.HoldOpen = true
	s, h := logsServer(ops, "web")
	srv := httptest.NewServer(h)
	defer srv.Close()

	var bodies []*http.Response
	for i := 0; i < maxLogStreams; i++ {
		resp := openStream(t, srv.URL+"/_helmo/api/logs")
		if resp.StatusCode != 200 {
			t.Fatalf("stream %d: %d", i, resp.StatusCode)
		}
		bodies = append(bodies, resp)
	}
	waitFor(t, "all streams registered", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.streams["cadastro"] == maxLogStreams })

	over := openStream(t, srv.URL+"/_helmo/api/logs")
	if over.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over limit: %d", over.StatusCode)
	}
	over.Body.Close()

	for _, b := range bodies {
		b.Body.Close()
	}
	waitFor(t, "slots released", func() bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.streams) == 0 })
}

func routingFor(apps []config.App, s *Server) http.Handler {
	return routing.Middleware(routing.Options{Resolver: routing.NewResolver(apps)}, s.Handler())
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func TestLogsSince(t *testing.T) {
	f := &docker.Fake{
		Projects: map[string][]docker.Container{"/srv/apps/cadastro": {{ID: "web1", Service: "web", State: "running"}}},
		Logs: map[string][]docker.LogLine{"web1": {
			{Time: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC), Stream: "stdout", Text: "before"},
			{Time: time.Date(2026, 10, 9, 8, 0, 1, 500_000_000, time.UTC), Stream: "stdout", Text: "after"},
		}},
	}
	_, h := logsServer(f, "web")
	since := time.Date(2026, 10, 9, 8, 0, 1, 0, time.UTC).Unix()
	body := get(h, "/_helmo/api/logs?follow=0&since="+strconv.FormatInt(since, 10)+".25", "8600").Body.String()
	if strings.Contains(body, `"before"`) || !strings.Contains(body, `"after"`) {
		t.Errorf("since not applied:\n%s", body)
	}
	for _, bad := range []string{"x", "-1", "1.", "1.1234567890", "1e9"} {
		if rec := get(h, "/_helmo/api/logs?follow=0&since="+bad, "8600"); rec.Code != http.StatusBadRequest {
			t.Errorf("since=%s: status %d", bad, rec.Code)
		}
	}
}

func TestParseUnix(t *testing.T) {
	got, ok := parseUnix("1760000000.5")
	if !ok || !got.Equal(time.Unix(1760000000, 500_000_000)) {
		t.Errorf("got %v %v", got, ok)
	}
}
