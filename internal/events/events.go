// Package events delivers durable, signed lifecycle notifications without delaying jobs.
package events

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dymmer-code/dymploy/internal/job"
	"github.com/dymmer-code/dymploy/internal/provider"
)

type Sink struct {
	Name, URL           string
	Key                 []byte
	Repositories, Types []string
}
type Store interface {
	NextEvent(string) (job.Transition, bool, error)
	AckEvent(string, uint64) error
}

func includes(xs []string, s string) bool {
	if len(xs) == 0 {
		return true
	}
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

type Target struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	ExitCode int    `json:"exit_code"`
}
type Payload struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	JobID      job.ID    `json:"job_id"`
	Name       string    `json:"name"`
	Number     int       `json:"number"`
	State      job.State `json:"state"`
	Reference  string    `json:"reference"`
	Commit     string    `json:"commit"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Targets    []Target  `json:"targets,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

func payload(t job.Transition) Payload {
	j := t.Job
	var ev provider.Event
	_ = json.Unmarshal(j.Input, &ev)
	typ := "finished"
	switch j.State {
	case job.Queued:
		typ = "queued"
	case job.Running:
		typ = "started"
	case job.Aborted:
		typ = "aborted"
	case job.Interrupted:
		typ = "interrupted"
	}
	p := Payload{ID: string(j.ID) + ":" + string(j.State), Type: typ, JobID: j.ID, Name: j.Name, Number: j.Number, State: j.State, Reference: ev.Ref, Commit: ev.Commit, ExitCode: j.ExitCode, CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt}
	if !j.StartedAt.IsZero() && !j.FinishedAt.IsZero() {
		p.DurationMS = j.FinishedAt.Sub(j.StartedAt).Milliseconds()
	}
	var details struct {
		Targets []Target `json:"targets"`
	}
	_ = json.Unmarshal(j.Details, &details)
	p.Targets = details.Targets
	return p
}

// Deliver runs one ordered stream per sink. A failed request retains its cursor; a restart safely
// replays unacknowledged events. Receivers must deduplicate by ID (delivery is at least once).
func Deliver(ctx context.Context, store Store, sink Sink, poll time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	delay := poll
	for ctx.Err() == nil {
		t, ok, err := store.NextEvent(sink.Name)
		if err == nil && ok {
			p := payload(t)
			if includes(sink.Repositories, p.Name) && includes(sink.Types, p.Type) {
				err = send(ctx, client, sink, p)
			}
			if err == nil {
				err = store.AckEvent(sink.Name, t.Sequence)
			}
			if err == nil {
				delay = poll
				continue
			}
		}
		if err != nil {
			log.Warn("event delivery pending", "sink", sink.Name, "event", t.Sequence)
			delay = min(max(delay*2, poll), time.Minute)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func send(ctx context.Context, client *http.Client, sink Sink, p Payload) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, sink.Key)
	mac.Write([]byte(stamp + "."))
	mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sink.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dymploy-Timestamp", stamp)
	req.Header.Set("X-Dymploy-Event-ID", p.ID)
	req.Header.Set("X-Dymploy-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("receiver returned %d", resp.StatusCode)
	}
	return nil
}
