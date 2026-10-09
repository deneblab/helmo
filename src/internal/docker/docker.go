// Package docker is Helmo's read-only view of the Docker Engine. Everything
// else talks to the DockerOps interface so it can be tested with a fake.
package docker

import (
	"context"
	"strings"
)

// Compose labels set on every container of a project.
const (
	LabelWorkingDir = "com.docker.compose.project.working_dir"
	LabelService    = "com.docker.compose.service"
)

// DockerOps is everything Helmo needs from Docker. Operations that change
// state are added by later steps.
type DockerOps interface {
	// Ping succeeds when the daemon answers.
	Ping(ctx context.Context) error
	// ProjectContainers lists all containers (running or not) of the Compose
	// project whose working directory is dir.
	ProjectContainers(ctx context.Context, dir string) ([]Container, error)
}

// Container is the part of a container Helmo shows.
type Container struct {
	ID      string
	Name    string
	Service string
	Image   string
	State   string // Docker state: running, exited, created, paused, restarting, ...
	Health  string // healthy, unhealthy, starting, or "" without a healthcheck
}

// Running reports whether the container is up.
func (c Container) Running() bool { return c.State == "running" }

// State is the summary of an app's containers.
type State string

const (
	StateRunning   State = "running"   // every container is up
	StateUnhealthy State = "unhealthy" // all up, at least one failing its healthcheck
	StatePartial   State = "partial"   // some containers are up
	StateStopped   State = "stopped"   // containers exist, none is up
	StateDown      State = "down"      // no containers at all (never started or removed)
)

// Summarize reduces a project's containers to one State.
func Summarize(cs []Container) State {
	if len(cs) == 0 {
		return StateDown
	}
	up, unhealthy := 0, false
	for _, c := range cs {
		if c.Running() {
			up++
		}
		if c.Health == "unhealthy" {
			unhealthy = true
		}
	}
	switch {
	case up == 0:
		return StateStopped
	case up < len(cs):
		return StatePartial
	case unhealthy:
		return StateUnhealthy
	}
	return StateRunning
}

// healthFromStatus extracts the healthcheck result from the human readable
// status of the list endpoint, e.g. "Up 2 hours (healthy)".
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}
