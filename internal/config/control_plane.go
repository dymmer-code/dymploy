package config

import (
	"errors"
	"net/url"
	"path/filepath"
)

// ControlPlaneConfig is local trust configuration, never supplied by Dymmer.
type ControlPlaneConfig struct {
	URL        string            `yaml:"url"`
	BuilderID  string            `yaml:"builder_id"`
	TokenFile  string            `yaml:"token_file"`
	SSHKeys    map[string]string `yaml:"ssh_keys"`
	KnownHosts map[string]string `yaml:"known_hosts"`
	Secrets    map[string]string `yaml:"secrets"` // opaque reference -> local target name
}

func (c ControlPlaneConfig) validate() error {
	if c.URL == "" {
		if c.BuilderID != "" || c.TokenFile != "" {
			return errors.New("control_plane.url is required")
		}
		return nil
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/builders/websocket" {
		return errors.New("control_plane.url must be wss://host/builders/websocket")
	}
	if c.BuilderID == "" || !filepath.IsAbs(c.TokenFile) {
		return errors.New("control_plane needs builder_id and absolute token_file")
	}
	for _, refs := range []map[string]string{c.SSHKeys, c.KnownHosts} {
		for name, path := range refs {
			if name == "" || !filepath.IsAbs(path) {
				return errors.New("control_plane credential references need names and absolute paths")
			}
		}
	}
	return nil
}
