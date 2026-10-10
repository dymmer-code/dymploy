package dryrun

import (
	"context"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/runner"
)

func TestDryRunNarratesAndSucceeds(t *testing.T) {
	var got []string
	rep := runner.NewLineReporter(func(s runner.Stream, l string) { got = append(got, string(s)+":"+l) })
	res, err := New().Run(context.Background(), runner.Task{
		Name: "github/a/b", Kind: "dryrun", Params: []byte(`{"x":1}`), Input: []byte(`{"commit":"abc123"}`), Log: rep,
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%v %v", res, err)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"dry run", "github/a/b", `{"commit":"abc123"}`, "nothing was executed"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in:\n%s", want, all)
		}
	}
}

func TestDryRunHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := runner.NewLineReporter(func(runner.Stream, string) {})
	if _, err := New().Run(ctx, runner.Task{Log: rep}); err == nil {
		t.Fatal("a cancelled context must be reported")
	}
}
