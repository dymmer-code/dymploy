// Package secretcmd obtains a target's secrets by running the command the server's configuration names.
//
// The command is run directly (no shell) with a minimal environment plus the variables of an optional
// file, and what it prints on its standard output is the secrets. dymploy treats them as opaque bytes:
// they are never logged, and neither is the output of the command beyond a short error message.
package secretcmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

const (
	// MaxBytes is the largest accepted output.
	MaxBytes = 256 << 10
	// DefaultTimeout bounds the command when the configuration does not say.
	DefaultTimeout = 30 * time.Second
	maxStderr      = 300
)

// Fetcher implements deploy.SecretFetcher.
type Fetcher struct{}

// New returns a Fetcher.
func New() *Fetcher { return &Fetcher{} }

// Check verifies, without running anything, that the command and its environment file can be used.
func Check(spec deploy.SecretsSpec) error {
	if len(spec.Command) == 0 {
		return errors.New("no command")
	}
	st, err := os.Stat(spec.Command[0])
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", spec.Command[0])
	}
	if spec.EnvFile != "" {
		if _, err := readEnv(spec.EnvFile); err != nil {
			return err
		}
	}
	return nil
}

// Fetch runs the command and returns its output.
func (Fetcher) Fetch(ctx context.Context, spec deploy.SecretsSpec) ([]byte, error) {
	if len(spec.Command) == 0 {
		return nil, errors.New("the secrets source has no command")
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + os.TempDir(), "LANG=C.UTF-8"}
	if spec.EnvFile != "" {
		extra, err := readEnv(spec.EnvFile)
		if err != nil {
			return nil, err
		}
		env = append(env, extra...)
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	cmd.Env = env
	var out, errb limited
	out.max, errb.max = MaxBytes+1, maxStderr
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the secrets source did not answer within %s", timeout)
		}
		msg := strings.TrimSpace(errb.buf.String())
		if msg != "" {
			return nil, fmt.Errorf("the secrets source failed (%v): %s", err, msg)
		}
		return nil, fmt.Errorf("the secrets source failed: %v", err)
	}
	data := out.buf.Bytes()
	switch {
	case len(data) > MaxBytes:
		return nil, fmt.Errorf("the secrets are larger than %d KiB", MaxBytes>>10)
	case len(bytes.TrimSpace(data)) == 0:
		return nil, errors.New("the secrets source returned nothing")
	case bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data):
		return nil, errors.New("the secrets are not text")
	}
	if data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	return data, nil
}

// readEnv parses NAME=value lines (blank lines and # comments are ignored). The values are never shown.
func readEnv(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets env_file: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Mode().Perm()&0o007 != 0 {
		return nil, fmt.Errorf("secrets env_file %s must not be readable by others", path)
	}
	var out []string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("secrets env_file %s line %d is not NAME=value", path, n)
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// limited keeps at most max bytes and discards the rest, so a runaway command cannot use memory.
type limited struct {
	buf bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}
