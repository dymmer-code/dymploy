package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/checkout"
	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/daemon"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/deploy/secretcmd"
	"github.com/dymmer-code/dymploy/internal/deploy/sshremote"
	"github.com/dymmer-code/dymploy/internal/intake"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/provider/github"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/clone"
	"github.com/dymmer-code/dymploy/internal/runner/docker"
	"github.com/dymmer-code/dymploy/internal/runner/dryrun"
	"github.com/dymmer-code/dymploy/internal/secret"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

// extraRunners lets tests register runners of their own (for example one that never ends).
var extraRunners []func(*runner.Registry)

func newServeCmd(version string) *cobra.Command {
	var configPath string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon (webhooks, queue and control socket)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(c.ErrOrStderr(), nil))
			// ctx lives as long as the daemon does; the lifecycle decides when that is (see lifecycle).
			ctx, finish := context.WithCancel(context.Background())
			defer finish()

			providers, err := buildProviders(cfg)
			if err != nil {
				return err
			}
			remote, targets := deploy.Remote(&sshremote.Remote{}), deploy.NewTargets()
			if err := applyTargets(cfg, remote, targets); err != nil {
				return err
			}
			deployer := &deploy.Deployer{Remote: remote, Targets: targets, Secrets: secretcmd.New()}
			mgr, repos, dockerRunner, closeState, err := buildQueue(cfg, log, providers, deployer)
			if err != nil {
				return err
			}
			defer closeState()
			if err := mgr.Start(ctx); err != nil {
				return err
			}
			pruneCtx, stopPrune := context.WithCancel(ctx)
			defer stopPrune()
			go mgr.PruneLoop(pruneCtx, time.Hour, time.Duration(cfg.Queue.KeepDays)*24*time.Hour, cfg.Queue.KeepJobs)
			if repos != nil && cfg.RepoKeepDays > 0 {
				go cleanRepos(pruneCtx, repos, time.Duration(cfg.RepoKeepDays)*24*time.Hour, log)
			}

			if dockerRunner != nil && (cfg.CacheKeepDays > 0 || cfg.CacheMaxMB > 0) {
				go cleanCaches(pruneCtx, dockerRunner, time.Duration(cfg.CacheKeepDays)*24*time.Hour, int64(cfg.CacheMaxMB)<<20, log)
			}
			if cfg.ArtifactKeepDays > 0 {
				go cleanArtifacts(pruneCtx, filepath.Join(cfg.DataDir, "artifacts"), time.Duration(cfg.ArtifactKeepDays)*24*time.Hour, log)
			}

			enq := intake.New(mgr, cfg.Runner.Kind, cfg.Runner.ParamsJSON()).Fresh(tipCheck(cfg, providers, log))
			hooks := buildReceiver(cfg, log, providers, enq)
			jobs := queueJobs{m: mgr, log: log, providers: providers, hooks: hooks, enq: enq}
			rl := &reloader{path: configPath, cur: cfg, hooks: hooks, remote: remote, targets: targets, mgr: mgr, log: log}
			reloadOnSIGHUP(ctx, rl)
			d := daemon.New(cfg, version).OnReload(rl.Reload).WithJobs(jobs)
			if err := mountAPI(d, cfg, jobs, log); err != nil {
				return err
			}
			if hooks != nil {
				d.Handle("/hooks/", hooks.Handler())
			}
			lc := &lifecycle{mgr: mgr, d: d, log: log, timeout: cfg.ShutdownTimeout, finish: finish}
			d.OnStop(lc.Stop).WithStats(func() (int, int) { s := mgr.Stats(); return s.Queued, s.Running })
			lc.handleSignals(c.Context(), ctx)
			runErr := d.Run(ctx, nil)
			stopPrune()
			if err := shutdownQueue(mgr, cfg.ShutdownTimeout); err != nil && runErr == nil {
				runErr = err
			}
			return runErr
		},
	}
	configFlag(c, &configPath)
	return c
}

func shutdownQueue(mgr *queue.Manager, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return mgr.Shutdown(ctx)
}

// buildQueue is the composition root for execution: it decides which runners exist, where state and
// logs live, and returns the queue, the working-copy manager (nil when git is not available and no
// job needs it) and a function that releases the state.
func buildQueue(cfg *config.Config, log *slog.Logger, providers map[string]provider.Provider, deployer *deploy.Deployer) (*queue.Manager, *checkout.Manager, *docker.Runner, func(), error) {
	runners := runner.NewRegistry()
	runners.MustRegister(dryrun.Kind, dryrun.New())
	for _, add := range extraRunners {
		add(runners)
	}
	var dockerRunner *docker.Runner
	repos, err := checkout.New(checkout.Config{Root: filepath.Join(cfg.DataDir, "repos")})
	switch {
	case err == nil:
		finder := func(name string) (provider.Cloner, bool) {
			c, ok := providers[name].(provider.Cloner)
			return c, ok
		}
		cl := clone.New(repos, finder)
		runners.MustRegister(clone.Kind, cl)
		dr, err := registerDocker(runners, cl, cfg, deployer)
		switch {
		case err != nil && cfg.Runner.Kind == docker.Kind:
			return nil, nil, nil, nil, fmt.Errorf("runner.kind %q: %w", docker.Kind, err)
		case err != nil:
			log.Warn("docker is not available: build jobs cannot run", "error", err)
		default:
			dockerRunner = dr
		}
	case cfg.Runner.Kind == clone.Kind || cfg.Runner.Kind == docker.Kind:
		return nil, nil, nil, nil, fmt.Errorf("runner.kind %q needs git: %w", cfg.Runner.Kind, err)
	default:
		log.Warn("git is not available: jobs that clone repositories cannot run", "error", err)
		repos = nil
	}
	if _, err := runners.Get(cfg.Runner.Kind); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("runner.kind: %w (available: %s)", err, strings.Join(runners.Kinds(), ", "))
	}
	store, err := job.OpenBolt(filepath.Join(cfg.DataDir, "state", "jobs.db"))
	if err != nil {
		return nil, nil, nil, nil, err
	}
	logs, err := job.NewFileLogs(filepath.Join(cfg.DataDir, "logs"), cfg.Queue.LogMaxBytes)
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	var observer func(queue.Event)
	if rep := newStatusReporter(cfg, providers, log); rep != nil {
		observer = rep.Observe
		log.Info("the state of every run will be shown next to its commit")
	}
	mgr, err := queue.New(queue.Config{
		MaxConcurrent: cfg.Queue.MaxConcurrent, MaxPending: cfg.Queue.MaxPending, OnBusy: queue.Policy(cfg.Queue.OnBusy),
		StopGrace: cfg.Queue.StopGrace,
	}, queue.Deps{Store: store, Logs: logs, Runners: runners, Log: log, Observer: observer})
	if err != nil {
		store.Close()
		return nil, nil, nil, nil, err
	}
	return mgr, repos, dockerRunner, func() { store.Close() }, nil
}

// minTokenLength keeps the API token out of guessing range.
const minTokenLength = 32

// mountAPI opens the read-only API on the public listener when a token is configured. Without one
// nothing is mounted: the jobs are then reachable only through the control socket.
func mountAPI(d *daemon.Daemon, cfg *config.Config, jobs control.Jobs, log *slog.Logger) error {
	if cfg.API.TokenFile == "" {
		return nil
	}
	token, err := secret.ReadFile(cfg.API.TokenFile)
	if err != nil {
		return fmt.Errorf("api.token_file: %w", err)
	}
	if len(token) < minTokenLength {
		return fmt.Errorf("api.token_file: the token is too short (%d characters, at least %d): generate one with `openssl rand -hex 32`", len(token), minTokenLength)
	}
	api := &control.API{Jobs: jobs, ReadOnly: true, OriginPatterns: cfg.API.AllowedOrigins, MaxStreams: cfg.API.MaxStreams, Closing: d.Closing()}
	d.Handle("/api/", http.StripPrefix("/api", control.RequireToken(token, api.Handler(), func(r *http.Request) {
		log.Warn("API request refused: missing or wrong token", "remote", r.RemoteAddr, "path", r.URL.Path)
	})))
	return nil
}

// applyTargets turns the configured targets into the set the deployer uses. Every one is checked
// first (its key and known_hosts must exist and be usable); a target that cannot work rejects the
// whole set, so what is in force is never half of what was asked.
func applyTargets(cfg *config.Config, remote deploy.Remote, into *deploy.Targets) error {
	set := make(map[string]deploy.Target, len(cfg.Targets))
	names := make([]string, 0, len(cfg.Targets))
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tc := cfg.Targets[name]
		t := deploy.Target{
			Name: name, Host: tc.Host, Port: tc.Port, User: tc.User, KeyFile: tc.KeyFile, KnownHosts: tc.KnownHosts,
			UploadDir: tc.UploadDir, Allow: webhook.Policy{Rules: rulesOf(tc.Allow)},
		}
		if tc.Secrets != nil {
			t.Secrets = &deploy.SecretsSpec{Command: tc.Secrets.Command, EnvFile: tc.Secrets.EnvFile, Timeout: tc.Secrets.Timeout}
			if err := secretcmd.Check(*t.Secrets); err != nil {
				return fmt.Errorf("targets.%s.secrets: %w", name, err)
			}
		}
		if err := remote.Check(t); err != nil {
			return fmt.Errorf("targets.%s: %w", name, err)
		}
		set[name] = t
	}
	into.Set(set)
	return nil
}

func rulesOf(in []config.PolicyRule) []webhook.Rule {
	rules := make([]webhook.Rule, len(in))
	for i, r := range in {
		rules[i] = webhook.Rule(r)
	}
	return rules
}

// registerDocker adds the build runner, which needs the docker command and works on top of the clone
// runner's checkouts. The engine itself is only contacted when a job runs.
func registerDocker(runners *runner.Registry, cl *clone.Runner, cfg *config.Config, deployer *deploy.Deployer) (*docker.Runner, error) {
	if _, err := docker.ParseParams(cfg.Runner.ParamsJSON()); err != nil && cfg.Runner.Kind == docker.Kind {
		return nil, err
	}
	engine, err := docker.NewCLI("")
	if err != nil {
		return nil, err
	}
	source := func(ctx context.Context, t runner.Task) (string, func(), provider.Event, error) {
		co, ev, err := cl.Prepare(ctx, t)
		if err != nil {
			return "", nil, ev, err
		}
		return co.Dir, co.Release, ev, nil
	}
	run, err := docker.New(docker.Config{
		Engine: engine, Source: source, Deployer: deployer,
		ArtifactsDir: filepath.Join(cfg.DataDir, "artifacts"), CacheDir: filepath.Join(cfg.DataDir, "cache"),
		User: strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
	})
	if err != nil {
		return nil, err
	}
	runners.MustRegister(docker.Kind, run)
	return run, nil
}

// cleanArtifacts removes the artifacts of old builds, once an hour.
func cleanArtifacts(ctx context.Context, dir string, olderThan time.Duration, log *slog.Logger) {
	runEvery(ctx, time.Hour, func() { cleanArtifactsOnce(dir, olderThan, log) })
}

func cleanArtifactsOnce(dir string, olderThan time.Duration, log *slog.Logger) {
	switch n, err := artifact.Clean(dir, olderThan); {
	case err != nil:
		log.Warn("cleaning old artifacts failed", "error", err)
	case n > 0:
		log.Info("old artifacts removed", "removed", n)
	}
}

// cleanCaches removes the build caches nobody uses any more, or that grew too much, once an hour (and once
// at start, which is when a long-stopped server catches up).
func cleanCaches(ctx context.Context, r *docker.Runner, olderThan time.Duration, maxBytes int64, log *slog.Logger) {
	once := func() {
		switch rep, err := r.CleanCaches(olderThan, maxBytes); {
		case err != nil:
			log.Warn("cleaning build caches failed", "error", err)
		case rep.Expired+rep.OverLimit > 0:
			log.Info("build caches removed", "unused", rep.Expired, "over the limit", rep.OverLimit)
		}
	}
	once()
	runEvery(ctx, time.Hour, once)
}

// cleanRepos removes working copies nobody has used for a while, once an hour.
func cleanRepos(ctx context.Context, repos *checkout.Manager, olderThan time.Duration, log *slog.Logger) {
	runEvery(ctx, time.Hour, func() { cleanOnce(ctx, repos, olderThan, log) })
}

func cleanOnce(ctx context.Context, repos *checkout.Manager, olderThan time.Duration, log *slog.Logger) {
	n, err := repos.Clean(ctx, olderThan)
	switch {
	case err != nil:
		log.Warn("cleaning old working copies failed", "error", err)
	case n > 0:
		log.Info("old working copies removed", "removed", n)
	}
}

// runEvery calls f every interval until ctx ends.
func runEvery(ctx context.Context, interval time.Duration, f func()) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			f()
		}
	}
}

// buildProviders is the composition root for repository providers: the only place that decides which
// implementation serves each configured name. It reads their secrets and, for GitHub, the App key.
func buildProviders(cfg *config.Config) (map[string]provider.Provider, error) {
	names := make([]string, 0, len(cfg.Webhooks))
	for name := range cfg.Webhooks {
		names = append(names, name)
	}
	sort.Strings(names)
	providers := map[string]provider.Provider{}
	for _, name := range names {
		wc := cfg.Webhooks[name]
		switch name {
		case "github":
			sec, err := secret.ReadFile(wc.SecretFile)
			if err != nil {
				return nil, fmt.Errorf("webhooks.%s: %w", name, err)
			}
			p, err := github.New(sec)
			if err != nil {
				return nil, fmt.Errorf("webhooks.%s: %w", name, err)
			}
			if wc.AppID != 0 {
				pemText, err := secret.ReadFile(wc.PrivateKeyFile)
				if err != nil {
					return nil, fmt.Errorf("webhooks.%s.private_key_file: %w", name, err)
				}
				key, err := github.ParsePrivateKey([]byte(pemText))
				if err != nil {
					return nil, fmt.Errorf("webhooks.%s.private_key_file %s: %w", name, wc.PrivateKeyFile, err)
				}
				app, err := github.NewApp(wc.AppID, key)
				if err != nil {
					return nil, fmt.Errorf("webhooks.%s: %w", name, err)
				}
				p = p.WithApp(app)
			}
			providers[name] = p
		default:
			return nil, fmt.Errorf("webhooks.%s: unsupported provider (supported: github)", name)
		}
	}
	return providers, nil
}

// tipCheck builds the check that a push is not stale. The setting is read from the configuration in force
// when the daemon started; changing it needs a restart, like every setting a reload does not apply. A
// provider that cannot be asked (on GitHub, no App configured) is not checked, and the log says so.
func tipCheck(cfg *config.Config, providers map[string]provider.Provider, log *slog.Logger) func(context.Context, provider.Event) error {
	find := func(name string) (provider.Resolver, bool) {
		r, ok := providers[name].(provider.Resolver)
		if !ok {
			return nil, false
		}
		if rd, ok := providers[name].(provider.Readier); ok && !rd.Ready() {
			return nil, false
		}
		return r, true
	}
	for name, w := range cfg.Webhooks {
		if _, ok := find(name); !ok && w.VerifiesTip() {
			log.Warn("deliveries of this provider cannot be checked against it, so a replayed one would not be detected (configure its App)", "provider", name)
		}
	}
	enabled := func(name string) bool { return cfg.Webhooks[name].VerifiesTip() }
	return intake.Tip(find, enabled)
}

// buildReceiver assembles the webhook receiver; it returns nil when no webhooks are configured.
func buildReceiver(cfg *config.Config, log *slog.Logger, providers map[string]provider.Provider, q webhook.Enqueuer) *webhook.Receiver {
	if len(providers) == 0 {
		return nil
	}
	return &webhook.Receiver{
		Providers:      providers,
		Policy:         policyRules(cfg),
		Queue:          q,
		Dedupe:         webhook.NewDedupe(24*time.Hour, 10000),
		Limiter:        webhook.NewLimiter(cfg.WebhookRatePerMinute, time.Minute),
		IPLimiter:      webhook.NewIPLimiter(cfg.WebhookRatePerIPPerMinute, time.Minute),
		TrustedProxies: cfg.TrustedPrefixes(),
		MaxBody:        cfg.WebhookMaxBody,
		Log:            log,
	}
}
