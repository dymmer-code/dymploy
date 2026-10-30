package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/dymmer-code/dymploy/internal/control"
	"github.com/dymmer-code/dymploy/internal/job"
)

func TestClientPhoenixProtocolAndLogStream(t *testing.T) {
	s, jobs, snapshot := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = jobs.store.Put(ctx, job.Job{ID: "job-a", State: job.Succeeded, Origin: &job.Origin{Owner: "owner", Project: "project", Source: "builder-a", App: "app-a"}})
	_ = jobs.logs.Append("job-a", job.Entry{Seq: 1, Text: "line one"})
	outcome := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Dym-Secret") != strings.Repeat("x", 43) {
			http.Error(w, "unauthorized", 401)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			outcome <- err
			return
		}
		defer conn.CloseNow()
		read := func() (frame, error) {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return frame{}, err
			}
			return decodeFrame(raw)
		}
		write := func(event, ref string, payload any) error {
			raw, _ := json.Marshal([]any{"1", ref, "builder:control", event, payload})
			return conn.Write(ctx, websocket.MessageText, raw)
		}
		f, err := read()
		if err != nil {
			outcome <- err
			return
		}
		if f.Event != "phx_join" {
			outcome <- context.Canceled
			return
		}
		if err := write("phx_reply", "join", map[string]any{"status": "ok", "response": snapshot}); err != nil {
			outcome <- err
			return
		}
		if _, err := read(); err != nil {
			outcome <- err
			return
		}
		if err := write("command", "", Command{ID: "watch-1", Action: "watch", App: "app-a", Job: "job-a"}); err != nil {
			outcome <- err
			return
		}
		if _, err := read(); err != nil {
			outcome <- err
			return
		}
		f, err = read()
		if err != nil {
			outcome <- err
			return
		}
		if f.Event != "log" || !strings.Contains(string(f.Payload), "line one") {
			outcome <- context.Canceled
			return
		}
		outcome <- nil
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(path, []byte(strings.Repeat("x", 43)), 0600)
	c := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), TokenFile: path, Service: s}
	done := make(chan error, 1)
	go func() { done <- c.session(ctx) }()
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	<-done
}

func TestClientCommandsEventsAndHeartbeat(t *testing.T) {
	s, jobs, snapshot := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			outcome <- err
			return
		}
		defer conn.CloseNow()
		read := func() (frame, error) {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return frame{}, err
			}
			return decodeFrame(raw)
		}
		write := func(topic, event, ref string, payload any) error {
			raw, _ := json.Marshal([]any{"1", ref, topic, event, payload})
			return conn.Write(ctx, websocket.MessageText, raw)
		}
		_, err = read()
		if err != nil {
			outcome <- err
			return
		}
		_ = write("builder:control", "phx_reply", "join", map[string]any{"status": "ok", "response": snapshot})
		_, err = read()
		if err != nil {
			outcome <- err
			return
		}
		_ = write("builder:control", "command", "", Command{ID: "history", Action: "history", App: "app-a"})
		_ = write("builder:control", "command", "", Command{ID: "invalid-watch", Action: "watch", App: "app-a", Job: "missing"})
		_ = write("builder:control", "command", "", Command{ID: "unwatch", Action: "unwatch", RequestID: "missing"})
		jobs.events <- control.Event{Type: "finished", Job: job.Job{Origin: &job.Origin{Owner: "owner", Project: "project", Source: s.Catalog.Source, App: "app-a"}}}
		seen := map[string]bool{}
		for len(seen) < 5 {
			f, err := read()
			if err != nil {
				outcome <- err
				return
			}
			switch f.Event {
			case "result":
				var result Result
				_ = json.Unmarshal(f.Payload, &result)
				seen[result.ID] = true
			case "event":
				seen["event"] = true
			case "heartbeat":
				seen["heartbeat"] = true
				_ = write("phoenix", "phx_reply", f.Ref, map[string]any{"status": "ok", "response": map[string]any{}})
			case "configuration":
				seen["configuration"] = true
				_ = write("builder:control", "phx_reply", f.Ref, map[string]any{"status": "ok", "response": snapshot})
			}
		}
		outcome <- nil
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(path, []byte(strings.Repeat("x", 43)), 0600)
	c := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), TokenFile: path, Service: s, tickInterval: time.Millisecond * 5, heartbeatInterval: time.Millisecond * 10, syncInterval: time.Millisecond * 10}
	done := make(chan error, 1)
	go func() { done <- c.session(ctx) }()
	select {
	case err := <-outcome:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	<-done
}

func TestClientRejectsInvalidProfilesAndFrames(t *testing.T) {
	for _, raw := range []string{`{}`, `[null,null,1,"event",{}]`, `[null,null,"topic",1,{}]`} {
		if _, err := decodeFrame([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if classifyConnectionError(ErrPending) != "configuration_rejected" || classifyConnectionError(context.Canceled) != "connection_failed" {
		t.Fatal("error classification")
	}
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
	}{
		{"bad permission", strings.Repeat("x", 43), 0644}, {"bad token", "x", 0600}, {"too large", strings.Repeat("x", 300), 0600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token")
			_ = os.WriteFile(path, []byte(tc.body), tc.mode)
			c := Client{TokenFile: path}
			if c.session(context.Background()) == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	c := Client{TokenFile: filepath.Join(t.TempDir(), "missing")}
	if c.session(context.Background()) == nil {
		t.Fatal("missing token")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Run(ctx)
}

func TestClientReconnectsAfterFailureWithoutAffectingJobs(t *testing.T) {
	s, _, _ := testService(t)
	reached := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached <- struct{}{}; http.Error(w, "temporary", 503) }))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(path, []byte(strings.Repeat("x", 43)), 0600)
	c := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), TokenFile: path, Service: s}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	for i := 0; i < 2; i++ {
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal("did not reconnect")
		}
	}
	cancel()
	<-done
	if _, err := s.Catalog.Accepted("app-a"); err != nil {
		t.Fatal("connection failure erased local approval", err)
	}
}

func TestClientSendsItsInventoryOnceReadyAndAgainLater(t *testing.T) {
	s, _, snapshot := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	received := make(chan string, 4)
	failure := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			failure <- err
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			f, err := decodeFrame(raw)
			if err != nil {
				failure <- err
				return
			}
			switch f.Event {
			case "phx_join":
				reply, _ := json.Marshal([]any{"1", "join", "builder:control", "phx_reply", map[string]any{"status": "ok", "response": snapshot}})
				_ = conn.Write(ctx, websocket.MessageText, reply)
			case "inventory":
				received <- string(f.Payload)
			}
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(path, []byte(strings.Repeat("x", 43)), 0600)
	asked := 0
	c := Client{URL: "ws" + strings.TrimPrefix(server.URL, "http"), TokenFile: path, Service: s,
		tickInterval: 20 * time.Millisecond, inventoryEvery: 100 * time.Millisecond,
		Inventory: func(context.Context) any {
			asked++
			return map[string]any{"schema_version": 1, "asked": asked}
		}}
	done := make(chan error, 1)
	go func() { done <- c.session(ctx) }()
	for i := 1; i <= 2; i++ {
		select {
		case payload := <-received:
			if !strings.Contains(payload, `"schema_version":1`) {
				t.Fatalf("inventory %d: %s", i, payload)
			}
		case err := <-failure:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatalf("inventory %d never arrived", i)
		}
	}
	cancel()
	<-done
}
