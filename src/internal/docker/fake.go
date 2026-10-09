package docker

import "context"

// Fake is an in-memory DockerOps for tests, keyed by project directory.
type Fake struct {
	Projects map[string][]Container
	Err      error
}

func (f *Fake) Ping(context.Context) error { return f.Err }

func (f *Fake) ProjectContainers(_ context.Context, dir string) ([]Container, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Projects[dir], nil
}
