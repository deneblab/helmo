package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	apiVersion   = "v1.41"
	maxBodyBytes = 16 << 20
)

// Client is a minimal Docker Engine API client over a unix socket or TCP
// (for example a docker-socket-proxy). It avoids the full Docker SDK to keep
// the binary and memory small.
type Client struct {
	http *http.Client
	base string
}

// NewClient accepts unix:///path/to.sock, tcp://host:port or http://host:port.
func NewClient(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}
		return &Client{http: &http.Client{Transport: tr}, base: "http://docker"}, nil
	case "tcp", "http":
		return &Client{http: &http.Client{}, base: "http://" + u.Host}, nil
	}
	return nil, fmt.Errorf("docker host %q: unsupported scheme %q", host, u.Scheme)
}

// open sends a GET and returns the response with a 200 status; the caller
// closes the body.
func (c *Client) open(ctx context.Context, path string, query url.Values) (*http.Response, error) {
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("docker %s: %s: %s", path, resp.Status, msg)
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	resp, err := c.open(ctx, path, query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.get(ctx, "/_ping", nil)
	return err
}

type listedContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

func (c *Client) ProjectContainers(ctx context.Context, dir string) ([]Container, error) {
	filters, err := json.Marshal(map[string][]string{"label": {LabelWorkingDir + "=" + dir}})
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, "/"+apiVersion+"/containers/json", url.Values{
		"all":     {"1"},
		"filters": {string(filters)},
	})
	if err != nil {
		return nil, err
	}
	var listed []listedContainer
	if err := json.Unmarshal(body, &listed); err != nil {
		return nil, fmt.Errorf("decode containers: %w", err)
	}
	out := make([]Container, 0, len(listed))
	for _, l := range listed {
		name := ""
		if len(l.Names) > 0 {
			name = strings.TrimPrefix(l.Names[0], "/")
		}
		id := l.ID
		if len(id) > 12 {
			id = id[:12]
		}
		out = append(out, Container{
			ID:      id,
			Name:    name,
			Service: l.Labels[LabelService],
			Image:   l.Image,
			State:   l.State,
			Health:  healthFromStatus(l.Status),
		})
	}
	return out, nil
}

func (c *Client) isTTY(ctx context.Context, id string) (bool, error) {
	body, err := c.get(ctx, "/"+apiVersion+"/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return false, err
	}
	var v struct {
		Config struct{ Tty bool }
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false, fmt.Errorf("decode container: %w", err)
	}
	return v.Config.Tty, nil
}

func (c *Client) StreamLogs(ctx context.Context, id string, opts LogOptions, emit func(LogLine) error) error {
	tty, err := c.isTTY(ctx, id)
	if err != nil {
		return err
	}
	q := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"timestamps": {"1"},
		"tail":       {strconv.Itoa(opts.Tail)},
	}
	if opts.Follow {
		q.Set("follow", "1")
	}
	resp, err := c.open(ctx, "/"+apiVersion+"/containers/"+url.PathEscape(id)+"/logs", q)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readLogs(resp.Body, tty, emit)
}
