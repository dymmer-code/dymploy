package deploy_test

import (
	"context"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/runner"
	"strings"
	"testing"
)

func TestMultiStopsAndKeepsIndividualResults(t *testing.T) {
	f := newFx(t)
	def := definition(t, "deploy:\n  targets: [first, second, third]\n  steps: [{run: install}]\n")
	f.fake.Handle = func(s string) (int, string, bool) {
		if s == "install" {
			return 1, "", false
		}
		return 0, "", false
	}
	rep := runner.NewLineReporter(func(runner.Stream, string) {})
	defer rep.Close()
	targets := []deploy.Target{target(), target(), target()}
	for i, n := range def.TargetNames() {
		targets[i].Name = n
	}
	out, err := f.d.RunMany(context.Background(), deploy.Request{Deploy: def, Event: ev, JobID: "job", Record: f.rec, File: f.file, Log: rep}, targets, nil)
	if err != nil || out.ExitCode != deploy.ExitStep || out.Targets[0].State != "failed" || out.Targets[1].State != "skipped" || len(f.fake.Dialed) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestMultiSuccessUsesEachTargetAndPrefixesLogs(t *testing.T) {
	f := newFx(t)
	def := definition(t, "deploy:\n  targets: [first, second]\n  max_parallel: 2\n  steps: [{run: install}]\n")
	var logs []string
	rep := runner.NewLineReporter(func(_ runner.Stream, s string) { logs = append(logs, s) })
	defer rep.Close()
	ts := []deploy.Target{target(), target()}
	ts[0].Name = "first"
	ts[1].Name = "second"
	out, err := f.d.RunMany(context.Background(), deploy.Request{Deploy: def, Event: ev, JobID: "job", Record: f.rec, File: f.file, Log: rep}, ts, nil)
	if err != nil || out.ExitCode != 0 || len(f.fake.Dialed) != 2 {
		t.Fatalf("%+v %v", out, err)
	}
	for _, n := range []string{"first", "second"} {
		if !strings.Contains(strings.Join(logs, "\n"), "["+n+"]") {
			t.Fatal(logs)
		}
	}
}

func TestMultiDefinitionRejectsAmbiguityAndDuplicates(t *testing.T) {
	for _, src := range []string{"target: first\n  targets: [second]", "targets: [first, first]", "targets: []", "targets: [first]\n  max_parallel: 0"} {
		_, err := manifest.Parse([]byte("version: 1\nbuild:\n  image: {from: alpine}\n  steps: [{run: test}]\nartifact: {paths: [out]}\ndeploy:\n  " + src + "\n  steps: [{run: install}]\n"))
		if err == nil {
			t.Fatal(src)
		}
	}
}

func TestAuthorizeEveryDestinationBeforeDeploying(t *testing.T) {
	f := newFx(t)
	one, two := target(), target()
	one.Name = "first"
	two.Name = "second"
	f.d.Targets.Set(map[string]deploy.Target{"first": one, "second": two})
	def := definition(t, "deploy:\n  targets: [first, second]\n  steps: [{run: install}]\n")
	if ts, err := f.d.AuthorizeMany(def, ev); err != nil || len(ts) != 2 {
		t.Fatalf("%v %v", ts, err)
	}
	f.d.Targets.Set(map[string]deploy.Target{"first": one})
	if _, err := f.d.AuthorizeMany(def, ev); err == nil {
		t.Fatal("accepted a missing target")
	}
}

func TestMultiProgressFailureAndCancellationStopDispatch(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		f := newFx(t)
		def := definition(t, "deploy:\n  targets: [first, second]\n  steps: [{run: install}]\n")
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		defer cancel()
		rep := runner.NewLineReporter(func(runner.Stream, string) {})
		defer rep.Close()
		_, err := f.d.RunMany(ctx, deploy.Request{Deploy: def, Log: rep}, []deploy.Target{target(), target()}, func(deploy.Outcome) error {
			if !cancelled {
				return context.Canceled
			}
			return nil
		})
		if err == nil || len(f.fake.Dialed) > 0 {
			t.Fatal("started work without durable progress or after cancel")
		}
	}
}
