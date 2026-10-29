package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/dymmer-code/dymploy/internal/job"
)

// Client keeps one outgoing Phoenix channel. Its failures never stop the queue.
type Client struct {
	tickInterval, heartbeatInterval, syncInterval, heartbeatTimeout time.Duration
	URL, TokenFile                                                  string
	Service                                                         *Service
	Log                                                             *slog.Logger
}
type frame struct {
	Topic, Event, Ref string
	Payload           json.RawMessage
}

func decodeFrame(raw []byte) (frame, error) {
	var v []json.RawMessage
	if json.Unmarshal(raw, &v) != nil || len(v) != 5 {
		return frame{}, errors.New("invalid frame")
	}
	f := frame{Payload: v[4]}
	if json.Unmarshal(v[2], &f.Topic) != nil || json.Unmarshal(v[3], &f.Event) != nil {
		return f, errors.New("invalid frame")
	}
	_ = json.Unmarshal(v[1], &f.Ref)
	return f, nil
}
func (c *Client) Run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if c.Log != nil {
			c.Log.Warn("Dymmer connection unavailable; builds continue locally", "retry_in", delay, "reason", classifyConnectionError(err))
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		} else if delay < 30*time.Second {
			delay *= 2
		}
	}
}
func classifyConnectionError(err error) string {
	if errors.Is(err, ErrPending) {
		return "configuration_rejected"
	}
	return "connection_failed"
}
func (c *Client) session(ctx context.Context) error {
	info, err := os.Stat(c.TokenFile)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return errors.New("token file must be private")
	}
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return err
	}
	secret := strings.TrimSpace(string(token))
	if len(secret) != 43 {
		return errors.New("invalid profile token")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, c.URL+"?vsn=2.0.0", &websocket.DialOptions{HTTPHeader: http.Header{"X-Dym-Secret": []string{secret}}, HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
	cancel()
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 << 20)
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	write := func(topic, event, ref string, payload any) error {
		raw, err := json.Marshal([]any{"1", ref, topic, event, payload})
		if err != nil {
			return err
		}
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, raw)
	}
	if err := write("builder:control", "phx_join", "join", map[string]any{}); err != nil {
		return err
	}
	frames := make(chan frame, 8)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			f, err := decodeFrame(raw)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	commands := make(chan Command, 8)
	results := make(chan Result, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cmd := <-commands:
				callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				r := c.Service.Handle(callCtx, cmd)
				cancel()
				select {
				case results <- r:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	events, unsubscribe := c.Service.Jobs.Subscribe()
	defer unsubscribe()
	tickEvery := durationOr(c.tickInterval, time.Second)
	heartbeatEvery := durationOr(c.heartbeatInterval, 20*time.Second)
	syncEvery := durationOr(c.syncInterval, 15*time.Second)
	heartbeatTimeout := durationOr(c.heartbeatTimeout, 60*time.Second)
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()
	watches := map[string]Command{}
	ready := false
	lastReply := time.Now()
	lastSync := time.Now()
	lastHeartbeat := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			return err
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if !ready || event.Type == "output" || event.Job.Origin == nil || event.Job.Origin.Source != c.Service.Catalog.OriginSource() {
				continue
			}
			if _, err := c.Service.Catalog.Accepted(event.Job.Origin.App); err != nil {
				continue
			}
			if err := write("builder:control", "event", "", map[string]any{"app": event.Job.Origin.App, "type": event.Type}); err != nil {
				return err
			}
		case result := <-results:
			if err := write("builder:control", "result", "", result); err != nil {
				return err
			}
		case f := <-frames:
			if f.Event == "phx_close" || f.Event == "phx_error" {
				return errors.New("channel closed")
			}
			if f.Event == "phx_reply" {
				var reply struct {
					Status   string          `json:"status"`
					Response json.RawMessage `json:"response"`
				}
				if json.Unmarshal(f.Payload, &reply) != nil || reply.Status != "ok" {
					return errors.New("channel refused")
				}
				if f.Ref == "heartbeat" {
					lastReply = time.Now()
					continue
				}
				if f.Ref == "join" || f.Ref == "configuration" {
					var snapshot Snapshot
					if json.Unmarshal(reply.Response, &snapshot) != nil {
						return ErrPending
					}
					if err := c.Service.Catalog.Update(snapshot); err != nil {
						return ErrPending
					}
					ready = true
					if err := write("builder:control", "ready", "", map[string]any{"revision": snapshot.Revision}); err != nil {
						return err
					}
				}
			}
			if f.Topic != "builder:control" || f.Event != "command" || !ready {
				continue
			}
			var cmd Command
			if json.Unmarshal(f.Payload, &cmd) != nil || cmd.ID == "" || len(cmd.ID) > 128 {
				return errors.New("invalid command")
			}
			switch cmd.Action {
			case "watch":
				check := cmd
				check.Action = "job"
				r := c.Service.Handle(ctx, check)
				if r.Error != "" || cmd.After < 0 || len(watches) >= 4 {
					if r.Error == "" {
						r.Error = "stream_limit"
					}
					r.Data = nil
					if err := write("builder:control", "result", "", r); err != nil {
						return err
					}
					continue
				}
				watches[cmd.ID] = cmd
				if err := write("builder:control", "result", "", Result{ID: cmd.ID, Data: map[string]any{"watching": true}}); err != nil {
					return err
				}
			case "unwatch":
				delete(watches, cmd.RequestID)
			default:
				select {
				case commands <- cmd:
				default:
					if err := write("builder:control", "result", "", Result{ID: cmd.ID, Error: "busy"}); err != nil {
						return err
					}
				}
			}
		case now := <-tick.C:
			if now.Sub(lastReply) > heartbeatTimeout {
				return errors.New("heartbeat timeout")
			}
			if now.Sub(lastHeartbeat) >= heartbeatEvery {
				if err := write("phoenix", "heartbeat", "heartbeat", map[string]any{}); err != nil {
					return err
				}
				lastHeartbeat = now
			}
			if ready && now.Sub(lastSync) >= syncEvery {
				if err := write("builder:control", "configuration", "configuration", map[string]any{}); err != nil {
					return err
				}
				lastSync = now
			}
			for id, cmd := range watches {
				cmd.Action = "logs"
				cmd.Limit = 200
				r := c.Service.Handle(ctx, cmd)
				if err := write("builder:control", "log", "", r); err != nil {
					return err
				}
				if r.Error != "" {
					delete(watches, id)
					continue
				}
				data := r.Data.(map[string]any)
				entries := data["entries"].([]job.Entry)
				if len(entries) > 0 {
					cmd.After = entries[len(entries)-1].Seq
					watches[id] = cmd
				}
				if data["complete"].(bool) {
					delete(watches, id)
				}
			}
		}
	}
}

func durationOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}
