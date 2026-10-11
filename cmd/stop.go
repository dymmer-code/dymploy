package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/control"
)

func newStopCmd() *cobra.Command {
	var configPath, socket, reason string
	var graceful, noWait bool
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon without leaving a mess",
		Long: `Without --graceful, running jobs are aborted: every request on the public listener is refused, each running
job is asked to stop (its processes are stopped, the reason is written down and the job is marked aborted)
and, when everything is clean, the daemon goes away. Jobs that were still waiting stay queued for the next
start.

With --graceful, nothing already accepted is lost: new jobs are refused (webhooks get a 503 that says why)
while every job, running or waiting, finishes; then the daemon goes away. Stopping while it drains turns
the drain into an abort.

The command waits until the daemon is gone, showing the progress; --no-wait returns at once.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			msg, err := cl.Stop(c.Context(), graceful, reason)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, msg)
			if noWait {
				return nil
			}
			ctx := c.Context()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			last, state := "", ""
			for {
				st, err := cl.Status(ctx)
				switch {
				case errors.Is(err, control.ErrUnreachable):
					fmt.Fprintln(out, "dymploy has stopped")
					return nil
				case ctx.Err() != nil:
					if c.Context().Err() != nil {
						fmt.Fprintln(out, "no longer waiting; the daemon keeps going with what you asked")
						return nil
					}
					return fmt.Errorf("dymploy is still %s after %s (%s); run `dymploy stop` to abort what is running", state, timeout, last)
				case err != nil:
					return err
				}
				state = st.State
				if now := fmt.Sprintf("%s, %s", plural(st.Running, "running job"), plural(st.Queued, "waiting job")); now != last {
					last = now
					fmt.Fprintf(out, "%s: %s\n", st.State, now)
				}
				select {
				case <-ctx.Done():
				case <-time.After(500 * time.Millisecond):
				}
			}
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().BoolVar(&graceful, "graceful", false, "finish the jobs in hand and refuse new ones instead of aborting what runs")
	c.Flags().StringVar(&reason, "reason", "", "why (kept with the jobs that get aborted)")
	c.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as the daemon has been asked")
	c.Flags().DurationVar(&timeout, "timeout", 0, "stop waiting after this long (0 = until the daemon is gone)")
	return c
}
