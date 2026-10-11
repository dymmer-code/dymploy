package sshremote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/dymmer-code/dymploy/internal/artifact"
	"github.com/dymmer-code/dymploy/internal/deploy"
	"github.com/dymmer-code/dymploy/internal/manifest"
	"github.com/dymmer-code/dymploy/internal/provider"
	"github.com/dymmer-code/dymploy/internal/runner"
)

// server is a small in-process SSH server that executes `exec` requests with the local shell, in a
// directory of its own. It lets the client be tested against real SSH without any machine.
type server struct {
	t        *testing.T
	l        net.Listener
	home     string
	hostKey  ssh.Signer
	client   ssh.PublicKey
	wg       sync.WaitGroup
	commands []string
	mu       sync.Mutex
}

func newSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, priv
}

func startServer(t *testing.T, hostKey ssh.Signer, clientKey ssh.PublicKey, more ...ssh.Signer) *server {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, l: l, home: t.TempDir(), hostKey: hostKey, client: clientKey}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() == "deploy" && bytes.Equal(k.Marshal(), clientKey.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("denied")
	}}
	cfg.AddHostKey(hostKey)
	for _, k := range more {
		cfg.AddHostKey(k)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(nc, cfg) }()
		}
	}()
	t.Cleanup(func() { l.Close(); s.wg.Wait() })
	return s
}

func (s *server) port() int { return s.l.Addr().(*net.TCPAddr).Port }

func (s *server) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	defer nc.Close()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

func (s *server) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var p struct{ Cmd string }
		ssh.Unmarshal(req.Payload, &p)
		req.Reply(true, nil)
		s.mu.Lock()
		s.commands = append(s.commands, p.Cmd)
		s.mu.Unlock()
		cmd := exec.Command("sh", "-c", p.Cmd)
		cmd.Dir = s.home
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
		code := 0
		if err := cmd.Start(); err != nil {
			code = 127
		} else {
			// signals sent by the client reach the command, as OpenSSH does
			go func() {
				for r := range reqs {
					if r.Type != "signal" {
						r.Reply(false, nil)
						continue
					}
					var sg struct{ Name string }
					ssh.Unmarshal(r.Payload, &sg)
					switch sg.Name {
					case "TERM":
						cmd.Process.Signal(syscall.SIGTERM)
					case "KILL":
						cmd.Process.Signal(syscall.SIGKILL)
					}
				}
			}()
			if err := cmd.Wait(); err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else {
					code = 127
				}
			}
		}
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, uint32(code))
		ch.SendRequest("exit-status", false, status)
		return
	}
}

// rig writes the key and known_hosts files a target needs.
type rig struct {
	srv    *server
	target deploy.Target
}

func newRig(t *testing.T) *rig {
	t.Helper()
	hostKey, _ := newSigner(t)
	clientSigner, clientPriv := newSigner(t)
	srv := startServer(t, hostKey, clientSigner.PublicKey())
	dir := t.TempDir()
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "id")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(dir, "known_hosts")
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.port()))
	if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{addr}, hostKey.PublicKey())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &rig{srv: srv, target: deploy.Target{Name: "t", Host: "127.0.0.1", Port: srv.port(), User: "deploy", KeyFile: keyFile, KnownHosts: hosts}}
}

func (r *rig) dial(t *testing.T) deploy.Conn {
	t.Helper()
	c, err := (&Remote{DialTimeout: 5 * time.Second}).Dial(context.Background(), r.target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRunStreamsOutputExitCodeAndEnvironment(t *testing.T) {
	r := newRig(t)
	c := r.dial(t)
	var out, errb bytes.Buffer
	code, err := c.Run(context.Background(), "echo \"hello $WHO\"; echo oops >&2; exit 3", map[string]string{"WHO": "it's \"me\" $(id) `x`\nline2"}, &out, &errb)
	if err != nil || code != 3 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.String() != "hello it's \"me\" $(id) `x`\nline2\n" || errb.String() != "oops\n" {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
	code, err = c.Run(context.Background(), "true", nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestTheScriptRunsWithErrexit(t *testing.T) {
	c := newRig(t).dial(t)
	var out bytes.Buffer
	code, _ := c.Run(context.Background(), "false\necho not-reached", nil, &out, io.Discard)
	if code == 0 || out.Len() != 0 {
		t.Fatalf("sh -e must stop at the first failure: code=%d out=%q", code, out.String())
	}
}

func TestInvalidCommandsAreRefusedBeforeBeingSent(t *testing.T) {
	r := newRig(t)
	c := r.dial(t)
	for name, tc := range map[string]struct {
		script string
		env    map[string]string
	}{
		"bad variable name": {"true", map[string]string{"A B": "x"}},
		"option as name":    {"true", map[string]string{"-i": "x"}},
		"NUL in the value":  {"true", map[string]string{"A": "x\x00y"}},
		"NUL in the script": {"true\x00", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Run(context.Background(), tc.script, tc.env, io.Discard, io.Discard); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if len(r.srv.commands) != 0 {
		t.Fatalf("nothing should have reached the server: %v", r.srv.commands)
	}
}

func TestUploadWritesTheFileAtomicallyWithPrivateMode(t *testing.T) {
	r := newRig(t)
	c := r.dial(t)
	payload := bytes.Repeat([]byte("0123456789"), 100_000)
	if err := c.Upload(context.Background(), "up dir/it's here.tar.gz", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(r.srv.home, "up dir", "it's here.tar.gz")
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("content differs (err=%v, %d bytes)", err, len(got))
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if _, err := os.Stat(p + ".part"); err == nil {
		t.Fatal("the temporary file is left behind")
	}
}

func TestUploadRefusals(t *testing.T) {
	r := newRig(t)
	c := r.dial(t)
	for _, p := range []string{"", "-rf", "dir/"} {
		if err := c.Upload(context.Background(), p, strings.NewReader("x")); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if err := c.Upload(context.Background(), "a\x00b", strings.NewReader("x")); err == nil {
		t.Error("NUL accepted")
	}
	// the target says no: the directory cannot be created
	os.WriteFile(filepath.Join(r.srv.home, "file"), nil, 0o600)
	err := c.Upload(context.Background(), "file/sub/x", strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "refused the upload") {
		t.Fatalf("%v", err)
	}
	// a plain relative name goes to the login directory
	if err := c.Upload(context.Background(), "plain.bin", strings.NewReader("ok")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.srv.home, "plain.bin")); string(b) != "ok" {
		t.Fatalf("%q", b)
	}
}

// readyWriter calls fn the first time anything is written to it.
type readyWriter struct {
	once sync.Once
	fn   func()
}

func (w *readyWriter) Write(p []byte) (int, error) {
	w.once.Do(w.fn)
	return len(p), nil
}

func TestCancellingAsksTheCommandToStopBeforeKillingIt(t *testing.T) {
	old := termGrace
	termGrace = 300 * time.Millisecond
	defer func() { termGrace = old }()
	r := newRig(t)
	c := r.dial(t)

	// a command that cleans up when asked: it gets TERM and the time to do it
	marker := filepath.Join(r.srv.home, "cleaned-up")
	ctx, cancel := context.WithCancel(context.Background())
	// cancel only once the trap is installed, or a slow machine delivers TERM before the shell is ready
	ready := &readyWriter{fn: cancel}
	start := time.Now()
	_, err := c.Run(ctx, "trap 'touch cleaned-up; exit 0' TERM; echo ready; sleep 30 & wait", nil, ready, io.Discard)
	if err != context.Canceled {
		t.Fatalf("%v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command was not given the chance to clean up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// one that ignores TERM is killed after the grace period, not left running
	ctx2, cancel2 := context.WithCancel(context.Background())
	ready2 := &readyWriter{fn: cancel2}
	start = time.Now()
	if _, err := c.Run(ctx2, "trap '' TERM; echo ready; while true; do sleep 1; done", nil, ready2, io.Discard); err != context.Canceled {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 5*time.Second {
		t.Fatalf("TERM, a grace period, then KILL: took %s", d)
	}
}

func TestCancellingEndsARunningCommand(t *testing.T) {
	c := newRig(t).dial(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.Run(ctx, "sleep 30", nil, io.Discard, io.Discard)
	if err != context.Canceled || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(start))
	}
	if _, err := c.Run(ctx, "true", nil, io.Discard, io.Discard); err != context.Canceled {
		t.Fatalf("a cancelled context runs nothing: %v", err)
	}
}

func TestCheck(t *testing.T) {
	r := newRig(t)
	if err := (&Remote{}).Check(r.target); err != nil {
		t.Fatal(err)
	}
	bad := r.target
	bad.KeyFile = filepath.Join(t.TempDir(), "none")
	if err := (&Remote{}).Check(bad); err == nil || !strings.Contains(err.Error(), "key_file") {
		t.Fatalf("%v", err)
	}
	open := filepath.Join(t.TempDir(), "id")
	b, _ := os.ReadFile(r.target.KeyFile)
	os.WriteFile(open, b, 0o644)
	bad = r.target
	bad.KeyFile = open
	if err := (&Remote{}).Check(bad); err == nil || !strings.Contains(err.Error(), "readable by other users") {
		t.Fatalf("a key anybody can read is refused: %v", err)
	}
	junk := filepath.Join(t.TempDir(), "id")
	os.WriteFile(junk, []byte("not a key"), 0o600)
	bad.KeyFile = junk
	if err := (&Remote{}).Check(bad); err == nil || !strings.Contains(err.Error(), "not a usable private key") {
		t.Fatalf("%v", err)
	}
	bad = r.target
	bad.KnownHosts = filepath.Join(t.TempDir(), "none")
	if err := (&Remote{}).Check(bad); err == nil || !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("%v", err)
	}
}

func TestAKeyWithAPassphraseIsExplained(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id")
	os.WriteFile(p, pem.EncodeToMemory(block), 0o600)
	r := newRig(t)
	tg := r.target
	tg.KeyFile = p
	if err := (&Remote{}).Check(tg); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("%v", err)
	}
}

func TestConnectionRefusals(t *testing.T) {
	r := newRig(t)
	remote := &Remote{DialTimeout: 3 * time.Second}

	// a host that presents another key than the one in known_hosts
	other, _ := newSigner(t)
	clientSigner, clientPriv := newSigner(t)
	_ = clientPriv
	imp := startServer(t, other, clientSigner.PublicKey())
	tg := r.target
	tg.Port = imp.port()
	if _, err := remote.Dial(context.Background(), tg); err == nil {
		t.Fatal("an unknown port is not in known_hosts")
	} else if !strings.Contains(err.Error(), "not in") {
		t.Fatalf("%v", err)
	}
	// same address as the real one, different key: a mismatch
	hosts := filepath.Join(t.TempDir(), "kh")
	wrong, _ := newSigner(t)
	os.WriteFile(hosts, []byte(knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(r.srv.port()))}, wrong.PublicKey())+"\n"), 0o644)
	tg = r.target
	tg.KnownHosts = hosts
	if _, err := remote.Dial(context.Background(), tg); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a changed host key must be refused: %v", err)
	}
	// a user the server does not accept
	tg = r.target
	tg.User = "root"
	if _, err := remote.Dial(context.Background(), tg); err == nil {
		t.Fatal("login as another user")
	}
	// nothing listening
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	tg = r.target
	tg.Port = port
	if _, err := remote.Dial(context.Background(), tg); err == nil {
		t.Fatal("connection refused")
	}
	// unusable files are reported by Dial too
	tg = r.target
	tg.KeyFile = "/nonexistent"
	if _, err := remote.Dial(context.Background(), tg); err == nil {
		t.Fatal("no key")
	}
	tg = r.target
	tg.KnownHosts = "/nonexistent"
	if _, err := remote.Dial(context.Background(), tg); err == nil {
		t.Fatal("no known_hosts")
	}
}

func TestAHostThatOffersAnotherKindOfKeyIsStillVerified(t *testing.T) {
	// known_hosts holds the server's RSA key while the client would negotiate ed25519 first.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	edHost, _ := newSigner(t)
	clientSigner, clientPriv := newSigner(t)
	srv := startServer(t, edHost, clientSigner.PublicKey(), rsaSigner)
	dir := t.TempDir()
	block, _ := ssh.MarshalPrivateKey(clientPriv, "")
	keyFile := filepath.Join(dir, "id")
	os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600)
	hosts := filepath.Join(dir, "kh")
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(srv.port()))
	os.WriteFile(hosts, []byte(knownhosts.Line([]string{addr}, rsaSigner.PublicKey())+"\n"), 0o644)
	tg := deploy.Target{Host: "127.0.0.1", Port: srv.port(), User: "deploy", KeyFile: keyFile, KnownHosts: hosts}
	c, err := (&Remote{DialTimeout: 5 * time.Second}).Dial(context.Background(), tg)
	if err != nil {
		t.Fatalf("the known RSA key should be asked for and verified: %v", err)
	}
	c.Close()
}

func TestCommandAndQuote(t *testing.T) {
	got, err := command("echo 'a b'", map[string]string{"B": "2", "A": "it's"})
	if err != nil {
		t.Fatal(err)
	}
	want := `exec env -- A='it'\''s' B='2' sh -ec 'echo '\''a b'\'''`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestDialHonoursContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Remote{DialTimeout: 3 * time.Second}).Dial(ctx, r.target); err == nil {
		t.Fatal("a cancelled context must not connect")
	}
}

// A whole deployment over real SSH: a real archive is uploaded, verified, extracted by the steps,
// checked, and the upload is removed.
func TestAFullDeploymentOverSSH(t *testing.T) {
	r := newRig(t)
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "rel", "bin"), 0o755)
	os.WriteFile(filepath.Join(root, "rel", "bin", "app"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	arts := t.TempDir()
	res, err := artifact.Pack(root, artifact.Spec{Name: "app", Paths: []string{"rel"}}, arts)
	if err != nil {
		t.Fatal(err)
	}
	rec := artifact.Record{Name: "app", File: "app.tar.gz", SHA256: res.SHA256, Size: res.Size}
	m, err := manifest.Parse([]byte(`version: 1
build:
  image: {from: "alpine:3"}
  steps: [{run: "true"}]
artifact: {paths: [rel]}
deploy:
  target: t
  steps:
    - name: install
      run: |
        mkdir -p apps/app
        tar -xzf "$DYMPLOY_ARTIFACT" -C apps/app --strip-components=1
    - name: mark
      run: echo "$DYMPLOY_COMMIT $DYMPLOY_ARTIFACT_SHA256" > apps/app/DEPLOYED
  check: {run: "test -x apps/app/bin/app", attempts: 2, interval: 1s}
  after:
    steps: [{run: "echo done > apps/after"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var logs []string
	rep := runner.NewLineReporter(func(s runner.Stream, l string) { logs = append(logs, string(s)+": "+l) })
	d := &deploy.Deployer{Remote: &Remote{DialTimeout: 5 * time.Second}, Targets: deploy.NewTargets()}
	tg := r.target
	tg.UploadDir = "uploads"
	code, err := d.Run(context.Background(), deploy.Request{
		Target: tg, Deploy: m.Deploy, JobID: "j1", Record: rec, File: res.Path, Log: rep,
		Event: provider.Event{Owner: "acme", Repo: "app", Commit: "0123456789abcdef0123456789abcdef01234567", Branch: "main"},
	})
	rep.Close()
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v\n%s", code, err, strings.Join(logs, "\n"))
	}
	home := r.srv.home
	if b, err := os.ReadFile(filepath.Join(home, "apps/app/bin/app")); err != nil || string(b) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("the release was not installed: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "apps/app/DEPLOYED")); strings.TrimSpace(string(b)) != "0123456789abcdef0123456789abcdef01234567 "+res.SHA256 {
		t.Fatalf("variables: %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, "apps/after")); err != nil {
		t.Fatal("after did not run")
	}
	if left, _ := filepath.Glob(filepath.Join(home, "uploads", "*")); len(left) != 0 {
		t.Fatalf("the upload was not removed: %v", left)
	}
}

// shim puts a sha256sum in a directory for machines (macOS) that only have shasum.
func shim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := exec.LookPath("sha256sum"); err == nil {
		return dir
	}
	if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nshift\nexec shasum -a 256 \"$@\"\n"), 0o755); err != nil {
		t.Skip("no sha256sum or shasum")
	}
	return dir
}
