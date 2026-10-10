package cmd

import (
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/daemon"
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
			ctx, stop := signal.NotifyContext(c.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return daemon.New(cfg, version).Run(ctx, nil)
		},
	}
	configFlag(c, &configPath)
	return c
}
