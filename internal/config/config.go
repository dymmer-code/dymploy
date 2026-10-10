// Package config loads and validates dymploy's own configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"

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

	// Webhooks configures how each repository provider's webhooks are authenticated, by provider name.
	Webhooks map[string]WebhookConfig `yaml:"webhooks"`
	// Policy is the allow-list: which organizations, repositories, branches and tags may start a job.
	// It lives here, never in a repository. With no rules nothing is built.
	Policy []PolicyRule `yaml:"policy"`
	// WebhookMaxBody is the largest accepted webhook body, in bytes.
	WebhookMaxBody int64 `yaml:"webhook_max_body_bytes"`
	// WebhookRatePerMinute limits webhook requests per minute across all providers; 0 disables it.
	WebhookRatePerMinute int `yaml:"webhook_rate_per_minute"`
}

// WebhookConfig is the authentication setup of one provider.
type WebhookConfig struct {
	// SecretFile is an absolute path to a file with the webhook secret (not readable by others).
	SecretFile string `yaml:"secret_file"`
}

// PolicyRule allows pushes of one provider to repositories of one owner (see webhook.Rule).
type PolicyRule struct {
	Provider      string   `yaml:"provider"`
	Owner         string   `yaml:"owner"`
	Repositories  []string `yaml:"repositories"`
	Branches      []string `yaml:"branches"`
	Tags          []string `yaml:"tags"`
	Installations []int64  `yaml:"installations"`
}

// Default returns the configuration used for every field the file leaves out.
func Default() *Config {
	return &Config{
		Listen:        "127.0.0.1:8080",
		ControlSocket: "/run/dymploy/control.sock",
		DataDir:       "/var/lib/dymploy",

		WebhookMaxBody:       5 << 20,
		WebhookRatePerMinute: 600,
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
	if c.WebhookMaxBody < 0 || c.WebhookRatePerMinute < 0 {
		return errors.New("webhook_max_body_bytes and webhook_rate_per_minute must not be negative")
	}
	names := make([]string, 0, len(c.Webhooks))
	for name := range c.Webhooks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if f := c.Webhooks[name].SecretFile; !filepath.IsAbs(f) {
			return fmt.Errorf("webhooks.%s.secret_file %q must be an absolute path", name, f)
		}
	}
	for i, r := range c.Policy {
		if err := r.validate(c.Webhooks); err != nil {
			return fmt.Errorf("policy[%d]: %w", i, err)
		}
	}
	return nil
}

func (r PolicyRule) validate(webhooks map[string]WebhookConfig) error {
	switch {
	case r.Provider == "":
		return errors.New("provider is required")
	case r.Owner == "":
		return errors.New("owner is required")
	case len(r.Repositories) == 0:
		return errors.New("repositories is required (use \"*\" for every repository of the owner)")
	case len(r.Branches) == 0 && len(r.Tags) == 0:
		return errors.New("branches or tags is required: a rule that allows nothing is a mistake")
	}
	if _, ok := webhooks[r.Provider]; !ok {
		return fmt.Errorf("provider %q has no entry under webhooks", r.Provider)
	}
	for _, list := range [][]string{r.Repositories, r.Branches, r.Tags} {
		for _, pat := range list {
			if _, err := path.Match(pat, "x"); err != nil {
				return fmt.Errorf("pattern %q: %w", pat, err)
			}
		}
	}
	return nil
}
