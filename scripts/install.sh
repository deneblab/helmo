#!/bin/sh
# Installs Helmo, or prepares an app for it. Works in the current directory.
#
#   curl -fsSL https://raw.githubusercontent.com/deneblab/helmo/production/scripts/install.sh | sh -s -- helmo
#
#   helmo [options]    write compose.yaml and .env here and start Helmo
#   app [options]      prepare the app in this directory (cd /srv/apps/<id> first)
#
# Options for helmo:
#   --version X.Y.Z    image version (default: the newest in ghcr.io/deneblab/helmo)
#   --apps-dir DIR     directory with one subdirectory per app (default /srv/apps)
#   --network NAME     Docker network Traefik reaches its backends on (default traefik)
#   --docker-config F  registry credentials file for Helmo (default ./docker-config/config.json)
#   --traefik-api URL  Traefik API (default: the host port Traefik publishes for its port 8080)
#   --yes              ask nothing: use the defaults and keep files that already exist
#
# Options for app:
#   --port N           Traefik port of the app (required, may be given more than once)
#   --host NAME        accept only this host name (may be given more than once)
#   --service NAME     the Compose service Helmo versions (needed with several services)
#   --apps-dir DIR     the apps directory of Helmo, to check where this app lives
#   --traefik-api URL  Traefik API, to print its configuration when it uses the File provider
#   --yes              ask nothing and keep files that already exist
#
# It writes only the files of Helmo itself; Traefik and your apps are not changed.
set -eu

IMAGE_REPO=deneblab/helmo
SOCKET=/var/run/docker.sock

YES=0
VERSION=${HELMO_TAG:-}
APPS_DIR=
NETWORK=
DOCKER_GID=
DOCKER_CONFIG_FILE=
TRAEFIK_API=

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

have_tty() { ( : </dev/tty ) 2>/dev/null; }

# ask PROMPT DEFAULT: prints the answer (or the default) on stdout.
ask() {
    if [ "$YES" = 1 ]; then
        printf '%s' "$2"
        return
    fi
    have_tty || die "no terminal to ask '$1'; pass the options or --yes"
    printf '%s [%s]: ' "$1" "$2" >/dev/tty
    read -r answer </dev/tty || answer=
    printf '%s' "${answer:-$2}"
}

# confirm PROMPT: succeeds when the user agrees; --yes means no.
confirm() {
    [ "$YES" = 1 ] && return 1
    have_tty || return 1
    printf '%s [y/N]: ' "$1" >/dev/tty
    read -r answer </dev/tty || answer=
    case $answer in y | Y | yes) return 0 ;; *) return 1 ;; esac
}

# safe VALUE NAME: values end up in .env and compose.yaml unquoted.
safe() {
    case $1 in
        '' | *[!A-Za-z0-9._/:@-]*) die "$2 must be non-empty and contain only letters, digits and . _ / : @ -" ;;
    esac
}

# ---- version -----------------------------------------------------------------

newest_version() {
    token=$(curl -fsSL "https://ghcr.io/token?scope=repository:$IMAGE_REPO:pull" |
        sed -n 's/.*"token" *: *"\([^"]*\)".*/\1/p') || return 1
    [ -n "$token" ] || return 1
    curl -fsSL -H "Authorization: Bearer $token" "https://ghcr.io/v2/$IMAGE_REPO/tags/list?n=1000" |
        grep -o '"[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*"' | tr -d '"' |
        sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1
}

resolve_version() {
    if [ -z "$VERSION" ]; then
        VERSION=$(newest_version 2>/dev/null || true)
        [ -n "$VERSION" ] || die "cannot find the newest version in ghcr.io/$IMAGE_REPO; pass --version X.Y.Z"
        say "newest version: $VERSION"
    fi
    case $VERSION in
        [0-9]*.[0-9]*.[0-9]*) ;;
        *) die "version must look like 1.2.3, got '$VERSION'" ;;
    esac
    safe "$VERSION" version
}

# ---- files -------------------------------------------------------------------

# write_file NAME: stores stdin as NAME; an existing different file is replaced
# only when the user agrees.
write_file() {
    tmp=$(mktemp)
    cat >"$tmp"
    if [ -f "$1" ]; then
        if cmp -s "$tmp" "$1"; then
            say "$1: up to date"
            rm -f "$tmp"
            return
        fi
        say "$1 already exists and differs:"
        diff -u "$1" "$tmp" || true
        if ! confirm "Overwrite $1?"; then
            say "$1: kept as it is"
            rm -f "$tmp"
            return
        fi
    fi
    cat "$tmp" >"$1"
    rm -f "$tmp"
    say "$1: written"
}

compose_yaml() {
    cat <<EOF
# Helmo. Settings live in .env next to this file. Helmo does not update
# itself: change HELMO_TAG there and run: docker compose up -d

services:
  helmo:
    image: ghcr.io/$IMAGE_REPO:\${HELMO_TAG:?set HELMO_TAG to an image version}
    restart: unless-stopped
    user: "1654:1654"            # owner of $APPS_DIR/*/.helmo
    group_add:
      - "\${DOCKER_GID:?set DOCKER_GID}"
    stop_grace_period: 30s
    # Idle Helmo uses about 10 MB. While deploying it also runs docker compose,
    # and child processes count against this limit.
    mem_limit: 128m
    environment:
      HELMO_APPS_DIR: $APPS_DIR
      HELMO_LISTEN: ":8080"
    volumes:
      # Same path inside and on the host: Compose resolves relative paths of
      # the apps in the CLI, and the Docker daemon needs the host paths.
      - $APPS_DIR:$APPS_DIR
      - $SOCKET:/var/run/docker.sock
      - \${DOCKER_CONFIG_FILE:?set DOCKER_CONFIG_FILE}:/docker-config/config.json:ro
    networks: [traefik]
    labels:
      traefik.enable: "true"
      traefik.docker.network: \${TRAEFIK_NETWORK:-traefik}
      # One router for every app: any host, any entrypoint (no entryPoints
      # label), ahead of the apps' own routers. Do NOT publish port 8080.
      traefik.http.routers.helmo.rule: PathPrefix(\`/_helmo\`)
      traefik.http.routers.helmo.priority: "10000"
      traefik.http.routers.helmo.service: helmo
      traefik.http.services.helmo.loadbalancer.server.port: "8080"

networks:
  traefik:
    name: \${TRAEFIK_NETWORK:-traefik}
    external: true
EOF
}

# without_labels: compose.yaml without the labels block, which only the Docker
# provider of Traefik reads.
without_labels() {
    awk '/^    labels:/ { skip = 1; next } skip && /^      / { next } { skip = 0; print }'
}

env_file() {
    cat <<EOF
HELMO_TAG=$VERSION
DOCKER_GID=$DOCKER_GID
DOCKER_CONFIG_FILE=$DOCKER_CONFIG_FILE
TRAEFIK_NETWORK=$NETWORK
EOF
}

# ---- helmo command -----------------------------------------------------------

# traefik_ids: ids of running Traefik containers (by image or container name).
traefik_ids() {
    docker ps --format '{{.ID}} {{.Image}} {{.Names}}' 2>/dev/null | grep -i traefik | cut -d' ' -f1 || true
}

# traefik_networks: the user-defined networks of running Traefik containers.
traefik_networks() {
    for cid in $(traefik_ids); do
        docker inspect -f '{{range $name, $net := .NetworkSettings.Networks}}{{println $name}}{{end}}' "$cid" 2>/dev/null
    done | grep -v -x -e bridge -e host -e none -e '' | sort -u
}

# traefik_ports: ports Traefik publishes as itself (same port on the host and in the
# container), without 80 and 443. These are the entrypoints of the apps.
traefik_ports() {
    for cid in $(traefik_ids); do
        docker port "$cid" 2>/dev/null |
            sed -n 's|^\([0-9][0-9]*\)/tcp -> .*:\([0-9][0-9]*\)$|\1 \2|p' |
            while read -r inner outer; do
                if [ "$inner" = "$outer" ] && [ "$outer" != 80 ] && [ "$outer" != 443 ]; then echo "$outer"; fi
            done
    done | sort -n -u
}

# traefik_api: the URL of the Traefik API: --traefik-api, else the host port
# a running Traefik publishes for its API port 8080.
traefik_api() {
    if [ -n "$TRAEFIK_API" ]; then
        printf '%s' "${TRAEFIK_API%/}"
        return
    fi
    for cid in $(traefik_ids); do
        hp=$(docker port "$cid" 8080/tcp 2>/dev/null | head -n 1 | sed 's/.*://')
        case $hp in '' | *[!0-9]*) continue ;; esac
        printf 'http://127.0.0.1:%s' "$hp"
        return
    done
}

# traefik_mode: "labels" when Traefik reads Docker labels, "file" when it does
# not (File provider only), nothing when its API cannot be read.
traefik_mode() {
    api=$(traefik_api)
    [ -n "$api" ] || return 0
    command -v curl >/dev/null 2>&1 || return 0
    overview=$(curl -fsS -m 5 "$api/api/overview" 2>/dev/null) || return 0
    providers=$(printf '%s' "$overview" | tr -d '\n' | sed -n 's/.*"providers":\[\([^]]*\)\].*/\1/p')
    [ -n "$providers" ] || return 0
    case $providers in
        *'"Docker"'* | *'"docker"'* | *'"Swarm"'* | *'"swarm"'*) echo labels ;;
        *) echo file ;;
    esac
}

# app_ports DIR: the ports of all apps in DIR, comma-separated.
app_ports() {
    for f in "$1"/*/.helmo/app.yaml; do
        [ -f "$f" ] || continue
        sed -n 's/^ports:[[:space:]]*\[\(.*\)\].*/\1/p' "$f" | tr ',' ' '
    done | tr ' ' '\n' | grep -x '[0-9][0-9]*' | sort -n -u | paste -s -d, - || true
}

# traefik_config IMAGE PORTS: Helmo's routers for Traefik's dynamic configuration,
# written by the Helmo binary from what the Traefik API reports.
traefik_config() {
    docker run --rm --network host "$1" -traefik-config -traefik-api "$(traefik_api)" -ports "$2"
}

# readable_by_helmo FILE: whether UID 1654 (groups 1654 and DOCKER_GID) can read FILE.
# Only the file's own mode matters: Docker mounts it from the host as root.
readable_by_helmo() {
    info=$(stat -c '%u %g %a' "$1" 2>/dev/null) || return 1
    uid=${info%% *}
    rest=${info#* }
    gid=${rest%% *}
    mode=${rest#* }
    while [ ${#mode} -lt 3 ]; do mode=0$mode; done
    mode=${mode#"${mode%???}"} # the last three digits: user, group, other
    u=${mode%??}
    g=${mode#?}
    g=${g%?}
    o=${mode#??}
    [ $((o & 4)) -ne 0 ] && return 0
    [ "$uid" = 1654 ] && [ $((u & 4)) -ne 0 ] && return 0
    { [ "$gid" = 1654 ] || [ "$gid" = "$DOCKER_GID" ]; } && [ $((g & 4)) -ne 0 ] && return 0
    return 1
}

detect_docker_gid() {
    gid=$(stat -c %g "$SOCKET" 2>/dev/null || true)
    [ -n "$gid" ] || gid=$(getent group docker 2>/dev/null | cut -d: -f3 || true)
    printf '%s' "$gid"
}

cmd_helmo() {
    while [ $# -gt 0 ]; do
        case $1 in
            --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION=$2; shift 2 ;;
            --apps-dir) [ $# -ge 2 ] || die "--apps-dir needs a value"; APPS_DIR=$2; shift 2 ;;
            --network) [ $# -ge 2 ] || die "--network needs a value"; NETWORK=$2; shift 2 ;;
            --docker-config) [ $# -ge 2 ] || die "--docker-config needs a value"; DOCKER_CONFIG_FILE=$2; shift 2 ;;
            --traefik-api) [ $# -ge 2 ] || die "--traefik-api needs a value"; TRAEFIK_API=$2; shift 2 ;;
            --yes | -y) YES=1; shift ;;
            *) die "unknown option '$1' for helmo" ;;
        esac
    done

    command -v docker >/dev/null 2>&1 || die "docker is not installed"
    docker version >/dev/null 2>&1 || die "cannot talk to Docker; is it running and are you allowed to use it?"
    docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is missing ('docker compose version' fails)"
    command -v curl >/dev/null 2>&1 || [ -n "$VERSION" ] || die "curl is needed to find the newest version; pass --version"

    resolve_version

    [ -n "$APPS_DIR" ] || APPS_DIR=$(ask "Directory with the apps (one subdirectory per app, e.g. /srv/apps/cadastro)" /srv/apps) || exit 1
    case $APPS_DIR in /*) ;; *) die "the apps directory must be an absolute path, got '$APPS_DIR'" ;; esac
    safe "$APPS_DIR" "the apps directory"

    if [ -z "$NETWORK" ]; then
        found=$(traefik_networks)
        default=traefik
        if [ -n "$found" ]; then
            default=$(printf '%s\n' "$found" | head -n 1)
            say "Traefik is running on the network(s): $(printf '%s' "$found" | tr '\n' ' ')"
        else
            say "No running Traefik found; list the networks with: docker network ls"
        fi
        NETWORK=$(ask "Docker network Traefik uses to reach Helmo" "$default") || exit 1
    fi
    safe "$NETWORK" "the network name"
    docker network inspect "$NETWORK" >/dev/null 2>&1 ||
        die "the Docker network '$NETWORK' does not exist; create it, or use the one Traefik is on (--network)"

    DOCKER_GID=$(detect_docker_gid)
    if [ -z "$DOCKER_GID" ]; then
        DOCKER_GID=$(ask "Group id of $SOCKET" "") || exit 1
    fi
    case $DOCKER_GID in '' | *[!0-9]*) die "cannot determine the group id of the Docker socket; check $SOCKET" ;; esac

    # A file of its own: ~/.docker/config.json holds the credentials of every
    # registry you use, and is usually readable by you only.
    dedicated=0
    if [ -z "$DOCKER_CONFIG_FILE" ]; then
        DOCKER_CONFIG_FILE=$(pwd)/docker-config/config.json
        dedicated=1
    fi
    case $DOCKER_CONFIG_FILE in /*) ;; *) DOCKER_CONFIG_FILE=$(pwd)/$DOCKER_CONFIG_FILE ;; esac
    safe "$DOCKER_CONFIG_FILE" "the path of config.json"
    if [ ! -f "$DOCKER_CONFIG_FILE" ]; then
        # Compose would create a directory for a missing file; start with an empty one.
        { mkdir -p "$(dirname "$DOCKER_CONFIG_FILE")" && printf '{}\n' >"$DOCKER_CONFIG_FILE"; } 2>/dev/null ||
            die "cannot create $DOCKER_CONFIG_FILE"
        say "created an empty $DOCKER_CONFIG_FILE: enough for public images"
    fi
    case $DOCKER_CONFIG_FILE in
        "$HOME"/.docker/*)
            say "warning: $DOCKER_CONFIG_FILE holds the credentials of every registry you logged in to;"
            say "         Helmo needs only its own (see the end of this output)." ;;
    esac
    if ! readable_by_helmo "$DOCKER_CONFIG_FILE"; then
        if [ "$dedicated" = 1 ] && [ "$(id -u)" = 0 ]; then
            chown 1654:1654 "$DOCKER_CONFIG_FILE" && chmod 600 "$DOCKER_CONFIG_FILE"
        else
            say "warning: Helmo (UID 1654) cannot read $DOCKER_CONFIG_FILE, so private registries will fail:"
            say "    sudo chown 1654:1654 '$DOCKER_CONFIG_FILE' && sudo chmod 600 '$DOCKER_CONFIG_FILE'"
        fi
    fi

    mkdir -p "$APPS_DIR" 2>/dev/null || die "cannot create $APPS_DIR; create it first (sudo mkdir -p $APPS_DIR)"

    MODE=$(traefik_mode)
    case $MODE in
        labels) say "Traefik reads Docker labels: compose.yaml carries Helmo's router" ;;
        file) say "Traefik does not read Docker labels (File provider): compose.yaml has none" ;;
        *) say "cannot read the Traefik API (pass --traefik-api URL); assuming Docker labels" ;;
    esac
    if [ "$MODE" = file ]; then
        compose_yaml | without_labels | write_file compose.yaml
    else
        compose_yaml | write_file compose.yaml
    fi
    env_file | write_file .env

    say "starting Helmo (version from .env)"
    docker compose up -d
    docker compose ps

    say ""
    say "Helmo is running. Not changed by this script, your part:"
    if [ "$MODE" = file ]; then
        image=ghcr.io/$IMAGE_REPO:$(sed -n 's/^HELMO_TAG=//p' .env)
        ports=$(app_ports "$APPS_DIR")
        say "  - Traefik uses the File provider. Add Helmo's routers to its dynamic configuration;"
        say "    they only take the entrypoints of the apps, so add the router again after each app:"
        if [ -n "$ports" ]; then
            traefik_config "$image" "$ports" | sed 's/^/      /' ||
                say "    (could not generate it; see docs/traefik.md)"
        else
            say "    no apps yet: the app command prints it."
        fi
    else
        say "  - Traefik must read Docker labels and use the network '$NETWORK'; the labels in"
        say "    compose.yaml add one router, PathPrefix(\`/_helmo\`) on every entrypoint."
    fi
    cat <<EOF
  - Port 8080 of Helmo must not be published.
For a private registry, log in once with a token that can only read images:
  docker --config '$(dirname "$DOCKER_CONFIG_FILE")' login <registry>
  sudo chown 1654:1654 '$DOCKER_CONFIG_FILE' && sudo chmod 600 '$DOCKER_CONFIG_FILE'
Next, for each app, in its directory:
  curl -fsSL https://raw.githubusercontent.com/deneblab/helmo/production/scripts/install.sh | sh -s -- app --port <port>
EOF
}

# ---- app command -------------------------------------------------------------

# helmo_container: "id image name" of a running Helmo container, if any.
helmo_container() {
    command -v docker >/dev/null 2>&1 || return 0
    docker ps --format '{{.ID}} {{.Image}} {{.Names}}' 2>/dev/null | grep " ghcr.io/${IMAGE_REPO}[:@]" | head -n 1 || true
}

# apps_dir_of_helmo: the HELMO_APPS_DIR of a running Helmo container, if any.
apps_dir_of_helmo() {
    cid=$(helmo_container | cut -d' ' -f1)
    [ -n "$cid" ] || return 0
    docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$cid" 2>/dev/null |
        sed -n 's/^HELMO_APPS_DIR=//p' | head -n 1
}

# ports_of_other_apps: "port app" lines for the sibling apps.
ports_of_other_apps() {
    for f in ../*/.helmo/app.yaml; do
        [ -f "$f" ] || continue
        dir=$(cd "$(dirname "$f")/.." && pwd -P)
        [ "$dir" = "$APP_DIR" ] && continue
        for p in $(sed -n 's/^ports:[[:space:]]*\[\(.*\)\].*/\1/p' "$f" | tr ',' ' '); do
            echo "$p $(basename "$dir")"
        done
    done
}

# app_containers: "id service" of the running containers of the Compose project here.
app_containers() {
    command -v docker >/dev/null 2>&1 || return 0
    for wd in "$(pwd)" "$APP_DIR"; do
        docker ps --filter "label=com.docker.compose.project.working_dir=$wd" \
            --format '{{.ID}} {{.Label "com.docker.compose.service"}}' 2>/dev/null || true
    done | sort -u
}

# running_ref SERVICE: the image reference the running container of SERVICE was created from.
running_ref() {
    cid=$(app_containers | awk -v s="$1" '$2 == s { print $1; exit }')
    [ -n "$cid" ] || return 0
    docker inspect -f '{{.Config.Image}}' "$cid" 2>/dev/null || true
}

# running_version SERVICE: tag@digest of the image the running container of SERVICE uses.
running_version() {
    cid=$(app_containers | awk -v s="$1" '$2 == s { print $1; exit }')
    [ -n "$cid" ] || return 0
    ref=$(docker inspect -f '{{.Config.Image}}' "$cid" 2>/dev/null) || return 0
    img=$(docker inspect -f '{{.Image}}' "$cid" 2>/dev/null) || return 0
    name=${ref%@*}
    case ${name##*/} in
        *:*) tag=${name##*:}; repo=${name%:*} ;;
        *) tag=latest; repo=$name ;;
    esac
    digests=$(docker image inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' "$img" 2>/dev/null || true)
    digest=$(printf '%s\n' "$digests" | grep -F "$repo@" | head -n 1 | cut -d@ -f2)
    [ -n "$digest" ] || digest=$(printf '%s\n' "$digests" | grep @ | head -n 1 | cut -d@ -f2)
    safe "$tag" tag
    printf '%s' "$tag${digest:+@$digest}"
}

# check_registry REF: warn when the registry of REF answers only over plain HTTP.
check_registry() {
    host=${1%%/*}
    case $1 in */*) ;; *) return 0 ;; esac
    case $host in *.* | *:* | localhost) ;; *) return 0 ;; esac
    command -v curl >/dev/null 2>&1 || return 0
    curl -s -o /dev/null -m 5 "https://$host/v2/" && return 0
    if curl -s -o /dev/null -m 5 "http://$host/v2/"; then
        say ""
        say "warning: the registry $host answers only over plain HTTP. Helmo talks to registries"
        say "over HTTPS, so it cannot list versions there yet."
    fi
}

join_list() {
    out=
    for item in "$@"; do out=${out:+$out, }$item; done
    printf '%s' "$out"
}

cmd_app() {
    PORTS=
    HOSTS=
    SERVICE=
    while [ $# -gt 0 ]; do
        case $1 in
            --port) [ $# -ge 2 ] || die "--port needs a value"; PORTS="$PORTS $2"; shift 2 ;;
            --host) [ $# -ge 2 ] || die "--host needs a value"; HOSTS="$HOSTS $2"; shift 2 ;;
            --service) [ $# -ge 2 ] || die "--service needs a value"; SERVICE=$2; shift 2 ;;
            --apps-dir) [ $# -ge 2 ] || die "--apps-dir needs a value"; APPS_DIR=$2; shift 2 ;;
            --traefik-api) [ $# -ge 2 ] || die "--traefik-api needs a value"; TRAEFIK_API=$2; shift 2 ;;
            --yes | -y) YES=1; shift ;;
            *) die "unknown option '$1' for app" ;;
        esac
    done

    APP_DIR=$(pwd -P)
    app_id=$(basename "$APP_DIR")
    case $app_id in '' | *[!a-z0-9-]*) die "the directory name '$app_id' is the app id and must match ^[a-z0-9-]+\$; rename it" ;; esac

    compose=
    for f in compose.yaml compose.yml docker-compose.yaml docker-compose.yml; do
        [ -f "$f" ] && { compose=$f; break; }
    done
    [ -n "$compose" ] || die "no Compose file in $APP_DIR; run this in the directory of the app"

    if [ -z "$PORTS" ]; then
        taken=$(ports_of_other_apps | cut -d' ' -f1)
        free=
        for port in $(traefik_ports); do
            printf '%s\n' "$taken" | grep -q -x "$port" || free="$free $port"
        done
        default=
        if [ -n "$free" ]; then
            say "Traefik ports not used by another app:$free"
            # shellcheck disable=SC2086
            [ "$(set -- $free; echo $#)" = 1 ] && default=${free# }
        fi
        PORTS=$(ask "Traefik port of the app" "$default") || exit 1
    fi
    for port in $PORTS; do
        case $port in '' | *[!0-9]*) die "port '$port' is not a number" ;; esac
        if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then die "port $port is out of range"; fi
    done
    [ -n "$PORTS" ] || die "give the Traefik port of the app: --port N"
    for host in $HOSTS; do safe "$host" "host"; done
    [ -z "$SERVICE" ] || safe "$SERVICE" "the service name"

    # shellcheck disable=SC2086
    set -- $PORTS
    for port in "$@"; do
        n=0
        for q in "$@"; do [ "$q" = "$port" ] && n=$((n + 1)); done
        [ "$n" = 1 ] || die "port $port is given twice"
    done
    other=$(ports_of_other_apps | awk -v p="$PORTS" 'BEGIN { n = split(p, a, " "); for (i = 1; i <= n; i++) want[a[i]] = 1 } want[$1] { print $1 " (app " $2 ")" }')
    [ -z "$other" ] || die "port already used by another app: $(printf '%s' "$other" | tr '\n' ' ')"

    mkdir -p .helmo 2>/dev/null || die "cannot create $APP_DIR/.helmo; create it first (sudo mkdir -p '$APP_DIR/.helmo')"

    {
        echo "enabled: true"
        # shellcheck disable=SC2086
        echo "ports: [$(join_list $PORTS)]"
        # shellcheck disable=SC2086
        [ -z "$HOSTS" ] || echo "hosts: [$(join_list $HOSTS)]"
        [ -z "$SERVICE" ] || echo "service: $SERVICE"
    } | write_file .helmo/app.yaml

    # The image must take its tag from APP_TAG. The compose file stays yours.
    images=$(sed -n 's/^[[:space:]]*image:[[:space:]]*["'"'"']\{0,1\}\([^"'"'"'[:space:]#]*\).*/\1/p' "$compose")
    count=$(printf '%s\n' "$images" | grep -c . || true)
    todo=0
    # shellcheck disable=SC2016 # a literal ${APP_TAG, not an expansion
    if printf '%s\n' "$images" | grep -q '\${APP_TAG'; then
        say "$compose: the image already uses APP_TAG"
    else
        todo=1
    fi

    if [ "$todo" = 1 ]; then
        say ""
        say "$compose: the tag of the versioned service's image must come from APP_TAG."
        say "The script does not edit your file; change the tag part of the image line:"
        if [ "$count" = 1 ]; then
            line=$(grep -n '^[[:space:]]*image:' "$compose" | head -n 1)
            say "  now:    line ${line%%:*}:${line#*:}"
            # shellcheck disable=SC2016 # literal ${APP_TAG in the replacement
            suggested=$(printf '%s\n' "${line#*:}" | sed -e 's|:${[^}]*}[[:space:]]*$|:${APP_TAG:?use .helmo/dc instead of docker compose}|' -e t \
                -e 's|:[A-Za-z0-9._-]*[[:space:]]*$|:${APP_TAG:?use .helmo/dc instead of docker compose}|')
            say "  change: $suggested"
        else
            # shellcheck disable=SC2016 # literal text for the user to copy
            say '  use  :${APP_TAG:?use .helmo/dc instead of docker compose}  as the tag of the service Helmo versions'
            say "Image lines:"
            grep -n '^[[:space:]]*image:' "$compose" | sed 's/^/    /' || true
        fi
    fi

    # The service Helmo versions: --service, or the only one running here.
    svc=$SERVICE
    if [ -z "$svc" ]; then
        services=$(app_containers | awk '{ print $2 }' | sort -u)
        [ "$(printf '%s\n' "$services" | grep -c .)" != 1 ] || svc=$services
    fi

    # The version that runs now, so Compose can resolve APP_TAG before the first
    # deployment. Pinned to the digest, so a rollback returns to exactly this image.
    if [ ! -f .helmo/env ]; then
        current=
        [ -z "$svc" ] || current=$(running_version "$svc")
        if [ -n "$current" ]; then
            say "the running '$svc' container uses $current"
            printf 'APP_TAG=%s\n' "$current" | write_file .helmo/env
        elif [ "$count" = 1 ] && [ "$todo" = 1 ]; then
            last=${images##*/}
            case $last in
                *:[0-9]*.[0-9]*.[0-9]* | *:v[0-9]*.[0-9]*.[0-9]*)
                    printf 'APP_TAG=%s\n' "${last##*:}" | write_file .helmo/env
                    ;;
            esac
        fi
        if [ ! -f .helmo/env ]; then
            say "Set the version that runs now: echo 'APP_TAG=<tag>' > .helmo/env"
            [ -n "$svc" ] || [ -z "$(app_containers)" ] ||
                say "(several services are running; pass --service NAME to read it from the container)"
        fi
    fi

    ref=
    [ -z "$svc" ] || ref=$(running_ref "$svc")
    if [ -z "$ref" ] && [ "$count" = 1 ]; then
        case $images in *'$'*) ;; *) ref=$images ;; esac
    fi
    [ -z "$ref" ] || check_registry "$ref"

    if [ "$(id -u)" = 0 ]; then
        chown -R 1654:1654 .helmo
    elif [ "$(stat -c %u .helmo 2>/dev/null || echo 0)" != 1654 ]; then
        say ""
        say "Helmo runs as UID 1654 and must own .helmo; run:"
        say "    sudo chown -R 1654:1654 '$APP_DIR/.helmo'"
    fi

    [ -n "$APPS_DIR" ] || APPS_DIR=$(apps_dir_of_helmo)
    if [ -n "$APPS_DIR" ] && [ "$(dirname "$APP_DIR")" != "$(cd "$APPS_DIR" 2>/dev/null && pwd -P)" ]; then
        say ""
        say "warning: Helmo manages $APPS_DIR, but this app is in $(dirname "$APP_DIR"); Helmo will not see it."
    fi

    say ""
    first=${PORTS# }
    first=${first%% *}
    helmo=$(helmo_container)
    if [ -n "$helmo" ] && [ "$(traefik_mode)" = file ]; then
        image=$(printf '%s' "$helmo" | cut -d' ' -f2)
        ports=$(app_ports "$(dirname "$APP_DIR")")
        say "Traefik uses the File provider: replace Helmo's routers in its dynamic configuration with"
        say "(this adds the entrypoint of port $first):"
        traefik_config "$image" "$ports" | sed 's/^/    /' || say "    (could not generate it; see docs/traefik.md)"
        say ""
    fi
    if [ -n "$helmo" ]; then
        say "Helmo reads the apps only when it starts; restart it to load '$app_id':"
        say "    docker restart $(printf '%s' "$helmo" | cut -d' ' -f3)"
        say ""
    fi
    say "App '$app_id' is ready. Once Traefik routes port $first to it, open:"
    say "    http://<host>:$first/_helmo/"
}

# ---- main --------------------------------------------------------------------

[ $# -ge 1 ] || die "usage: install.sh helmo|app [options]"
cmd=$1
shift
case $cmd in
    helmo) cmd_helmo "$@" ;;
    app) cmd_app "$@" ;;
    *) die "unknown command '$cmd' (use: helmo, app)" ;;
esac
