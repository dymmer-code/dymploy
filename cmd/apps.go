package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/controlplane"
	"github.com/spf13/cobra"
)

func appCatalog(cfg *config.Config) controlplane.Catalog {
	return controlplane.Catalog{Path: filepath.Join(cfg.DataDir, "state", "controlplane.db"), Authority: cfg.ControlPlane.URL, Source: cfg.ControlPlane.BuilderID}
}
func newAppsCmd() *cobra.Command {
	root := &cobra.Command{Use: "apps", Short: "Review, accept and revoke Dymmer applications locally"}
	for _, action := range []string{"list", "accept", "revoke"} {
		var path, digest string
		cmd := &cobra.Command{Use: action, Args: cobra.NoArgs}
		if action != "list" {
			cmd.Use += " APP_ID"
			cmd.Args = cobra.ExactArgs(1)
		}
		cmd.RunE = func(c *cobra.Command, args []string) error {
			cfg, err := config.Load(path)
			if err != nil {
				return err
			}
			if cfg.ControlPlane.URL == "" {
				return fmt.Errorf("control_plane is disabled")
			}
			catalog := appCatalog(cfg)
			if action == "revoke" {
				return catalog.Revoke(args[0])
			}
			apps, err := catalog.Applications()
			if err != nil {
				return err
			}
			if action == "accept" {
				for _, a := range apps {
					if a.ID == args[0] {
						if err := controlplane.CheckLocal(cfg, a); err != nil {
							return err
						}
						return catalog.Accept(a.ID, digest)
					}
				}
				return controlplane.ErrPending
			}
			rows := []map[string]any{}
			for _, a := range apps {
				_, accepted := catalog.Accepted(a.ID)
				localErr := controlplane.CheckLocal(cfg, a)
				reason := ""
				if localErr != nil {
					reason = localErr.Error()
				}
				rows = append(rows, map[string]any{"application": a, "digest": a.Digest(), "accepted": accepted == nil, "local_error": reason})
			}
			return printJSON(c.OutOrStdout(), rows)
		}
		configFlag(cmd, &path)
		if action == "accept" {
			cmd.Flags().StringVar(&digest, "digest", "", "reviewed application digest from apps list")
			_ = cmd.MarkFlagRequired("digest")
		}
		root.AddCommand(cmd)
	}
	return root
}
