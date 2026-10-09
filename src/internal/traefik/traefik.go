// Package traefik reads the API of a running Traefik and writes the dynamic
// configuration Helmo needs when Traefik uses the File provider, where the
// labels in Helmo's compose file are ignored.
package traefik

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// DefaultServiceURL is Helmo as Traefik reaches it: the Compose service name
// is a DNS alias on the shared network, unlike the container name.
const DefaultServiceURL = "http://helmo:8080"

type entryPoint struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type router struct {
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	EntryPoints []string `json:"entryPoints"`
	TLS         *TLS     `json:"tls"`
}

// TLS is the part of a router's TLS configuration that is copied to Helmo's
// router, so the panel is served with the same certificate as the app.
type TLS struct {
	CertResolver string   `json:"certResolver,omitempty"`
	Domains      []Domain `json:"domains,omitempty"`
	Options      string   `json:"options,omitempty"`
}

type Domain struct {
	Main string   `json:"main"`
	SANs []string `json:"sans,omitempty"`
}

// Client talks to the Traefik API, e.g. http://127.0.0.1:8081.
type Client struct {
	HTTP *http.Client
	API  string
}

func (c Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.API, "/")+path, nil)
	if err != nil {
		return err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("traefik %s: %s", path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// route is one router Helmo needs: the entrypoints that share a TLS setup.
type route struct {
	entryPoints []string
	tls         *TLS
}

// Config returns the dynamic configuration (YAML) that routes /_helmo on the
// entrypoints of the given ports to serviceURL. Entrypoints whose routers use
// TLS get a TLS router with the same settings as the first such router.
func (c Client) Config(ctx context.Context, ports []int, serviceURL string) (string, error) {
	var eps []entryPoint
	if err := c.get(ctx, "/api/entrypoints", &eps); err != nil {
		return "", err
	}
	var routers []router
	if err := c.get(ctx, "/api/http/routers?per_page=1000", &routers); err != nil {
		return "", err
	}
	sort.Slice(routers, func(i, j int) bool { return routers[i].Name < routers[j].Name })

	byPort := map[int]string{}
	for _, ep := range eps {
		if p, ok := port(ep.Address); ok {
			if _, seen := byPort[p]; !seen {
				byPort[p] = ep.Name
			}
		}
	}

	var missing []int
	groups := map[string]*route{}
	for _, p := range ports {
		name, ok := byPort[p]
		if !ok {
			missing = append(missing, p)
			continue
		}
		tls := tlsOf(routers, name)
		key := ""
		if tls != nil {
			b, _ := json.Marshal(tls)
			key = string(b)
		}
		g := groups[key]
		if g == nil {
			g = &route{tls: tls}
			groups[key] = g
		}
		if !contains(g.entryPoints, name) {
			g.entryPoints = append(g.entryPoints, name)
		}
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys) // "" (plain HTTP) first

	var b strings.Builder
	b.WriteString("# Helmo: add to the dynamic configuration of Traefik (File provider).\n")
	for _, p := range missing {
		fmt.Fprintf(&b, "# No Traefik entrypoint listens on port %d; add one, or check the port.\n", p)
	}
	b.WriteString("http:\n  routers:\n")
	tlsCount := 0
	for _, k := range keys {
		g := groups[k]
		name := "helmo"
		if g.tls != nil {
			tlsCount++
			name = "helmo-tls"
			if tlsCount > 1 {
				name += "-" + strconv.Itoa(tlsCount)
			}
		}
		fmt.Fprintf(&b, "    %s:\n", name)
		b.WriteString("      rule: PathPrefix(`/_helmo`)\n")
		b.WriteString("      priority: 10000\n")
		fmt.Fprintf(&b, "      entryPoints: [%s]\n", quoteList(g.entryPoints))
		b.WriteString("      service: helmo\n")
		if g.tls != nil {
			writeTLS(&b, g.tls)
		}
	}
	if len(keys) == 0 {
		b.WriteString("    # no app ports yet: run the install script's app command first\n")
	}
	b.WriteString("  services:\n    helmo:\n      loadBalancer:\n        servers:\n")
	fmt.Fprintf(&b, "          - url: %s\n", strconv.Quote(serviceURL))
	return b.String(), nil
}

// Providers returns the configuration providers Traefik uses, e.g. [Docker File].
func (c Client) Providers(ctx context.Context) ([]string, error) {
	var o struct {
		Providers []string `json:"providers"`
	}
	if err := c.get(ctx, "/api/overview", &o); err != nil {
		return nil, err
	}
	return o.Providers, nil
}

// tlsOf returns the TLS settings of the first router (by name) on the
// entrypoint that has any, ignoring Traefik's own and Helmo's routers.
func tlsOf(routers []router, entryPoint string) *TLS {
	for _, r := range routers {
		if r.TLS == nil || r.Provider == "internal" || strings.HasPrefix(r.Name, "helmo") {
			continue
		}
		if contains(r.EntryPoints, entryPoint) {
			return r.TLS
		}
	}
	return nil
}

func writeTLS(b *strings.Builder, t *TLS) {
	if t.CertResolver == "" && len(t.Domains) == 0 && t.Options == "" {
		b.WriteString("      tls: {}\n")
		return
	}
	b.WriteString("      tls:\n")
	if t.CertResolver != "" {
		fmt.Fprintf(b, "        certResolver: %s\n", strconv.Quote(t.CertResolver))
	}
	if t.Options != "" {
		fmt.Fprintf(b, "        options: %s\n", strconv.Quote(t.Options))
	}
	if len(t.Domains) > 0 {
		b.WriteString("        domains:\n")
		for _, d := range t.Domains {
			fmt.Fprintf(b, "          - main: %s\n", strconv.Quote(d.Main))
			if len(d.SANs) > 0 {
				fmt.Fprintf(b, "            sans: [%s]\n", quoteList(d.SANs))
			}
		}
	}
}

// port extracts the port of an entrypoint address such as ":8102",
// "0.0.0.0:8102", "[::]:8102" or ":8102/tcp".
func port(addr string) (int, bool) {
	addr, _, _ = strings.Cut(addr, "/")
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0, false
	}
	p, err := strconv.Atoi(addr[i+1:])
	return p, err == nil && p > 0 && p < 65536
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
