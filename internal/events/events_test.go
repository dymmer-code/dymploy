package events_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/events"
	"github.com/dymmer-code/dymploy/internal/job"
)

func TestDurableSignedDeliveryRetriesInOrder(t *testing.T) {
	file := filepath.Join(t.TempDir(), "jobs.db")
	store, err := job.OpenBolt(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EventSinks([]string{"test"}); err != nil {
		t.Fatal(err)
	}
	j := job.Job{ID: "j1", Name: "github/acme/repo", State: job.Queued, Input: []byte(`{"provider":"github","owner":"acme","repo":"repo","commit":"abc","secret":"NEVER"}`), Error: "NEVER"}
	store.Put(context.Background(), j)
	j.State = job.Running
	store.Put(context.Background(), j)
	j.State = job.Failed
	store.Put(context.Background(), j)
	store.Close()
	store, err = job.OpenBolt(file)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var mu sync.Mutex
	var got []string
	attempts := 0
	key := []byte("12345678901234567890123456789012")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "NEVER") {
			t.Error("leaked private data")
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(r.Header.Get("X-Dymploy-Timestamp") + "."))
		mac.Write(b)
		if r.Header.Get("X-Dymploy-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
			t.Error("bad signature")
		}
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			w.WriteHeader(503)
			return
		}
		got = append(got, r.Header.Get("X-Dymploy-Event-ID"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		events.Deliver(ctx, store, events.Sink{Name: "test", URL: srv.URL, Key: key}, 10*time.Millisecond, nil)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "j1:queued,j1:running,j1:failed" {
		t.Fatalf("%v attempts %d", got, attempts)
	}
	if _, ok, err := store.NextEvent("test"); err != nil || ok {
		t.Fatalf("queue not drained: %v %v", ok, err)
	}
}

func TestFiltersAndTerminalEventTypes(t *testing.T) {
	store, err := job.OpenBolt(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.EventSinks([]string{"filtered"})
	start := time.Now().Add(-time.Second)
	end := time.Now()
	for i, state := range []job.State{job.Queued, job.Running, job.Aborted, job.Interrupted, job.Succeeded} {
		j := job.Job{ID: job.ID(fmt.Sprint(i)), Name: "github/acme/app", State: state, StartedAt: start, FinishedAt: end}
		store.Put(context.Background(), j)
	}
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		events.Deliver(ctx, store, events.Sink{Name: "filtered", URL: srv.URL, Key: []byte("key"), Repositories: []string{"github/other/app", "github/acme/app"}, Types: []string{"aborted", "interrupted"}}, time.Millisecond, nil)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := store.NextEvent("filtered"); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"type":"aborted"`) || !strings.Contains(bodies[1], `"type":"interrupted"`) {
		t.Fatal(bodies)
	}
}
