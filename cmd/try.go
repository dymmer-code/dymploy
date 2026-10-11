package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/deploy/sshremote"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/docker"
)

func newTryCmd() *cobra.Command {
	var configPath, name, ref, out string
	var doDeploy, keep bool
	c := &cobra.Command{
		Use:   "try [directory]",
		Short: "Run a repository's dymploy.yaml here, the way a job would, without webhook, clone or daemon",
		Long: `Builds what git tracks in the directory (plus changes and new files not ignored: your dymploy.yaml does
not need to be committed yet) in a throw-away copy, using the same runner as the daemon: containers, limits,
caches, artifact. The log is shown as it happens. The deploy section is left alone unless you ask for it
with --deploy, which needs a configuration with the target (the target's "allow" rules are applied to the
repository name and branch given by --name and --ref, so a try cannot do what a push could not).

It is for writing and checking a dymploy.yaml. It needs Docker, like the daemon.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			// Ctrl-C asks the containers to stop, like any other stop; it does not just kill the command.
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runTry(ctx, c.OutOrStdout(), tryOptions{
				dir: dir, configPath: configPath, name: name, ref: ref, out: out, deploy: doDeploy, keep: keep,
			})
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "configuration to take the targets (for --deploy) and the runner limits from")
	c.Flags().StringVar(&name, "name", "", "repository as provider/owner/repo (default: from the origin remote)")
	c.Flags().StringVar(&ref, "ref", "", "branch the run pretends to be for (default: the current branch)")
	c.Flags().StringVar(&out, "out", "", "where artifacts and caches are kept (default: under the user cache directory)")
	c.Flags().BoolVar(&doDeploy, "deploy", false, "also deliver the artifact to the target the definition names")
	c.Flags().BoolVar(&keep, "keep", false, "keep the throw-away copy of the sources, to look at what was built")
	return c
}

type tryOptions struct {
	dir, configPath, name, ref, out string
	deploy, keep                    bool
}

var remoteRE = regexp.MustCompile(`^(?:git@|ssh://git@|https://)([^:/]+)[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// identify works out which repository and branch a directory is, for the event the run pretends to be.
func identify(ctx context.Context, dir, name, ref string) (provider.Event, error) {
	commit, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return provider.Event{}, fmt.Errorf("%s is not a git repository with a commit: try builds what git tracks, like a job would", dir)
	}
	ev := provider.Event{Kind: provider.KindPush, Commit: strings.TrimSpace(commit)}
	if name == "" {
		remote, _ := git(ctx, dir, "remote", "get-url", "origin")
		m := remoteRE.FindStringSubmatch(strings.TrimSpace(remote))
		switch {
		case m != nil && m[1] == "github.com":
			name = "github/" + m[2] + "/" + m[3]
		default:
			abs, _ := filepath.Abs(dir)
			name = "local/local/" + filepath.Base(abs)
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return provider.Event{}, fmt.Errorf("--name must be provider/owner/repo, got %q", name)
	}
	ev.Provider, ev.Owner, ev.Repo = parts[0], parts[1], parts[2]
	ev.FullName = ev.Owner + "/" + ev.Repo
	if ref == "" {
		b, _ := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
		if ref = strings.TrimSpace(b); ref == "" || ref == "HEAD" {
			ref = "main"
		}
	}
	ev.Ref = "refs/heads/" + ref
	ev.Branch = ref
	return ev, nil
}

// snapshot copies what git tracks in dir, plus new files that are not ignored, into a fresh directory.
// Ignored files (signing keys, build output, node_modules…) never go in, which is also what makes a
// try as clean as a clone.
func snapshot(ctx context.Context, dir, parent string) (string, error) {
	list, err := git(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	// Under the cache directory and not in the system temporary one: with a container engine that runs in a
	// virtual machine (Colima, Docker Desktop) only the user's home is visible to the containers.
	dst, err := os.MkdirTemp(parent, "src-")
	if err != nil {
		return "", err
	}
	for _, rel := range strings.Split(list, "\x00") {
		if rel == "" {
			continue
		}
		src := filepath.Join(dir, rel)
		fi, err := os.Lstat(src)
		if err != nil || fi.IsDir() { // deleted but still listed, or a submodule
			continue
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			os.RemoveAll(dst)
			return "", err
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(src)
			if err == nil {
				err = os.Symlink(l, target)
			}
			if err != nil {
				os.RemoveAll(dst)
				return "", err
			}
		case fi.Mode().IsRegular():
			if err := copyFile(src, target, fi.Mode().Perm()); err != nil {
				os.RemoveAll(dst)
				return "", err
			}
		}
	}
	return dst, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func runTry(ctx context.Context, w io.Writer, o tryOptions) error {
	engine, err := docker.NewCLI("")
	if err != nil {
		return err
	}
	ev, err := identify(ctx, o.dir, o.name, o.ref)
	if err != nil {
		return err
	}
	cfg := config.Default()
	if o.configPath != "" {
		if cfg, err = config.Load(o.configPath); err != nil {
			return err
		}
	} else if o.deploy {
		return errors.New("--deploy needs --config: the targets live in the configuration")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	root := filepath.Join(cache, "dymploy", "try")
	if o.out != "" {
		root = o.out
	}
	if root, err = filepath.Abs(root); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}

	sources := filepath.Join(root, "sources")
	if err := os.MkdirAll(sources, 0o700); err != nil {
		return err
	}
	src, err := snapshot(ctx, o.dir, sources)
	if err != nil {
		return err
	}
	release := func() { os.RemoveAll(src) }
	if o.keep {
		release = func() { fmt.Fprintf(w, "# the sources of this run are in %s\n", src) }
	}

	dc := docker.Config{
		Engine: engine, ArtifactsDir: filepath.Join(root, "artifacts"), CacheDir: filepath.Join(root, "cache"),
		User: strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), NoDeploy: !o.deploy,
		Source: func(context.Context, runner.Task) (string, func(), provider.Event, error) {
			return src, release, ev, nil
		},
	}
	if o.deploy {
		targets := deploy.NewTargets()
		remote := deploy.Remote(&sshremote.Remote{})
		if err := applyTargets(cfg, remote, targets); err != nil {
			return err
		}
		dc.Deployer = &deploy.Deployer{Remote: remote, Targets: targets}
	}
	r, err := docker.New(dc)
	if err != nil {
		return err
	}
	var params []byte
	if cfg.Runner.Kind == docker.Kind {
		params = cfg.Runner.ParamsJSON()
	}

	id := "try-" + strconv.FormatInt(time.Now().Unix(), 10)
	fmt.Fprintf(w, "# trying %s (%s at %.12s); artifacts and caches under %s\n", ev.Provider+"/"+ev.FullName, ev.Branch, ev.Commit, root)
	var seq int64
	rep := runner.NewLineReporter(func(s runner.Stream, text string) {
		seq++
		fmt.Fprintln(w, entryLine(job.Entry{Seq: seq, Time: time.Now(), Stream: s, Text: text}))
	})
	res, err := r.Run(ctx, runner.Task{JobID: id, Name: ev.Provider + "/" + ev.FullName, Kind: docker.Kind, Params: params, Log: rep})
	rep.Close()
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return errors.New("interrupted")
	case err != nil:
		return err
	case res.ExitCode != 0:
		return fmt.Errorf("the run failed with exit code %d", res.ExitCode)
	}
	fmt.Fprintln(w, "# the run succeeded")
	return nil
}
