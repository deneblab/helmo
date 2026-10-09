# Traefik setup

Helmo expects Traefik to run with **one entrypoint per app port** and **one
global router** that sends `/_helmo` to Helmo on every entrypoint.

## Entrypoints

Each app is reached on its own port, for example `node.example:8600`. Bind the
entrypoints to the Tailscale address so they are not reachable from anywhere else:

```yaml
# traefik.yml (static configuration)
entryPoints:
  app-cadastro:
    address: "100.64.0.10:8600"     # the node's Tailscale IP
  app-blog:
    address: "100.64.0.10:8601"

providers:
  docker:
    exposedByDefault: false
    network: traefik
```

The ports you list in each `.helmo/app.yaml` must match these entrypoints.

## Helmo's router

Which variant applies depends on the providers Traefik uses. The install script
asks the Traefik API (`/api/overview`) and picks the variant itself; to check by
hand:

```sh
curl -s http://127.0.0.1:8081/api/overview | grep -o '"providers":[^]]*]'   # 8081: the host port of Traefik's API
```

### Docker provider: labels

Helmo carries the labels itself ([compose.example.yaml](../compose.example.yaml)); the
install script writes the same file:

```yaml
traefik.enable: "true"
traefik.http.routers.helmo.rule: PathPrefix(`/_helmo`)
traefik.http.routers.helmo.priority: "10000"
traefik.http.routers.helmo.service: helmo
traefik.http.services.helmo.loadbalancer.server.port: "8080"
```

- There is **no `entryPoints` label**, so the router listens on all entrypoints.
  This is what makes one Helmo serve every app without changing its labels.
  On ports that belong to no app Helmo answers 404.
- The **priority** must be higher than the routers of the apps. Their rules
  often match every path, and the longer rule would otherwise win.
- The router belongs to Helmo, not to an app, so the panel keeps working when an
  app is stopped or removed.
- A router without TLS serves plain HTTP only. If the apps' entrypoints are
  HTTPS, add a second router with `tls` (see the File variant below).
- Helmo must be on a network Traefik can reach. Do not publish port 8080.

### File provider: dynamic configuration

When Traefik does not use the Docker provider it ignores labels, and Helmo's
routers go into its dynamic configuration file next to the apps' routers. The
install script then writes `compose.yaml` without labels and prints the routers
for you, generated from the Traefik API by `helmo -traefik-config`; the `app`
command prints them again with the new app's entrypoint. For example:

```yaml
http:
  routers:
    helmo-tls:
      rule: PathPrefix(`/_helmo`)
      priority: 10000
      entryPoints: ["extraction-manager-develop"]   # the entrypoints of the apps' ports
      service: helmo
      tls:                                           # the same as the app's router
        domains:
          - main: "node.example.ts.net"
  services:
    helmo:
      loadBalancer:
        servers:
          - url: "http://helmo:8080"
```

- **Limit `entryPoints` to the apps' entrypoints.** Without them the router
  also takes `/_helmo` on every other port, such as 80, 443 and the dashboard.
  Add the entrypoint of each new app.
- **TLS like the app.** An entrypoint that serves HTTPS needs a router with
  `tls`, with the same certificate settings as the app's router; a router
  without `tls` would only match plain HTTP there. Entrypoints with plain HTTP
  get a router without `tls`.
- **Address Helmo by its Compose service name** (`helmo`), which is a DNS alias
  on the shared network. The container name (`helmo-helmo-1`) depends on the
  project name.
- Traefik reloads the file by itself when the provider has `watch: true`;
  check with `curl -s http://127.0.0.1:8081/api/http/routers/helmo-tls@file`.

To generate the routers by hand (with the Helmo version you run):

```sh
docker run --rm --network host ghcr.io/deneblab/helmo:<version> -traefik-config -traefik-api http://127.0.0.1:8081 -ports 8102,8085
```

## How Helmo recognises the app

Traefik sets `X-Forwarded-Port` to the port of the entrypoint that accepted the
connection, overwriting any value the client sent. Helmo takes the app from that
header. The port in `Host`, when there is one, must agree with it, and when
`hosts` is set in `app.yaml` the host name must be listed. Anything else gets 404.

This is why:

- Helmo must only be reachable through Traefik. A client that could talk to
  Helmo directly could send any `X-Forwarded-Port`.
- `forwardedHeaders.trustedIPs` should stay empty (the default) on these
  entrypoints. If another proxy sits in front of Traefik, make sure it forwards
  the real port and that you trust only it.

## Check it

From a machine on the tailnet:

```sh
curl -sI http://node.example:8600/_helmo/        # 200, text/html
curl -sI http://node.example:8601/_helmo/        # 200, the other app
curl -sI http://node.example:8600/               # the app itself, not Helmo
docker stop cadastro-web-1; curl -sI http://node.example:8600/_helmo/   # still 200
```

Helmo's own log prints one `audit` line for each change made through the panel.
