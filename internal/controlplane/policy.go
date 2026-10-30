package controlplane

import (
	"fmt"
	"net"
	"slices"

	"github.com/dymmer-code/dymploy/internal/config"
	"github.com/dymmer-code/dymploy/internal/deploy"
)

// lookupHost resolves a host name; it is a variable so that the check can be tested without a network.
var lookupHost = net.LookupHost

// sameHost says whether the host the control plane names is the machine of the local target. The catalogue decides where
// a target connects, so this is a consistency check on what a person is shown and accepts, not a gate: the same text, or,
// when the control plane gives the addresses it has for the host, one address in common with what the local one resolves to.
func sameHost(localHost string, r Target) bool {
	if localHost == r.Host {
		return true
	}
	if len(r.Addresses) == 0 {
		return false
	}
	local, err := lookupHost(localHost)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(local, func(address string) bool { return slices.Contains(r.Addresses, address) })
}

// A port or a directory left out means the default one, on either side.
func portOrDefault(port int) int {
	if port == 0 {
		return 22
	}
	return port
}

func uploadDirOrDefault(dir string) string {
	if dir == "" {
		return deploy.DefaultUploadDir
	}
	return dir
}

// CheckLocal compares remote intent with the existing local target catalogue.
// It cannot add a target, select a secret command, or change resource limits.
func CheckLocal(cfg *config.Config, a Application) error {
	if cfg.Runner.Kind != "docker" {
		return fmt.Errorf("application builds require the docker runner")
	}
	seen := map[string]bool{}
	for _, r := range a.Targets {
		t, ok := cfg.Targets[r.Name]
		if !ok || seen[r.Name] || (t.Kind != "" && t.Kind != config.KindSSH) || !sameHost(t.Host, r) ||
			portOrDefault(t.Port) != portOrDefault(r.Port) || t.User != r.User ||
			uploadDirOrDefault(t.UploadDir) != uploadDirOrDefault(r.UploadDir) {
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
