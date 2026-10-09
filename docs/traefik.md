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

## The global router

Helmo carries the labels itself ([compose.example.yaml](../compose.example.yaml)):

```yaml
traefik.enable: "true"
traefik.http.routers.helmo.rule: PathPrefix(`/_helmo`)
traefik.http.routers.helmo.priority: "10000"
traefik.http.routers.helmo.service: helmo
traefik.http.services.helmo.loadbalancer.server.port: "8080"
```

- There is **no `entryPoints` label**, so the router listens on all entrypoints.
  This is what makes one Helmo serve every app.
- The **priority** must be higher than the routers of the apps. Their rules
  often match every path, and the longer rule would otherwise win.
- The router belongs to Helmo, not to an app, so the panel keeps working when an
  app is stopped or removed. Its certificate, if you use TLS, must not depend on
  the app either.
- Helmo must be on a network Traefik can reach. Do not publish port 8080.

If your apps are defined in Traefik's file provider rather than with labels,
the same router can be written there:

```yaml
http:
  routers:
    helmo:
      rule: PathPrefix(`/_helmo`)
      priority: 10000
      service: helmo
  services:
    helmo:
      loadBalancer:
        servers:
          - url: http://helmo:8080
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
