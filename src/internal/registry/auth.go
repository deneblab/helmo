package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Creds are the basic credentials for one registry.
type Creds struct{ User, Pass string }

// Auth holds the credentials from a Docker config.json (written by
// "docker login"). Credential helpers are not supported: they need
// executables that do not exist inside the Helmo container.
type Auth struct {
	entries     map[string]Creds
	helperHosts map[string]bool
	credsStore  string
}

type configFile struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// LoadAuth reads a config.json. A missing file means "no credentials".
func LoadAuth(path string) (*Auth, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Auth{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f configFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	a := &Auth{entries: map[string]Creds{}, helperHosts: map[string]bool{}, credsStore: f.CredsStore}
	for host, e := range f.Auths {
		c := Creds{User: e.Username, Pass: e.Password}
		if e.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				continue
			}
			c.User, c.Pass, _ = strings.Cut(string(raw), ":")
		}
		if c.User != "" {
			a.entries[normalizeHost(host)] = c
		}
	}
	for host := range f.CredHelpers {
		a.helperHosts[normalizeHost(host)] = true
	}
	return a, nil
}

// For returns the credentials for a registry (e.g. "ghcr.io", "docker.io").
func (a *Auth) For(registry string) (Creds, bool) {
	if a == nil {
		return Creds{}, false
	}
	c, ok := a.entries[normalizeHost(registry)]
	return c, ok
}

// NeedsHelper reports that the config delegates this registry to a
// credential helper, which Helmo cannot run.
func (a *Auth) NeedsHelper(registry string) bool {
	if a == nil {
		return false
	}
	if _, ok := a.entries[normalizeHost(registry)]; ok {
		return false
	}
	return a.helperHosts[normalizeHost(registry)] || a.credsStore != ""
}

// normalizeHost maps the many spellings of a registry key to one.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(h, "/v1/")
	h = strings.TrimSuffix(h, "/")
	switch h {
	case "index.docker.io", dockerHubAPI:
		return dockerHub
	}
	return h
}
