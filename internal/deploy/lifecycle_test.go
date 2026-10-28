package deploy_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

const lifecycle = `deploy:
  target: production
  steps: [{run: install}, {run: restart}]
  check: {run: probe, attempts: 1}
  after: {steps: [{run: unlock}]}
  steps_undo: [{run: restore}, {run: probe-old}, {run: unlock-old}]
  finally: {steps: [{run: notify}, {run: cleanup}]}
`

func TestDeploymentLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, fail, want, undo string
		code                   int
	}{
		{"success", "", "install|restart|probe|unlock|notify|cleanup", "not_run", 0},
		{"step failure", "install", "install|restore|probe-old|unlock-old|notify|cleanup", "succeeded", deploy.ExitStep},
		{"check failure", "probe", "install|restart|probe|restore|probe-old|unlock-old|notify|cleanup", "succeeded", deploy.ExitCheck},
		{"after failure", "unlock", "install|restart|probe|unlock|notify|cleanup", "not_run", deploy.ExitAfter},
		{"undo failure", "install,restore", "install|restore|notify|cleanup", "failed", deploy.ExitStep},
		{"restored unhealthy", "probe,probe-old", "install|restart|probe|restore|probe-old|notify|cleanup", "failed", deploy.ExitCheck},
		{"finally failure", "notify", "install|restart|probe|unlock|notify|cleanup", "not_run", deploy.ExitFinally},
		{"preserve failure", "install,notify", "install|restore|probe-old|unlock-old|notify|cleanup", "succeeded", deploy.ExitStep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			f.fake.Handle = func(s string) (int, string, bool) {
				for _, bad := range strings.Split(tc.fail, ",") {
					if s == bad {
						return 1, "", false
					}
				}
				return 0, "", false
			}
			code, err := f.run(context.Background(), definition(t, lifecycle))
			if err != nil || code != tc.code {
				t.Fatalf("code=%d err=%v logs=%s", code, err, f.log())
			}
			if got := strings.Join(f.fake.Scripts(), "|"); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
			for _, r := range f.fake.Runs {
				if r.Script == "notify" {
					expectedStatus := "succeeded"
					if tc.code != 0 && tc.code != deploy.ExitFinally {
						expectedStatus = "failed"
					}
					if r.Env["DYMPLOY_DEPLOY_STATUS"] != expectedStatus {
						t.Fatalf("wrong status: %v", r.Env)
					}
				}
				if r.Script == "notify" && r.Env["DYMPLOY_UNDO_STATUS"] != tc.undo {
					t.Fatalf("env=%v", r.Env)
				}
			}
		})
	}
}

func TestLifecycleTimeoutAndCancellation(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[abort], func(t *testing.T) {
			f := newFx(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.fake.Handle = func(s string) (int, string, bool) {
				if s == "install" {
					if abort {
						cancel()
					}
					return 0, "", true
				}
				return 0, "", false
			}
			def := definition(t, lifecycle+"  timeout: 1s\n")
			code, err := f.run(ctx, def)
			if abort {
				if err == nil {
					t.Fatal("expected cancellation")
				}
			} else if err != nil || code != deploy.ExitTimeout {
				t.Fatalf("%d %v", code, err)
			}
			want := "install|restore|probe-old|unlock-old|notify|cleanup"
			if abort {
				want = "install|notify|cleanup"
			}
			if got := strings.Join(f.fake.Scripts(), "|"); got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		})
	}
}

func TestLifecycleBeforeSteps(t *testing.T) {
	for _, connection := range []bool{false, true} {
		f := newFx(t)
		if connection {
			f.fake.DialErr = fmt.Errorf("offline")
		} else {
			f.fake.UploadErr = fmt.Errorf("disk full")
		}
		_, err := f.run(context.Background(), definition(t, lifecycle))
		if err == nil {
			t.Fatal("expected error")
		}
		want := "notify|cleanup"
		if connection {
			want = ""
		}
		if got := strings.Join(f.fake.Scripts(), "|"); got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
}

func TestLifecycleBucketRejected(t *testing.T) {
	for _, extra := range []string{"  finally: {steps: [{run: notify}]}\n", "  steps: [{run: install}]\n  steps_undo: [{run: restore}]\n"} {
		p := newPub()
		p.d.Targets.Set(map[string]deploy.Target{"garage-books": bucketTarget()})
		_, err := p.d.AuthorizeDeploy(definition(t, "deploy:\n  target: garage-books\n"+extra), ev)
		if err == nil {
			t.Fatal("bucket must reject SSH lifecycle")
		}
	}
}

func TestCancellationDuringRecoveryDoesNotUnlock(t *testing.T) {
	f := newFx(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.fake.Handle = func(s string) (int, string, bool) {
		if s == "install" {
			return 1, "", false
		}
		if s == "restore" {
			cancel()
			return 0, "", true
		}
		return 0, "", false
	}
	code, err := f.run(ctx, definition(t, lifecycle))
	if err != nil || code != deploy.ExitStep {
		t.Fatalf("%d %v", code, err)
	}
	if got := strings.Join(f.fake.Scripts(), "|"); got != "install|restore|notify|cleanup" {
		t.Fatal(got)
	}
}
