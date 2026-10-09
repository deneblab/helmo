package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetAndGetVar(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")

	if v, ok, err := GetVar(p, "A"); v != "" || ok || err != nil {
		t.Fatalf("missing file: %q %v %v", v, ok, err)
	}
	if err := SetVar(p, "APP_TAG", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "APP_TAG=v1.0.0\n" {
		t.Fatalf("created: %q", b)
	}

	os.WriteFile(p, []byte("# note\nA=1\nexport APP_TAG=\"old\"\n\nB='two words'\nAPP_TAG=dup\n"), 0o600)
	os.Chmod(p, 0o600) // WriteFile keeps the mode of an existing file
	if err := SetVar(p, "APP_TAG", "v2.0.0@sha256:"+strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	want := "# note\nA=1\nAPP_TAG=v2.0.0@sha256:" + strings.Repeat("a", 64) + "\n\nB='two words'\n"
	if b, _ := os.ReadFile(p); string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if v, ok, _ := GetVar(p, "B"); !ok || v != "two words" {
		t.Errorf("quoted value: %q", v)
	}
	if entries, _ := os.ReadDir(filepath.Dir(p)); len(entries) != 1 {
		t.Errorf("temp file left: %v", entries)
	}

	if err := UnsetVar(p, "APP_TAG"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := GetVar(p, "APP_TAG"); ok {
		t.Error("still set")
	}
	if v, ok, _ := GetVar(p, "A"); !ok || v != "1" {
		t.Error("other lines lost")
	}
	if err := UnsetVar(filepath.Join(t.TempDir(), "none"), "A"); err != nil {
		t.Errorf("missing file: %v", err)
	}
}

func TestSetVarRefusesUnsafeValues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "env")
	for _, v := range []string{"", "a b", "a\nB=1", "$(x)", "a\"b", "`x`", "a\\b"} {
		if err := SetVar(p, "APP_TAG", v); err == nil {
			t.Errorf("%q must be refused", v)
		}
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("file created for refused value")
	}
}
