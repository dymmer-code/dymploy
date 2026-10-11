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
	"github.com/dymmer-code/dymploy/internal/deploy"
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
	// Deployer, if set, delivers the artifact to the target a definition names. Without it a definition
	// that asks to deploy fails before anything is built.
	Deployer *deploy.Deployer
	// NoDeploy builds and packs but does not deliver: a definition's deploy section is left alone (not
	// even its target is looked at). For trying a definition out.
	NoDeploy bool
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
	params, err := ParseParams(t.Params)
	if err != nil {
		return runner.Result{}, err
	}
	src := t
	src.Params = params.cloneParams() // the clone does not know the runner's own settings
	dir, release, ev, err := r.cfg.Source(ctx, src)
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

	var target deploy.Target
	if m.HasDeploy() && r.cfg.NoDeploy {
		log.System("this definition deploys to " + m.Deploy.Target + ": not delivered in this run")
	} else if m.HasDeploy() {
		// Checked before building: a definition that asks for a target it cannot use must fail at once.
		if r.cfg.Deployer == nil {
			log.Stderr().Write([]byte("this server is not set up to deploy\n"))
			return runner.Result{ExitCode: deploy.ExitTargetDenied}, nil
		}
		var err error
		if target, err = r.cfg.Deployer.Authorize(m.Deploy.Target, ev); err != nil {
			log.Stderr().Write([]byte(err.Error() + "\n"))
			log.System("nothing was built")
			return runner.Result{ExitCode: deploy.ExitTargetDenied}, nil
		}
	}

	limits, err := params.resolve(m.Build.Limits)
	if err != nil {
		log.Stderr().Write([]byte(err.Error() + "\n"))
		log.System("nothing was built")
		return runner.Result{ExitCode: ExitBadManifest}, nil
	}

	b := &build{r: r, t: t, m: m, ev: ev, dir: dir, id: safe(t.JobID), limits: limits}
	if b.id == "" {
		return runner.Result{}, errors.New("the task has no usable job id")
	}
	res, err := b.run(ctx)
	if err != nil || res.ExitCode != 0 || !m.HasDeploy() || r.cfg.NoDeploy {
		return res, err
	}
	return b.deploy(ctx, target)
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

	limits effective
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
	bctx, cancel := context.WithTimeout(ctx, b.limits.timeout)
	defer cancel()

	// fail turns the end of bctx into the right outcome: a cancelled job is an error of the runner
	// (the queue marks it cancelled), an exceeded timeout is a failed build.
	fail := func(err error) (runner.Result, error) {
		if ctx.Err() != nil {
			return runner.Result{}, fmt.Errorf("build interrupted: %w", ctx.Err())
		}
		if bctx.Err() != nil {
			log.System(fmt.Sprintf("the build exceeded its time limit (%s) and was stopped", b.limits.timeout))
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

	if !b.m.HasArtifact() {
		log.System("no artifact declared: nothing to pack")
		return runner.Result{}, nil
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
	rec := artifact.Record{
		Name: b.m.Artifact.Name, File: filepath.Base(res.Path), SHA256: res.SHA256, Size: res.Size, Entries: res.Files,
		Provider: b.ev.Provider, Owner: b.ev.Owner, Repo: b.ev.Repo, Commit: b.ev.Commit, Ref: b.ev.Ref,
		Job: b.t.JobID, CreatedAt: time.Now().UTC(),
	}
	if err := artifact.WriteRecord(filepath.Dir(res.Path), rec); err != nil {
		return runner.Result{}, fmt.Errorf("writing the artifact record: %w", err)
	}
	return runner.Result{}, nil
}

// deploy delivers the artifact just built. It reads it back through its record, so what is deployed
// is verified to be exactly what was packed.
func (b *build) deploy(ctx context.Context, target deploy.Target) (runner.Result, error) {
	rec, file, err := artifact.ReadRecord(filepath.Join(b.r.cfg.ArtifactsDir, b.id))
	if err != nil {
		return runner.Result{}, err
	}
	code, err := b.r.cfg.Deployer.Run(ctx, deploy.Request{
		Target: target, Deploy: b.m.Deploy, Event: b.ev, JobID: b.t.JobID, Record: rec, File: file, Log: b.t.Log,
	})
	return runner.Result{ExitCode: code}, err
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
	err = b.r.cfg.Engine.BuildImage(ctx, BuildSpec{Tag: tag, Dockerfile: df, Context: cx, Args: im.Args, Memory: b.limits.memory, CPUs: b.limits.cpus}, b.t.Log.Stdout(), b.t.Log.Stderr())
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
	// The git directory is read-only inside the build: the daemon runs git on this working copy
	// afterwards, and a configuration or hook planted by the build would run on the host.
	if fi, err := os.Lstat(filepath.Join(b.dir, ".git")); err == nil && fi.IsDir() {
		mounts = append(mounts, Mount{Host: filepath.Join(b.dir, ".git"), Container: "/work/.git", ReadOnly: true})
	}
	for _, c := range caches {
		mounts = append(mounts, Mount{Host: c.host, Container: c.container})
	}
	spec := ContainerSpec{
		Name: fmt.Sprintf("dymploy-%s-%d", b.id, i+1), Image: image, Cmd: []string{"sh", "-ec", s.Run},
		Workdir: workdir, User: b.r.cfg.User, Env: env, Mounts: mounts, Network: b.m.Build.Network,
		Memory: b.limits.memory, CPUs: b.limits.cpus, Labels: map[string]string{"dymploy.job": b.t.JobID},
	}
	id, err := eng.Create(ctx, spec)
	if err != nil {
		return 0, err
	}
	// Whatever happens, the container ends up stopped and removed, also when ctx is already over.
	defer func() {
		cctx, c := context.WithTimeout(context.Background(), b.r.cfg.StopGrace+30*time.Second)
		defer c()
		_ = eng.Stop(cctx, id, b.r.cfg.StopGrace) // a no-op when it already ended
		if err := eng.Remove(cctx, id); err != nil {
			log.System("could not remove the container: " + err.Error())
		}
	}()
	if err := eng.Start(ctx, id); err != nil {
		return 0, err
	}

	// A stop is a request: when ctx ends the container is asked to stop (SIGTERM, then SIGKILL after the
	// grace period) and its output keeps being followed until it exits, so whatever it says while
	// cleaning up reaches the log. Following is therefore not tied to ctx.
	fctx, fcancel := context.WithCancel(context.Background())
	defer fcancel()
	ended := make(chan struct{})
	stopper := make(chan struct{})
	go func() {
		defer close(stopper)
		select {
		case <-ended:
		case <-ctx.Done():
			log.System(fmt.Sprintf("stopping the container: it gets %s to end before it is killed", b.r.cfg.StopGrace))
			cctx, c := context.WithTimeout(context.Background(), b.r.cfg.StopGrace+30*time.Second)
			defer c()
			if err := eng.Stop(cctx, id, b.r.cfg.StopGrace); err != nil {
				log.System("could not stop the container: " + err.Error())
			}
			// If it still has not ended, stop following: the deferred removal forces the issue.
			select {
			case <-ended:
			case <-time.After(b.r.cfg.StopGrace + 10*time.Second):
				fcancel()
			}
		}
	}()
	code, err := eng.Follow(fctx, id, b.t.Log.Stdout(), b.t.Log.Stderr())
	close(ended)
	<-stopper // it may still be writing to the log: wait before the job's reporter is closed
	if ctx.Err() != nil {
		return 0, ctx.Err() // however it ended, the work was interrupted
	}
	return code, err
}
