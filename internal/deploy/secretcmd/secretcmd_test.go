package secretcmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

// sh runs a script as the secrets source: the real thing is a program with arguments, run without a shell.
func sh(script string) deploy.SecretsSpec {
	return deploy.SecretsSpec{Command: []string{"/bin/sh", "-c", script}, Timeout: 5 * time.Second}
}

func TestTheOutputIsTheSecrets(t *testing.T) {
	got, err := New().Fetch(context.Background(), sh(`printf 'A=1\nB=two'`))
	if err != nil || string(got) != "A=1\nB=two\n" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTheCommandIsNotRunThroughAShell(t *testing.T) {
	spec := deploy.SecretsSpec{Command: []string{"/bin/echo", "$(id) `id` ; x"}}
	got, err := New().Fetch(context.Background(), spec)
	if err != nil || strings.TrimSpace(string(got)) != "$(id) `id` ; x" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTheEnvironmentIsMinimalPlusTheEnvFile(t *testing.T) {
	t.Setenv("DAEMON_ONLY", "must-not-pass")
	env := filepath.Join(t.TempDir(), "source.env")
	if err := os.WriteFile(env, []byte("# credential\nTOKEN=abc\n\nOTHER=x=y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := sh(`echo "token=$TOKEN other=$OTHER daemon=${DAEMON_ONLY-unset}"`)
	spec.EnvFile = env
	got, err := New().Fetch(context.Background(), spec)
	if err != nil || strings.TrimSpace(string(got)) != "token=abc other=x=y daemon=unset" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestAnEnvFileOthersCanReadIsRefused(t *testing.T) {
	env := filepath.Join(t.TempDir(), "source.env")
	if err := os.WriteFile(env, []byte("TOKEN=abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := sh("echo x")
	spec.EnvFile = env
	if _, err := New().Fetch(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "must not be readable by others") {
		t.Fatalf("err=%v", err)
	}
	if err := Check(spec); err == nil {
		t.Fatal("Check must refuse it too")
	}
}

func TestAFailureSaysWhyButNeverShowsTheOutput(t *testing.T) {
	_, err := New().Fetch(context.Background(), sh(`echo SECRET=leaked; echo 'Project not found' >&2; exit 3`))
	if err == nil || !strings.Contains(err.Error(), "Project not found") || strings.Contains(err.Error(), "leaked") {
		t.Fatalf("err=%v", err)
	}
}

func TestNothingOrNotTextOrTooMuchIsRefused(t *testing.T) {
	for name, script := range map[string]string{
		"nothing":  `printf '  \n'`,
		"binary":   `printf 'A=\000b'`,
		"too much": `head -c 300000 /dev/zero | tr '\0' 'a'`,
	} {
		if _, err := New().Fetch(context.Background(), sh(script)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestACommandThatDoesNotAnswerIsStopped(t *testing.T) {
	spec := sh("sleep 30")
	spec.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := New().Fetch(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "did not answer") || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
}

func TestCheckNeedsAnExecutableFile(t *testing.T) {
	if err := Check(deploy.SecretsSpec{Command: []string{"/nonexistent/dym"}}); err == nil {
		t.Fatal("a missing program must be refused")
	}
	if err := Check(deploy.SecretsSpec{Command: []string{"/bin/sh"}}); err != nil {
		t.Fatal(err)
	}
}
