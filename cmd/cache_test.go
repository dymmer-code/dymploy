package cmd

import (
	"context"
	"errors"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/runner"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCaches struct{}

func (testCaches) ListCaches(q runner.CacheRequest) ([]runner.CacheEntry, error) {
	if q.Repository == "bad" {
		return nil, errors.New("bad repo")
	}
	return []runner.CacheEntry{{Repository: "github/acme/app", Name: "plt", Scope: "repository", Bytes: 12}}, nil
}
func (testCaches) InvalidateCaches(q runner.CacheRequest) (runner.CacheInvalidation, error) {
	if q.Name == "" && !q.All {
		return runner.CacheInvalidation{}, errors.New("name required")
	}
	return runner.CacheInvalidation{Invalidated: 1}, nil
}
func TestCacheCLIAndSecretStatus(t *testing.T) {
	socket := filepath.Join(tmpDir(t), "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		done <- control.Serve(ctx, socket, control.Handlers{Caches: testCaches{}, Status: func() control.Status {
			return control.Status{Version: "test", Secrets: []control.SecretStatus{{Target: "prod", State: "changed", DeployedAt: time.Now()}}}
		}}, func() { close(ready) })
	}()
	<-ready
	defer func() { cancel(); <-done }()
	for _, args := range [][]string{{"cache", "list"}, {"cache", "list", "github/acme/app", "--json"}, {"cache", "invalidate", "github/acme/app", "--name", "plt"}, {"cache", "invalidate", "github/acme/app", "--all", "--json"}, {"status"}} {
		out, _, err := run(t, ctx, append(args, "--socket", socket)...)
		if err != nil || out == "" {
			t.Fatalf("%v %s %v", args, out, err)
		}
		if args[0] == "status" && !strings.Contains(out, "prod: changed") {
			t.Fatal(out)
		}
	}
	for _, args := range [][]string{{"cache", "list", "bad"}, {"cache", "invalidate", "github/acme/app"}} {
		if _, _, err := run(t, ctx, append(args, "--socket", socket)...); err == nil {
			t.Fatal("error hidden")
		}
	}
}
