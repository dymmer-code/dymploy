package controlplane

import (
	"net"
	"testing"

	"github.com/dymmer-code/dymploy/internal/config"
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

func localConfig(target config.TargetConfig) *config.Config {
	cfg := config.Default()
	cfg.Runner.Kind = "docker"
	cfg.ControlPlane.SSHKeys = map[string]string{"app": "/local/key"}
	cfg.ControlPlane.KnownHosts = map[string]string{"app": "/local/known"}
	target.KeyFile, target.KnownHosts = "/local/key", "/local/known"
	cfg.Targets = map[string]config.TargetConfig{"app": target}
	return cfg
}

func definition(target Target) Application {
	target.Name, target.SSHKeyRef, target.KnownHostsRef = "app", "app", "app"
	return Application{Repository: Repository{Provider: "github", Owner: "acme", Name: "app"}, Targets: []Target{target}}
}

func TestAPortOrDirectoryLeftOutIsTheDefaultOnEitherSide(t *testing.T) {
	cfg := localConfig(config.TargetConfig{Host: "10.0.0.7", User: "deploy"})
	// the control plane says them, the catalogue leaves them out
	if err := CheckLocal(cfg, definition(Target{Host: "10.0.0.7", Port: 22, User: "deploy", UploadDir: "dymploy-uploads"})); err != nil {
		t.Fatal(err)
	}
	// and the other way round
	cfg = localConfig(config.TargetConfig{Host: "10.0.0.7", Port: 22, User: "deploy", UploadDir: "dymploy-uploads"})
	if err := CheckLocal(cfg, definition(Target{Host: "10.0.0.7", User: "deploy"})); err != nil {
		t.Fatal(err)
	}
	// a different one is still a different target
	for _, other := range []Target{{Host: "10.0.0.7", Port: 2222, User: "deploy"}, {Host: "10.0.0.7", User: "deploy", UploadDir: "/etc"}} {
		if CheckLocal(cfg, definition(other)) == nil {
			t.Fatalf("%+v", other)
		}
	}
}

func TestTheHostIsRecognisedByAddressWhenTheNameIsNotTheSame(t *testing.T) {
	original := lookupHost
	defer func() { lookupHost = original }()
	lookupHost = func(host string) ([]string, error) {
		switch host {
		case "app.internal":
			return []string{"192.0.2.57", "2001:db8::57"}, nil
		case "192.0.2.57":
			return []string{"192.0.2.57"}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}

	for _, local := range []string{"app.internal", "192.0.2.57"} {
		cfg := localConfig(config.TargetConfig{Host: local, User: "deploy"})
		// the control plane calls the host by another name, with the addresses it has
		if err := CheckLocal(cfg, definition(Target{Host: "rowan", User: "deploy", Addresses: []string{"198.51.100.1", "192.0.2.57"}})); err != nil {
			t.Fatalf("%s: %v", local, err)
		}
		// no address in common: it is another machine
		if CheckLocal(cfg, definition(Target{Host: "rowan", User: "deploy", Addresses: []string{"198.51.100.1"}})) == nil {
			t.Fatalf("%s: another machine was accepted", local)
		}
		// a name that is not the local one and no addresses: nothing to recognise it by
		if CheckLocal(cfg, definition(Target{Host: "rowan", User: "deploy"})) == nil {
			t.Fatalf("%s: a different name with no addresses was accepted", local)
		}
	}

	// a local host that does not resolve is not recognised by address
	cfg := localConfig(config.TargetConfig{Host: "gone.internal", User: "deploy"})
	if CheckLocal(cfg, definition(Target{Host: "rowan", User: "deploy", Addresses: []string{"192.0.2.57"}})) == nil {
		t.Fatal("an unresolvable host was accepted")
	}

	// the user is never relaxed
	cfg = localConfig(config.TargetConfig{Host: "app.internal", User: "deploy"})
	if CheckLocal(cfg, definition(Target{Host: "rowan", User: "root", Addresses: []string{"192.0.2.57"}})) == nil {
		t.Fatal("another user was accepted")
	}
}

func TestTheAddressesOfAHostDoNotChangeWhatWasAccepted(t *testing.T) {
	a := definition(Target{Host: "rowan", User: "deploy"})
	b := definition(Target{Host: "rowan", User: "deploy", Addresses: []string{"192.0.2.57"}})
	if a.Digest() != b.Digest() {
		t.Fatal("the digest changed with the addresses")
	}
	if b.Targets[0].Addresses == nil {
		t.Fatal("the digest cleared the addresses of the definition")
	}
	b.Targets[0].User = "root"
	if a.Digest() == b.Digest() {
		t.Fatal("the digest ignores the user")
	}
}
