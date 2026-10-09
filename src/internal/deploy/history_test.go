package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appDirWithHelmo(t *testing.T) string {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".helmo"), 0o755)
	return dir
}

func TestHistoryAppendAndRecent(t *testing.T) {
	dir := appDirWithHelmo(t)
	if h, err := Recent(dir, 5); h != nil || err != nil {
		t.Fatalf("empty: %v %v", h, err)
	}
	for i := 0; i < 5; i++ {
		if err := Append(dir, Entry{Time: time.Unix(int64(i), 0).UTC(), Action: "deploy", To: fmt.Sprintf("v1.%d.0", i), Result: "ok"}); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := os.OpenFile(filepath.Join(dir, ".helmo", "history.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n{\"x\":1}\n")
	f.Close()

	h, err := Recent(dir, 3)
	if err != nil || len(h) != 3 || h[0].To != "v1.2.0" || h[2].To != "v1.4.0" {
		t.Fatalf("got %+v err=%v", h, err)
	}
}

func TestRecentReadsOnlyTheTailOfHugeFiles(t *testing.T) {
	dir := appDirWithHelmo(t)
	f, _ := os.Create(filepath.Join(dir, ".helmo", "history.jsonl"))
	filler := `{"time":"2026-01-01T00:00:00Z","action":"deploy","to":"v0.0.1","result":"ok","error":"` + strings.Repeat("x", 900) + `"}` + "\n"
	for i := 0; i < 3000; i++ { // ~2.8 MB
		f.WriteString(filler)
	}
	f.Close()
	Append(dir, Entry{Action: "deploy", To: "v9.9.9", Result: "ok"})

	h, err := Recent(dir, 2)
	if err != nil || len(h) != 2 || h[1].To != "v9.9.9" || h[0].To != "v0.0.1" {
		t.Fatalf("got %+v err=%v", h, err)
	}
}

func TestRollbackTarget(t *testing.T) {
	ok := func(action, from, to string) Entry { return Entry{Action: action, From: from, To: to, Result: "ok"} }
	tests := []struct {
		name    string
		entries []Entry
		want    string
		found   bool
	}{
		{"empty", nil, "", false},
		{"first deploy goes back to what it replaced", []Entry{ok("deploy", "v1.0.0", "v1.1.0@sha256:x")}, "v1.0.0", true},
		{"first deploy replaced nothing known", []Entry{ok("deploy", "", "v1.1.0")}, "", false},
		{"two deploys", []Entry{ok("deploy", "v1.0.0", "v1.1.0"), ok("deploy", "v1.1.0", "v1.2.0")}, "v1.1.0", true},
		{"failed attempts are ignored", []Entry{
			ok("deploy", "v1.0.0", "v1.1.0"),
			{Action: "deploy", From: "v1.1.0", To: "v1.2.0", Result: "rolled_back"},
			ok("auto-rollback", "v1.2.0", "v1.1.0"),
		}, "v1.0.0", true},
		{"after a rollback the older one is the target", []Entry{
			ok("deploy", "v1.0.0", "v1.1.0"), ok("deploy", "v1.1.0", "v1.2.0"), ok("rollback", "v1.2.0", "v1.1.0"),
		}, "v1.2.0", true},
	}
	for _, tt := range tests {
		v, found := RollbackTarget(tt.entries)
		if found != tt.found || (found && v.String() != tt.want) {
			t.Errorf("%s: got %q %v, want %q %v", tt.name, v, found, tt.want, tt.found)
		}
	}
}

func TestVersion(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	v, err := NewVersion("v1.2.3", d)
	if err != nil || v.String() != "v1.2.3@"+d {
		t.Fatalf("%v %v", v, err)
	}
	if got := parseCurrent("v1.2.3@" + d); got != v {
		t.Errorf("parse: %+v", got)
	}
	if got := parseCurrent(" latest "); got.Tag != "latest" || got.Digest != "" {
		t.Errorf("loose: %+v", got)
	}
	for _, bad := range [][2]string{{"latest", d}, {"v1.2.3", "sha256:short"}, {"v1.2.3", ""}} {
		if _, err := NewVersion(bad[0], bad[1]); err == nil {
			t.Errorf("%v must fail", bad)
		}
	}
	if !(Version{}).IsZero() || v.IsZero() {
		t.Error("IsZero")
	}
}
