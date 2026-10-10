package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/manifest"
)

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [file-or-directory]",
		Short: "Check a dymploy.yaml before pushing it",
		Long: `Reads dymploy.yaml (the file you give, or the one in the directory you give, or in the
current directory) and reports every problem it finds, with the place of each. It also checks that the
files it refers to exist. It does not contact any server.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			target := "."
			if len(args) == 1 {
				target = args[0]
			}
			dir, name, err := locate(target)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			m, err := manifest.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := m.CheckFiles(os.DirFS(dir)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fmt.Fprintf(c.OutOrStdout(), "%s: OK (%s)\n", name, m.Summary())
			return nil
		},
	}
}

// locate turns what the user typed into a directory and the manifest's name inside it.
func locate(target string) (dir, name string, err error) {
	fi, err := os.Stat(target)
	if err != nil {
		return "", "", err
	}
	if !fi.IsDir() {
		return filepath.Dir(target), filepath.Base(target), nil
	}
	name, err = manifest.Find(os.DirFS(target))
	if errors.Is(err, manifest.ErrNotFound) {
		return "", "", fmt.Errorf("%s has no dymploy.yaml (or dymploy.yml)", target)
	}
	if err != nil {
		return "", "", err
	}
	return target, name, nil
}

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of dymploy.yaml",
		Long:  "Prints the JSON Schema of dymploy.yaml, for editors: dymploy schema > dymploy.schema.json",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			_, err := c.OutOrStdout().Write(manifest.Schema())
			return err
		},
	}
}
