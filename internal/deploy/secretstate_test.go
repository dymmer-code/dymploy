package deploy_test

import (
	"context"
	"encoding/json"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretStateDetectsChangesWithoutPersistingSecrets(t *testing.T) {
	dir := t.TempDir()
	targets := deploy.NewTargets()
	src := &fakeSecrets{data: []byte(secretValue)}
	tg := target()
	tg.Secrets = &deploy.SecretsSpec{Command: []string{"source"}}
	targets.Set(map[string]deploy.Target{tg.Name: tg})
	state, err := deploy.NewSecretState(dir, targets, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Record(tg.Name, src.data); err != nil {
		t.Fatal(err)
	}
	if got := state.Check(context.Background(), tg.Name); got.State != "current" {
		t.Fatal(got)
	}
	src.data = []byte("TOKEN=changed")
	if got := state.Check(context.Background(), tg.Name); got.State != "changed" {
		t.Fatal(got)
	}
	state, err = deploy.NewSecretState(dir, targets, src)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Check(context.Background(), tg.Name); got.State != "changed" {
		t.Fatal(got)
	}
	if err := state.Record(tg.Name, src.data); err != nil {
		t.Fatal(err)
	}
	if got := state.Check(context.Background(), tg.Name); got.State != "current" {
		t.Fatal(got)
	}
	src.err = context.DeadlineExceeded
	if got := state.Check(context.Background(), tg.Name); got.State != "unavailable" {
		t.Fatal(got)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "fingerprints.json"))
	if strings.Contains(string(raw), "TOKEN") || strings.Contains(string(raw), secretValue) {
		t.Fatal("plaintext persisted")
	}
	public, _ := json.Marshal(state.Snapshot())
	if strings.Contains(string(public), "fingerprint") {
		t.Fatal("fingerprint exposed")
	}
	time.Sleep(10 * time.Millisecond)
}

func TestSecretSnapshotRefreshAndForget(t *testing.T) {
	targets := deploy.NewTargets()
	tg := target()
	tg.Secrets = &deploy.SecretsSpec{Command: []string{"source"}}
	targets.Set(map[string]deploy.Target{tg.Name: tg})
	src := &fakeSecrets{data: []byte("a")}
	state, err := deploy.NewSecretState(t.TempDir(), targets, src)
	if err != nil {
		t.Fatal(err)
	}
	state.Record(tg.Name, []byte("a"))
	first := state.Snapshot()
	if len(first) != 1 || first[0].State != "checking" {
		t.Fatal(first)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if state.Snapshot()[0].State == "current" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if state.Snapshot()[0].State != "current" {
		t.Fatal("refresh never completed")
	}
	if err := state.Forget(tg.Name); err != nil {
		t.Fatal(err)
	}
	state.Forget(tg.Name)
	if len(state.Snapshot()) != 0 {
		t.Fatal("warning retained for a destination that no longer requests secrets")
	}
}
func TestSecretStateRefusesCorruptOrPublicState(t *testing.T) {
	for _, bad := range []string{"permissions", "key", "json"} {
		t.Run(bad, func(t *testing.T) {
			dir := t.TempDir()
			ts := deploy.NewTargets()
			_, err := deploy.NewSecretState(dir, ts, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch bad {
			case "permissions":
				os.Chmod(filepath.Join(dir, "key"), 0644)
			case "key":
				os.WriteFile(filepath.Join(dir, "key"), []byte("bad"), 0600)
			case "json":
				os.WriteFile(filepath.Join(dir, "fingerprints.json"), []byte("bad"), 0600)
			}
			if _, err := deploy.NewSecretState(dir, ts, nil); err == nil {
				t.Fatal("accepted bad secret state")
			}
		})
	}
}

func TestOnlySuccessfulDeploymentsRecordSecrets(t *testing.T) {
	f := newFx(t)
	src := &fakeSecrets{data: []byte("a")}
	tg := withSource(f, src)
	f.d.Targets.Set(map[string]deploy.Target{tg.Name: tg})
	state, err := deploy.NewSecretState(t.TempDir(), f.d.Targets, src)
	if err != nil {
		t.Fatal(err)
	}
	f.d.SecretState = state
	code, err := f.runWith(context.Background(), tg, definition(t, withSecrets))
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if state.Check(context.Background(), tg.Name).State != "current" {
		t.Fatal("successful delivery was not recorded")
	}
	src.data = []byte("b")
	f.fake.Handle = func(string) (int, string, bool) { return 1, "", false }
	code, err = f.runWith(context.Background(), tg, definition(t, withSecrets))
	if err != nil || code == 0 {
		t.Fatal(code, err)
	}
	if state.Check(context.Background(), tg.Name).State != "changed" {
		t.Fatal("failed delivery overwrote baseline")
	}
	f.fake.Handle = nil
	code, err = f.runWith(context.Background(), tg, definition(t, simple))
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	if len(state.Snapshot()) != 0 {
		t.Fatal("kept secrets tracking for a no-secrets definition")
	}
}
