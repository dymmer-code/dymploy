package runner

import (
	"context"
	"errors"
	"testing"
)

type nop struct{}

func (nop) Run(context.Context, Task) (Result, error) { return Result{}, nil }

func TestRegistryReturnsWhatWasRegistered(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("docker", nop{}); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get("docker")
	if err != nil || got == nil {
		t.Fatalf("%v %v", got, err)
	}
}

func TestUnknownKindIsADistinctError(t *testing.T) {
	_, err := NewRegistry().Get("podman")
	if !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("got %v", err)
	}
	if err == nil || err.Error() == ErrUnknownKind.Error() {
		t.Fatal("the error must name the kind")
	}
}

func TestRegisterRejectsDuplicatesAndEmpties(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("a", nop{})
	if err := r.Register("a", nop{}); err == nil {
		t.Fatal("registering a kind twice hides a configuration mistake")
	}
	if err := r.Register("", nop{}); err == nil {
		t.Fatal("empty kind")
	}
	if err := r.Register("b", nil); err == nil {
		t.Fatal("nil runner")
	}
}

func TestKindsAreListedSorted(t *testing.T) {
	r := NewRegistry()
	_ = r.Register("zeta", nop{})
	_ = r.Register("alpha", nop{})
	got := r.Kinds()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("%v", got)
	}
}

func TestMustRegisterPanicsOnProgrammingErrors(t *testing.T) {
	r := NewRegistry()
	r.MustRegister("a", nop{})
	for name, f := range map[string]func(){
		"duplicate": func() { r.MustRegister("a", nop{}) },
		"empty":     func() { r.MustRegister("", nop{}) },
		"nil":       func() { r.MustRegister("b", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic", name)
				}
			}()
			f()
		}()
	}
}
