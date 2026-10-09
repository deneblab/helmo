package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"helmo/internal/docker"
	"helmo/internal/routing"
)

const (
	defaultTail    = 100
	maxTail        = 1000
	maxLogStreams  = 5 // concurrent log streams per app
	logChannelSize = 64
	heartbeat      = 15 * time.Second
	writeTimeout   = 15 * time.Second
)

type logEvent struct {
	Time   string `json:"ts,omitempty"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// logs streams container output as Server-Sent Events:
//
//	GET /_helmo/api/logs?service=web&tail=100&follow=1
//
// The container is always picked from the resolved app's own Compose
// project; the client never supplies a container ID. Lines pass through a
// small channel, so a slow client slows Docker reading instead of growing
// memory.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	info, ok := routing.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	tail := defaultTail
	if v := q.Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxTail {
			http.Error(w, "tail must be 0-"+strconv.Itoa(maxTail), http.StatusBadRequest)
			return
		}
		tail = n
	}
	follow := q.Get("follow") != "0"

	lookupCtx, cancel := context.WithTimeout(r.Context(), dockerTimeout)
	cs, err := s.Docker.ProjectContainers(lookupCtx, info.App.Dir)
	cancel()
	if err != nil {
		log.Printf("logs app=%s: %v", info.App.ID, err)
		http.Error(w, "docker unavailable", http.StatusBadGateway)
		return
	}
	service := q.Get("service")
	if service == "" {
		service = info.App.Service
	}
	c, status, msg := pickContainer(cs, service)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}

	if !s.acquireStream(info.App.ID) {
		http.Error(w, "too many log streams for this app", http.StatusTooManyRequests)
		return
	}
	defer s.releaseStream(info.App.ID)

	rc := http.NewResponseController(w)
	write := func(text string) error {
		rc.SetWriteDeadline(time.Now().Add(writeTimeout)) // unsupported on some writers; fine
		if _, err := io.WriteString(w, text); err != nil {
			return err
		}
		return rc.Flush()
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := write(": ok\n\n"); err != nil {
		return
	}

	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	lines := make(chan docker.LogLine, logChannelSize)
	errc := make(chan error, 1)
	go func() {
		errc <- s.Docker.StreamLogs(ctx, c.ID, docker.LogOptions{Tail: tail, Follow: follow}, func(l docker.LogLine) error {
			select {
			case lines <- l:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(lines)
	}()

	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case l, open := <-lines:
			if !open {
				end := logEvent{}
				if err := <-errc; err != nil && ctx.Err() == nil {
					log.Printf("logs app=%s service=%s: %v", info.App.ID, c.Service, err)
					end.Text = "log stream failed"
				}
				data, _ := json.Marshal(end)
				write("event: end\ndata: " + string(data) + "\n\n")
				return
			}
			ev := logEvent{Stream: l.Stream, Text: l.Text}
			if !l.Time.IsZero() {
				ev.Time = l.Time.UTC().Format(time.RFC3339Nano)
			}
			data, _ := json.Marshal(ev)
			if err := write("data: " + string(data) + "\n\n"); err != nil {
				return
			}
		case <-tick.C:
			if err := write(": ping\n\n"); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// pickContainer chooses the container for a service name; an empty name is
// allowed only when the project has exactly one container. A non-zero status
// means the request must fail with msg.
func pickContainer(cs []docker.Container, service string) (docker.Container, int, string) {
	if service != "" {
		for _, c := range cs {
			if c.Service == service {
				return c, 0, ""
			}
		}
		return docker.Container{}, http.StatusNotFound, fmt.Sprintf("no container for service %q", service)
	}
	switch len(cs) {
	case 0:
		return docker.Container{}, http.StatusNotFound, "app has no containers"
	case 1:
		return cs[0], 0, ""
	}
	return docker.Container{}, http.StatusBadRequest, "several services: pass ?service=<name>"
}

func (s *Server) acquireStream(appID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = map[string]int{}
	}
	if s.streams[appID] >= maxLogStreams {
		return false
	}
	s.streams[appID]++
	return true
}

func (s *Server) releaseStream(appID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams[appID]--; s.streams[appID] <= 0 {
		delete(s.streams, appID)
	}
}
