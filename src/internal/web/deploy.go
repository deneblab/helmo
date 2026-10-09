package web

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"helmo/internal/compose"
	"helmo/internal/deploy"
	"helmo/internal/registry"
	"helmo/internal/routing"
)

type jobJSON struct {
	Action   string `json:"action"`
	State    string `json:"state"`
	Phase    string `json:"phase"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Error    string `json:"error,omitempty"`
	Started  string `json:"started"`
	Finished string `json:"finished,omitempty"`
}

func toJSON(j deploy.Job) jobJSON {
	out := jobJSON{Action: j.Action, State: j.State, Phase: j.Phase, From: j.From, To: j.To, Error: j.Error,
		Started: j.Started.Format(time.RFC3339)}
	if !j.Finished.IsZero() {
		out.Finished = j.Finished.Format(time.RFC3339)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// deployError maps a deploy error to an HTTP answer. Messages that name only
// the registry or the request are shown; anything else is logged and hidden.
func deployError(w http.ResponseWriter, appID string, err error) {
	switch {
	case errors.Is(err, deploy.ErrBadTag):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, compose.ErrBusy), errors.Is(err, deploy.ErrSameVersion),
		errors.Is(err, deploy.ErrNoPrevious), errors.Is(err, deploy.ErrNoImage):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, registry.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, registry.ErrUnauthorized):
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		log.Printf("deploy app=%s: %v", appID, err)
		http.Error(w, "registry or docker unavailable", http.StatusBadGateway)
	}
}

// deploy: POST /_helmo/api/deploy  tag=v1.3.0 [dry_run=1]
// With dry_run it only reports what would change; otherwise it starts the
// deployment in the background and answers 202 (follow it with GET).
func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	tag := r.FormValue("tag")
	if r.FormValue("dry_run") == "1" {
		p, err := s.Deployer.Plan(r.Context(), info.App, tag)
		if err != nil {
			deployError(w, info.App.ID, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run": true,
			"service": p.Service,
			"image":   p.Ref.Name(),
			"current": p.Current.String(),
			"target":  p.Target.String(),
			"changes": p.Current != p.Target,
		})
		return
	}
	job, err := s.Deployer.Deploy(r.Context(), info.App, tag, info.Identity.Name)
	if err != nil {
		deployError(w, info.App.ID, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toJSON(job))
}

// rollback: POST /_helmo/api/rollback returns to the previous version.
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, err := s.Deployer.Rollback(r.Context(), info.App, info.Identity.Name)
	if err != nil {
		deployError(w, info.App.ID, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toJSON(job))
}

// deployStatus: GET /_helmo/api/deploy shows the latest deploy or rollback
// since Helmo started; 404 when there was none.
func (s *Server) deployStatus(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	job, found := s.Deployer.Last(info.App.ID)
	if !found {
		http.Error(w, "no deployment since Helmo started", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(job))
}

// history: GET /_helmo/api/history?limit=20 lists past changes, newest first.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			http.Error(w, "limit must be 1-200", http.StatusBadRequest)
			return
		}
		limit = n
	}
	entries, err := deploy.Recent(info.App.Dir, limit)
	if err != nil {
		log.Printf("history app=%s: %v", info.App.ID, err)
		http.Error(w, "cannot read history", http.StatusInternalServerError)
		return
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 { // newest first
		entries[i], entries[j] = entries[j], entries[i]
	}
	if entries == nil {
		entries = []deploy.Entry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
