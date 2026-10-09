package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"helmo/internal/config"
	"helmo/internal/registry"
	"helmo/internal/routing"
)

const (
	registryTimeout = 20 * time.Second
	defaultVersions = 20
	maxVersions     = 100
)

// TagSource lists registry tags; *registry.Source implements it.
type TagSource interface {
	Tags(ctx context.Context, ref registry.Ref, keep func(string) bool) ([]string, error)
}

type versionsJSON struct {
	Image     string   `json:"image"`
	Current   string   `json:"current,omitempty"`
	Tags      []string `json:"tags"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated,omitempty"`
}

// versions lists the deployable versions (X.Y.Z or vX.Y.Z tags, newest
// first) of the app's service image:
//
//	GET /_helmo/api/versions?service=web&limit=20
func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	limit := defaultVersions
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxVersions {
			http.Error(w, "limit must be 1-"+strconv.Itoa(maxVersions), http.StatusBadRequest)
			return
		}
		limit = n
	}
	service := q.Get("service")
	if service == "" {
		service = info.App.Service
	}

	ctx, cancel := context.WithTimeout(r.Context(), registryTimeout)
	defer cancel()

	image, err := s.serviceImage(ctx, info.App, service)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	ref, err := registry.ParseRef(image)
	if err != nil {
		http.Error(w, "invalid image reference", http.StatusConflict)
		return
	}

	tags, err := s.Registry.Tags(ctx, ref, registry.IsVersionTag)
	if err != nil {
		log.Printf("versions app=%s image=%s: %v", info.App.ID, ref.Name(), err)
		msg := "registry unavailable"
		if errors.Is(err, registry.ErrUnauthorized) {
			msg = err.Error() // names the registry, never a secret
		}
		http.Error(w, msg, http.StatusBadGateway)
		return
	}
	sorted := registry.SortVersions(tags)
	out := versionsJSON{Image: ref.Name(), Current: ref.Tag, Total: len(sorted), Tags: sorted}
	if len(sorted) > limit {
		out.Tags, out.Truncated = sorted[:limit], true
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// serviceImage asks Compose what the service runs (authoritative, includes
// APP_TAG); when that is impossible, for example before .helmo/env exists,
// it falls back to the image of an existing container.
func (s *Server) serviceImage(ctx context.Context, app config.App, service string) (string, error) {
	img, err := s.Compose.ServiceImage(ctx, app, service)
	if err == nil {
		return img, nil
	}
	cs, derr := s.Docker.ProjectContainers(ctx, app.Dir)
	if derr == nil {
		if c, status, _ := pickContainer(cs, service); status == 0 && c.Image != "" && !strings.HasPrefix(c.Image, "sha256:") {
			return c.Image, nil
		}
	}
	return "", errors.New("cannot determine the image: define APP_TAG in .helmo/env or start the app once")
}
