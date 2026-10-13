package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsForEmptyFile(t *testing.T) {
	c, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || c.ControlSocket != "/run/dymploy/control.sock" || c.DataDir != "/var/lib/dymploy" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := Load(write(t, "listen: 0.0.0.0:9000\ncontrol_socket: /tmp/x.sock\ndata_dir: /srv/dp\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.ControlSocket != "/tmp/x.sock" || c.DataDir != "/srv/dp" {
		t.Fatalf("overrides lost: %+v", c)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	_, err := Load(write(t, "listen: 127.0.0.1:1\nlisten_adress: typo\n"))
	if err == nil || !strings.Contains(err.Error(), "listen_adress") {
		t.Fatalf("a typo in the configuration must be reported, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"listen without port":     "listen: localhost\n",
		"relative control socket": "control_socket: run/control.sock\n",
		"relative data dir":       "data_dir: data\n",
		"empty listen":            "listen: \"\"\n",
		"malformed yaml":          "listen: [\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("%q must be rejected", body)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
		t.Fatalf("the error must name the file, got %v", err)
	}
}

const withWebhooks = `
webhooks:
  github:
    secret_file: /etc/dymploy/github.secret
policy:
  - provider: github
    owner: acme
    repositories: ["*"]
    branches: [main]
    installations: [4242]
  - provider: github
    owner: globex
    repositories: [dymploy]
    tags: ["v*"]
`

func TestWebhooksAndPolicy(t *testing.T) {
	c, err := Load(write(t, withWebhooks))
	if err != nil {
		t.Fatal(err)
	}
	if c.Webhooks["github"].SecretFile != "/etc/dymploy/github.secret" {
		t.Fatalf("%+v", c.Webhooks)
	}
	if len(c.Policy) != 2 || c.Policy[0].Installations[0] != 4242 || c.Policy[1].Tags[0] != "v*" {
		t.Fatalf("%+v", c.Policy)
	}
	if c.WebhookMaxBody != 5<<20 || c.WebhookRatePerMinute != 600 {
		t.Fatalf("limits: %d %d", c.WebhookMaxBody, c.WebhookRatePerMinute)
	}
}

func TestLimitsCanBeSet(t *testing.T) {
	c, err := Load(write(t, "webhook_max_body_bytes: 1024\nwebhook_rate_per_minute: 0\n"))
	if err != nil || c.WebhookMaxBody != 1024 || c.WebhookRatePerMinute != 0 {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestPolicyValidation(t *testing.T) {
	hook := "webhooks:\n  github:\n    secret_file: /s\n"
	cases := map[string]string{
		"relative secret file":              "webhooks:\n  github:\n    secret_file: s\n",
		"missing secret file":               "webhooks:\n  github: {}\n",
		"rule for an unconfigured provider": hook + "policy:\n  - {provider: gitlab, owner: a, repositories: [x], branches: [main]}\n",
		"rule without provider":             hook + "policy:\n  - {owner: a, repositories: [x], branches: [main]}\n",
		"rule without owner":                hook + "policy:\n  - {provider: github, repositories: [x], branches: [main]}\n",
		"rule without repositories":         hook + "policy:\n  - {provider: github, owner: a, branches: [main]}\n",
		"rule without branches nor tags":    hook + "policy:\n  - {provider: github, owner: a, repositories: [x]}\n",
		"bad glob":                          hook + "policy:\n  - {provider: github, owner: a, repositories: [\"[\"], branches: [main]}\n",
		"negative body limit":               "webhook_max_body_bytes: -1\n",
		"negative rate":                     "webhook_rate_per_minute: -5\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("must be rejected:\n%s", body)
			}
		})
	}
}

func TestQueueAndRunnerDefaults(t *testing.T) {
	c, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	q := c.Queue
	if q.MaxConcurrent != 2 || q.MaxPending != 1000 || q.OnBusy != "queue" || q.LogMaxBytes != 10<<20 || q.KeepJobs != 500 || q.KeepDays != 30 {
		t.Fatalf("%+v", q)
	}
	if c.Runner.Kind != "dryrun" || len(c.Runner.Params) != 0 {
		t.Fatalf("until a real runner is configured, jobs are only reported: %+v", c.Runner)
	}
}

func TestQueueAndRunnerOverrides(t *testing.T) {
	c, err := Load(write(t, `
queue:
  max_concurrent: 4
  max_pending: 50
  on_busy: coalesce
  log_max_bytes: 1048576
  keep_jobs: 10
  keep_days: 0
runner:
  kind: docker
  params:
    image: debian:13
    nested: {list: [1, 2, {x: y}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Queue.MaxConcurrent != 4 || c.Queue.OnBusy != "coalesce" || c.Queue.KeepDays != 0 {
		t.Fatalf("%+v", c.Queue)
	}
	if c.Runner.Kind != "docker" || c.Runner.Params["image"] != "debian:13" {
		t.Fatalf("%+v", c.Runner)
	}
	if raw := c.Runner.ParamsJSON(); string(raw) != `{"image":"debian:13","nested":{"list":[1,2,{"x":"y"}]}}` {
		t.Fatalf("the parameters travel as opaque JSON: %s", raw)
	}
}

func TestEmptyParamsAreNoParams(t *testing.T) {
	if raw := (RunnerConfig{Kind: "x"}).ParamsJSON(); raw != nil {
		t.Fatalf("%q", raw)
	}
}

func TestQueueValidation(t *testing.T) {
	for name, body := range map[string]string{
		"no concurrency":            "queue:\n  max_concurrent: 0\n",
		"negative pending":          "queue:\n  max_pending: -1\n",
		"unknown policy":            "queue:\n  on_busy: sometimes\n",
		"negative log cap":          "queue:\n  log_max_bytes: -1\n",
		"negative keep":             "queue:\n  keep_jobs: -1\n",
		"negative days":             "queue:\n  keep_days: -1\n",
		"empty runner":              "runner:\n  kind: \"\"\n",
		"params that cannot travel": "runner:\n  kind: x\n  params:\n    ratio: .inf\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("must be rejected:\n%s", body)
			}
		})
	}
}

func TestGitHubAppSettings(t *testing.T) {
	c, err := Load(write(t, `
webhooks:
  github:
    secret_file: /etc/dymploy/hook.secret
    app_id: 123456
    private_key_file: /etc/dymploy/app.pem
repo_keep_days: 7
`))
	if err != nil {
		t.Fatal(err)
	}
	g := c.Webhooks["github"]
	if g.AppID != 123456 || g.PrivateKeyFile != "/etc/dymploy/app.pem" || c.RepoKeepDays != 7 {
		t.Fatalf("%+v %d", g, c.RepoKeepDays)
	}
}

func TestArtifactKeepDaysDefaultsToTwoWeeks(t *testing.T) {
	c, _ := Load(write(t, ""))
	if c.ArtifactKeepDays != 14 {
		t.Fatal(c.ArtifactKeepDays)
	}
}

func TestRepoKeepDaysDefault(t *testing.T) {
	c, _ := Load(write(t, ""))
	if c.RepoKeepDays != 14 {
		t.Fatal(c.RepoKeepDays)
	}
}

func TestAppSettingsValidation(t *testing.T) {
	base := "webhooks:\n  github:\n    secret_file: /s\n"
	for name, body := range map[string]string{
		"app id without key":          base + "    app_id: 1\n",
		"key without app id":          base + "    private_key_file: /k\n",
		"relative key":                base + "    app_id: 1\n    private_key_file: k.pem\n",
		"negative app id":             base + "    app_id: -1\n    private_key_file: /k\n",
		"negative repo keep days":     "repo_keep_days: -1\n",
		"negative artifact keep days": "artifact_keep_days: -1\n",
		"negative cache keep days":    "cache_keep_days: -1\n",
		"negative cache limit":        "cache_max_mb: -5\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("must be rejected:\n%s", body)
			}
		})
	}
}
