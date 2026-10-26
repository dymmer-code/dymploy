package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/queue"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

// reloader applies a new configuration to a running daemon. Only what can change safely while jobs
// run is applied (today the policy); if anything else differs the whole reload is refused, so the
// daemon never ends up half-way between two configurations. A configuration that does not load
// leaves the previous one in force.
type reloader struct {
	mu    sync.Mutex
	path  string
	cur   *config.Config
	hooks *webhook.Receiver // nil when no webhooks are configured
	log   *slog.Logger

	remote  deploy.Remote
	store   deploy.Store
	targets *deploy.Targets
	mgr     *queue.Manager // jobs the new policy no longer allows are stopped; nil in tests
}

func policyRules(cfg *config.Config) webhook.Policy {
	return webhook.Policy{Rules: rulesOf(cfg.Policy)}
}

// Reload implements the daemon's reload hook.
func (r *reloader) Reload() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	next, err := config.Load(r.path)
	if err != nil {
		r.log.Error("configuration not reloaded: it is invalid, the previous one stays in force", "error", err)
		return "", err
	}
	if changed := needRestart(r.cur, next); len(changed) > 0 {
		err := fmt.Errorf("these settings cannot change while running, restart the daemon to apply them: %s", strings.Join(changed, ", "))
		r.log.Error("configuration not reloaded", "error", err)
		return "", err
	}
	// Targets first: they can fail (missing key files), and a failure must change nothing.
	if err := applyTargets(next, r.remote, r.store, r.targets); err != nil {
		err = fmt.Errorf("the previous targets stay in force: %w", err)
		r.log.Error("configuration not reloaded", "error", err)
		return "", err
	}
	if r.hooks != nil {
		r.hooks.SetPolicy(policyRules(next))
	}
	r.cur = next
	msg := fmt.Sprintf("configuration reloaded: %d policy rules, %d targets", len(next.Policy), len(next.Targets))
	if n := r.stopDisallowed(); n > 0 {
		msg += fmt.Sprintf("; stop requested for %s the new configuration no longer allows", plural(n, "job"))
	}
	r.log.Info(msg)
	return msg, nil
}

// stopDisallowed asks the jobs that the policy in force no longer allows to stop. A repository that
// leaves the policy does not just stop being accepted: what it still has waiting or running is stopped
// properly (the same stop as `dymploy abort`, with the reason written down), never dropped.
func (r *reloader) stopDisallowed() int {
	if r.mgr == nil || r.hooks == nil {
		return 0
	}
	ctx := context.Background()
	jobs, err := r.mgr.List(ctx, job.Filter{States: []job.State{job.Queued, job.Running}})
	if err != nil {
		r.log.Error("could not list the jobs to check them against the new policy", "error", err)
		return 0
	}
	stopped := 0
	for _, j := range jobs {
		var ev provider.Event
		if json.Unmarshal(j.Input, &ev) != nil || ev.Provider == "" || ev.Owner == "" {
			continue // not a job that came from a repository event: the policy says nothing about it
		}
		if allowed, why := r.hooks.Allowed(&ev); !allowed {
			reason := "no longer allowed by the configuration after a reload (" + why + ")"
			if err := r.mgr.Abort(ctx, j.ID, reason); err == nil {
				r.log.Info("audit: job stopped because the new policy does not allow it", "job", j.ID, "name", j.Name, "reason", why)
				stopped++
			}
		}
	}
	return stopped
}

// needRestart names the settings (by their key in the file) that differ between a and b, except those
// that can be applied while running.
func needRestart(a, b *config.Config) []string {
	var out []string
	va, vb := reflect.ValueOf(*a), reflect.ValueOf(*b)
	t := va.Type()
	for i := 0; i < t.NumField(); i++ {
		key := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if key == "policy" || key == "targets" { // applied while running
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, key)
		}
	}
	return out
}

// reloadOnSIGHUP reloads the configuration every time the process receives SIGHUP, until ctx ends.
func reloadOnSIGHUP(ctx context.Context, r *reloader) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sigs)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigs:
				_, _ = r.Reload() // the outcome is logged
			}
		}
	}()
}

func newReloadCmd() *cobra.Command {
	var configPath, socket string
	c := &cobra.Command{
		Use:   "reload",
		Short: "Make the running daemon re-read its configuration (config.yaml and config.d/)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			path, err := socketPath(socket, configPath)
			if err != nil {
				// The configuration to be loaded may be exactly the broken one: the daemon should
				// still be asked, so that it explains why it keeps the previous one.
				var lerr error
				if path, lerr = config.ControlSocket(configPath); lerr != nil {
					return err
				}
			}
			msg, err := (&control.Client{Socket: path}).Reload(c.Context())
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), msg)
			return nil
		},
	}
	configFlag(c, &configPath)
	c.Flags().StringVar(&socket, "socket", "", "control socket path (overrides the configuration)")
	return c
}
