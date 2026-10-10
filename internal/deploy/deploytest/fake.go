// Package deploytest gives tests a Remote that reaches no machine: connections are scripted and
// record what was uploaded and run.
package deploytest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

// Run is one command executed on the fake target.
type Run struct {
	Script string
	Env    map[string]string
}

// Fake is a deploy.Remote.
type Fake struct {
	// Handle decides the outcome of each script; nil means exit 0. Hang makes it wait for ctx.
	Handle func(script string) (exit int, stdout string, hang bool)
	// DialErr, UploadErr and CheckErr make the corresponding calls fail. Corrupt changes the uploaded
	// content, as a damaged transfer would.
	DialErr, UploadErr, CheckErr error
	Corrupt                      bool

	mu      sync.Mutex
	Dialed  []deploy.Target
	Runs    []Run
	Files   map[string][]byte
	Closed  int
	Removed []string
}

// Check implements deploy.Remote.
func (f *Fake) Check(deploy.Target) error { return f.CheckErr }

// Dial implements deploy.Remote.
func (f *Fake) Dial(_ context.Context, t deploy.Target) (deploy.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DialErr != nil {
		return nil, f.DialErr
	}
	f.Dialed = append(f.Dialed, t)
	if f.Files == nil {
		f.Files = map[string][]byte{}
	}
	return &conn{f: f}, nil
}

// Scripts returns the scripts run so far, except the housekeeping ones (verification, clean-up).
func (f *Fake) Scripts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.Runs {
		if !strings.HasPrefix(r.Script, "sha256sum") && !strings.HasPrefix(r.Script, "rm -f") {
			out = append(out, r.Script)
		}
	}
	return out
}

type conn struct{ f *Fake }

func (c *conn) Upload(_ context.Context, remote string, r io.Reader) error {
	if c.f.UploadErr != nil {
		return c.f.UploadErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if c.f.Corrupt {
		data = append(data, 'x')
	}
	c.f.mu.Lock()
	c.f.Files[remote] = data
	c.f.mu.Unlock()
	return nil
}

func (c *conn) Run(ctx context.Context, script string, env map[string]string, stdout, _ io.Writer) (int, error) {
	f := c.f
	f.mu.Lock()
	f.Runs = append(f.Runs, Run{script, env})
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(script, "sha256sum"):
		f.mu.Lock()
		data, ok := f.Files[env["DYMPLOY_ARTIFACT"]]
		f.mu.Unlock()
		if !ok {
			return 1, nil
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(stdout, "%s  %s\n", hex.EncodeToString(sum[:]), env["DYMPLOY_ARTIFACT"])
		return 0, nil
	case strings.HasPrefix(script, "rm -f"):
		f.mu.Lock()
		f.Removed = append(f.Removed, env["DYMPLOY_ARTIFACT"])
		delete(f.Files, env["DYMPLOY_ARTIFACT"])
		f.mu.Unlock()
		return 0, nil
	}
	if f.Handle == nil {
		return 0, nil
	}
	exit, out, hang := f.Handle(script)
	io.WriteString(stdout, out)
	if hang {
		<-ctx.Done()
		return 0, errors.New("interrupted")
	}
	return exit, nil
}

func (c *conn) Close() error {
	c.f.mu.Lock()
	c.f.Closed++
	c.f.mu.Unlock()
	return nil
}
