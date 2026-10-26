// Package config loads and validates dymploy's own configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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
	// Targets are the places a build may be deployed to, by name. A repository's dymploy.yaml only names
	// one; where it is, how to log in and who may use it is decided here.
	Targets map[string]TargetConfig `yaml:"targets"`
	// API opens a read-only view of the jobs (and the live channel) on the public listener. Without a
	// token file it stays closed: the control socket is always available to the local user.
	API APIConfig `yaml:"api"`
	// WebhookMaxBody is the largest accepted webhook body, in bytes.
	WebhookMaxBody int64 `yaml:"webhook_max_body_bytes"`
	// TrustedProxies are the addresses (CIDR) of the reverse proxies in front of dymploy. Only requests that
	// come from them have their X-Forwarded-For believed, so the real address of a client is known (for the
	// per-address limit and the logs). Without any, the address of the connection is used.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// WebhookRatePerIPPerMinute limits webhook requests per minute from each client address, before the
	// shared limit below counts anything; 0 disables it.
	WebhookRatePerIPPerMinute int `yaml:"webhook_rate_per_ip_per_minute"`
	// WebhookRatePerMinute limits webhook requests per minute across all providers; 0 disables it.
	WebhookRatePerMinute int `yaml:"webhook_rate_per_minute"`

	// Queue sets the job limits and how long history is kept.
	Queue QueueConfig `yaml:"queue"`
	// Runner is how the jobs started by webhooks are executed.
	Runner RunnerConfig `yaml:"runner"`
	// ShutdownTimeout is how long a stop waits for running jobs to stop before the daemon leaves anyway.
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	// RepoKeepDays removes working copies of repositories not used for this many days; 0 keeps them.
	RepoKeepDays int `yaml:"repo_keep_days"`
	// ArtifactKeepDays removes the artifacts of builds older than this many days; 0 keeps them.
	ArtifactKeepDays int `yaml:"artifact_keep_days"`
	// CacheKeepDays removes build caches (the `build.cache` directories kept between builds) not used for this
	// many days; 0 keeps them. CacheMaxMB empties a cache that grows past this size in MiB, so the next build
	// starts from nothing; 0 means no limit. Without them a build can make a cache grow without bound.
	CacheKeepDays int `yaml:"cache_keep_days"`
	CacheMaxMB    int `yaml:"cache_max_mb"`
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
	// StopGrace is how long a job may take to stop after being asked to before the daemon's log
	// complains about it.
	StopGrace time.Duration `yaml:"stop_grace"`
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
	// ReportStatus shows the state of every run next to its commit in the provider (on GitHub, the green
	// tick or the red cross). It needs the App, and the App needs the commit-status permission accepted by
	// each installation.
	ReportStatus bool `yaml:"report_status"`
	// VerifyTip asks the provider, before queueing a push, whether its commit is still where the branch or
	// tag points, and ignores it if not (a late, out-of-order or replayed delivery). On by default; it needs
	// the provider to be able to answer (on GitHub, the App).
	VerifyTip *bool `yaml:"verify_tip"`
}

// VerifiesTip reports whether pushes of this provider are checked against the provider (default: yes).
func (w WebhookConfig) VerifiesTip() bool { return w.VerifyTip == nil || *w.VerifyTip }

// APIConfig configures the read-only API of the public listener, under /api/v1/.
type APIConfig struct {
	// TokenFile holds the bearer token clients must send (not readable by others). Empty = no API.
	TokenFile string `yaml:"token_file"`
	// AllowedOrigins are the web origins (host patterns) that may open the live channel from a browser.
	AllowedOrigins []string `yaml:"allowed_origins"`
	// MaxStreams bounds the live channels open at once; 0 means 16.
	MaxStreams int `yaml:"max_streams"`
}

// Target kinds. A target is a machine reached over SSH, where the steps of the definition run (the default),
// or a bucket of an S3 server, where the files of the artifact are put.
const (
	KindSSH = "ssh"
	KindS3  = "s3"
)

// TargetConfig is a deployment target: a machine reached over SSH, or an S3 bucket (kind: s3). The key, the
// known_hosts file and the credentials are paths to files on this machine; nothing secret is written in the
// configuration itself.
type TargetConfig struct {
	// Kind is ssh (default) or s3. The fields of the other kind must be left out.
	Kind string `yaml:"kind"`
	// Only for s3: Endpoint is the URL of the S3 server (http or https, path-style requests), Bucket the bucket
	// and Region the one the server signs for (default us-east-1). CredentialsFile holds the lines
	// S3_ACCESS_KEY_ID=... and S3_SECRET_ACCESS_KEY=... (not readable by others): a key that can only write in
	// that bucket.
	Endpoint        string `yaml:"endpoint"`
	Bucket          string `yaml:"bucket"`
	Region          string `yaml:"region"`
	CredentialsFile string `yaml:"credentials_file"`

	Host string `yaml:"host"`
	Port int    `yaml:"port"` // default 22
	User string `yaml:"user"`
	// KeyFile is the private key to log in with (not readable by others). KnownHosts holds the host's
	// public key: the connection is refused if the host does not present it.
	KeyFile    string `yaml:"key_file"`
	KnownHosts string `yaml:"known_hosts"`
	// UploadDir is where the artifact is copied on the target (absolute, or relative to the login
	// directory). Default: dymploy-uploads.
	UploadDir string `yaml:"upload_dir"`
	// Allow says which repositories, branches and tags may deploy here. Required: a target nobody can
	// use is a mistake, and one everybody can use is a risk. It is the same rule as `policy`.
	Allow []PolicyRule `yaml:"allow"`
	// Secrets is where the secrets of what is deployed here come from, for definitions that ask for them
	// (deploy.secrets). It lives here and never in dymploy.yaml: a repository must not be able to name
	// the source of another application's secrets.
	Secrets *SecretsConfig `yaml:"secrets"`
}

// SecretsConfig is a command that prints the target's secrets on its standard output. dymploy runs it
// itself (no shell, a minimal environment) when a deployment asks for them, and never logs what it prints.
type SecretsConfig struct {
	// Command is the program and its arguments; the program must be an absolute path.
	Command []string `yaml:"command"`
	// EnvFile is an optional file of NAME=value lines added to the command's environment, for the
	// credential the command needs (not readable by others).
	EnvFile string `yaml:"env_file"`
	// Timeout bounds the command. Default 30s, at most 5m.
	Timeout time.Duration `yaml:"timeout"`
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

		WebhookMaxBody:            5 << 20,
		WebhookRatePerMinute:      600,
		WebhookRatePerIPPerMinute: 60,

		ShutdownTimeout:  2 * time.Minute,
		RepoKeepDays:     14,
		ArtifactKeepDays: 14,
		CacheKeepDays:    30,
		CacheMaxMB:       5120,
		Queue:            QueueConfig{StopGrace: 30 * time.Second, MaxConcurrent: 2, MaxPending: 1000, OnBusy: "queue", LogMaxBytes: 10 << 20, KeepJobs: 500, KeepDays: 30},
		Runner:           RunnerConfig{Kind: "dryrun"},
	}
}

// Load reads the main file at path and every config.d/*.yaml next to it, applies defaults and validates
// the result. Unknown keys are an error: a typo in a security-relevant setting must not be silently
// ignored. See merge for how the files are combined.
func Load(path string) (*Config, error) {
	files := []string{path}
	extra, err := dropIns(path)
	if err != nil {
		return nil, err
	}
	files = append(files, extra...)
	srcs := make([]source, 0, len(files))
	for _, f := range files {
		s, err := readSource(f)
		if err != nil {
			return nil, err
		}
		srcs = append(srcs, s)
	}
	merged, err := merge(srcs)
	if err != nil {
		return nil, err
	}
	c := Default()
	if len(merged.Content) > 0 {
		if err := merged.Decode(c); err != nil {
			return nil, fmt.Errorf("configuration %s: %w", path, err)
		}
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
	if c.ShutdownTimeout < time.Second || c.Queue.StopGrace < 0 {
		return errors.New("shutdown_timeout must be at least 1s (for example 2m) and queue.stop_grace must not be negative")
	}
	if c.RepoKeepDays < 0 || c.ArtifactKeepDays < 0 || c.CacheKeepDays < 0 || c.CacheMaxMB < 0 {
		return errors.New("repo_keep_days, artifact_keep_days, cache_keep_days and cache_max_mb must not be negative")
	}
	if c.WebhookMaxBody < 0 || c.WebhookRatePerMinute < 0 || c.WebhookRatePerIPPerMinute < 0 {
		return errors.New("webhook_max_body_bytes, webhook_rate_per_minute and webhook_rate_per_ip_per_minute must not be negative")
	}
	for _, p := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			if _, err := netip.ParseAddr(p); err != nil {
				return fmt.Errorf("trusted_proxies: %q is not an address or a CIDR range", p)
			}
		}
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
		if w.ReportStatus && w.AppID == 0 {
			return fmt.Errorf("webhooks.%s.report_status needs app_id and private_key_file: statuses are published as the App", name)
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
	if err := c.API.validate(); err != nil {
		return err
	}
	return c.validateTargets()
}

func (a APIConfig) validate() error {
	switch {
	case a.TokenFile != "" && !filepath.IsAbs(a.TokenFile):
		return fmt.Errorf("api.token_file %q must be an absolute path", a.TokenFile)
	case a.TokenFile == "" && (len(a.AllowedOrigins) > 0 || a.MaxStreams != 0):
		return errors.New("api.allowed_origins and api.max_streams need api.token_file: without a token the API is closed")
	case a.MaxStreams < 0:
		return errors.New("api.max_streams must not be negative")
	}
	for _, o := range a.AllowedOrigins {
		if _, err := path.Match(o, "x"); err != nil || o == "" {
			return fmt.Errorf("api.allowed_origins: %q is not a valid pattern", o)
		}
	}
	return nil
}

var (
	targetNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	bucketRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE     = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	targetUserRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	uploadDirRE  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
)

func (c *Config) validateTargets() error {
	names := make([]string, 0, len(c.Targets))
	for name := range c.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t := c.Targets[name]
		p := "targets." + name
		if !targetNameRE.MatchString(name) {
			return fmt.Errorf("%s: the name must be lowercase letters, digits and - (it is what dymploy.yaml writes in deploy.target)", p)
		}
		switch t.Kind {
		case "", KindSSH:
			if t.Endpoint != "" || t.Bucket != "" || t.Region != "" || t.CredentialsFile != "" {
				return fmt.Errorf("%s: endpoint, bucket, region and credentials_file are for kind: s3", p)
			}
		case KindS3:
			if err := t.validateS3(p, c.Webhooks); err != nil {
				return err
			}
			continue
		default:
			return fmt.Errorf("%s.kind %q is not valid: use ssh or s3", p, t.Kind)
		}
		switch {
		case t.Host == "" || strings.ContainsAny(t.Host, " /@\t\n"):
			return fmt.Errorf("%s.host %q is not a host name or address", p, t.Host)
		case t.Port < 0 || t.Port > 65535:
			return fmt.Errorf("%s.port %d is not valid", p, t.Port)
		case !targetUserRE.MatchString(t.User):
			return fmt.Errorf("%s.user %q is not a valid user name", p, t.User)
		case !filepath.IsAbs(t.KeyFile):
			return fmt.Errorf("%s.key_file %q must be an absolute path", p, t.KeyFile)
		case !filepath.IsAbs(t.KnownHosts):
			return fmt.Errorf("%s.known_hosts %q must be an absolute path (host keys are never accepted blindly)", p, t.KnownHosts)
		case t.UploadDir != "" && (!uploadDirRE.MatchString(t.UploadDir) || strings.Contains(t.UploadDir, "..") || strings.HasPrefix(t.UploadDir, "-")):
			return fmt.Errorf("%s.upload_dir %q is not a plain path", p, t.UploadDir)
		case len(t.Allow) == 0:
			return fmt.Errorf("%s.allow is required: say which repositories may deploy here", p)
		}
		if err := t.Secrets.validate(); err != nil {
			return fmt.Errorf("%s.secrets: %w", p, err)
		}
		for i, r := range t.Allow {
			if err := r.validate(c.Webhooks); err != nil {
				return fmt.Errorf("%s.allow[%d]: %w", p, i, err)
			}
		}
	}
	return nil
}

func (t TargetConfig) validateS3(p string, webhooks map[string]WebhookConfig) error {
	u, err := url.Parse(t.Endpoint)
	switch {
	case t.Host != "" || t.Port != 0 || t.User != "" || t.KeyFile != "" || t.KnownHosts != "" || t.UploadDir != "" || t.Secrets != nil:
		return fmt.Errorf("%s: host, port, user, key_file, known_hosts, upload_dir and secrets are for kind: ssh", p)
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "":
		return fmt.Errorf("%s.endpoint %q must be the URL of the server, such as http://10.0.0.5:3900", p, t.Endpoint)
	case !bucketRE.MatchString(t.Bucket):
		return fmt.Errorf("%s.bucket %q is not a bucket name", p, t.Bucket)
	case t.Region != "" && !regionRE.MatchString(t.Region):
		return fmt.Errorf("%s.region %q is not valid", p, t.Region)
	case !filepath.IsAbs(t.CredentialsFile):
		return fmt.Errorf("%s.credentials_file %q must be an absolute path", p, t.CredentialsFile)
	case len(t.Allow) == 0:
		return fmt.Errorf("%s.allow is required: say which repositories may deploy here", p)
	}
	for i, r := range t.Allow {
		if err := r.validate(webhooks); err != nil {
			return fmt.Errorf("%s.allow[%d]: %w", p, i, err)
		}
	}
	return nil
}

func (s *SecretsConfig) validate() error {
	if s == nil {
		return nil
	}
	switch {
	case len(s.Command) == 0 || !filepath.IsAbs(s.Command[0]):
		return errors.New("command is required and its program must be an absolute path")
	case s.EnvFile != "" && !filepath.IsAbs(s.EnvFile):
		return fmt.Errorf("env_file %q must be an absolute path", s.EnvFile)
	case s.Timeout < 0 || s.Timeout > 5*time.Minute:
		return errors.New("timeout must be between 0 (the default, 30s) and 5m")
	}
	for _, a := range s.Command {
		if strings.ContainsRune(a, 0) {
			return errors.New("command arguments must not contain NUL")
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

// TrustedPrefixes returns trusted_proxies as prefixes (a single address is a /32 or /128).
func (c *Config) TrustedPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, p := range c.TrustedProxies {
		if pf, err := netip.ParsePrefix(p); err == nil {
			out = append(out, pf)
		} else if a, err := netip.ParseAddr(p); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}
