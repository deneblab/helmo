# syntax=docker/dockerfile:1

# Build: docker build --build-arg VERSION=1.2.3 -t ghcr.io/deneblab/helmo:1.2.3 .
# scripts/build.sh computes VERSION with abcversion. linux/amd64 only.

ARG GO_VERSION=1.24

# The docker CLI and the compose plugin are what Helmo runs to change
# versions. Both are static binaries, pinned and checked against a hash that
# is kept here, not downloaded next to the binary. Bump version and hash together.
FROM debian:12-slim AS tools
ARG DOCKER_VERSION=29.8.2
ARG DOCKER_SHA256=995d1ef289677f74fd58d8d2c35727b6a4ee389c69db8638a3e42d0487aa5b0f
ARG COMPOSE_VERSION=5.5.1
ARG COMPOSE_SHA256=db1889184726840f75c4f9c001048430d4f25b3be3cb084d3ddd762bc0aed576
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/docker.tgz "https://download.docker.com/linux/static/stable/x86_64/docker-${DOCKER_VERSION}.tgz" \
 && echo "${DOCKER_SHA256}  /tmp/docker.tgz" | sha256sum -c - \
 && tar -xzf /tmp/docker.tgz -C /tmp docker/docker \
 && curl -fsSL -o /tmp/docker-compose "https://github.com/docker/compose/releases/download/v${COMPOSE_VERSION}/docker-compose-linux-x86_64" \
 && echo "${COMPOSE_SHA256}  /tmp/docker-compose" | sha256sum -c - \
 && chmod +x /tmp/docker-compose \
 && mkdir /docker-config

FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src/ ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/helmo ./cmd/helmo

FROM gcr.io/distroless/static-debian12
COPY --from=tools /tmp/docker/docker /usr/local/bin/docker
COPY --from=tools /tmp/docker-compose /usr/local/lib/docker/cli-plugins/docker-compose
COPY --from=tools --chown=1654:1654 /docker-config /docker-config
COPY --from=build /out/helmo /usr/local/bin/helmo

# Registry credentials: mount the host's ~/.docker/config.json read-only at
# /docker-config/config.json. Both Helmo and "docker compose pull" read it.
ENV DOCKER_CONFIG=/docker-config \
    HELMO_APPS_DIR=/srv/apps \
    HELMO_LISTEN=:8080

# Must match the owner of /srv/apps/*/.helmo on the host.
USER 1654:1654
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD ["/usr/local/bin/helmo", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/helmo"]
