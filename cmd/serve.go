package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/daemon"
	"github.com/dymmer-code/dymploy/internal/intake"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/provider/github"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/dryrun"
	"github.com/dymmer-code/dymploy/internal/secret"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

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
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			mgr, closeState, err := buildQueue(cfg, log)
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

			params, err := cfg.Runner.ParamsJSON()
			if err != nil {
				return fmt.Errorf("runner.params: %w", err)
			}
			hooks, err := buildReceiver(cfg, log, intake.New(mgr, cfg.Runner.Kind, params))
			if err != nil {
				_ = shutdownQueue(mgr)
				return err
			}
			d := daemon.New(cfg, version)
			if hooks != nil {
				d.Handle("/hooks/", hooks.Handler())
			}
			runErr := d.Run(ctx, nil)
			stopPrune()
			if err := shutdownQueue(mgr); err != nil && runErr == nil {
				runErr = err
			}
			return runErr
		},
	}
	configFlag(c, &configPath)
	return c
}

func shutdownQueue(mgr *queue.Manager) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return mgr.Shutdown(ctx)
}

// buildQueue is the composition root for execution: it decides which runners exist, where state and
// logs live, and returns the queue plus a function that releases the state.
func buildQueue(cfg *config.Config, log *slog.Logger) (*queue.Manager, func(), error) {
	runners := runner.NewRegistry()
	if err := runners.Register(dryrun.Kind, dryrun.New()); err != nil {
		return nil, nil, err
	}
	if _, err := runners.Get(cfg.Runner.Kind); err != nil {
		return nil, nil, fmt.Errorf("runner.kind: %w (available: %s)", err, strings.Join(runners.Kinds(), ", "))
	}
	store, err := job.OpenBolt(filepath.Join(cfg.DataDir, "state", "jobs.db"))
	if err != nil {
		return nil, nil, err
	}
	logs, err := job.NewFileLogs(filepath.Join(cfg.DataDir, "logs"), cfg.Queue.LogMaxBytes)
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	mgr, err := queue.New(queue.Config{
		MaxConcurrent: cfg.Queue.MaxConcurrent, MaxPending: cfg.Queue.MaxPending, OnBusy: queue.Policy(cfg.Queue.OnBusy),
	}, queue.Deps{Store: store, Logs: logs, Runners: runners, Log: log})
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return mgr, func() { store.Close() }, nil
}

// buildReceiver is the composition root for webhooks: the only place that decides which provider
// implementation serves each configured name. It returns nil when no webhooks are configured.
func buildReceiver(cfg *config.Config, log *slog.Logger, q webhook.Enqueuer) (*webhook.Receiver, error) {
	if len(cfg.Webhooks) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(cfg.Webhooks))
	for name := range cfg.Webhooks {
		names = append(names, name)
	}
	sort.Strings(names)
	providers := map[string]provider.Provider{}
	for _, name := range names {
		switch name {
		case "github":
			sec, err := secret.ReadFile(cfg.Webhooks[name].SecretFile)
			if err != nil {
				return nil, fmt.Errorf("webhooks.%s: %w", name, err)
			}
			p, err := github.New(sec)
			if err != nil {
				return nil, fmt.Errorf("webhooks.%s: %w", name, err)
			}
			providers[name] = p
		default:
			return nil, fmt.Errorf("webhooks.%s: unsupported provider (supported: github)", name)
		}
	}
	rules := make([]webhook.Rule, len(cfg.Policy))
	for i, r := range cfg.Policy {
		rules[i] = webhook.Rule(r)
	}
	return &webhook.Receiver{
		Providers: providers,
		Policy:    webhook.Policy{Rules: rules},
		Queue:     q,
		Dedupe:    webhook.NewDedupe(24*time.Hour, 10000),
		Limiter:   webhook.NewLimiter(cfg.WebhookRatePerMinute, time.Minute),
		MaxBody:   cfg.WebhookMaxBody,
		Log:       log,
	}, nil
}
