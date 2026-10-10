package cmd

import (
	"fmt"
	"log/slog"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/daemon"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/provider/github"
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
			hooks, err := buildReceiver(cfg, log)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			d := daemon.New(cfg, version)
			if hooks != nil {
				d.Handle("/hooks/", hooks.Handler())
			}
			return d.Run(ctx, nil)
		},
	}
	configFlag(c, &configPath)
	return c
}

// buildReceiver is the composition root for webhooks: the only place that decides which provider
// implementation serves each configured name. It returns nil when no webhooks are configured.
func buildReceiver(cfg *config.Config, log *slog.Logger) (*webhook.Receiver, error) {
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
		Queue:     webhook.LogQueue{Log: log}, // replaced by the real job queue
		Dedupe:    webhook.NewDedupe(24*time.Hour, 10000),
		Limiter:   webhook.NewLimiter(cfg.WebhookRatePerMinute, time.Minute),
		MaxBody:   cfg.WebhookMaxBody,
		Log:       log,
	}, nil
}
