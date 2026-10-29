package cmd

import (
	"context"
	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/controlplane"
	"os"
	"strings"
	"testing"
)

func TestAppsCLIReviewAcceptAndRevoke(t *testing.T) {
	path := writeConfig(t, tmpDir(t))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("runner:\n  kind: docker\ncontrol_plane:\n  url: wss://dymmer.test/builders/websocket\n  builder_id: builder\n  token_file: /etc/dymploy/token\n")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cat := appCatalog(cfg)
	a := controlplane.Application{ID: "app", OwnerID: "owner", ProjectID: "project", Repository: controlplane.Repository{Provider: "github", Owner: "acme", Name: "app"}}
	if err := cat.Update(controlplane.Snapshot{SchemaVersion: 1, BuilderID: "builder", Applications: []controlplane.Application{a}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	out, _, err := run(t, ctx, "apps", "list", "--config", path)
	if err != nil || !strings.Contains(out, a.Digest()) {
		t.Fatal(out, err)
	}
	if _, _, err := run(t, ctx, "apps", "accept", "app", "--digest", "old", "--config", path); err == nil {
		t.Fatal("stale digest")
	}
	if _, _, err := run(t, ctx, "apps", "accept", "app", "--digest", a.Digest(), "--config", path); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Accepted("app"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, ctx, "apps", "revoke", "app", "--config", path); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.Accepted("app"); err == nil {
		t.Fatal("not revoked")
	}
	if _, _, err := run(t, ctx, "apps", "accept", "missing", "--digest", a.Digest(), "--config", path); err == nil {
		t.Fatal("missing accepted")
	}
	a.Targets = []controlplane.Target{{Name: "unconfigured"}}
	if err := cat.Update(controlplane.Snapshot{SchemaVersion: 1, BuilderID: "builder", Applications: []controlplane.Application{a}}); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, ctx, "apps", "list", "--config", path)
	if err != nil || !strings.Contains(out, "differs from the local catalogue") {
		t.Fatal(out, err)
	}
	if _, _, err := run(t, ctx, "apps", "accept", "app", "--digest", a.Digest(), "--config", path); err == nil {
		t.Fatal("unconfigured target accepted")
	}
	disabled := writeConfig(t, tmpDir(t))
	if _, _, err := run(t, ctx, "apps", "list", "--config", disabled); err == nil {
		t.Fatal("disabled")
	}
}

func TestAppsCLIReportsAnUnreadableConfiguration(t *testing.T) {
	if _, _, err := run(t, context.Background(), "apps", "list", "--config", tmpDir(t)+"/missing.yaml"); err == nil {
		t.Fatal("missing configuration accepted")
	}
}
