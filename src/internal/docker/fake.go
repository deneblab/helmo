package docker

import "context"

// Fake is an in-memory DockerOps for tests, keyed by project directory.
type Fake struct {
	Projects map[string][]Container
	Err      error

	Logs      map[string][]LogLine // by container ID
	HoldOpen  bool                 // with Follow, wait for ctx after the last line
	LogsStart func()               // called when a stream starts, if set
}

func (f *Fake) Ping(context.Context) error { return f.Err }

func (f *Fake) ProjectContainers(_ context.Context, dir string) ([]Container, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Projects[dir], nil
}

func (f *Fake) StreamLogs(ctx context.Context, id string, opts LogOptions, emit func(LogLine) error) error {
	if f.Err != nil {
		return f.Err
	}
	if f.LogsStart != nil {
		f.LogsStart()
	}
	var lines []LogLine
	for _, l := range f.Logs[id] {
		if opts.Since.IsZero() || !l.Time.Before(opts.Since) {
			lines = append(lines, l)
		}
	}
	if opts.Tail >= 0 && opts.Tail < len(lines) {
		lines = lines[len(lines)-opts.Tail:]
	}
	for _, l := range lines {
		if err := emit(l); err != nil {
			return err
		}
	}
	if opts.Follow && f.HoldOpen {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
