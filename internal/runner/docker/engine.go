package docker

import (
	"context"
	"io"
	"time"
)

// Mount makes a directory of the host visible inside a container.
type Mount struct {
	Host      string // absolute path on the host
	Container string // absolute path inside the container
	ReadOnly  bool
}

// ContainerSpec describes one container. It is deliberately small: whatever is not here (privileged
// mode, devices, the engine's socket, published ports) cannot be asked for.
type ContainerSpec struct {
	Name    string
	Image   string
	Cmd     []string
	Workdir string
	User    string // "uid:gid"
	Env     map[string]string
	Mounts  []Mount
	Network string // "default" or "none"
	CPUs    float64
	Memory  string // as the manifest writes it: 512m, 2g
	Labels  map[string]string
}

// BuildSpec describes an image built from a Dockerfile of the repository.
type BuildSpec struct {
	Tag        string
	Dockerfile string // absolute path
	Context    string // absolute path
	Args       map[string]string
	// Memory and CPUs bound the build as far as the engine's builder honours them.
	Memory string
	CPUs   float64
}

// Engine is everything dymploy needs from a container engine. The real implementation talks to the
// docker command line; tests use dockertest.Fake. Nothing outside this package knows more than this.
type Engine interface {
	// Ping fails when the engine cannot be used.
	Ping(ctx context.Context) error
	// EnsureImage makes ref available locally, pulling it if needed.
	EnsureImage(ctx context.Context, ref string, out io.Writer) error
	// BuildImage builds an image; the build's own output goes to out and errOut.
	BuildImage(ctx context.Context, b BuildSpec, out, errOut io.Writer) error
	// RemoveImage deletes a local image. A missing image is not an error.
	RemoveImage(ctx context.Context, ref string) error

	// Create makes a container without starting it and returns its id.
	Create(ctx context.Context, c ContainerSpec) (string, error)
	// Start starts a created container.
	Start(ctx context.Context, id string) error
	// Follow streams the container's output until it exits and returns its exit code. If ctx ends first
	// it returns ctx's error and leaves the container as it is (the caller stops it).
	Follow(ctx context.Context, id string, out, errOut io.Writer) (int, error)
	// Stop asks the container to stop and kills it after grace.
	Stop(ctx context.Context, id string, grace time.Duration) error
	// Remove deletes the container and its anonymous volumes. A missing container is not an error.
	Remove(ctx context.Context, id string) error
}
