package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/queue"
)

// queueJobs adapts the queue to the small interface of the control package, which must not depend on it.
type queueJobs struct {
	m   *queue.Manager
	log *slog.Logger
}

func (q queueJobs) List(ctx context.Context, f job.Filter) ([]job.Job, error) {
	return q.m.List(ctx, f)
}
func (q queueJobs) Get(ctx context.Context, id job.ID) (job.Job, error) { return q.m.Get(ctx, id) }
func (q queueJobs) Logs(id job.ID, after int64, limit int) ([]job.Entry, error) {
	return q.m.Logs(id, after, limit)
}

// conflict marks the errors that mean "not in this state" so that the API answers 409.
func conflict(err error) error {
	for _, c := range []error{queue.ErrFinished, queue.ErrNotFinished, queue.ErrRejected, queue.ErrQueueFull, queue.ErrClosed} {
		if errors.Is(err, c) {
			return fmt.Errorf("%w: %s", control.ErrConflict, err)
		}
	}
	return err
}

func (q queueJobs) Cancel(ctx context.Context, id job.ID) error {
	err := conflict(q.m.Cancel(ctx, id))
	q.log.Info("audit: cancel requested through the control socket", "job", id, "ok", err == nil)
	return err
}
func (q queueJobs) Retry(ctx context.Context, id job.ID) (job.Job, error) {
	j, err := q.m.Retry(ctx, id)
	q.log.Info("audit: retry requested through the control socket", "job", id, "new_job", j.ID, "ok", err == nil)
	return j, conflict(err)
}

func (q queueJobs) Subscribe() (<-chan control.Event, func()) {
	in, cancel := q.m.Subscribe()
	out := make(chan control.Event, 256)
	go func() {
		defer close(out)
		for ev := range in {
			select {
			case out <- control.Event{Type: string(ev.Type), Job: ev.Job, Entry: ev.Entry}:
			default: // a reader that cannot keep up loses events, as with the queue itself
			}
		}
	}()
	return out, cancel
}

// clientFor resolves the control socket the way every client command does.
func clientFor(socket, configPath string) (*control.Client, error) {
	path, err := socketPath(socket, configPath)
	if err != nil {
		return nil, err
	}
	return &control.Client{Socket: path}, nil
}

func socketFlags(c *cobra.Command, configPath, socket *string) {
	configFlag(c, configPath)
	c.Flags().StringVar(socket, "socket", "", "control socket path (overrides the configuration)")
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func jobLine(j job.Job) string {
	result := string(j.State)
	if j.ExitCode != nil && j.State != job.Succeeded {
		result += fmt.Sprintf(" (exit %d)", *j.ExitCode)
	}
	return fmt.Sprintf("%-18s %-22s %-34s %s ago", j.ID, result, j.Name, since(j.CreatedAt))
}

func newJobsCmd() *cobra.Command {
	var configPath, socket, name, states string
	var limit int
	var asJSON bool
	c := &cobra.Command{
		Use:   "jobs",
		Short: "List jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			f := job.Filter{Name: name, Limit: limit}
			for _, s := range strings.Split(states, ",") {
				if s = strings.TrimSpace(s); s != "" {
					f.States = append(f.States, job.State(s))
				}
			}
			jobs, err := cl.Jobs(c.Context(), f)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				return printJSON(out, jobs)
			}
			if len(jobs) == 0 {
				fmt.Fprintln(out, "no jobs")
			}
			for _, j := range jobs {
				fmt.Fprintln(out, jobLine(j))
			}
			return nil
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().StringVar(&name, "name", "", "only jobs of this repository (provider/owner/repo)")
	c.Flags().StringVar(&states, "state", "", "only these states, comma separated (queued, running, succeeded, failed, canceled, interrupted)")
	c.Flags().IntVar(&limit, "limit", 20, "at most this many jobs (0 = all)")
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

func newJobCmd() *cobra.Command {
	var configPath, socket string
	var asJSON bool
	c := &cobra.Command{
		Use:   "job <id>",
		Short: "Show one job",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			j, err := cl.Job(c.Context(), args[0])
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				return printJSON(out, j)
			}
			fmt.Fprintf(out, "job      %s\nname     %s\nkind     %s\nstate    %s\n", j.ID, j.Name, j.Kind, j.State)
			if j.ExitCode != nil {
				fmt.Fprintf(out, "exit     %d\n", *j.ExitCode)
			}
			if j.Error != "" {
				fmt.Fprintf(out, "error    %s\n", j.Error)
			}
			if j.RetryOf != "" {
				fmt.Fprintf(out, "retry of %s\n", j.RetryOf)
			}
			fmt.Fprintf(out, "created  %s\n", j.CreatedAt.Format(time.RFC3339))
			if !j.StartedAt.IsZero() {
				fmt.Fprintf(out, "started  %s\n", j.StartedAt.Format(time.RFC3339))
			}
			if !j.FinishedAt.IsZero() {
				fmt.Fprintf(out, "finished %s (took %s)\n", j.FinishedAt.Format(time.RFC3339), j.FinishedAt.Sub(j.StartedAt).Round(time.Second))
			}
			return nil
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

func entryLine(e job.Entry) string {
	mark := " "
	switch e.Stream {
	case "stderr":
		mark = "!"
	case "system":
		mark = "#"
	}
	return fmt.Sprintf("%s %s %s", e.Time.Local().Format("15:04:05"), mark, e.Text)
}

func newLogsCmd() *cobra.Command {
	var configPath, socket string
	var follow bool
	var after int64
	c := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show the log of a job (-f to follow it until it ends)",
		Long:  "Lines marked # are dymploy narrating, lines marked ! are the standard error of the work.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if follow {
				return cl.Watch(c.Context(), args[0], after, func(m control.Message) error {
					if m.Type == "output" && m.Entry != nil {
						fmt.Fprintln(out, entryLine(*m.Entry))
					}
					return nil
				})
			}
			for {
				entries, err := cl.Logs(c.Context(), args[0], after, 1000)
				if err != nil {
					return err
				}
				for _, e := range entries {
					fmt.Fprintln(out, entryLine(e))
					after = e.Seq
				}
				if len(entries) < 1000 {
					return nil
				}
			}
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep following until the job ends")
	c.Flags().Int64Var(&after, "after", 0, "start after this sequence number")
	return c
}

func newCancelCmd() *cobra.Command {
	var configPath, socket string
	c := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a waiting or running job",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			msg, err := cl.Cancel(c.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), msg)
			return nil
		},
	}
	socketFlags(c, &configPath, &socket)
	return c
}

func newRetryCmd() *cobra.Command {
	var configPath, socket string
	c := &cobra.Command{
		Use:   "retry <id>",
		Short: "Run a finished job again as a new job",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			j, err := cl.Retry(c.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "queued %s (retry of %s)\n", j.ID, args[0])
			return nil
		},
	}
	socketFlags(c, &configPath, &socket)
	return c
}

func newWatchCmd() *cobra.Command {
	var configPath, socket string
	var asJSON bool
	c := &cobra.Command{
		Use:   "watch",
		Short: "Follow what the daemon is doing, live (Ctrl-C to stop)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			err = cl.Watch(c.Context(), "", 0, func(m control.Message) error {
				if asJSON {
					return json.NewEncoder(out).Encode(m)
				}
				switch m.Type {
				case "snapshot":
					if len(m.Jobs) == 0 {
						fmt.Fprintln(out, "nothing waiting or running")
					}
					for _, j := range m.Jobs {
						fmt.Fprintln(out, jobLine(j))
					}
				case "queued", "started", "finished":
					fmt.Fprintf(out, "%-9s %s\n", m.Type, jobLine(*m.Job))
				case "output":
					fmt.Fprintf(out, "%-18s %s\n", m.Job.ID, entryLine(*m.Entry))
				}
				return nil
			})
			if errors.Is(err, context.Canceled) {
				return nil // Ctrl-C is how this command ends
			}
			return err
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON message per line")
	return c
}
