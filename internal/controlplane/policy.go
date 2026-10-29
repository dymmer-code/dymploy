package controlplane

import (
	"fmt"
	"github.com/dymmer-code/dymploy/internal/config"
)

// CheckLocal compares remote intent with the existing local target catalogue.
// It cannot add a target, select a secret command, or change resource limits.
func CheckLocal(cfg *config.Config, a Application) error {
	if cfg.Runner.Kind != "docker" {
		return fmt.Errorf("application builds require the docker runner")
	}
	seen := map[string]bool{}
	for _, r := range a.Targets {
		t, ok := cfg.Targets[r.Name]
		if !ok || seen[r.Name] || (t.Kind != "" && t.Kind != config.KindSSH) || t.Host != r.Host || t.Port != r.Port || t.User != r.User || t.UploadDir != r.UploadDir {
			return fmt.Errorf("target %q differs from the local catalogue", r.Name)
		}
		seen[r.Name] = true
		if cfg.ControlPlane.SSHKeys[r.SSHKeyRef] != t.KeyFile || cfg.ControlPlane.KnownHosts[r.KnownHostsRef] != t.KnownHosts {
			return fmt.Errorf("target %q has unbound credential references", r.Name)
		}
		if (r.SecretsRef == "" && t.Secrets != nil) || (r.SecretsRef != "" && (t.Secrets == nil || cfg.ControlPlane.Secrets[r.SecretsRef] != r.Name)) {
			return fmt.Errorf("target %q has an unbound secrets reference", r.Name)
		}
	}
	return nil
}
