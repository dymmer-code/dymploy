package cmd

import (
	"bufio"
	"fmt"
	"path/filepath"
	"strings"

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
						if digest == "" {
							if err := confirmAcceptance(c, a); err != nil {
								return err
							}
							digest = a.Digest()
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
			cmd.Flags().StringVar(&digest, "digest", "", "digest reviewed beforehand (from apps list): accepts exactly that definition without asking")
		}
		root.AddCommand(cmd)
	}
	return root
}

// confirmAcceptance shows what the application asks for and its digest, and accepts only an explicit yes on the input.
// Anything else (no input, a script without --digest) leaves the application pending.
func confirmAcceptance(c *cobra.Command, a controlplane.Application) error {
	out := c.OutOrStdout()
	if err := printJSON(out, a); err != nil {
		return err
	}
	fmt.Fprintf(out, "digest: %s\nAccept this definition? [y/N] ", a.Digest())
	answer, _ := bufio.NewReader(c.InOrStdin()).ReadString('\n')
	if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
		return fmt.Errorf("not accepted: nothing changed (use --digest to accept a reviewed definition without asking)")
	}
	return nil
}
