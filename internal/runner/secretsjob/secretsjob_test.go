package secretsjob_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/deploy/deploytest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
	"github.com/dymmer-code/dymploy/internal/runner/secretsjob"
	"github.com/dymmer-code/dymploy/internal/webhook"
)

type source struct{ data []byte }

func (s source) Fetch(context.Context, deploy.SecretsSpec) ([]byte, error) { return s.data, nil }

var ev = provider.Event{Provider: "github", Owner: "acme", Repo: "app", Commit: "0123456789abcdef0123456789abcdef01234567", Branch: "main"}

const header = "version: 1\nbuild:\n  image: {from: \"alpine:3\"}\n  steps: [{run: \"true\"}]\nartifact: {paths: [a]}\n"

type fx struct {
	fake *deploytest.Fake
	r    *secretsjob.Runner
	mu   sync.Mutex
	log  []string
}

func newFx(t *testing.T, manifest string) *fx {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "dymploy.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fx{fake: &deploytest.Fake{}}
	targets := deploy.NewTargets()
	targets.Set(map[string]deploy.Target{"production": {
		Name: "production", Host: "h", User: "deploy", UploadDir: "up",
		Secrets: &deploy.SecretsSpec{Command: []string{"/x"}},
		Allow:   webhook.Policy{Rules: []webhook.Rule{{Provider: "github", Owner: "acme", Repositories: []string{"app"}, Branches: []string{"main"}}}},
	}})
	d := &deploy.Deployer{Remote: f.fake, Targets: targets, Secrets: source{data: []byte("K=v\n")}}
	released := 0
	r, err := secretsjob.New(secretsjob.Config{
		Source: func(context.Context, runner.Task) (string, func(), provider.Event, error) {
			return dir, func() { released++ }, ev, nil
		},
		Deployer: d,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	t.Cleanup(func() {
		if released != 1 {
			t.Errorf("the working copy must be released exactly once, was %d", released)
		}
	})
	return f
}

func (f *fx) run(t *testing.T) (int, string) {
	t.Helper()
	rep := runner.NewLineReporter(func(s runner.Stream, line string) {
		f.mu.Lock()
		f.log = append(f.log, line)
		f.mu.Unlock()
	})
	res, err := f.r.Run(context.Background(), runner.Task{JobID: "j1", Kind: secretsjob.Kind, Log: rep})
	rep.Close()
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return res.ExitCode, strings.Join(f.log, "\n")
}

func TestItReadsTheDefinitionOfTheCommitAndChangesTheSecrets(t *testing.T) {
	f := newFx(t, header+"deploy:\n  target: production\n  secrets: true\n  steps: [{run: install}]\n  secrets_steps: [{name: env, run: put-env}, {name: restart, run: restart}]\n")
	code, log := f.run(t)
	if code != 0 {
		t.Fatalf("code=%d\n%s", code, log)
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "put-env|restart" {
		t.Fatalf("ran: %s", got)
	}
}

func TestADefinitionThatDoesNotSayHowIsRefused(t *testing.T) {
	f := newFx(t, header+"deploy:\n  target: production\n  secrets: true\n  steps: [{run: install}]\n")
	code, _ := f.run(t)
	if code != deploy.ExitNoSecretsSteps || len(f.fake.Dialed) != 0 {
		t.Fatalf("code=%d dialed=%d", code, len(f.fake.Dialed))
	}
}

func TestADefinitionWithoutDeployHasNoSecretsToChange(t *testing.T) {
	f := newFx(t, header)
	if code, _ := f.run(t); code != deploy.ExitNoSecretsSteps {
		t.Fatalf("code=%d", code)
	}
}

func TestAMissingOrBrokenManifestChangesNothing(t *testing.T) {
	for _, doc := range []string{"", "version: 2\n"} {
		f := newFx(t, doc)
		code, _ := f.run(t)
		if code != secretsjob.ExitBadManifest || len(f.fake.Dialed) != 0 {
			t.Fatalf("%q: code=%d dialed=%d", doc, code, len(f.fake.Dialed))
		}
	}
}

func TestATargetTheRepositoryMayNotUseIsDenied(t *testing.T) {
	f := newFx(t, header+"deploy:\n  target: staging\n  secrets: true\n  steps: [{run: install}]\n  secrets_steps: [{run: put-env}]\n")
	code, _ := f.run(t)
	if code != deploy.ExitTargetDenied || len(f.fake.Dialed) != 0 {
		t.Fatalf("code=%d dialed=%d", code, len(f.fake.Dialed))
	}
}

func TestItNeedsASourceAndADeployer(t *testing.T) {
	if _, err := secretsjob.New(secretsjob.Config{}); err == nil {
		t.Fatal("an incomplete configuration must be refused")
	}
}
