package cmd

import (
	"bufio"
	"fmt"
	"path/filepath"
	"slices"
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
		var path string
		var expect expectation
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
						if expect.stated(c) {
							if err := expect.matches(a); err != nil {
								return err
							}
						} else if err := confirmAcceptance(c, a); err != nil {
							return err
						}
						return catalog.Accept(a.ID, a.Digest())
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
			expect.flags(cmd)
		}
		root.AddCommand(cmd)
	}
	return root
}

// acceptanceItems lists, in words, each thing an application asks to be allowed to do. A person answers for every one.
func acceptanceItems(a controlplane.Application) []string {
	items := []string{"Deploy the repository " + a.RepositoryName()}
	if len(a.Repository.Branches) > 0 {
		items = append(items, "From the branches: "+strings.Join(a.Repository.Branches, ", "))
	}
	if len(a.Repository.Tags) > 0 {
		items = append(items, "From the tags: "+strings.Join(a.Repository.Tags, ", "))
	}
	if a.Environment != "" {
		items = append(items, "For the environment: "+a.Environment)
	}
	for _, t := range a.Targets {
		item := fmt.Sprintf("To the destination %s (%s@%s", t.Name, t.User, t.Host)
		if t.Port != 0 {
			item += fmt.Sprintf(":%d", t.Port)
		}
		item += ")"
		if t.UploadDir != "" {
			item += ", directory " + t.UploadDir
		}
		if t.SecretsRef != "" {
			item += ", reading its secrets"
		}
		items = append(items, item)
	}
	return items
}

// confirmAcceptance asks about each element the application asks for and accepts only if every answer is an explicit yes.
// Anything else (no input, a script that states no expectation) leaves the application pending: nothing is written.
func confirmAcceptance(c *cobra.Command, a controlplane.Application) error {
	out := c.OutOrStdout()
	in := bufio.NewReader(c.InOrStdin())
	fmt.Fprintf(out, "Application %s asks to:\n", a.ID)
	for _, item := range acceptanceItems(a) {
		fmt.Fprintf(out, "  %s. Allow it? [y/N] ", item)
		answer, _ := in.ReadString('\n')
		if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
			fmt.Fprintln(out)
			return fmt.Errorf("not accepted: nothing changed (a script states what it expects with --repository, --branch, --tag, --environment and --target)")
		}
	}
	return nil
}

// expectation is what a script says it has reviewed. Every element must be stated and must be exactly what the
// application asks for: a missing, extra or different one refuses, and nothing is written.
type expectation struct {
	repository  string
	branches    []string
	tags        []string
	environment string
	targets     []string
}

func (e *expectation) flags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&e.repository, "repository", "", "expected repository, provider/owner/name (with these flags nothing is asked)")
	f.StringSliceVar(&e.branches, "branch", nil, "expected branch (repeat for each one)")
	f.StringSliceVar(&e.tags, "tag", nil, "expected tag (repeat for each one)")
	f.StringVar(&e.environment, "environment", "", "expected environment")
	f.StringSliceVar(&e.targets, "target", nil, "expected destination name (repeat for each one)")
}

func (e *expectation) stated(c *cobra.Command) bool {
	for _, name := range []string{"repository", "branch", "tag", "environment", "target"} {
		if c.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func (e *expectation) matches(a controlplane.Application) error {
	var diff []string
	check := func(what string, asked, stated []string) {
		asked, stated = slices.Clone(asked), slices.Clone(stated)
		slices.Sort(asked)
		slices.Sort(stated)
		if !slices.Equal(asked, stated) {
			diff = append(diff, fmt.Sprintf("%s: the application asks for [%s], you stated [%s]", what, strings.Join(asked, " "), strings.Join(stated, " ")))
		}
	}
	nonEmpty := func(s string) []string {
		if s == "" {
			return nil
		}
		return []string{s}
	}
	check("repository", []string{a.RepositoryName()}, nonEmpty(e.repository))
	check("branches", a.Repository.Branches, e.branches)
	check("tags", a.Repository.Tags, e.tags)
	check("environment", nonEmpty(a.Environment), nonEmpty(e.environment))
	check("destinations", a.TargetNames(), e.targets)
	if len(diff) > 0 {
		return fmt.Errorf("not accepted: nothing changed\n  %s", strings.Join(diff, "\n  "))
	}
	return nil
}
