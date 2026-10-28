package job

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEventOutboxTransactionCursorsAndRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EventSinks([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "one", State: Queued}
	if err := s.Put(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	first, ok, err := s.NextEvent("a")
	if err != nil || !ok || first.Job.State != Queued {
		t.Fatalf("%+v %v %v", first, ok, err)
	}
	j.Details = []byte(`{"progress":1}`)
	s.Put(context.Background(), j)
	j.State = Running
	s.Put(context.Background(), j)
	if err := s.AckEvent("a", first.Sequence); err != nil {
		t.Fatal(err)
	}
	s.AckEvent("a", 0)
	second, ok, err := s.NextEvent("a")
	if err != nil || !ok || second.Sequence != first.Sequence+1 || second.Job.State != Running {
		t.Fatal(second, err)
	}
	s.AckEvent("a", second.Sequence)
	still, ok, _ := s.NextEvent("b")
	if !ok || still.Sequence != first.Sequence {
		t.Fatal("one sink lost another sink's backlog")
	}
	s.Close()
	s, err = OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, _ := s.NextEvent("a"); ok {
		t.Fatal("ack lost on restart")
	}
	if err := s.EventSinks([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.NextEvent("missing"); err == nil {
		t.Fatal("unknown sink")
	}
	if err := s.AckEvent("missing", 0); err == nil {
		t.Fatal("unknown ack")
	}
	if err := s.EventSinks([]string{""}); err == nil {
		t.Fatal("empty sink")
	}
	j.State = Interrupted
	s.Put(context.Background(), j)
	if ev, ok, _ := s.NextEvent("a"); !ok || ev.Job.State != Interrupted {
		t.Fatal("missing crash recovery")
	}
	if err := s.EventSinks(nil); err != nil {
		t.Fatal(err)
	}
}
