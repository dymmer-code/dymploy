package cmd

import (
	"context"
	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/queue"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueueWiresDurableEventsAndRejectsBadSigningKeys(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Runner.Kind = "dryrun"
	key := filepath.Join(t.TempDir(), "signing-key")
	os.WriteFile(key, []byte(strings.Repeat("k", 32)), 0600)
	cfg.Events.Sinks = []config.EventSink{{Name: "audit", URL: "https://127.0.0.1:1/events", SecretFile: key}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &deploy.Deployer{Targets: deploy.NewTargets()}
	mgr, _, _, closeState, err := buildQueue(cfg, log, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Submit(context.Background(), queue.Request{Name: "event-test", Kind: "dryrun"}); err != nil {
		t.Fatal(err)
	}
	if err := shutdownQueue(mgr, cfg.ShutdownTimeout); err != nil {
		t.Fatal(err)
	}
	closeState()
	store, err := job.OpenBolt(filepath.Join(cfg.DataDir, "state", "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.NextEvent("audit"); err != nil || !ok {
		t.Fatal("lost pending events", err)
	}
	store.Close()
	os.WriteFile(key, []byte("short"), 0600)
	if _, _, _, close, err := buildQueue(cfg, log, nil, d); err == nil {
		close()
		t.Fatal("accepted weak signing key")
	}
}
