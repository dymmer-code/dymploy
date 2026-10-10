package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CLI is an Engine that drives the docker command line.
type CLI struct {
	bin string
	env []string
}

// NewCLI finds the docker binary (bin, or "docker" from the PATH when empty). It does not talk to the
// engine yet: Ping does.
func NewCLI(bin string) (*CLI, error) {
	if bin == "" {
		bin = "docker"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("docker is required but was not found: %w", err)
	}
	// The command gets a small environment of its own: how to reach the engine, nothing else.
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || k == "XDG_RUNTIME_DIR" || strings.HasPrefix(k, "DOCKER_") {
			env = append(env, kv)
		}
	}
	return &CLI{bin: path, env: env}, nil
}

func (c *CLI) cmd(ctx context.Context, out, errOut io.Writer, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Env = c.env
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// run executes a command and returns its trimmed stdout; a failure carries the tail of stderr.
func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	if err := c.cmd(ctx, &out, &errb, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, tail(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if i := len(s) - 600; i > 0 {
		s = "…" + s[i:]
	}
	return s
}

// Ping implements Engine.
func (c *CLI) Ping(ctx context.Context) error {
	if _, err := c.run(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("the container engine is not reachable: %w", err)
	}
	return nil
}

// EnsureImage implements Engine.
func (c *CLI) EnsureImage(ctx context.Context, ref string, out io.Writer) error {
	if _, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return err
	}
	if err := c.cmd(ctx, out, out, "pull", "--quiet", ref).Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("pulling image %s: %w", ref, err)
	}
	return nil
}

// BuildImage implements Engine.
func (c *CLI) BuildImage(ctx context.Context, b BuildSpec, out, errOut io.Writer) error {
	args := []string{"build", "--progress=plain", "--tag", b.Tag, "--file", b.Dockerfile}
	if b.Memory != "" {
		args = append(args, "--memory", b.Memory, "--memory-swap", b.Memory)
	}
	if b.CPUs > 0 {
		args = append(args, "--cpu-period", "100000", "--cpu-quota", strconv.Itoa(int(b.CPUs*100000)))
	}
	for _, k := range sortedKeys(b.Args) {
		args = append(args, "--build-arg", k+"="+b.Args[k])
	}
	args = append(args, b.Context)
	if err := c.cmd(ctx, out, errOut, args...).Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("building the image: %w", err)
	}
	return nil
}

// RemoveImage implements Engine.
func (c *CLI) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.run(ctx, "image", "rm", "--force", ref)
	if err != nil && strings.Contains(err.Error(), "No such image") {
		return nil
	}
	return err
}

// Create implements Engine.
func (c *CLI) Create(ctx context.Context, s ContainerSpec) (string, error) {
	args, err := createArgs(s)
	if err != nil {
		return "", err
	}
	return c.run(ctx, args...)
}

// createArgs builds the `docker create` command. Every container is unprivileged: all capabilities
// dropped, no new privileges, a bounded number of processes, no swap and a reaper as pid 1.
func createArgs(s ContainerSpec) ([]string, error) {
	if s.Image == "" || len(s.Cmd) == 0 {
		return nil, errors.New("a container needs an image and a command")
	}
	args := []string{"create", "--init", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pids-limit=1024"}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.User != "" {
		args = append(args, "--user", s.User)
	}
	if s.Workdir != "" {
		args = append(args, "--workdir", s.Workdir)
	}
	switch s.Network {
	case "none":
		args = append(args, "--network", "none")
	case "", "default":
	default:
		return nil, fmt.Errorf("unknown network mode %q", s.Network)
	}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', -1, 64))
	}
	if s.Memory != "" {
		args = append(args, "--memory", s.Memory, "--memory-swap", s.Memory)
	}
	for _, k := range sortedKeys(s.Labels) {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	for _, m := range s.Mounts {
		if strings.ContainsAny(m.Host+m.Container, ",\n\x00") || !strings.HasPrefix(m.Host, "/") || !strings.HasPrefix(m.Container, "/") {
			return nil, fmt.Errorf("mount %q → %q is not allowed", m.Host, m.Container)
		}
		v := "type=bind,source=" + m.Host + ",target=" + m.Container
		if m.ReadOnly {
			v += ",readonly"
		}
		args = append(args, "--mount", v)
	}
	args = append(args, s.Image)
	return append(args, s.Cmd...), nil
}

// Start implements Engine.
func (c *CLI) Start(ctx context.Context, id string) error {
	_, err := c.run(ctx, "start", id)
	return err
}

// Follow implements Engine.
func (c *CLI) Follow(ctx context.Context, id string, out, errOut io.Writer) (int, error) {
	// `logs --follow` returns when the container exits; `wait` then gives the exit code at once.
	if err := c.cmd(ctx, out, errOut, "logs", "--follow", id).Run(); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("following the container's output: %w", err)
	}
	text, err := c.run(ctx, "wait", id)
	if err != nil {
		return 0, err
	}
	code, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("unexpected answer from docker wait: %q", text)
	}
	return code, nil
}

// Stop implements Engine.
func (c *CLI) Stop(ctx context.Context, id string, grace time.Duration) error {
	_, err := c.run(ctx, "stop", "--time", strconv.Itoa(int(grace.Seconds())), id)
	return err
}

// Remove implements Engine.
func (c *CLI) Remove(ctx context.Context, id string) error {
	_, err := c.run(ctx, "rm", "--force", "--volumes", id)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
