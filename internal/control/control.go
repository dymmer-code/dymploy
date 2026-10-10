// Package control is the private channel between the dymploy CLI and the running daemon:
// a small JSON API over a Unix socket that only its owner can use.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Status describes the running daemon.
type Status struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
}

// Handlers are what the daemon offers through the socket. A nil Reload means the daemon cannot
// reload its configuration.
type Handlers struct {
	Status func() Status
	// Reload re-reads the configuration and applies what can change without a restart. The message
	// says what happened; an error means nothing was changed.
	Reload func() (string, error)
	// Jobs, if set, adds the job endpoints and the live channel (see API).
	Jobs Jobs
}

// ErrNoReload is returned by Client.Reload when the daemon cannot reload.
var ErrNoReload = errors.New("this daemon cannot reload its configuration")

// Serve listens on the Unix socket at path until ctx is cancelled. A stale socket file (left by a
// crash) is replaced; a live one is an error, so two daemons never share state. ready, if not nil,
// is called once the socket accepts connections.
func Serve(ctx context.Context, path string, h Handlers, ready func()) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating the control socket directory: %w", err)
	}
	if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
		conn.Close()
		return fmt.Errorf("another dymploy is already running on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the stale control socket: %w", err)
	}
	// Restrict permissions at creation time: no window in which another user could connect.
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return fmt.Errorf("listening on the control socket %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.Status())
	})
	mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if h.Reload == nil {
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": ErrNoReload.Error()})
			return
		}
		msg, err := h.Reload()
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	})
	if h.Jobs != nil {
		api := (&API{Jobs: h.Jobs, MaxStreams: 64}).Handler()
		for _, p := range []string{"/v1/jobs", "/v1/jobs/", "/v1/events", "/v1/queue"} {
			mux.Handle(p, api)
		}
	}
	// Only the local user can reach this socket, but a client that stalls still must not hold a connection.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	if ready != nil {
		ready()
	}
	select {
	case err := <-errc:
		os.Remove(path)
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	os.Remove(path)
	if err != nil {
		return err
	}
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return nil
}

// Client talks to a running daemon.
type Client struct {
	Socket string
}

// Status asks the daemon who it is.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var st Status
	err := c.get(ctx, "/v1/status", &st)
	return st, err
}

// Reload asks the daemon to re-read its configuration. A refused configuration comes back as an
// error carrying the daemon's explanation; the daemon keeps working with the previous one.
func (c *Client) Reload(ctx context.Context) (string, error) {
	var out struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	code, err := c.do(ctx, http.MethodPost, "/v1/reload", &out)
	switch {
	case err != nil:
		return "", err
	case code == http.StatusNotImplemented:
		return "", ErrNoReload
	case code != http.StatusOK:
		return "", fmt.Errorf("the daemon did not accept the new configuration and keeps the previous one: %s", out.Error)
	}
	return out.Message, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	code, err := c.do(ctx, http.MethodGet, path, out)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("daemon answered %d for %s", code, path)
	}
	return nil
}

// do calls the daemon and decodes the JSON answer, whatever the status; it returns the status.
func (c *Client) do(ctx context.Context, method, path string, out any) (int, error) {
	return c.doBody(ctx, method, path, nil, out)
}

func (c *Client) doBody(ctx context.Context, method, path string, body []byte, out any) (int, error) {
	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", c.Socket)
			},
		},
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://dymploy"+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach the daemon at %s (is it running? start it with `dymploy serve`): %w", c.Socket, err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && resp.StatusCode == http.StatusOK {
		return 0, err
	}
	return resp.StatusCode, nil
}
