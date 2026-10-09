package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	pageSize     = 1000
	maxPages     = 20
	maxJSONBytes = 4 << 20
)

// ErrUnauthorized means the registry refused access; the message says why
// without ever containing a secret.
var ErrUnauthorized = errors.New("registry authentication failed")

// ErrNotFound means the repository or reference does not exist (or the
// credentials cannot see it).
var ErrNotFound = errors.New("not found")

// Client talks to registries. Zero value works (no credentials).
type Client struct {
	HTTP *http.Client
	Auth *Auth
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func scheme(host string) string {
	h := host
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	switch h {
	case "localhost", "127.0.0.1", "[::1]":
		return "http"
	}
	return "https"
}

// Tags lists the tags of ref.Repository for which keep returns true (nil
// keeps all). Filtering while paging keeps memory small for repositories
// with many sha-* tags.
func (c *Client) Tags(ctx context.Context, ref Ref, keep func(string) bool) ([]string, error) {
	base := scheme(ref.apiHost()) + "://" + ref.apiHost()
	next := fmt.Sprintf("%s/v2/%s/tags/list?n=%d", base, ref.Repository, pageSize)

	var out []string
	for page := 0; next != "" && page < maxPages; page++ {
		resp, err := c.do(ctx, ref, http.MethodGet, next, "application/json")
		if err != nil {
			return nil, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(&body)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode tags: %w", err)
		}
		for _, t := range body.Tags {
			if keep == nil || keep(t) {
				out = append(out, t)
			}
		}
		next = nextLink(base, link)
	}
	return out, nil
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

func nextLink(base, header string) string {
	m := linkNext.FindStringSubmatch(header)
	if m == nil {
		return ""
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return ""
	}
	b, _ := url.Parse(base)
	return b.ResolveReference(u).String()
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// Digest returns the content digest the registry serves for ref.Repository
// at reference (a tag or a digest).
func (c *Client) Digest(ctx context.Context, ref Ref, reference string) (string, error) {
	base := scheme(ref.apiHost()) + "://" + ref.apiHost()
	resp, err := c.do(ctx, ref, http.MethodHead, fmt.Sprintf("%s/v2/%s/manifests/%s", base, ref.Repository, url.PathEscape(reference)), manifestAccept)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	d := resp.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", errors.New("registry returned no digest")
	}
	return d, nil
}

// do sends the request, answering a 401 challenge once (Bearer token or
// Basic). The returned response has status 2xx; the caller closes the body.
func (c *Client) do(ctx context.Context, ref Ref, method, target, accept string) (*http.Response, error) {
	resp, err := c.send(ctx, method, target, accept, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		authz, err := c.answer(ctx, ref, challenge)
		if err != nil {
			return nil, err
		}
		if resp, err = c.send(ctx, method, target, accept, authz); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, c.denied(ref)
		case http.StatusNotFound:
			return nil, fmt.Errorf("%s: %w (repository or tag missing, or no access)", ref.Name(), ErrNotFound)
		}
		return nil, fmt.Errorf("%s: registry answered %s", ref.Name(), resp.Status)
	}
	return resp, nil
}

func (c *Client) denied(ref Ref) error {
	if c.Auth.NeedsHelper(ref.Registry) {
		return fmt.Errorf("%w for %s: config.json uses a credential helper, which Helmo cannot run; log in so config.json holds the credentials", ErrUnauthorized, ref.Registry)
	}
	if _, ok := c.Auth.For(ref.Registry); !ok {
		return fmt.Errorf("%w for %s: no credentials in config.json (run docker login on the host)", ErrUnauthorized, ref.Registry)
	}
	return fmt.Errorf("%w for %s: credentials rejected", ErrUnauthorized, ref.Registry)
}

func (c *Client) send(ctx context.Context, method, target, accept, authz string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	return c.httpClient().Do(req)
}

// answer turns a WWW-Authenticate challenge into an Authorization header.
func (c *Client) answer(ctx context.Context, ref Ref, challenge string) (string, error) {
	scheme, params := parseChallenge(challenge)
	creds, haveCreds := c.Auth.For(ref.Registry)
	switch strings.ToLower(scheme) {
	case "basic":
		if !haveCreds {
			return "", c.denied(ref)
		}
		req, _ := http.NewRequest(http.MethodGet, "http://x", nil)
		req.SetBasicAuth(creds.User, creds.Pass)
		return req.Header.Get("Authorization"), nil
	case "bearer":
		token, err := c.fetchToken(ctx, ref, params, creds, haveCreds)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	}
	return "", c.denied(ref)
}

func (c *Client) fetchToken(ctx context.Context, ref Ref, params map[string]string, creds Creds, haveCreds bool) (string, error) {
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("registry challenge has no realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("bad token realm: %w", err)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	// The realm comes from the registry's answer; only send credentials to
	// it when it belongs to the same site as the registry.
	if haveCreds && sameSite(u.Hostname(), ref.apiHost()) {
		req.SetBasicAuth(creds.User, creds.Pass)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", sanitize(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", c.denied(ref)
		}
		return "", fmt.Errorf("token endpoint answered %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", errors.New("token endpoint returned no token")
}

// sameSite compares the last two labels (auth.docker.io ~ registry-1.docker.io).
// IP addresses and single-label hosts must match exactly.
func sameSite(a, b string) bool {
	a, b = hostOnly(a), hostOnly(b)
	if a == b {
		return true
	}
	return site(a) != "" && site(a) == site(b)
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func site(h string) string {
	parts := strings.Split(h, ".")
	if len(parts) < 2 {
		return ""
	}
	last := parts[len(parts)-1]
	if _, err := strconv.Atoi(last); err == nil { // IPv4 literal
		return ""
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// sanitize drops the URL from transport errors: the token URL may carry
// nothing secret, but request errors should never echo request details.
func sanitize(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

func parseChallenge(h string) (scheme string, params map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params = map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(rest, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	return scheme, params
}

// Source lists tags using the credentials currently stored in a Docker
// config.json. The file is read on every call, so a fresh "docker login"
// on the host takes effect without restarting Helmo.
type Source struct {
	ConfigPath string
	HTTP       *http.Client
}

func (s Source) client() (*Client, error) {
	a, err := LoadAuth(s.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("docker config: %w", err)
	}
	return &Client{HTTP: s.HTTP, Auth: a}, nil
}

func (s Source) Tags(ctx context.Context, ref Ref, keep func(string) bool) ([]string, error) {
	c, err := s.client()
	if err != nil {
		return nil, err
	}
	return c.Tags(ctx, ref, keep)
}

func (s Source) Digest(ctx context.Context, ref Ref, reference string) (string, error) {
	c, err := s.client()
	if err != nil {
		return "", err
	}
	return c.Digest(ctx, ref, reference)
}
