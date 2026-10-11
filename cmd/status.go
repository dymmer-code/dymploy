package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/control"
)

func newStatusCmd(_ string) *cobra.Command {
	var configPath, socket string
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the running daemon",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			path, err := socketPath(socket, configPath)
			if err != nil {
				return err
			}
			st, err := (&control.Client{Socket: path}).Status(c.Context())
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			fmt.Fprintf(out, "dymploy %s\n  pid     %d\n  started %s (up %s)\n",
				st.Version, st.PID, st.StartedAt.Format(time.RFC3339), time.Since(st.StartedAt).Round(time.Second))
			if st.State != "" {
				fmt.Fprintf(out, "  state   %s (%s, %s)\n", st.State, plural(st.Running, "running job"), plural(st.Queued, "waiting job"))
			}
			return nil
		},
	}
	configFlag(c, &configPath)
	c.Flags().StringVar(&socket, "socket", "", "control socket path (overrides the configuration)")
	c.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return c
}
