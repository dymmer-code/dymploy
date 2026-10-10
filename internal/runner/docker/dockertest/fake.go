// Package dockertest gives tests an Engine that runs nothing: containers are scripted, so the Docker
// runner can be tested for what it asks of an engine without any engine.
package dockertest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/dymmer-code/dymploy/internal/runner/docker"
)

// Behavior is what a scripted container does when it runs.
type Behavior struct {
	Stdout, Stderr string
	ExitCode       int
	// Hang makes the container run until it is stopped (or the context ends).
	Hang bool
	// Effect runs when the container starts, for example to write files into a mounted directory.
	Effect func(docker.ContainerSpec)
}

// Fake is a docker.Engine driven by a script.
type Fake struct {
	// Script decides the behavior of each container. Nil means "exit 0, no output".
	Script func(docker.ContainerSpec) Behavior
	// PingErr, PullErr, BuildErr and CreateErr make the corresponding calls fail.
	PingErr, PullErr, BuildErr, CreateErr error

	mu         sync.Mutex
	n          int
	Calls      []string // "ping", "pull alpine", "build tag", "create id", "start id", "stop id", "rm id", "rmi tag"
	Created    []docker.ContainerSpec
	Builds     []docker.BuildSpec
	containers map[string]*container
}

type container struct {
	spec docker.ContainerSpec
	b    Behavior
	stop chan struct{}
	once sync.Once
}

func (f *Fake) call(format string, a ...any) {
	f.Calls = append(f.Calls, fmt.Sprintf(format, a...))
}

// Ping implements docker.Engine.
func (f *Fake) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("ping")
	return f.PingErr
}

// EnsureImage implements docker.Engine.
func (f *Fake) EnsureImage(_ context.Context, ref string, out io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("pull %s", ref)
	if f.PullErr != nil {
		return f.PullErr
	}
	fmt.Fprintf(out, "pulled %s\n", ref)
	return nil
}

// BuildImage implements docker.Engine.
func (f *Fake) BuildImage(_ context.Context, b docker.BuildSpec, out, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("build %s", b.Tag)
	f.Builds = append(f.Builds, b)
	if f.BuildErr != nil {
		return f.BuildErr
	}
	fmt.Fprintf(out, "built %s\n", b.Tag)
	return nil
}

// RemoveImage implements docker.Engine.
func (f *Fake) RemoveImage(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("rmi %s", ref)
	return nil
}

// Create implements docker.Engine.
func (f *Fake) Create(_ context.Context, s docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return "", f.CreateErr
	}
	f.n++
	id := fmt.Sprintf("c%d", f.n)
	var b Behavior
	if f.Script != nil {
		b = f.Script(s)
	}
	if f.containers == nil {
		f.containers = map[string]*container{}
	}
	f.containers[id] = &container{spec: s, b: b, stop: make(chan struct{})}
	f.Created = append(f.Created, s)
	f.call("create %s", id)
	return id, nil
}

func (f *Fake) get(id string) (*container, error) {
	c, ok := f.containers[id]
	if !ok {
		return nil, errors.New("no such container " + id)
	}
	return c, nil
}

// Start implements docker.Engine.
func (f *Fake) Start(_ context.Context, id string) error {
	f.mu.Lock()
	c, err := f.get(id)
	f.call("start %s", id)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if c.b.Effect != nil {
		c.b.Effect(c.spec)
	}
	return nil
}

// Follow implements docker.Engine.
func (f *Fake) Follow(ctx context.Context, id string, out, errOut io.Writer) (int, error) {
	f.mu.Lock()
	c, err := f.get(id)
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	io.WriteString(out, c.b.Stdout)
	io.WriteString(errOut, c.b.Stderr)
	if c.b.Hang {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-c.stop:
			return 137, nil
		}
	}
	return c.b.ExitCode, nil
}

// Stop implements docker.Engine.
func (f *Fake) Stop(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("stop %s", id)
	c, err := f.get(id)
	if err != nil {
		return err
	}
	c.once.Do(func() { close(c.stop) })
	return nil
}

// Remove implements docker.Engine.
func (f *Fake) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("rm %s", id)
	delete(f.containers, id)
	return nil
}

// Snapshot returns a copy of the calls so far.
func (f *Fake) Snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}
