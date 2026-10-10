// Package docker is the runner that builds a repository in containers and packs the result.
//
// It is the only place that knows about containers. It reads dymploy.yaml from the checked-out commit,
// runs each build step in an unprivileged container with the repository mounted in /work, and packs
// the files named by `artifact` into <name>.tar.gz. Everything it does is narrated through the task's
// Reporter, so the job's log tells what happened and in which order.
package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// Kind is the name this runner is registered under.
const Kind = "docker"

// Exit codes the runner itself reports for a job that failed for reasons of the repository.
const (
	ExitBadManifest = 2   // dymploy.yaml is missing or invalid
	ExitTimeout     = 124 // the build exceeded build.limits.timeout (same code as timeout(1))
	ExitNoArtifact  = 3   // the steps succeeded but the artifact could not be packed
)

// Source provides the working copy of the commit a task is about, and the event that caused the task.
// dir is where the repository is; release gives it back and must be called exactly once. The clone
// runner is the usual source.
type Source func(ctx context.Context, t runner.Task) (dir string, release func(), ev provider.Event, err error)

// Config sets up a Runner.
type Config struct {
	Engine Engine
	Source Source
	// ArtifactsDir receives <job>/<name>.tar.gz. CacheDir holds the build caches, per repository.
	ArtifactsDir string
	CacheDir     string
	// User ("uid:gid") the build containers run as: the daemon's own, so that what they write into
	// the working copy and the caches can be read and removed by it.
	User string
	// StopGrace is how long a container gets to stop before being killed; 0 means 10 s.
	StopGrace time.Duration
}

// Runner implements runner.Runner.
type Runner struct{ cfg Config }

// New returns a Docker runner.
func New(cfg Config) (*Runner, error) {
	if cfg.Engine == nil || cfg.Source == nil {
		return nil, errors.New("docker runner: an engine and a source are required")
	}
	for _, d := range []string{cfg.ArtifactsDir, cfg.CacheDir} {
		if !filepath.IsAbs(d) {
			return nil, errors.New("docker runner: the artifacts and cache directories must be absolute paths")
		}
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 10 * time.Second
	}
	return &Runner{cfg: cfg}, nil
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func safe(s string) string { return strings.Trim(unsafeChars.ReplaceAllString(s, "-"), "-.") }

// Run implements runner.Runner.
func (r *Runner) Run(ctx context.Context, t runner.Task) (runner.Result, error) {
	if err := r.cfg.Engine.Ping(ctx); err != nil {
		return runner.Result{}, err
	}
	dir, release, ev, err := r.cfg.Source(ctx, t)
	if err != nil {
		return runner.Result{}, err
	}
	defer release()
	log := t.Log

	m, bad := readManifest(dir)
	if bad != "" {
		log.Stderr().Write([]byte(bad + "\n"))
		log.System("dymploy.yaml is not usable: nothing was built")
		return runner.Result{ExitCode: ExitBadManifest}, nil
	}
	log.System("manifest: " + m.Summary())

	b := &build{r: r, t: t, m: m, ev: ev, dir: dir, id: safe(t.JobID)}
	if b.id == "" {
		return runner.Result{}, errors.New("the task has no usable job id")
	}
	return b.run(ctx)
}

// readManifest returns the manifest of the working copy, or a message for the log.
func readManifest(dir string) (*manifest.Manifest, string) {
	fsys := os.DirFS(dir)
	name, err := manifest.Find(fsys)
	if err != nil {
		return nil, err.Error()
	}
	// The file comes from the repository: it must be a plain file of it, not a link to something else.
	full := filepath.Join(dir, name)
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, name + " must be a regular file"
	}
	if fi.Size() > manifest.MaxSize {
		return nil, fmt.Sprintf("%s is too large (the limit is %d bytes)", name, manifest.MaxSize)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, "reading " + name + ": " + err.Error()
	}
	m, err := manifest.Parse(data)
	if err != nil {
		return nil, name + ": " + err.Error()
	}
	if err := m.CheckFiles(fsys); err != nil {
		return nil, name + ": " + err.Error()
	}
	return m, ""
}

type build struct {
	r   *Runner
	t   runner.Task
	m   *manifest.Manifest
	ev  provider.Event
	dir string
	id  string
}

func (b *build) env() map[string]string {
	e := map[string]string{
		"HOME":                 "/tmp",
		"DYMPLOY_COMMIT":       b.ev.Commit,
		"DYMPLOY_SHORT_COMMIT": b.ev.Commit[:min(12, len(b.ev.Commit))],
		"DYMPLOY_OWNER":        b.ev.Owner,
		"DYMPLOY_REPO":         b.ev.Repo,
		"DYMPLOY_JOB":          b.t.JobID,
	}
	if b.ev.Branch != "" {
		e["DYMPLOY_BRANCH"] = b.ev.Branch
	}
	if b.ev.Tag != "" {
		e["DYMPLOY_TAG"] = b.ev.Tag
	}
	return e
}

func (b *build) run(ctx context.Context) (runner.Result, error) {
	log, eng := b.t.Log, b.r.cfg.Engine
	lim := b.m.Build.Limits
	bctx, cancel := context.WithTimeout(ctx, lim.Timeout.D())
	defer cancel()

	// fail turns the end of bctx into the right outcome: a cancelled job is an error of the runner
	// (the queue marks it cancelled), an exceeded timeout is a failed build.
	fail := func(err error) (runner.Result, error) {
		if ctx.Err() != nil {
			return runner.Result{}, fmt.Errorf("build interrupted: %w", ctx.Err())
		}
		if bctx.Err() != nil {
			log.System(fmt.Sprintf("the build exceeded its time limit (%s) and was stopped", lim.Timeout.D()))
			return runner.Result{ExitCode: ExitTimeout}, nil
		}
		return runner.Result{}, err
	}

	image := b.m.Build.Image.From
	if image == "" {
		im := b.m.Build.Image
		var err error
		if image, err = b.buildImage(bctx, im); err != nil {
			return fail(err)
		}
		defer func() {
			cctx, c := context.WithTimeout(context.Background(), 30*time.Second)
			defer c()
			eng.RemoveImage(cctx, image)
		}()
	}

	caches, err := b.cacheMounts()
	if err != nil {
		return runner.Result{}, err
	}
	pulled := map[string]bool{}
	for i, s := range b.m.Build.Steps {
		img := image
		if s.Image != "" {
			img = s.Image
		}
		name := s.Name
		if name == "" {
			name = fmt.Sprintf("step %d", i+1)
		}
		if !pulled[img] && (s.Image != "" || b.m.Build.Image.From != "") {
			log.System("preparing image " + img)
			if err := eng.EnsureImage(bctx, img, log.Stderr()); err != nil {
				return fail(err)
			}
			pulled[img] = true
		}
		log.System(fmt.Sprintf("[%d/%d] %s (image %s)", i+1, len(b.m.Build.Steps), name, img))
		code, err := b.step(bctx, i, s, img, caches)
		if err != nil {
			return fail(err)
		}
		if code != 0 {
			log.System(fmt.Sprintf("[%d/%d] %s failed with exit code %d", i+1, len(b.m.Build.Steps), name, code))
			return runner.Result{ExitCode: code}, nil
		}
	}

	log.System("packing the artifact")
	res, err := artifact.Pack(b.dir, artifact.Spec{
		Name: b.m.Artifact.Name, Paths: b.m.Artifact.Paths, Exclude: b.m.Artifact.Exclude,
	}, filepath.Join(b.r.cfg.ArtifactsDir, b.id))
	if err != nil {
		log.Stderr().Write([]byte("artifact: " + err.Error() + "\n"))
		log.System("the build ran but its artifact could not be packed")
		return runner.Result{ExitCode: ExitNoArtifact}, nil
	}
	log.System(fmt.Sprintf("artifact %s: %d entries, %d bytes, sha256 %s", res.Path, res.Files, res.Size, res.SHA256))
	return runner.Result{}, nil
}

// buildImage builds build.image.dockerfile and returns the tag of the image, which belongs to this job.
func (b *build) buildImage(ctx context.Context, im manifest.Image) (string, error) {
	df, err := b.inside(im.Dockerfile)
	if err != nil {
		return "", fmt.Errorf("build.image.dockerfile: %w", err)
	}
	cx := b.dir
	if im.Context != "" && im.Context != "." {
		if cx, err = b.inside(im.Context); err != nil {
			return "", fmt.Errorf("build.image.context: %w", err)
		}
	}
	tag := "dymploy-build-" + strings.ToLower(b.id)
	b.t.Log.System(fmt.Sprintf("building the image from %s", im.Dockerfile))
	err = b.r.cfg.Engine.BuildImage(ctx, BuildSpec{Tag: tag, Dockerfile: df, Context: cx, Args: im.Args}, b.t.Log.Stdout(), b.t.Log.Stderr())
	return tag, err
}

// inside resolves a path of the repository and refuses anything that ends outside of it.
func (b *build) inside(rel string) (string, error) {
	root, err := filepath.EvalSymlinks(b.dir)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("%s does not exist", rel)
	}
	if r, err := filepath.Rel(root, p); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s leads outside the repository", rel)
	}
	return p, nil
}

type cacheMount struct{ host, container string }

func (b *build) cacheMounts() ([]cacheMount, error) {
	var out []cacheMount
	base := filepath.Join(b.r.cfg.CacheDir, safe(b.ev.Provider), safe(b.ev.Owner), safe(b.ev.Repo))
	for _, c := range b.m.Build.Cache {
		host := filepath.Join(base, c.Name)
		if err := os.MkdirAll(host, 0o700); err != nil {
			return nil, fmt.Errorf("creating cache %s: %w", c.Name, err)
		}
		out = append(out, cacheMount{host, c.Path})
	}
	return out, nil
}

// step runs one build step and returns its exit code.
func (b *build) step(ctx context.Context, i int, s manifest.Step, image string, caches []cacheMount) (int, error) {
	eng, log := b.r.cfg.Engine, b.t.Log
	env := map[string]string{}
	for _, layer := range []map[string]string{b.m.Build.Env, s.Env, b.env()} { // later layers win
		for k, v := range layer {
			env[k] = v
		}
	}
	workdir := "/work"
	if s.Workdir != "" {
		workdir = path.Join("/work", s.Workdir)
	}
	mounts := []Mount{{Host: b.dir, Container: "/work"}}
	for _, c := range caches {
		mounts = append(mounts, Mount{Host: c.host, Container: c.container})
	}
	spec := ContainerSpec{
		Name: fmt.Sprintf("dymploy-%s-%d", b.id, i+1), Image: image, Cmd: []string{"sh", "-ec", s.Run},
		Workdir: workdir, User: b.r.cfg.User, Env: env, Mounts: mounts, Network: b.m.Build.Network,
		Memory: b.m.Build.Limits.Memory, Labels: map[string]string{"dymploy.job": b.t.JobID},
	}
	if c := b.m.Build.Limits.CPUs; c != nil {
		spec.CPUs = *c
	}
	id, err := eng.Create(ctx, spec)
	if err != nil {
		return 0, err
	}
	// Whatever happens, the container is stopped and removed, also when ctx is already over.
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), b.r.cfg.StopGrace+30*time.Second)
		defer c()
		if err := eng.Stop(cctx, id, b.r.cfg.StopGrace); err != nil {
			log.System("could not stop the container (it may have already exited): " + err.Error())
		}
		if err := eng.Remove(cctx, id); err != nil {
			log.System("could not remove the container: " + err.Error())
		}
	}()
	if err := eng.Start(ctx, id); err != nil {
		return 0, err
	}
	return eng.Follow(ctx, id, b.t.Log.Stdout(), b.t.Log.Stderr())
}
