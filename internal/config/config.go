// Package config loads and validates dymploy's own configuration file.
package config

import (
	"bytes"
	"encoding/json"
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

	// Queue sets the job limits and how long history is kept.
	Queue QueueConfig `yaml:"queue"`
	// Runner is how the jobs started by webhooks are executed.
	Runner RunnerConfig `yaml:"runner"`
	// RepoKeepDays removes working copies of repositories not used for this many days; 0 keeps them.
	RepoKeepDays int `yaml:"repo_keep_days"`
}

// QueueConfig sets the limits of the job queue.
type QueueConfig struct {
	// MaxConcurrent is how many jobs run at once; jobs with the same name never overlap.
	MaxConcurrent int `yaml:"max_concurrent"`
	// MaxPending is how many jobs may wait before new ones are refused.
	MaxPending int `yaml:"max_pending"`
	// OnBusy is what happens to a new job whose name is already waiting or running:
	// queue (wait), coalesce (replace the waiting one) or reject.
	OnBusy string `yaml:"on_busy"`
	// LogMaxBytes caps the log of each job.
	LogMaxBytes int64 `yaml:"log_max_bytes"`
	// KeepJobs and KeepDays bound the history of finished jobs; 0 disables each limit.
	KeepJobs int `yaml:"keep_jobs"`
	KeepDays int `yaml:"keep_days"`
}

// RunnerConfig selects the runner for webhook jobs and carries its configuration, which nothing
// but that runner interprets.
type RunnerConfig struct {
	Kind   string         `yaml:"kind"`
	Params map[string]any `yaml:"params"`

	paramsJSON json.RawMessage // set by Validate
}

// ParamsJSON returns Params as the opaque JSON the queue transports; nil when there are none. It is
// ready after Load/Validate, which reject parameters that cannot be represented.
func (r RunnerConfig) ParamsJSON() json.RawMessage { return r.paramsJSON }

func (r *RunnerConfig) prepare() error {
	r.paramsJSON = nil
	if len(r.Params) == 0 {
		return nil
	}
	raw, err := json.Marshal(r.Params)
	if err != nil {
		return fmt.Errorf("runner.params cannot be handed to the runner: %w", err)
	}
	r.paramsJSON = raw
	return nil
}

// WebhookConfig is the authentication setup of one provider.
type WebhookConfig struct {
	// SecretFile is an absolute path to a file with the webhook secret (not readable by others).
	SecretFile string `yaml:"secret_file"`
	// AppID and PrivateKeyFile identify the GitHub App used to clone private repositories (both or
	// neither). The key file must not be readable by other users.
	AppID          int64  `yaml:"app_id"`
	PrivateKeyFile string `yaml:"private_key_file"`
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

		RepoKeepDays: 14,
		Queue:        QueueConfig{MaxConcurrent: 2, MaxPending: 1000, OnBusy: "queue", LogMaxBytes: 10 << 20, KeepJobs: 500, KeepDays: 30},
		Runner:       RunnerConfig{Kind: "dryrun"},
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
	if c.RepoKeepDays < 0 {
		return errors.New("repo_keep_days must not be negative")
	}
	if c.WebhookMaxBody < 0 || c.WebhookRatePerMinute < 0 {
		return errors.New("webhook_max_body_bytes and webhook_rate_per_minute must not be negative")
	}
	if err := c.Queue.validate(); err != nil {
		return err
	}
	if c.Runner.Kind == "" {
		return errors.New("runner.kind must not be empty")
	}
	if err := c.Runner.prepare(); err != nil {
		return err
	}
	names := make([]string, 0, len(c.Webhooks))
	for name := range c.Webhooks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w := c.Webhooks[name]
		if !filepath.IsAbs(w.SecretFile) {
			return fmt.Errorf("webhooks.%s.secret_file %q must be an absolute path", name, w.SecretFile)
		}
		if w.AppID < 0 || (w.AppID == 0) != (w.PrivateKeyFile == "") {
			return fmt.Errorf("webhooks.%s: app_id and private_key_file must be given together", name)
		}
		if w.PrivateKeyFile != "" && !filepath.IsAbs(w.PrivateKeyFile) {
			return fmt.Errorf("webhooks.%s.private_key_file %q must be an absolute path", name, w.PrivateKeyFile)
		}
	}
	for i, r := range c.Policy {
		if err := r.validate(c.Webhooks); err != nil {
			return fmt.Errorf("policy[%d]: %w", i, err)
		}
	}
	return nil
}

func (q QueueConfig) validate() error {
	switch {
	case q.MaxConcurrent < 1:
		return errors.New("queue.max_concurrent must be at least 1")
	case q.MaxPending < 0 || q.LogMaxBytes < 0 || q.KeepJobs < 0 || q.KeepDays < 0:
		return errors.New("queue limits must not be negative")
	}
	switch q.OnBusy {
	case "queue", "coalesce", "reject":
	default:
		return fmt.Errorf("queue.on_busy %q must be queue, coalesce or reject", q.OnBusy)
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
