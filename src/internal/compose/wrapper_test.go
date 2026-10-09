package compose

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func appDir(t *testing.T) string {
	t.Helper()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(dir, ".helmo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEnsureWrapperCreatesReplacesAndIsIdempotent(t *testing.T) {
	dir := appDir(t)
	path := filepath.Join(dir, ".helmo", WrapperName)

	if err := EnsureWrapper(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("stat %v mode %v", err, fi)
	}
	got, _ := os.ReadFile(path)
	if string(got) != wrapperScript {
		t.Fatal("content differs")
	}

	// edited by hand: restored
	os.WriteFile(path, []byte("echo hacked"), 0o755)
	if err := EnsureWrapper(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != wrapperScript {
		t.Fatal("not restored")
	}

	// wrong mode: fixed; unchanged file: not rewritten
	os.Chmod(path, 0o644)
	if err := EnsureWrapper(dir); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	before, _ := os.Stat(path)
	EnsureWrapper(dir)
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("rewritten although unchanged")
	}

	if entries, _ := os.ReadDir(filepath.Join(dir, ".helmo")); len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestEnsureWrapperMissingHelmoDir(t *testing.T) {
	if err := EnsureWrapper(t.TempDir()); err == nil {
		t.Fatal("want error without .helmo")
	}
}

// Runs the script against a stub docker that prints its arguments.
func TestWrapperArguments(t *testing.T) {
	stubDir := t.TempDir()
	stub := "#!/bin/sh\necho \"cwd=$(pwd -P) args=$*\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string) string {
		cmd := exec.Command("/bin/sh", filepath.Join(dir, ".helmo", WrapperName), "up", "-d")
		cmd.Dir = t.TempDir() // started from an unrelated directory
		cmd.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}

	dir := appDir(t)
	if err := EnsureWrapper(dir); err != nil {
		t.Fatal(err)
	}
	if got, want := run(dir), "cwd="+dir+" args=compose up -d"; got != want {
		t.Errorf("no env files: %q, want %q", got, want)
	}

	os.WriteFile(filepath.Join(dir, ".helmo", "env"), []byte("APP_TAG=v1\n"), 0o644)
	if got, want := run(dir), "cwd="+dir+" args=compose --env-file .helmo/env up -d"; got != want {
		t.Errorf("only helmo env: %q, want %q", got, want)
	}

	os.WriteFile(filepath.Join(dir, ".env"), []byte("A=1\n"), 0o644)
	if got, want := run(dir), "cwd="+dir+" args=compose --env-file .env --env-file .helmo/env up -d"; got != want {
		t.Errorf("both: %q, want %q", got, want)
	}
}

func TestWrapperWithRealComposeCLI(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose not available")
	}
	dir := appDir(t)
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.yaml", "services:\n  web:\n    image: ghcr.io/org/app:${APP_TAG:?use .helmo/dc}\n    environment:\n      S: ${SECRET}\n")
	write(".env", "SECRET=x\n")
	write(".helmo/env", "APP_TAG=v2.0.0\n")
	if err := EnsureWrapper(dir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, ".helmo", WrapperName), "config", "--images")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "ghcr.io/org/app:v2.0.0" {
		t.Fatalf("err=%v out=%q", err, out)
	}
}
