//go:build integration

// Against a real OpenSSH server. Point it at any machine you may log in to with a key:
//
//	DYMPLOY_IT_SSH=127.0.0.1:2222 DYMPLOY_IT_USER=me DYMPLOY_IT_KEY=~/.ssh/id_ed25519 \
//	  go test -tags integration -run RealOpenSSH ./internal/deploy/sshremote/
//
// The host key is read with ssh-keyscan, so the test checks the protocol, not the trust. Files are
// written under a temporary directory in the login directory and removed afterwards.
package sshremote

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dymmer-code/dymploy/internal/deploy"
)

func TestRealOpenSSH(t *testing.T) {
	addr, user, key := os.Getenv("DYMPLOY_IT_SSH"), os.Getenv("DYMPLOY_IT_USER"), os.Getenv("DYMPLOY_IT_KEY")
	if addr == "" || user == "" || key == "" {
		t.Skip("set DYMPLOY_IT_SSH, DYMPLOY_IT_USER and DYMPLOY_IT_KEY")
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	scan, err := exec.Command("ssh-keyscan", "-p", portText, host).Output()
	if err != nil || len(scan) == 0 {
		t.Fatalf("ssh-keyscan: %v", err)
	}
	hosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(hosts, scan, 0o644); err != nil {
		t.Fatal(err)
	}
	tg := deploy.Target{Name: "it", Host: host, Port: port, User: user, KeyFile: key, KnownHosts: hosts}
	r := &Remote{DialTimeout: 10 * time.Second}
	if err := r.Check(tg); err != nil {
		t.Fatal(err)
	}
	c, err := r.Dial(context.Background(), tg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var out bytes.Buffer
	code, err := c.Run(context.Background(), `echo "$A|$B"; exit 4`, map[string]string{"A": "it's", "B": "x y\nz"}, &out, os.Stderr)
	if err != nil || code != 4 || out.String() != "it's|x y\nz\n" {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}

	dir := "dymploy-it-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	defer c.Run(context.Background(), `rm -rf -- "$D"`, map[string]string{"D": dir}, os.Stdout, os.Stderr)
	payload := bytes.Repeat([]byte("dymploy"), 300_000)
	if err := c.Upload(context.Background(), dir+"/sub/file.bin", bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code, err = c.Run(context.Background(), `sha256sum -- "$F"; stat -c '%a' "$F" 2>/dev/null || stat -f '%Lp' "$F"; ls "$D/sub"`,
		map[string]string{"F": dir + "/sub/file.bin", "D": dir}, &out, os.Stderr)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
	lines := strings.Fields(out.String())
	if len(lines) < 4 || lines[2] != "600" || lines[3] != "file.bin" {
		t.Fatalf("unexpected listing: %q", out.String())
	}
}
