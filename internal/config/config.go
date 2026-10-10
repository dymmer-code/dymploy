// Package config loads and validates dymploy's own configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the daemon configuration. Everything that is specific to an installation
// (listen address, paths) lives here, never in the code.
type Config struct {
	// Listen is the HTTP address for webhooks and the API.
	Listen string `yaml:"listen"`
	// ControlSocket is the Unix socket the CLI uses to talk to the running daemon.
	ControlSocket string `yaml:"control_socket"`
	// DataDir holds repositories, artifacts, job history and logs.
	DataDir string `yaml:"data_dir"`
}

// Default returns the configuration used for every field the file leaves out.
func Default() *Config {
	return &Config{
		Listen:        "127.0.0.1:8080",
		ControlSocket: "/run/dymploy/control.sock",
		DataDir:       "/var/lib/dymploy",
	}
}

// Load reads path, applies defaults and validates. Unknown keys are an error: a typo in a
// security-relevant setting must not be silently ignored.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading configuration %s: %w", path, err)
	}
	c := Default()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing configuration %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("configuration %s: %w", path, err)
	}
	return c, nil
}

// Validate checks the values that would otherwise fail later and far from the cause.
func (c *Config) Validate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q must be host:port: %w", c.Listen, err)
	}
	for name, p := range map[string]string{"control_socket": c.ControlSocket, "data_dir": c.DataDir} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%s %q must be an absolute path", name, p)
		}
	}
	return nil
}
