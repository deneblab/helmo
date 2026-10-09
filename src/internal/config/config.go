// Package config loads per-application Helmo settings from
// <apps dir>/<id>/.helmo/app.yaml.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	appFile              = "app.yaml"
	helmoDir             = ".helmo"
	defaultHealthTimeout = 60 * time.Second
)

var idPattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// App is one managed Compose project living in <apps dir>/<ID>.
type App struct {
	ID            string
	Dir           string
	Ports         []int
	Hosts         []string
	Service       string
	HealthTimeout time.Duration
}

type appFileYAML struct {
	Enabled       bool     `yaml:"enabled"`
	Ports         []int    `yaml:"ports"`
	Hosts         []string `yaml:"hosts"`
	Service       string   `yaml:"service"`
	HealthTimeout string   `yaml:"health_timeout"`
}

// Load reads every <root>/*/.helmo/app.yaml and returns the enabled apps
// sorted by ID. A directory without the file is ignored; an invalid file,
// an invalid ID or a port claimed by two apps is an error.
func Load(root string) ([]App, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read apps dir: %w", err)
	}

	var apps []App
	portOwner := map[int]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		path := filepath.Join(root, id, helmoDir, appFile)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("app %q: %w", id, err)
		}
		app, enabled, err := parse(id, filepath.Join(root, id), data)
		if err != nil {
			return nil, fmt.Errorf("app %q: %w", id, err)
		}
		if !enabled {
			continue
		}
		for _, p := range app.Ports {
			if other, taken := portOwner[p]; taken {
				return nil, fmt.Errorf("port %d is claimed by both %q and %q", p, other, id)
			}
			portOwner[p] = id
		}
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return apps, nil
}

func parse(id, dir string, data []byte) (App, bool, error) {
	if !idPattern.MatchString(id) {
		return App{}, false, fmt.Errorf("invalid id, want %s", idPattern)
	}
	var f appFileYAML
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return App{}, false, fmt.Errorf("parse %s: %w", appFile, err)
	}
	if !f.Enabled {
		return App{}, false, nil
	}
	if len(f.Ports) == 0 {
		return App{}, false, errors.New("ports is required")
	}
	seen := map[int]bool{}
	for _, p := range f.Ports {
		if p < 1 || p > 65535 {
			return App{}, false, fmt.Errorf("port %d out of range", p)
		}
		if seen[p] {
			return App{}, false, fmt.Errorf("port %d listed twice", p)
		}
		seen[p] = true
	}
	timeout := defaultHealthTimeout
	if f.HealthTimeout != "" {
		d, err := time.ParseDuration(f.HealthTimeout)
		if err != nil || d <= 0 {
			return App{}, false, fmt.Errorf("invalid health_timeout %q", f.HealthTimeout)
		}
		timeout = d
	}
	hosts := make([]string, 0, len(f.Hosts))
	for _, h := range f.Hosts {
		hosts = append(hosts, strings.ToLower(strings.TrimSpace(h)))
	}
	return App{
		ID:            id,
		Dir:           dir,
		Ports:         f.Ports,
		Hosts:         hosts,
		Service:       f.Service,
		HealthTimeout: timeout,
	}, true, nil
}
