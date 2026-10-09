// Package registry reads image tags and digests from OCI/Docker Registry v2
// servers (GHCR, Docker Hub, self-hosted), public or private.
package registry

import (
	"errors"
	"fmt"
	"strings"
)

const (
	dockerHub    = "docker.io"
	dockerHubAPI = "registry-1.docker.io"
)

// Ref is an image reference split into its parts.
type Ref struct {
	Registry   string // e.g. ghcr.io, docker.io, localhost:5000
	Repository string // e.g. org/app, library/nginx
	Tag        string // empty when the reference has none (or is digest-pinned)
	Digest     string // sha256:... when pinned
}

// ParseRef parses references like ghcr.io/org/app:v1.2.3, org/app,
// nginx, localhost:5000/app:1.0 or ghcr.io/org/app@sha256:abc....
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("empty image reference")
	}
	var r Ref
	if i := strings.Index(s, "@"); i >= 0 {
		r.Digest, s = s[i+1:], s[:i]
	}

	first, rest, hasSlash := strings.Cut(s, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, s = strings.ToLower(first), rest
	} else {
		r.Registry = dockerHub
	}

	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "/") {
		r.Tag, s = s[i+1:], s[:i]
	}
	r.Repository = s
	if r.Repository == "" {
		return Ref{}, fmt.Errorf("image reference %q has no repository", s)
	}
	if r.Registry == dockerHub && !strings.Contains(r.Repository, "/") {
		r.Repository = "library/" + r.Repository
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Name is the reference without tag or digest.
func (r Ref) Name() string { return r.Registry + "/" + r.Repository }

func (r Ref) apiHost() string {
	if r.Registry == dockerHub {
		return dockerHubAPI
	}
	return r.Registry
}
