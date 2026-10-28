package deploy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/runner"
)

const secretsOnly = `deploy:
  target: production
  secrets: true
  steps:
    - {name: install, run: "install it"}
  secrets_steps:
    - {name: env, run: "put env"}
    - {name: restart, run: "restart it"}
  secrets_undo:
    - {name: back, run: "restore env"}
  check: {run: "check it", attempts: 1}
`

const secretsOnlyNoUndo = `deploy:
  target: production
  secrets: true
  steps:
    - {run: "install it"}
  secrets_steps:
    - {name: env, run: "put env"}
`

func (f *fx) runSecrets(ctx context.Context, tg deploy.Target, text string) (int, error) {
	rep := runner.NewLineReporter(func(s runner.Stream, line string) {
		f.mu.Lock()
		f.logs = append(f.logs, string(s)+": "+line)
		f.mu.Unlock()
	})
	defer rep.Close()
	return f.d.RunSecrets(ctx, deploy.SecretsRequest{Target: tg, Deploy: definition(f.t, text), Event: ev, JobID: "job/1", Log: rep})
}

func TestChangingOnlyTheSecretsRunsItsStepsAndTheCheckAndNothingElse(t *testing.T) {
	f := newFx(t)
	src := &fakeSecrets{data: []byte(secretValue + "\n")}
	tg := withSource(f, src)
	var during string
	f.fake.Handle = func(script string) (int, string, bool) {
		if script == "put env" {
			during = string(f.fake.UploadedAt("up/job-1-secrets"))
		}
		return 0, "", false
	}
	code, err := f.runSecrets(context.Background(), tg, secretsOnly)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "put env|restart it|check it" {
		t.Fatalf("only the secrets steps and the check must run, ran: %s", got)
	}
	var env map[string]string
	for _, r := range f.fake.Runs {
		if r.Script == "put env" {
			env = r.Env
		}
	}
	if env["DYMPLOY_SECRETS_FILE"] != "up/job-1-secrets" || env["DYMPLOY_TARGET"] != "production" || env["DYMPLOY_COMMIT"] != ev.Commit {
		t.Fatalf("env: %v", env)
	}
	if _, ok := env["DYMPLOY_ARTIFACT"]; ok {
		t.Fatal("there is no artifact in this job")
	}
	if during != secretValue+"\n" {
		t.Fatalf("on the target while the steps ran: %q", during)
	}
	if f.fake.UploadedAt("up/job-1-secrets") != nil {
		t.Fatal("the secrets file must be gone when the job ends")
	}
	if strings.Join(f.fake.Removed, ",") != "up/job-1-secrets" {
		t.Fatalf("only the secrets upload is removed: %v", f.fake.Removed)
	}
	if !strings.Contains(f.log(), "secrets changed on production") || strings.Contains(f.log(), "s3cr3t") {
		t.Fatalf("log:\n%s", f.log())
	}
}

func TestAFailingSecretsStepRunsTheUndoAndStops(t *testing.T) {
	f := newFx(t)
	tg := withSource(f, &fakeSecrets{data: []byte("X=1\n")})
	f.fake.Handle = func(script string) (int, string, bool) {
		if script == "put env" {
			return 3, "", false
		}
		return 0, "", false
	}
	code, err := f.runSecrets(context.Background(), tg, secretsOnly)
	if err != nil || code != deploy.ExitStep {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "put env|restore env" {
		t.Fatalf("a failed step stops the rest and runs the undo: %s", got)
	}
	if !strings.Contains(f.log(), "the secrets steps were undone") {
		t.Fatalf("log:\n%s", f.log())
	}
}

func TestAFailingCheckRunsTheUndo(t *testing.T) {
	f := newFx(t)
	tg := withSource(f, &fakeSecrets{data: []byte("X=1\n")})
	f.fake.Handle = func(script string) (int, string, bool) {
		if script == "check it" {
			return 1, "", false
		}
		return 0, "", false
	}
	code, err := f.runSecrets(context.Background(), tg, secretsOnly)
	if err != nil || code != deploy.ExitCheck {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "put env|restart it|check it|restore env" {
		t.Fatalf("ran: %s", got)
	}
}

func TestWithoutUndoTheLogSaysNothingWasUndone(t *testing.T) {
	f := newFx(t)
	tg := withSource(f, &fakeSecrets{data: []byte("X=1\n")})
	f.fake.Handle = func(script string) (int, string, bool) { return 1, "", false }
	code, err := f.runSecrets(context.Background(), tg, secretsOnlyNoUndo)
	if err != nil || code != deploy.ExitStep || !strings.Contains(f.log(), "nothing was undone") {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
}

func TestAnUndoThatFailsIsReportedAndStops(t *testing.T) {
	f := newFx(t)
	tg := withSource(f, &fakeSecrets{data: []byte("X=1\n")})
	f.fake.Handle = func(script string) (int, string, bool) { return 1, "", false }
	code, err := f.runSecrets(context.Background(), tg, secretsOnly)
	if err != nil || code != deploy.ExitStep || !strings.Contains(f.log(), "did not work") {
		t.Fatalf("code=%d err=%v\n%s", code, err, f.log())
	}
}

func TestWhenTheSourceFailsChangingSecretsTouchesNothing(t *testing.T) {
	f := newFx(t)
	tg := withSource(f, &fakeSecrets{err: errors.New("the secrets source failed: Project not found")})
	code, err := f.runSecrets(context.Background(), tg, secretsOnly)
	if err == nil || !strings.Contains(err.Error(), "Project not found") || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if len(f.fake.Dialed) != 0 || len(f.fake.Runs) != 0 {
		t.Fatalf("the target was reached: dialed=%d runs=%d", len(f.fake.Dialed), len(f.fake.Runs))
	}
}

func TestADamagedSecretsUploadStopsChangingSecrets(t *testing.T) {
	f := newFx(t)
	f.fake.Corrupt = true
	tg := withSource(f, &fakeSecrets{data: []byte("X=1\n")})
	if _, err := f.runSecrets(context.Background(), tg, secretsOnly); err == nil {
		t.Fatal("a damaged upload must stop it")
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "" {
		t.Fatalf("steps ran: %s", got)
	}
}

func TestWhoMayChangeSecrets(t *testing.T) {
	f := newFx(t)
	src := &fakeSecrets{}
	tg := withSource(f, src)
	f.d.Targets.Set(map[string]deploy.Target{"production": tg})
	if _, err := f.d.AuthorizeSecrets(definition(t, secretsOnly), ev); err != nil {
		t.Fatalf("a definition with secrets_steps, for an allowed repository: %v", err)
	}
	if _, err := f.d.AuthorizeSecrets(definition(t, withSecrets), ev); !errors.Is(err, deploy.ErrNoSecretsSteps) {
		t.Fatalf("without secrets_steps the definition does not say what to do: %v", err)
	}
	other := ev
	other.Repo = "other"
	if _, err := f.d.AuthorizeSecrets(definition(t, secretsOnly), other); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("the policy of the target applies as to a deploy: %v", err)
	}
	noSource := tg
	noSource.Secrets = nil
	f.d.Targets.Set(map[string]deploy.Target{"production": noSource})
	if _, err := f.d.AuthorizeSecrets(definition(t, secretsOnly), ev); err == nil || !strings.Contains(err.Error(), "no secrets source") {
		t.Fatalf("%v", err)
	}
	bucket := tg
	bucket.Kind = deploy.KindS3
	f.d.Targets.Set(map[string]deploy.Target{"production": bucket})
	if _, err := f.d.AuthorizeSecrets(definition(t, secretsOnly), ev); err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("a bucket has no secrets: %v", err)
	}
}
