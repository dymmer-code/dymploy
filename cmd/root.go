// Package cmd is the dymploy command line: one binary that is both the daemon (`serve`) and
// the client of that daemon (`status` and, as they are built, the rest).
package cmd

import (
	"errors"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/config"
)

// DefaultConfigPath is where the daemon and the client look for the configuration.
const DefaultConfigPath = "/etc/dymploy/config.yaml"

// NewRootCmd builds the command tree. version is injected at build time.
func NewRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "dymploy",
		Short:         "dymploy — build and deploy from your repositories",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newVersionCmd(version), newServeCmd(version), newStatusCmd(version), newReloadCmd(), newJobsCmd(), newJobCmd(), newLogsCmd(), newCancelCmd(), newRetryCmd(), newWatchCmd(), newQueueCmd(), newValidateCmd(), newSchemaCmd())
	return root
}

func configFlag(c *cobra.Command, dst *string) {
	def := os.Getenv("DYMPLOY_CONFIG")
	if def == "" {
		def = DefaultConfigPath
	}
	c.Flags().StringVar(dst, "config", def, "path of the configuration file (env DYMPLOY_CONFIG)")
}

// socketPath resolves the control socket: an explicit --socket wins; otherwise the one in the
// configuration; with no configuration file at all, the default.
func socketPath(explicit, configPath string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return config.Default().ControlSocket, nil
		}
		return "", err
	}
	return cfg.ControlSocket, nil
}
