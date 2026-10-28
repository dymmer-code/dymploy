package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/intake"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

// queueJobs adapts the queue to the small interface of the control package, which must not depend on it.
type queueJobs struct {
	m   *queue.Manager
	log *slog.Logger
	// What starting a job by hand needs: the providers (to resolve a reference), the receiver (for its
	// policy, which applies exactly as it does to a webhook) and the intake that makes the job.
	providers map[string]provider.Provider
	hooks     *webhook.Receiver
	enq       *intake.Enqueuer
	// secretsEnq makes the jobs that only change the secrets of a target (control.EnqueueRequest.Secrets).
	secretsEnq *intake.Enqueuer
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
	for _, c := range []error{queue.ErrFinished, queue.ErrNotFinished, queue.ErrRejected, queue.ErrQueueFull, queue.ErrClosed, queue.ErrDraining} {
		if errors.Is(err, c) {
			return fmt.Errorf("%w: %s", control.ErrConflict, err)
		}
	}
	return err
}

func (q queueJobs) Abort(ctx context.Context, id job.ID, reason string) error {
	err := conflict(q.m.Abort(ctx, id, reason))
	q.log.Info("audit: stop of a job requested through the control socket", "job", id, "reason", reason, "ok", err == nil)
	return err
}
func (q queueJobs) Retry(ctx context.Context, id job.ID) (job.Job, error) {
	j, err := q.m.Retry(ctx, id)
	q.log.Info("audit: retry requested through the control socket", "job", id, "new_job", j.ID, "ok", err == nil)
	return j, conflict(err)
}

// Enqueue starts a job by hand. It is not a way around anything: the reference is resolved by the
// provider and the resulting event must pass the same policy as a webhook would.
func (q queueJobs) Enqueue(ctx context.Context, r control.EnqueueRequest) (job.Job, error) {
	name, owner, repo, err := q.splitRepo(r.Repo)
	if err != nil {
		return job.Job{}, err
	}
	res, ok := q.providers[name].(provider.Resolver)
	if !ok {
		return job.Job{}, fmt.Errorf("%w: provider %q cannot resolve references", control.ErrInvalid, name)
	}
	ev, err := res.ResolveRef(ctx, owner, repo, r.Ref)
	if err != nil {
		if errors.Is(err, provider.ErrRefNotFound) {
			return job.Job{}, fmt.Errorf("%w: %s", control.ErrInvalid, err)
		}
		return job.Job{}, err
	}
	if q.hooks == nil {
		return job.Job{}, fmt.Errorf("%w: no policy is configured", control.ErrForbidden)
	}
	if allowed, why := q.hooks.Allowed(ev); !allowed {
		q.log.Warn("audit: manual job refused by the policy", "repo", ev.FullName, "ref", ev.Ref, "reason", why)
		return job.Job{}, fmt.Errorf("%w: the policy does not allow it (%s)", control.ErrForbidden, why)
	}
	enq, what := q.enq, "job"
	if r.Secrets {
		if q.secretsEnq == nil {
			return job.Job{}, fmt.Errorf("%w: this server cannot run jobs that change only the secrets", control.ErrInvalid)
		}
		enq, what = q.secretsEnq, "secrets job"
	}
	j, err := enq.Submit(ctx, *ev)
	q.log.Info("audit: "+what+" started by hand through the control socket", "repo", ev.FullName, "ref", ev.Ref, "commit", ev.Commit, "job", j.ID, "ok", err == nil)
	return j, conflict(err)
}

// splitRepo reads provider/owner/repo; owner/repo is enough when there is only one provider.
func (q queueJobs) splitRepo(s string) (name, owner, repo string, err error) {
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 3:
		name, owner, repo = parts[0], parts[1], parts[2]
	case len(parts) == 2 && len(q.providers) == 1:
		for name = range q.providers {
		}
		owner, repo = parts[0], parts[1]
	default:
		return "", "", "", fmt.Errorf("%w: write the repository as provider/owner/repo (for example github/acme/shop)", control.ErrInvalid)
	}
	if _, ok := q.providers[name]; !ok {
		return "", "", "", fmt.Errorf("%w: unknown provider %q", control.ErrInvalid, name)
	}
	if owner == "" || repo == "" {
		return "", "", "", fmt.Errorf("%w: the repository %q is incomplete", control.ErrInvalid, s)
	}
	return name, owner, repo, nil
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
	return fmt.Sprintf("%-36s %-22s %s ago  (%s)", runName(j), result, since(j.CreatedAt), j.ID)
}

// runName is how a run is called: "github/acme/app #12".
func runName(j job.Job) string {
	if j.Number > 0 {
		return fmt.Sprintf("%s #%d", j.Name, j.Number)
	}
	return j.Name
}

// resolveJob turns a reference into a job id. "github/acme/app#12" (or "github/acme/app #12") is the
// twelfth run of that name; anything else is taken as an id.
func resolveJob(ctx context.Context, cl *control.Client, ref string) (string, error) {
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return ref, nil
	}
	name, num := strings.TrimSpace(ref[:i]), strings.TrimSpace(ref[i+1:])
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || name == "" {
		return "", fmt.Errorf("%q is not a run: write it like github/owner/repo#12", ref)
	}
	jobs, err := cl.Jobs(ctx, job.Filter{Name: name, Number: n, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "", fmt.Errorf("there is no run #%d of %s", n, name)
	}
	return string(jobs[0].ID), nil
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
		Use:   "job <id | name#N>",
		Short: "Show one job",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			id, err := resolveJob(c.Context(), cl, args[0])
			if err != nil {
				return err
			}
			j, err := cl.Job(c.Context(), id)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				return printJSON(out, j)
			}
			fmt.Fprintf(out, "run      %s\njob      %s\nkind     %s\nstate    %s\n", runName(j), j.ID, j.Kind, j.State)
			if j.StopReason != "" {
				fmt.Fprintf(out, "stopped  %s\n", j.StopReason)
			}
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
		Use:   "logs <id | name#N>",
		Short: "Show the log of a job (-f to follow it until it ends)",
		Long:  "Lines marked # are dymploy narrating, lines marked ! are the standard error of the work.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			id, err := resolveJob(c.Context(), cl, args[0])
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if follow {
				return cl.Watch(c.Context(), id, after, func(m control.Message) error {
					if m.Type == "output" && m.Entry != nil {
						fmt.Fprintln(out, entryLine(*m.Entry))
					}
					return nil
				})
			}
			for {
				entries, err := cl.Logs(c.Context(), id, after, 1000)
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

func newAbortCmd() *cobra.Command {
	var configPath, socket, reason string
	c := &cobra.Command{
		Use:   "abort <id | name#N>",
		Short: "Stop a waiting or running job, saying why",
		Long: `A waiting job never runs. A running job is asked to stop: its runner stops the processes it started
(containers get a chance to end before being killed), releases what it holds, and only then the job is
marked aborted, with the reason you give. The command returns once the request is made; follow the
job with "dymploy logs -f <id>".`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			id, err := resolveJob(c.Context(), cl, args[0])
			if err != nil {
				return err
			}
			msg, err := cl.Abort(c.Context(), id, reason)
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), msg)
			return nil
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().StringVar(&reason, "reason", "", "why the job is being stopped (kept with the job)")
	return c
}

func newRetryCmd() *cobra.Command {
	var configPath, socket string
	c := &cobra.Command{
		Use:   "retry <id | name#N>",
		Short: "Run a finished job again as a new job",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			id, err := resolveJob(c.Context(), cl, args[0])
			if err != nil {
				return err
			}
			j, err := cl.Retry(c.Context(), id)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "queued %s (retry of %s)\n", runName(j), args[0])
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

func newQueueCmd() *cobra.Command {
	var configPath, socket, ref string
	var follow bool
	c := &cobra.Command{
		Use:   "queue [provider/owner/repo --ref <branch-or-tag>]",
		Short: "Start a job by hand, or show what is waiting and running",
		Long: `With a repository and a reference, resolves the reference to its commit, applies the same policy as a
webhook and queues the job (it obeys the queue's rules: one job per repository, on_busy, limits).
Without arguments it shows what is waiting and running now.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if len(args) == 0 {
				if ref != "" || follow {
					return errors.New("say which repository: dymploy queue provider/owner/repo --ref main")
				}
				jobs, err := cl.Jobs(c.Context(), job.Filter{States: []job.State{job.Queued, job.Running}})
				if err != nil {
					return err
				}
				if len(jobs) == 0 {
					fmt.Fprintln(out, "nothing waiting or running")
				}
				for _, j := range jobs {
					fmt.Fprintln(out, jobLine(j))
				}
				return nil
			}
			if ref == "" {
				return errors.New("--ref is required: a branch or a tag (for example --ref main)")
			}
			j, err := cl.Queue(c.Context(), args[0], ref)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "queued %s at %s (%s)\n", runName(j), ref, j.ID)
			if !follow {
				return nil
			}
			return cl.Watch(c.Context(), string(j.ID), 0, func(m control.Message) error {
				if m.Type == "output" && m.Entry != nil {
					fmt.Fprintln(out, entryLine(*m.Entry))
				}
				return nil
			})
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().StringVar(&ref, "ref", "", "branch or tag to build (or a full refs/heads/… / refs/tags/…)")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow the job's log until it ends")
	return c
}

func newSecretsCmd() *cobra.Command {
	var configPath, socket, ref string
	var follow bool
	c := &cobra.Command{
		Use:   "secrets provider/owner/repo --ref <branch-or-tag>",
		Short: "Change only the secrets of a target: no build, no new release",
		Long: `Resolves the reference to its commit, applies the same policy as a webhook and queues a job that fetches
the target's current secrets and runs the deploy.secrets_steps of that commit's dymploy.yaml (typically install
them and restart the service), then its health check. Nothing is built and the release on the target stays as
it is. If a step or the check fails, deploy.secrets_undo runs, if the definition has it.

The job obeys the queue's rules like any other: one job per repository, on_busy, limits. A definition without
deploy.secrets_steps is refused. The steps are the ones of the commit you name: use the branch that is deployed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if ref == "" {
				return errors.New("--ref is required: a branch or a tag (for example --ref main)")
			}
			cl, err := clientFor(socket, configPath)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			j, err := cl.QueueSecrets(c.Context(), args[0], ref)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "queued the change of secrets %s at %s (%s)\n", runName(j), ref, j.ID)
			if !follow {
				return nil
			}
			return cl.Watch(c.Context(), string(j.ID), 0, func(m control.Message) error {
				if m.Type == "output" && m.Entry != nil {
					fmt.Fprintln(out, entryLine(*m.Entry))
				}
				return nil
			})
		},
	}
	socketFlags(c, &configPath, &socket)
	c.Flags().StringVar(&ref, "ref", "", "branch or tag whose dymploy.yaml says what to do (or a full refs/heads/… / refs/tags/…)")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow the job's log until it ends")
	return c
}
