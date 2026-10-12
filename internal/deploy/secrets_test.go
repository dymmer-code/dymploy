package deploy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

const secretValue = "API_TOKEN=s3cr3t-value-that-must-not-leak"

type fakeSecrets struct {
	data  []byte
	err   error
	calls int
	spec  deploy.SecretsSpec
}

func (s *fakeSecrets) Fetch(_ context.Context, spec deploy.SecretsSpec) ([]byte, error) {
	s.calls++
	s.spec = spec
	return s.data, s.err
}

const withSecrets = `deploy:
  target: production
  secrets: true
  steps:
    - {name: install, run: "install it"}
`

func withSource(f *fx, src *fakeSecrets) deploy.Target {
	f.d.Secrets = src
	t := target()
	t.Secrets = &deploy.SecretsSpec{Command: []string{"/usr/local/bin/source", "app"}}
	return t
}

func TestSecretsAreFetchedUploadedGivenToTheStepsAndRemoved(t *testing.T) {
	f := newFx(t)
	src := &fakeSecrets{data: []byte(secretValue + "\n")}
	tg := withSource(f, src)
	var during string
	f.fake.Handle = func(script string) (int, string, bool) {
		if script == "install it" { // the file is on the target while the steps run
			during = string(f.fake.UploadedAt("up/job-1-secrets"))
		}
		return 0, "", false
	}
	code, err := f.runWith(context.Background(), tg, definition(t, withSecrets))
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
	if src.calls != 1 || src.spec.Command[0] != "/usr/local/bin/source" {
		t.Fatalf("the source was asked %d times with %v", src.calls, src.spec)
	}
	var env map[string]string
	for _, r := range f.fake.Runs {
		if r.Script == "install it" {
			env = r.Env
		}
	}
	if env["DYMPLOY_SECRETS_FILE"] != "up/job-1-secrets" {
		t.Fatalf("the steps must find the file: %q", env["DYMPLOY_SECRETS_FILE"])
	}
	if during != secretValue+"\n" {
		t.Fatalf("on the target while the steps ran: %q", during)
	}
	if f.fake.UploadedAt("up/job-1-secrets") != nil {
		t.Fatal("the secrets file must be gone when the deployment ends")
	}
	if strings.Join(f.fake.Removed, ",") != "up/job-1-app.tar.gz,up/job-1-secrets" {
		t.Fatalf("both uploads must be removed: %v", f.fake.Removed)
	}
	if !strings.Contains(f.log(), "secrets uploaded and verified") {
		t.Fatalf("log: %s", f.log())
	}
	if strings.Contains(f.log(), "s3cr3t") {
		t.Fatalf("the secrets reached the log:\n%s", f.log())
	}
}

func TestADefinitionWithoutSecretsNeverTouchesTheSource(t *testing.T) {
	f := newFx(t)
	src := &fakeSecrets{data: []byte("X=1\n")}
	tg := withSource(f, src)
	if code, err := f.runWith(context.Background(), tg, definition(t, simple)); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if src.calls != 0 {
		t.Fatal("the source was asked although the definition did not want secrets")
	}
	for _, r := range f.fake.Runs {
		if _, ok := r.Env["DYMPLOY_SECRETS_FILE"]; ok {
			t.Fatal("no secrets file was given")
		}
	}
}

func TestWhenTheSourceFailsNothingHappensOnTheTarget(t *testing.T) {
	f := newFx(t)
	src := &fakeSecrets{err: errors.New("the secrets source failed: Project not found")}
	tg := withSource(f, src)
	code, err := f.runWith(context.Background(), tg, definition(t, withSecrets))
	if err == nil || !strings.Contains(err.Error(), "Project not found") || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(f.fake.Dialed) != 0 || len(f.fake.Runs) != 0 || len(f.fake.Files) != 0 {
		t.Fatalf("the target was reached: dialed=%d runs=%d files=%d", len(f.fake.Dialed), len(f.fake.Runs), len(f.fake.Files))
	}
}

func TestADamagedSecretsUploadRunsNothing(t *testing.T) {
	f := newFx(t)
	f.fake.Corrupt = true
	tg := withSource(f, &fakeSecrets{data: []byte("X=1\n")})
	_, err := f.runWith(context.Background(), tg, definition(t, withSecrets))
	if err == nil {
		t.Fatal("a damaged upload must stop the deployment")
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "" {
		t.Fatalf("steps ran: %s", got)
	}
}

func TestADefinitionThatWantsSecretsNeedsASourceOnTheServer(t *testing.T) {
	f := newFx(t)
	f.d.Secrets = &fakeSecrets{}
	tg := target() // no secrets source
	f.d.Targets.Set(map[string]deploy.Target{"production": tg})
	_, err := f.d.AuthorizeDeploy(definition(t, withSecrets), ev)
	if err == nil || !strings.Contains(err.Error(), "no secrets source") {
		t.Fatalf("err=%v", err)
	}
	tg.Secrets = &deploy.SecretsSpec{Command: []string{"/x"}}
	f.d.Targets.Set(map[string]deploy.Target{"production": tg})
	if _, err := f.d.AuthorizeDeploy(definition(t, withSecrets), ev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.AuthorizeDeploy(definition(t, simple), ev); err != nil {
		t.Fatalf("a definition that does not ask is not affected: %v", err)
	}
}
