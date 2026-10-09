# Helmo

[![Release](https://img.shields.io/github/v/release/deneblab/helmo?label=image)](https://github.com/deneblab/helmo/releases/latest)
[![image](https://github.com/deneblab/helmo/actions/workflows/image.yml/badge.svg?branch=production)](https://github.com/deneblab/helmo/actions/workflows/image.yml)

A small self-hosted panel for web apps that run with Docker Compose behind
Traefik. One Helmo process serves many apps, each on its own Traefik port, and
needs no change in the apps' code.

For each app it can:

- show the state of its containers, also when they are stopped;
- start, stop and restart the whole Compose project;
- stream the container logs live;
- list the versions available in the registry (GHCR, Docker Hub or a private
  registry);
- deploy a version and roll back, keeping volumes, networks, variables and
  labels, with an automatic rollback when the new version is not healthy.

The panel of an app is at `http://<host>:<app port>/_helmo/`.

**There is no login.** Helmo is meant to be reachable only from a private
network such as a Tailscale tailnet. Anyone who can open the page can restart or
redeploy the app. Read [Security](#security) before exposing it anywhere else.

## How it works

```
browser ──► Traefik (one entrypoint per app port) ──► the app          (any other path)
                                                  └─► Helmo            (/_helmo, all entrypoints)
                                                       │
                                                       ├─ docker.sock         state, logs
                                                       ├─ docker compose      pull, up -d
                                                       └─ /srv/apps/<id>/     .helmo/app.yaml, .helmo/env
```

- A Traefik router of Helmo's own sends every `/_helmo` request to Helmo, on the
  apps' ports, also while the app itself is stopped. It comes from labels or,
  with the File provider, from Traefik's dynamic configuration. See
  [docs/traefik.md](docs/traefik.md).
- Helmo knows which app a request is for from the port Traefik accepted it on
  (`X-Forwarded-Port`), checked against the apps' `.helmo/app.yaml`.
- The Compose file is the source of truth. Helmo changes only one variable,
  `APP_TAG`, in `/srv/apps/<id>/.helmo/env`, written as `tag@sha256:digest` so a
  rollback returns to exactly the same image, then runs `docker compose pull`
  and `up -d` for that one service.
- There is no database. Deployment history is `.helmo/history.jsonl` in each app.

## Quick start

Needs Docker with the Compose plugin, `curl`, and a running Traefik that is set up
as in [docs/traefik.md](docs/traefik.md).

1. Install Helmo from the directory where it should live; the script works in the
   current one:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/deneblab/helmo/production/scripts/install.sh | sh -s -- helmo
   ```

   It finds the newest image version, asks for the apps directory (default
   `/srv/apps`), writes `compose.yaml` and `.env` here and starts Helmo. It
   creates an empty `docker-config/config.json` for registry credentials (see
   [docs/app-setup.md](docs/app-setup.md#5-registry-credentials)). When Traefik
   uses the File provider instead of Docker labels, it prints the routers to add
   to Traefik's dynamic configuration. Helmo runs as the owner of the apps
   directory, so it can read the apps' `.env` files. Pass `--version X.Y.Z`,
   `--apps-dir DIR`, `--user UID:GID`, `--network NAME`, `--docker-config FILE`,
   `--traefik-api URL` and `--yes` to skip the questions.

2. Prepare each app from its own directory (the one with its Compose file):

   ```sh
   curl -fsSL https://raw.githubusercontent.com/deneblab/helmo/production/scripts/install.sh | sh -s -- app --port 8600
   ```

   Without `--port` it lists the ports Traefik publishes that no other app uses
   and asks which one is this app's.

   It writes `.helmo/app.yaml`, tells you what to change in the app's Compose
   file (it never edits it) and how to give Helmo ownership of `.helmo`. Details
   and the manual steps are in [docs/app-setup.md](docs/app-setup.md).

3. Open `http://<host>:<app port>/_helmo/`.

The script is not tied to a release: it is read from the `production` branch,
whose every commit is a release. To install from a fixed version of the script,
replace `production` in the URL with a commit hash. The script writes only
Helmo's own files and asks before it overwrites one that differs; with `--yes` it
keeps the existing file. Read it before you run it, as with any `curl | sh`.

## Configuration

Helmo is configured through the environment and the apps' own files.

| Variable | Default | Meaning |
|---|---|---|
| `HELMO_APPS_DIR` | `/srv/apps` | Directory with one subdirectory per app. |
| `HELMO_LISTEN` | `:8080` | Address Helmo listens on. Do not publish it. |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | `unix://` or `tcp://` Docker endpoint. |
| `DOCKER_CONFIG` | `~/.docker` | Directory with `config.json` (registry credentials). |

Per app, `/srv/apps/<id>/.helmo/app.yaml`:

```yaml
enabled: true
ports: [8600]          # the Traefik ports of this app (required)
hosts: [node.example]  # optional: only these host names are accepted
service: web           # the Compose service Helmo versions (optional when there is one)
health_timeout: 60s    # how long a new version gets to become healthy (default 60s)
```

An app is managed only when it has this file with `enabled: true`. A directory
name must match `^[a-z0-9-]+$`, and a port can belong to one app only.

## API

All under `/_helmo/api`, JSON unless noted. State-changing requests need to
come from the page itself: they must carry browser same-origin headers, and
plain `curl` is refused with 403. This protects against other sites driving your
browser; it is not authentication.

| Request | Does |
|---|---|
| `GET /status` | State and containers of the app. |
| `POST /start`, `/stop`, `/restart` | Act on the whole Compose project. |
| `GET /versions?limit=20` | Versions `X.Y.Z` / `vX.Y.Z` from the registry, newest first. |
| `POST /deploy` `tag=v1.3.0` | Start a deployment (202), or with `dry_run=1` only show the change. |
| `GET /deploy` | Progress of the latest deployment or rollback. |
| `POST /rollback` | Return to the version before the current one. |
| `GET /history?limit=20` | Past changes, newest first. |
| `GET /logs?service=web&tail=100&follow=1` | Server-Sent Events, at most 5 streams per app. |

One operation runs at a time per app; a second one gets 409.

## Security

- **No authentication.** Network access is the only protection. Keep the
  Traefik entrypoints on the Tailscale address (or another private network) and
  use Tailscale ACLs to decide who can reach them.
- Helmo can start and stop containers through the Docker socket, which is
  equivalent to root on the host. Do not expose the socket or Helmo's port 8080
  anywhere. A restricted `docker-socket-proxy` is not supported yet because
  `docker compose` needs broad API access.
- Unknown ports and hosts get 404. The app is taken only from the port Traefik
  sets itself; a value sent by the client is overwritten.
- State-changing requests are checked for same-origin (CSRF) and every one is
  logged with its source address.
- The page has a strict Content-Security-Policy and renders container output as
  text only.
- Registry credentials are read from a `config.json` mounted read-only and are
  never logged or shown. Use a dedicated token that can only read packages.

## Resources

Idle Helmo uses about 10 MB of RAM. The example limits the container to 128 MB
because the `docker compose` processes it starts count against it; a simple
Compose command peaks at about 30 MB, and `pull`/`up` were not measured.

## Development

Requires Go 1.24. The code is in `src/`.

```sh
cd src
go vet ./...
go test ./...
go run ./cmd/helmo        # needs HELMO_APPS_DIR and a Docker socket to be useful
```

The tests use a fake Docker daemon, a fake registry and stub `docker` scripts;
nothing needs a running daemon. `go test` also runs one test against the real
`docker compose` CLI when it is installed.

## Versioning and releases

Versions come from the git history with
[abcversion](https://github.com/deneblab/abcversion) (`.abcversion.json` sets
the base version; the patch number counts commits). Image tags are plain
versions such as `0.1.12`, the same form Helmo itself accepts for the apps it
manages.

```sh
scripts/build.sh         # builds ghcr.io/deneblab/helmo:<version>
docker push ghcr.io/deneblab/helmo:<version>
```

`.github/workflows/image.yml` runs the tests and builds the image on every push
to `develop` or `production` and on pull requests. Only `production` pushes the
image to GHCR, and creates a GitHub release `v<version>` for it: the latest
release (and the badge at the top) is the current image,
`ghcr.io/deneblab/helmo:<version>`.

### Branches

- `production` is the main branch. Every commit on it is a release: its image is
  published under the version abcversion computes for it.
- `develop` is the working and testing branch. Merge it into `production` with a
  pull request when it is ready to ship.

abcversion counts commits, and it counts them differently on the two branches:
on `develop` every commit adds one, on `production` a merged pull request adds
one. The two sequences overlap over time, so a `develop` image under a plain
version number could later clash with a real `production` release. That is why
`develop` is built and tested but never pushed.

Helmo does not update itself; change `HELMO_TAG` and run `docker compose up -d`.

## Limits

- `linux/amd64` image only.
- One versioned service per app; start, stop and restart act on the whole project.
- Registry credential helpers (`credsStore`) are not supported in the container.
  Use a `config.json` that holds the credentials directly.
- Deployment progress is kept in memory; after a restart of Helmo only
  `history.jsonl` remains.
