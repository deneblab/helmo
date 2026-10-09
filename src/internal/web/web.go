// Package web holds the HTTP handlers of the panel. Handlers expect to run
// behind routing.Middleware, which puts the resolved app in the context.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"helmo/internal/compose"
	"helmo/internal/deploy"
	"helmo/internal/docker"
	"helmo/internal/routing"
)

const dockerTimeout = 10 * time.Second

// Server serves the panel for whichever app the request resolved to.
type Server struct {
	Docker   docker.DockerOps
	Compose  *compose.Manager
	Registry TagSource
	Deployer *deploy.Deployer
	Version  string // shown in the page footer

	mu      sync.Mutex
	streams map[string]int // open log streams per app
}

// Handler returns the panel routes, mounted under /_helmo/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.uiRoutes(mux)
	mux.HandleFunc("GET /_helmo/api/status", s.status)
	mux.HandleFunc("GET /_helmo/api/logs", s.logs)
	mux.HandleFunc("GET /_helmo/api/versions", s.versions)
	mux.HandleFunc("POST /_helmo/api/deploy", s.deploy)
	mux.HandleFunc("POST /_helmo/api/rollback", s.rollback)
	mux.HandleFunc("GET /_helmo/api/deploy", s.deployStatus)
	mux.HandleFunc("GET /_helmo/api/history", s.history)
	mux.HandleFunc("POST /_helmo/api/{op}", s.operate)
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

type opJSON struct {
	Op     string `json:"op"`
	Error  string `json:"error,omitempty"`
	Output string `json:"output,omitempty"`
}

// tidyOutput drops the indentation and padding docker compose puts around its
// progress lines (" Container x  Started"), so they line up in the page.
func tidyOutput(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// operate handles POST /_helmo/api/{start,stop,restart} for the resolved app.
func (s *Server) operate(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	op := compose.Op(r.PathValue("op"))
	res, err := s.Compose.Do(r.Context(), info.App, op)

	status, body := http.StatusOK, opJSON{Op: string(op), Output: tidyOutput(res.Output)}
	switch {
	case errors.Is(err, compose.ErrUnknownOp):
		http.NotFound(w, r)
		return
	case errors.Is(err, compose.ErrBusy):
		status, body = http.StatusConflict, opJSON{Op: string(op), Error: err.Error()}
	case err != nil:
		log.Printf("operation app=%s: %v", info.App.ID, err)
		status, body.Error = http.StatusInternalServerError, string(op)+" failed"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
