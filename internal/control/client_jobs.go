package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/dymmer-code/dymploy/internal/job"
)

// ErrNoJobs is returned when the daemon does not offer the job endpoints.
var ErrNoJobs = errors.New("this daemon does not offer job information")

// call sends a request and decodes the answer; an error answer becomes an error with the daemon's words.
func (c *Client) call(ctx context.Context, method, path string, out any) error {
	return c.send(ctx, method, path, nil, out)
}

func (c *Client) send(ctx context.Context, method, path string, body []byte, out any) error {
	var raw json.RawMessage
	code, err := c.doBody(ctx, method, path, body, &raw)
	if err != nil {
		return err
	}
	if code >= 200 && code < 300 {
		if out == nil || len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	switch {
	case code == http.StatusNotFound && e.Error == "":
		return ErrNoJobs
	case e.Error != "":
		return errors.New(e.Error)
	}
	return fmt.Errorf("the daemon answered %d for %s", code, path)
}

// Jobs lists jobs, newest first.
func (c *Client) Jobs(ctx context.Context, f job.Filter) ([]job.Job, error) {
	q := url.Values{}
	var states []string
	for _, s := range f.States {
		states = append(states, string(s))
	}
	if len(states) > 0 {
		q.Set("state", strings.Join(states, ","))
	}
	if f.Name != "" {
		q.Set("name", f.Name)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	var out []job.Job
	err := c.call(ctx, http.MethodGet, "/v1/jobs?"+q.Encode(), &out)
	return out, err
}

// Job returns one job.
func (c *Client) Job(ctx context.Context, id string) (job.Job, error) {
	var j job.Job
	err := c.call(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(id), &j)
	return j, err
}

// Logs returns up to limit log entries of a job after a sequence number.
func (c *Client) Logs(ctx context.Context, id string, after int64, limit int) ([]job.Entry, error) {
	var out []job.Entry
	err := c.call(ctx, http.MethodGet, fmt.Sprintf("/v1/jobs/%s/logs?after=%d&limit=%d", url.PathEscape(id), after, limit), &out)
	return out, err
}

// Abort asks a job to stop, saying why. It returns when the request is made, not when the job has stopped.
func (c *Client) Abort(ctx context.Context, id, reason string) (string, error) {
	var out struct {
		Message string `json:"message"`
	}
	body, _ := json.Marshal(map[string]string{"reason": reason})
	err := c.send(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/abort", body, &out)
	return out.Message, err
}

// Stop asks the daemon to stop: gracefully (finish the work in hand, take no more) or by aborting what
// runs. It returns what the daemon says it will do; the daemon goes away when it is safe.
func (c *Client) Stop(ctx context.Context, graceful bool, reason string) (string, error) {
	var out struct {
		Message string `json:"message"`
	}
	body, _ := json.Marshal(map[string]any{"graceful": graceful, "reason": reason})
	err := c.send(ctx, http.MethodPost, "/v1/stop", body, &out)
	if err != nil && err.Error() == ErrNoStop.Error() {
		return "", ErrNoStop
	}
	return out.Message, err
}

// Queue starts a job by hand and returns it.
func (c *Client) Queue(ctx context.Context, repo, ref string) (job.Job, error) {
	body, _ := json.Marshal(EnqueueRequest{Repo: repo, Ref: ref})
	var j job.Job
	err := c.send(ctx, http.MethodPost, "/v1/queue", body, &j)
	return j, err
}

// Retry submits a copy of a finished job and returns it.
func (c *Client) Retry(ctx context.Context, id string) (job.Job, error) {
	var j job.Job
	err := c.call(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(id)+"/retry", &j)
	return j, err
}

// Watch follows the live channel and calls fn for every message until ctx ends, fn returns an error or
// the daemon closes the channel (normally, when the watched job finishes: Watch then returns nil). With
// a job id it replays that job's log from after first.
func (c *Client) Watch(ctx context.Context, jobID string, after int64, fn func(Message) error) error {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
		},
	}}
	q := url.Values{}
	if jobID != "" {
		q.Set("job", jobID)
		q.Set("after", strconv.FormatInt(after, 10))
	}
	conn, resp, err := websocket.Dial(ctx, "ws://dymploy/v1/events?"+q.Encode(), &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		if resp != nil {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			resp.Body.Close()
			if e.Error != "" {
				return errors.New(e.Error)
			}
			if resp.StatusCode == http.StatusNotFound {
				return ErrNoJobs
			}
		}
		return fmt.Errorf("%w at %s (is it running? start it with `dymploy serve`): %v", ErrUnreachable, c.Socket, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case websocket.CloseStatus(err) == websocket.StatusNormalClosure:
				return nil
			}
			return err
		}
		var m Message
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("unreadable message from the daemon: %w", err)
		}
		if m.Type == "error" {
			return errors.New(m.Error)
		}
		if err := fn(m); err != nil {
			conn.Close(websocket.StatusNormalClosure, "bye")
			return err
		}
	}
}
