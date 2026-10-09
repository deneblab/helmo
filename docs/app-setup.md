# Preparing an app

Helmo manages a Docker Compose project that lives in `/srv/apps/<id>/`.
The directory name is the app id and must match `^[a-z0-9-]+$`.

```
/srv/apps/cadastro/
  compose.yaml        your Compose file
  .env                your variables and secrets (Helmo never edits it)
  .helmo/
    app.yaml          you write this
    env               Helmo writes this: APP_TAG
    history.jsonl     Helmo writes this: past deployments
    dc                Helmo writes this: wrapper for manual commands
```

## Quick way: the script

In the app directory (the one with its Compose file):

```sh
curl -fsSL https://raw.githubusercontent.com/deneblab/helmo/production/scripts/install.sh | sh -s -- app --port 8600
```

Options: `--port N` (repeat for more ports; when omitted, the script lists the ports
Traefik publishes that no other app uses, and a single free one is the default), `--host NAME` (repeatable),
`--service NAME`, `--apps-dir DIR`, `--yes`. The script does steps 2 and 3 below,
checks that no other app uses the port, and for step 1 only tells you what to
change, because the Compose file is yours. It also does step 4: when the app is running it
writes the tag and digest of the running image (for example
`latest@sha256:…`), otherwise a literal tag such as `v1.2.3` from the Compose file.
It warns when the registry answers only over plain HTTP, which Helmo does not
support yet. It is safe to run again.

The rest of this page is what the script does, step by step.

## 1. Use `APP_TAG` in the Compose file

The image of the service Helmo versions must take its tag from `APP_TAG`:

```yaml
services:
  web:
    image: ghcr.io/org/app:${APP_TAG:?use .helmo/dc instead of docker compose}
```

The message after `:?` is what you see if you run plain `docker compose` and
forget the wrapper below.

Only this one service is deployed by Helmo (pulled and recreated). Other
services, such as a database, are not touched by a deployment.

## 2. Write `.helmo/app.yaml`

```yaml
enabled: true
ports: [8600]
```

Optional keys: `hosts`, `service` (needed when the project has several services),
`health_timeout`. See the [README](../README.md#configuration). Helmo reads these
files only when it starts: restart it after adding or changing an app.

## 3. Give Helmo access

Helmo runs as UID 1654 in [compose.example.yaml](../compose.example.yaml). It
reads `compose.yaml` and `.env` (Compose needs them) and writes only `.helmo/`.
The simplest setup is that the app directory already belongs to that user:

```sh
ls -ln /srv/apps/cadastro                      # owner 1654?
mkdir -p /srv/apps/cadastro/.helmo
chown -R 1654:1654 /srv/apps/cadastro/.helmo   # needed in any case
```

If your files belong to someone else, run Helmo as that user (`user:` in the
example) instead of loosening permissions. Do not make `.env` world-readable: it
holds your secrets.

## 4. Set the starting version

Before the first deployment tell Compose which version runs now:

```sh
echo 'APP_TAG=v1.2.3' > /srv/apps/cadastro/.helmo/env
chown 1654:1654 /srv/apps/cadastro/.helmo/env
```

Without this file, Helmo can still find the image from a running container of
the service, but plain Compose cannot resolve `${APP_TAG:?...}`.

## 5. Registry credentials

Public images need no credentials. For a private image Helmo reads a
`config.json` mounted read-only (`DOCKER_CONFIG_FILE` in Helmo's `.env`). Use a
file of its own, not `~/.docker/config.json`: that one holds the credentials of
every registry you use and is readable only by you, while Helmo runs as UID
1654. The install script creates an empty `docker-config/config.json` in Helmo's
directory and warns when Helmo cannot read the file. Log in into it once, with
a token that can only read images, and give it to Helmo:

```sh
docker --config /srv/system/helmo/docker-config login registry.example:5000
sudo chown 1654:1654 /srv/system/helmo/docker-config/config.json && sudo chmod 600 /srv/system/helmo/docker-config/config.json
```

The credentials must be stored in the file itself, not in a credential helper
(`credsStore`), which a fresh `--config` directory does not use. Both the version
list and `docker compose pull` use this file. Helmo reads it on every request,
so renewing the token needs no restart (run the login with `sudo` once the file
belongs to 1654).

## 6. Run Compose by hand

Plain `docker compose` does not read `.helmo/env`. Helmo writes a wrapper that
passes the right files, from any directory:

```sh
/srv/apps/cadastro/.helmo/dc ps
/srv/apps/cadastro/.helmo/dc up -d
/srv/apps/cadastro/.helmo/dc logs -f web
```

Helmo rewrites this script when it starts, so do not edit it. If Compose
refuses to start because `APP_TAG` is missing, you used plain `docker compose`.

If the same variable is also exported in your shell, the shell wins over the
files. Do not export `APP_TAG`.

## What a deployment does

1. Resolves the digest of the chosen tag in the registry.
2. Writes `APP_TAG=<tag>@<digest>` to `.helmo/env`.
3. Runs `docker compose pull <service>` and `up -d <service>`.
4. Waits until the service is healthy. Without a `healthcheck` in the Compose
   file it must stay running without restarting for the whole `health_timeout`.
5. If that fails, restores the previous value and recreates the service, then
   records both attempts in `history.jsonl`.

A rollback from the panel returns to the version that was active before the
current one, by digest.
