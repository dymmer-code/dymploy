package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/dymmer-code/dymploy/internal/config"
)

func inventoryConfig() *config.Config {
	return &config.Config{Targets: map[string]config.TargetConfig{
		"shop": {
			Host: "app.internal", User: "deploy-shop", Secrets: &config.SecretsConfig{},
			Allow: []config.PolicyRule{{Provider: "github", Owner: "acme", Repositories: []string{"shop", "docs"}, Branches: []string{"main"}}},
		},
		"shop-staging": {
			Host: "staging.internal", Port: 2222, User: "deploy-staging",
			Allow: []config.PolicyRule{{Provider: "github", Owner: "acme", Repositories: []string{"shop"}, Branches: []string{"staging", "main"}}},
		},
		"bucket": {
			Kind:  config.KindS3,
			Allow: []config.PolicyRule{{Provider: "github", Owner: "acme", Repositories: []string{"docs"}, Branches: []string{"main"}}},
		},
	}}
}

func TestInventoryNamesTheTargetsAndWhatMayDeployToThem(t *testing.T) {
	lookup := func(host string) ([]string, error) {
		if host == "app.internal" {
			return []string{"192.0.2.57", "2001:db8::57"}, nil
		}
		return nil, errors.New("no such host")
	}
	resolve := func(_ context.Context, provider, owner, repo string) (int64, error) {
		if repo == "docs" {
			return 0, errors.New("not installed")
		}
		return 4242, nil
	}
	inv := BuildInventory(context.Background(), inventoryConfig(), resolve, lookup)

	if inv.SchemaVersion != 1 || len(inv.Targets) != 3 {
		t.Fatalf("%+v", inv)
	}
	bucket, shop, staging := inv.Targets[0], inv.Targets[1], inv.Targets[2]
	if bucket.Name != "bucket" || bucket.Kind != "s3" || bucket.Host != "" || len(bucket.Addresses) != 0 {
		t.Errorf("a bucket has no host: %+v", bucket)
	}
	if shop.Name != "shop" || shop.Kind != "ssh" || shop.Port != 22 || shop.User != "deploy-shop" || !shop.HasSecrets ||
		len(shop.Addresses) != 2 || shop.Addresses[0] != "192.0.2.57" {
		t.Errorf("shop: %+v", shop)
	}
	if staging.Port != 2222 || staging.HasSecrets || len(staging.Addresses) != 0 {
		t.Errorf("a host that does not resolve is reported without addresses: %+v", staging)
	}

	byName := map[string]InventoryRepository{}
	for _, r := range inv.Repositories {
		byName[r.Name] = r
	}
	if len(byName) != 2 {
		t.Fatalf("one entry per repository: %+v", inv.Repositories)
	}
	if r := byName["shop"]; r.InstallationID != 4242 || len(r.Targets) != 2 || r.Targets[0] != "shop" || r.Targets[1] != "shop-staging" ||
		len(r.Branches) != 2 || r.Branches[0] != "main" || r.Branches[1] != "staging" {
		t.Errorf("shop: %+v", r)
	}
	if r := byName["docs"]; r.InstallationID != 0 || len(r.Targets) != 2 {
		t.Errorf("a repository the App does not cover has no installation: %+v", r)
	}
}

func TestInventoryWithoutResolverReportsNoInstallation(t *testing.T) {
	inv := BuildInventory(context.Background(), inventoryConfig(), nil, func(string) ([]string, error) { return nil, nil })
	for _, r := range inv.Repositories {
		if r.InstallationID != 0 {
			t.Errorf("%+v", r)
		}
	}
}
