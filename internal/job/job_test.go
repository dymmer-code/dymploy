package job_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/job/jobtest"
	"github.com/dymmer-code/dymploy/internal/runner"
)

func TestMemStoreHonoursTheContract(t *testing.T) {
	jobtest.RunStore(t, func(*testing.T) job.Store { return job.NewMemStore() }, nil)
}

func TestBoltStoreHonoursTheContract(t *testing.T) {
	open := func(t *testing.T, dir string) *job.BoltStore {
		s, err := job.OpenBolt(filepath.Join(dir, "state", "jobs.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	var dir string
	jobtest.RunStore(t,
		func(t *testing.T) job.Store {
			dir = t.TempDir()
			s := open(t, dir)
			t.Cleanup(func() { s.Close() })
			return s
		},
		func(t *testing.T, s job.Store) job.Store {
			s.(*job.BoltStore).Close()
			s2 := open(t, dir)
			t.Cleanup(func() { s2.Close() })
			return s2
		})
}

func TestBoltFileIsPrivateAndSingleOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "jobs.db")
	s, err := job.OpenBolt(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("the job database must be 0600, is %o", fi.Mode().Perm())
	}
	start := time.Now()
	if _, err := job.OpenBolt(p); err == nil || !strings.Contains(err.Error(), "another dymploy") {
		t.Fatalf("a second owner must be refused with a useful message, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the refusal must not hang")
	}
}

func TestMemLogsHonourTheContract(t *testing.T) {
	jobtest.RunLogs(t, func(*testing.T) job.Logs { return job.NewMemLogs() })
}

func TestFileLogsHonourTheContract(t *testing.T) {
	jobtest.RunLogs(t, func(t *testing.T) job.Logs {
		l, err := job.NewFileLogs(t.TempDir(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return l
	})
}

func TestFileLogsRejectUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	l, _ := job.NewFileLogs(filepath.Join(dir, "logs"), 0)
	for _, id := range []job.ID{"../escape", "a/b", "", ".hidden", job.ID(strings.Repeat("a", 200)), "x\x00y"} {
		if err := l.Append(id, job.Entry{Seq: 1}); err == nil {
			t.Errorf("Append(%q) must be refused", id)
		}
		if _, err := l.Read(id, 0, 0); err == nil {
			t.Errorf("Read(%q) must be refused", id)
		}
		if err := l.Delete(id); err == nil {
			t.Errorf("Delete(%q) must be refused", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.jsonl")); err == nil {
		t.Fatal("nothing may be written outside the log directory")
	}
}

func TestFileLogsAreTruncatedOnceAtTheLimit(t *testing.T) {
	l, _ := job.NewFileLogs(t.TempDir(), 400)
	for i := int64(1); i <= 50; i++ {
		if err := l.Append("j1", job.Entry{Seq: i, Time: time.Unix(i, 0), Stream: runner.Stdout, Text: strings.Repeat("x", 30)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := l.Read("j1", 0, 0)
	markers := 0
	for _, e := range got {
		if e.Stream == runner.System && strings.Contains(e.Text, "truncated") {
			markers++
		}
	}
	if markers != 1 || len(got) >= 50 || len(got) < 3 {
		t.Fatalf("expected a bounded log with exactly one truncation marker, got %d entries and %d markers", len(got), markers)
	}
	last := got[len(got)-1]
	if last.Stream != runner.System {
		t.Fatalf("the marker must be the last thing in the log: %+v", last)
	}
}

func TestFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	l, _ := job.NewFileLogs(filepath.Join(dir, "logs"), 0)
	_ = l.Append("j1", job.Entry{Seq: 1})
	fi, err := os.Stat(filepath.Join(dir, "logs", "j1.jsonl"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	di, _ := os.Stat(filepath.Join(dir, "logs"))
	if di.Mode().Perm() != 0o750 {
		t.Fatalf("log directory is %o", di.Mode().Perm())
	}
}

func TestStateFinality(t *testing.T) {
	for s, want := range map[job.State]bool{job.Queued: false, job.Running: false, job.Succeeded: true, job.Failed: true, job.Canceled: true, job.Interrupted: true} {
		if s.Final() != want {
			t.Errorf("%s: Final() = %v", s, !want)
		}
	}
}

func TestIDsSortByCreationTime(t *testing.T) {
	a := job.NewID(time.Unix(1000, 0))
	b := job.NewID(time.Unix(2000, 0))
	if !(a < b) {
		t.Fatalf("%s should sort before %s", a, b)
	}
	if job.NewID(time.Unix(1000, 0)) == a {
		t.Fatal("ids created in the same millisecond must still differ")
	}
}

func TestMemStoreDoesNotAliasCallerData(t *testing.T) {
	s := job.NewMemStore()
	code := 1
	j := job.Job{ID: "x", ExitCode: &code, Params: []byte(`{"a":1}`)}
	_ = s.Put(context.Background(), j)
	code = 9
	j.Params[2] = 'Z'
	got, _ := s.Get(context.Background(), "x")
	if *got.ExitCode != 1 || string(got.Params) != `{"a":1}` {
		t.Fatalf("stored data was modified through the caller's references: %+v", got)
	}
}
