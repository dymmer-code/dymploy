package runnertest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/runner"
)

func TestScriptsReportWhatTheyAreTold(t *testing.T) {
	if r, err := New(Exit(3)).Run(context.Background(), runner.Task{Name: "a"}); err != nil || r.ExitCode != 3 {
		t.Fatalf("%v %v", r, err)
	}
	boom := errors.New("docker is down")
	if _, err := New(Fail(boom)).Run(context.Background(), runner.Task{Name: "a"}); !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
}

func TestWritesGoToTheTaskLog(t *testing.T) {
	var lines []string
	rep := runner.NewLineReporter(func(s runner.Stream, l string) { lines = append(lines, string(s)+":"+l) })
	r, err := New(Writes("hello\n", "oops\n", 1)).Run(context.Background(), runner.Task{Name: "a", Log: rep})
	if err != nil || r.ExitCode != 1 || strings.Join(lines, "|") != "stdout:hello|stderr:oops" {
		t.Fatalf("%v %v %v", r, err, lines)
	}
}

func TestHoldWaitsForTheGateAndHonoursCancellation(t *testing.T) {
	g := NewGate()
	f := New(Hold(g, 0))
	done := make(chan error, 1)
	go func() { _, err := f.Run(context.Background(), runner.Task{Name: "a"}); done <- err }()
	<-f.Started()
	select {
	case <-done:
		t.Fatal("must wait for the gate")
	case <-time.After(50 * time.Millisecond):
	}
	g.Open()
	g.Open() // idempotent
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	g2 := NewGate()
	f2 := New(Hold(g2, 0))
	go func() { _, err := f2.Run(ctx, runner.Task{Name: "b"}); done <- err }()
	<-f2.Started()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled run must report the context's error, got %v", err)
	}
}

func TestConcurrencyIsMeasured(t *testing.T) {
	g := NewGate()
	f := New(Hold(g, 0))
	done := make(chan struct{}, 3)
	for _, name := range []string{"a", "a", "b"} {
		go func(n string) { f.Run(context.Background(), runner.Task{Name: n}); done <- struct{}{} }(name)
	}
	for i := 0; i < 3; i++ {
		<-f.Started()
	}
	g.Open()
	for i := 0; i < 3; i++ {
		<-done
	}
	if f.MaxConcurrent() != 3 || f.MaxConcurrentFor("a") != 2 || f.MaxConcurrentFor("b") != 1 || len(f.Calls()) != 3 {
		t.Fatalf("max=%d a=%d b=%d calls=%d", f.MaxConcurrent(), f.MaxConcurrentFor("a"), f.MaxConcurrentFor("b"), len(f.Calls()))
	}
}
