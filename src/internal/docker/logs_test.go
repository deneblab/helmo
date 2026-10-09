package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func frame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func collect(t *testing.T, input []byte, tty bool) []LogLine {
	t.Helper()
	var got []LogLine
	err := readLogs(bytes.NewReader(input), tty, func(l LogLine) error { got = append(got, l); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReadLogsMultiplexed(t *testing.T) {
	ts := "2026-10-09T08:00:00.123456789Z"
	var in []byte
	in = append(in, frame(1, ts+" hello\n"+ts+" wor")...) // line split across frames
	in = append(in, frame(2, ts+" err one\n")...)
	in = append(in, frame(1, "ld\r\nno timestamp\nlast without newline")...)

	got := collect(t, in, false)
	// "wor" + "ld" arrive in two frames and form one line
	want := []LogLine{
		{Time: mustTime(ts), Stream: "stdout", Text: "hello"},
		{Time: mustTime(ts), Stream: "stderr", Text: "err one"},
		{Time: mustTime(ts), Stream: "stdout", Text: "world"},
		{Stream: "stdout", Text: "no timestamp"},
		{Stream: "stdout", Text: "last without newline"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestReadLogsTTYIsRaw(t *testing.T) {
	got := collect(t, []byte("a\nb\n"), true)
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" || got[0].Stream != "stdout" {
		t.Fatalf("got %+v", got)
	}
}

func TestReadLogsTruncatesLongLines(t *testing.T) {
	long := strings.Repeat("x", maxLineBytes*3)
	got := collect(t, frame(1, long+"\nnext\n"), false)
	if len(got) != 2 {
		t.Fatalf("got %d lines", len(got))
	}
	if !strings.HasSuffix(got[0].Text, truncMark) || len(got[0].Text) != maxLineBytes+len(truncMark) {
		t.Fatalf("len %d", len(got[0].Text))
	}
	if got[1].Text != "next" {
		t.Fatalf("second: %q", got[1].Text)
	}
}

func TestReadLogsBigFrameDoesNotNeedBigBuffer(t *testing.T) {
	line := strings.Repeat("y", 100) + "\n"
	payload := strings.Repeat(line, 5000) // 500 KB in one frame
	n := 0
	err := readLogs(bytes.NewReader(frame(1, payload)), false, func(LogLine) error { n++; return nil })
	if err != nil || n != 5000 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestReadLogsStopsOnEmitError(t *testing.T) {
	stop := errors.New("stop")
	err := readLogs(bytes.NewReader(frame(1, "a\nb\n")), false, func(LogLine) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("got %v", err)
	}
}

func TestReadLogsTruncatedStream(t *testing.T) {
	in := frame(1, "hello\n")
	err := readLogs(bytes.NewReader(in[:len(in)-3]), false, func(LogLine) error { return nil })
	if err == nil {
		t.Fatal("want error for cut stream")
	}
}

func TestClientStreamLogs(t *testing.T) {
	var logQuery map[string][]string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/abc123/json"):
			w.Write([]byte(`{"Config":{"Tty":false}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/abc123/logs"):
			logQuery = r.URL.Query()
			w.Write(frame(1, "2026-10-09T08:00:00Z first\n"))
			w.Write(frame(2, "2026-10-09T08:00:01Z second\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = l
	srv.Start()
	defer srv.Close()

	c, _ := NewClient("unix://" + sock)
	var got []LogLine
	err = c.StreamLogs(context.Background(), "abc123", LogOptions{Tail: 50, Follow: true},
		func(l LogLine) error { got = append(got, l); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "first" || got[1].Stream != "stderr" || got[1].Text != "second" {
		t.Fatalf("got %+v", got)
	}
	for k, want := range map[string]string{"follow": "1", "tail": "50", "timestamps": "1", "stdout": "1", "stderr": "1"} {
		if v := logQuery[k]; len(v) != 1 || v[0] != want {
			t.Errorf("query %s = %v, want %s", k, v, want)
		}
	}

	if err := c.StreamLogs(context.Background(), "missing", LogOptions{}, func(LogLine) error { return nil }); err == nil {
		t.Fatal("want error for unknown container")
	}
}

func TestClientStreamLogsStopsWhenContextEnds(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/json") {
			w.Write([]byte(`{"Config":{"Tty":true}}`))
			return
		}
		w.Write([]byte("line\n"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // held open like a followed log
	}))
	defer srv.Close()

	c, _ := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.StreamLogs(ctx, "x", LogOptions{Follow: true}, func(LogLine) error { return nil })
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("StreamLogs did not return after cancel")
	}
}
