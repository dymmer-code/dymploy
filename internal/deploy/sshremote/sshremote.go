// Package sshremote is the deploy.Remote that reaches targets over SSH.
//
// The host must present the key listed in the target's known_hosts file (nothing is accepted on
// first use) and the login uses the target's own private key. Commands run as `sh -ec` with the
// environment passed through env(1), so the server needs no AcceptEnv setting, and files travel as
// the standard input of a remote `cat`, so it needs no SFTP subsystem.
package sshremote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/secret"
)

// Remote implements deploy.Remote.
type Remote struct {
	// DialTimeout bounds connecting and the SSH handshake; 0 means 15 s.
	DialTimeout time.Duration
}

func addr(t deploy.Target) string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

func loadKey(path string) (ssh.Signer, error) {
	pemText, err := secret.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("key_file: %w", err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(pemText))
	if err != nil {
		var pm *ssh.PassphraseMissingError
		if errors.As(err, &pm) {
			return nil, fmt.Errorf("key_file %s is protected by a passphrase, which is not supported: use a dedicated key without one", path)
		}
		return nil, fmt.Errorf("key_file %s is not a usable private key: %w", path, err)
	}
	return signer, nil
}

func loadHosts(t deploy.Target) (ssh.HostKeyCallback, error) {
	cb, err := knownhosts.New(t.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	return cb, nil
}

// Check implements deploy.Remote.
func (r *Remote) Check(t deploy.Target) error {
	if _, err := loadKey(t.KeyFile); err != nil {
		return err
	}
	_, err := loadHosts(t)
	return err
}

// Dial implements deploy.Remote.
func (r *Remote) Dial(ctx context.Context, t deploy.Target) (deploy.Conn, error) {
	signer, err := loadKey(t.KeyFile)
	if err != nil {
		return nil, err
	}
	hosts, err := loadHosts(t)
	if err != nil {
		return nil, err
	}
	timeout := r.DialTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User:            t.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hosts,
		Timeout:         timeout,
	}
	c, err := r.connect(ctx, t, cfg, timeout)
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) && len(ke.Want) > 0 {
		// The host offered a kind of key other than the one known for it: ask for the known kind, once.
		// A different key of the same kind fails again, and that is a real mismatch.
		cfg.HostKeyAlgorithms = nil
		for _, w := range ke.Want {
			if w.Key.Type() == ssh.KeyAlgoRSA { // RSA keys are used through their SHA-2 signatures
				cfg.HostKeyAlgorithms = append(cfg.HostKeyAlgorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
				continue
			}
			cfg.HostKeyAlgorithms = append(cfg.HostKeyAlgorithms, w.Key.Type())
		}
		c, err = r.connect(ctx, t, cfg, timeout)
	}
	if err != nil {
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				return nil, fmt.Errorf("%s is not in %s: add its host key before using the target", addr(t), t.KnownHosts)
			}
			return nil, fmt.Errorf("the host key of %s does not match %s: refusing to connect (is it a different machine?)", addr(t), t.KnownHosts)
		}
		return nil, err
	}
	return c, nil
}

func (r *Remote) connect(ctx context.Context, t deploy.Target, cfg *ssh.ClientConfig, timeout time.Duration) (deploy.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nc, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr(t))
	if err != nil {
		return nil, err
	}
	// The handshake does not take a context: closing the connection is what interrupts it.
	stop := context.AfterFunc(dctx, func() { nc.Close() })
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr(t), cfg)
	stop()
	if err != nil {
		nc.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return &conn{client: ssh.NewClient(cc, chans, reqs)}, nil
}

// termGrace is how long a command gets to end after TERM before it is killed.
var termGrace = 5 * time.Second

type conn struct{ client *ssh.Client }

func (c *conn) Close() error { return c.client.Close() }

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)

// quote makes s one literal word for a POSIX shell.
func quote(s string) (string, error) {
	if strings.ContainsRune(s, 0) {
		return "", errors.New("a NUL character cannot be part of a command")
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'", nil
}

// command builds `exec env -- NAME='v' … sh -ec '<script>'`. The exec replaces the login shell, so a signal sent to the
// session reaches the script's shell even when the login shell would otherwise fork (dash does).
func command(script string, env map[string]string) (string, error) {
	names := make([]string, 0, len(env))
	for k := range env {
		if !envName.MatchString(k) {
			return "", fmt.Errorf("%q is not a valid variable name", k)
		}
		names = append(names, k)
	}
	sort.Strings(names)
	parts := []string{"exec", "env", "--"}
	for _, k := range names {
		q, err := quote(env[k])
		if err != nil {
			return "", fmt.Errorf("variable %s: %w", k, err)
		}
		parts = append(parts, k+"="+q)
	}
	q, err := quote(script)
	if err != nil {
		return "", err
	}
	return strings.Join(append(parts, "sh", "-ec", q), " "), nil
}

// exec runs a command on a new session and waits for it, or for ctx to end.
func (c *conn) exec(ctx context.Context, cmd string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	sess, err := c.client.NewSession()
	if err != nil {
		return 0, err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr
	if err := sess.Start(cmd); err != nil {
		return 0, err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		// Ask first, then insist: TERM lets the command clean up; if it is still there after termGrace
		// the signal is KILL and the session is closed. (OpenSSH delivers signals to the command's
		// shell only: processes the script started may outlive it, so steps should be short and safe to repeat.)
		_ = sess.Signal(ssh.SIGTERM)
		select {
		case <-done:
		case <-time.After(termGrace):
			_ = sess.Signal(ssh.SIGKILL)
		}
		sess.Close()
		return 0, ctx.Err()
	case err := <-done:
		var exit *ssh.ExitError
		switch {
		case err == nil:
			return 0, nil
		case errors.As(err, &exit):
			return exit.ExitStatus(), nil
		}
		return 0, err
	}
}

// Run implements deploy.Conn.
func (c *conn) Run(ctx context.Context, script string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	cmd, err := command(script, env)
	if err != nil {
		return 0, err
	}
	return c.exec(ctx, cmd, nil, stdout, stderr)
}

// Upload implements deploy.Conn.
func (c *conn) Upload(ctx context.Context, remotePath string, r io.Reader) error {
	if remotePath == "" || strings.HasPrefix(remotePath, "-") || strings.HasSuffix(remotePath, "/") {
		return fmt.Errorf("invalid remote path %q", remotePath)
	}
	file, err := quote(remotePath)
	if err != nil {
		return err
	}
	part, _ := quote(remotePath + ".part")
	dir := "."
	if i := strings.LastIndex(remotePath, "/"); i > 0 {
		dir = remotePath[:i]
	}
	qdir, _ := quote(dir)
	// Written next to its final name and renamed, so a broken transfer never leaves a file that looks complete.
	cmd := fmt.Sprintf("umask 077 && mkdir -p -- %s && cat > %s && mv -f -- %s %s", qdir, part, part, file)
	var errb strings.Builder
	code, err := c.exec(ctx, cmd, r, io.Discard, &limitedWriter{w: &errb, left: 2048})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("the target refused the upload (exit %d): %s", code, strings.TrimSpace(errb.String()))
	}
	return nil
}

// limitedWriter keeps the first bytes written, for error messages, and drops the rest.
type limitedWriter struct {
	w    io.Writer
	left int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.left > 0 {
		if len(p) > l.left {
			p = p[:l.left]
		}
		l.left -= len(p)
		l.w.Write(p)
	}
	return n, nil
}

var _ deploy.Remote = (*Remote)(nil)
