// Package web holds the HTTP handlers of the panel. Handlers expect to run
// behind routing.Middleware, which puts the resolved app in the context.
package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"helmo/internal/docker"
	"helmo/internal/routing"
)

const dockerTimeout = 10 * time.Second

// Server serves the panel for whichever app the request resolved to.
type Server struct {
	Docker docker.DockerOps
}

// Handler returns the panel routes, mounted under /_helmo/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_helmo/api/status", s.status)
	return mux
}

type containerJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
}

type statusJSON struct {
	App        string          `json:"app"`
	State      docker.State    `json:"state"`
	Containers []containerJSON `json:"containers"`
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dockerTimeout)
	defer cancel()

	cs, err := s.Docker.ProjectContainers(ctx, info.App.Dir)
	if err != nil {
		log.Printf("status app=%s: %v", info.App.ID, err)
		http.Error(w, "docker unavailable", http.StatusBadGateway)
		return
	}
	out := statusJSON{App: info.App.ID, State: docker.Summarize(cs), Containers: make([]containerJSON, 0, len(cs))}
	for _, c := range cs {
		out.Containers = append(out.Containers, containerJSON{
			ID: c.ID, Name: c.Name, Service: c.Service, Image: c.Image, State: c.State, Health: c.Health,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
