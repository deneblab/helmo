package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeApp(t *testing.T, root, id, yaml string) {
	t.Helper()
	dir := filepath.Join(root, id, ".helmo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	root := t.TempDir()
	writeApp(t, root, "cadastro", "enabled: true\nports: [8600]\n")
	writeApp(t, root, "blog", "enabled: true\nports: [8601, 8602]\nhosts: [\" Node.TS.net \"]\nservice: web\nhealth_timeout: 30s\n")
	writeApp(t, root, "off", "enabled: false\nports: [8700]\n")
	writeApp(t, root, "empty", "")
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}

	apps, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].ID != "blog" || apps[1].ID != "cadastro" {
		t.Fatalf("unexpected apps: %+v", apps)
	}
	blog, cad := apps[0], apps[1]
	if !reflect.DeepEqual(blog.Ports, []int{8601, 8602}) || blog.Service != "web" ||
		blog.HealthTimeout != 30*time.Second || !reflect.DeepEqual(blog.Hosts, []string{"node.ts.net"}) {
		t.Errorf("blog: %+v", blog)
	}
	if cad.HealthTimeout != 60*time.Second || cad.Dir != filepath.Join(root, "cadastro") {
		t.Errorf("cadastro defaults: %+v", cad)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"missing ports", map[string]string{"a": "enabled: true\n"}, "ports is required"},
		{"port out of range", map[string]string{"a": "enabled: true\nports: [70000]\n"}, "out of range"},
		{"port twice", map[string]string{"a": "enabled: true\nports: [80, 80]\n"}, "listed twice"},
		{"bad id", map[string]string{"Bad_ID": "enabled: true\nports: [80]\n"}, "invalid id"},
		{"bad timeout", map[string]string{"a": "enabled: true\nports: [80]\nhealth_timeout: soon\n"}, "health_timeout"},
		{"unknown key", map[string]string{"a": "enabled: true\nports: [80]\nbogus: 1\n"}, "bogus"},
		{"port shared", map[string]string{
			"a": "enabled: true\nports: [8600]\n",
			"b": "enabled: true\nports: [8600]\n",
		}, "claimed by both"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for id, y := range tt.files {
				writeApp(t, root, id, y)
			}
			_, err := Load(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingRoot(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want error")
	}
}
