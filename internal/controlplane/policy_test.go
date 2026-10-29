package controlplane

import (
	"github.com/dymmer-code/dymploy/internal/config"
	"testing"
)

func TestLocalTargetAndCredentialBindings(t *testing.T) {
	cfg := config.Default()
	cfg.Runner.Kind = "docker"
	cfg.ControlPlane.SSHKeys = map[string]string{"key": "/local/key"}
	cfg.ControlPlane.KnownHosts = map[string]string{"known": "/local/known"}
	cfg.ControlPlane.Secrets = map[string]string{"secret": "rowan"}
	cfg.Targets = map[string]config.TargetConfig{"rowan": {Host: "rowan", Port: 22, User: "deploy", UploadDir: "uploads", KeyFile: "/local/key", KnownHosts: "/local/known", Secrets: &config.SecretsConfig{Command: []string{"/local/secret"}}}}
	a := Application{Repository: Repository{Provider: "github", Owner: "acme", Name: "app"}, Targets: []Target{{Name: "rowan", Host: "rowan", Port: 22, User: "deploy", UploadDir: "uploads", SSHKeyRef: "key", KnownHostsRef: "known", SecretsRef: "secret"}}}
	if err := CheckLocal(cfg, a); err != nil {
		t.Fatal(err)
	}
	if a.RepositoryName() != "github/acme/app" || a.TargetNames()[0] != "rowan" {
		t.Fatal(a)
	}
	for _, change := range []func(*Target){func(t *Target) { t.Name = "other" }, func(t *Target) { t.Host = "attacker" }, func(t *Target) { t.User = "root" }, func(t *Target) { t.UploadDir = "/etc" }, func(t *Target) { t.SSHKeyRef = "unknown" }, func(t *Target) { t.SecretsRef = "" }, func(t *Target) { t.KnownHostsRef = "unknown" }} {
		copy := a
		copy.Targets = append([]Target{}, a.Targets...)
		change(&copy.Targets[0])
		if CheckLocal(cfg, copy) == nil {
			t.Fatal(copy)
		}
	}
	cfg.Runner.Kind = "dryrun"
	if CheckLocal(cfg, a) == nil {
		t.Fatal("unsupported runner")
	}
}
