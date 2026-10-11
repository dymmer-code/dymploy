package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/dymmer-code/dymploy/internal/daemon"
	"github.com/dymmer-code/dymploy/internal/queue"
)

// lifecycle is how dymploy stops. There are two ways, and neither is abrupt:
//
//   - graceful: the queue refuses new jobs (webhooks get 503 and a message), carries on with everything
//     it has, running and waiting, and when none is left the daemon goes away;
//   - stop: the public listener refuses everything, every running job is asked to stop (its processes
//     are stopped by its runner, the reason is written down and the job is marked aborted), and when
//     all are clean the daemon goes away. Jobs that were still waiting stay queued for the next start.
//
// Stopping while draining turns the drain into a stop; nothing turns back.
type lifecycle struct {
	mgr     *queue.Manager
	d       *daemon.Daemon
	log     *slog.Logger
	timeout time.Duration // how long to wait for running jobs to stop before leaving anyway
	finish  context.CancelFunc

	mu          sync.Mutex
	mode        daemon.Mode
	cancelDrain context.CancelFunc
}

func (l *lifecycle) progress() string {
	s := l.mgr.Stats()
	return fmt.Sprintf("%s, %s", plural(s.Running, "running job"), plural(s.Queued, "waiting job"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Stop implements the daemon's stop hook. It answers at once; the daemon goes away when it is safe.
func (l *lifecycle) Stop(graceful bool, reason string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if reason == "" {
		reason = "dymploy was asked to stop"
	}
	if graceful {
		switch l.mode {
		case daemon.ModeStopping:
			return "", errors.New("dymploy is already stopping and aborting its jobs")
		case daemon.ModeDraining:
			return "already draining: " + l.progress(), nil
		}
		l.mode = daemon.ModeDraining
		l.mgr.Drain()
		l.d.SetMode(daemon.ModeDraining)
		l.log.Info("audit: graceful stop requested; no new jobs are accepted", "reason", reason, "work", l.progress())
		ctx, cancel := context.WithCancel(context.Background())
		l.cancelDrain = cancel
		go func() {
			if err := l.mgr.WaitIdle(ctx); err != nil {
				return // turned into a stop
			}
			l.log.Info("nothing left to do: stopping")
			l.d.SetMode(daemon.ModeStopping)
			l.finish()
		}()
		return "draining: no new jobs are accepted; waiting for " + l.progress(), nil
	}
	if l.mode == daemon.ModeStopping {
		return "already stopping: " + l.progress(), nil
	}
	if l.cancelDrain != nil {
		l.cancelDrain()
	}
	l.mode = daemon.ModeStopping
	l.d.SetMode(daemon.ModeStopping)
	s := l.mgr.Stats()
	l.mgr.Halt(reason)
	l.log.Info("audit: stop requested; aborting the running jobs", "reason", reason, "running", s.Running, "left_queued", s.Queued)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
		defer cancel()
		if err := l.mgr.WaitStopped(ctx); err != nil {
			l.log.Error("some jobs did not stop in time; leaving anyway", "timeout", l.timeout, "error", err)
		}
		l.finish()
	}()
	return fmt.Sprintf("stopping: aborting %s (%s stay queued for the next start)", plural(s.Running, "running job"), plural(s.Queued, "waiting job")), nil
}

// handleSignals ties the process signals and the end of the command's context to the lifecycle:
// SIGTERM and SIGINT stop (a second one leaves at once), SIGQUIT stops gracefully.
func (l *lifecycle) handleSignals(parent, daemonCtx context.Context) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		defer signal.Stop(sigs)
		forced := false
		done := parent.Done()
		for {
			select {
			case <-daemonCtx.Done():
				return // the daemon has gone: nothing left to signal
			case s := <-sigs:
				switch {
				case s == syscall.SIGQUIT:
					_, _ = l.Stop(true, "signal "+s.String())
				case forced:
					l.log.Warn("second signal: leaving without waiting")
					l.finish()
					return
				default:
					forced = true
					_, _ = l.Stop(false, "signal "+s.String())
				}
			case <-done:
				done = nil
				_, _ = l.Stop(false, "dymploy was asked to stop")
			}
		}
	}()
}
