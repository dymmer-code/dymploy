package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsForEmptyFile(t *testing.T) {
	c, err := Load(write(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || c.ControlSocket != "/run/dymploy/control.sock" || c.DataDir != "/var/lib/dymploy" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestOverrides(t *testing.T) {
	c, err := Load(write(t, "listen: 0.0.0.0:9000\ncontrol_socket: /tmp/x.sock\ndata_dir: /srv/dp\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || c.ControlSocket != "/tmp/x.sock" || c.DataDir != "/srv/dp" {
		t.Fatalf("overrides lost: %+v", c)
	}
}

func TestUnknownFieldsAreRejected(t *testing.T) {
	_, err := Load(write(t, "listen: 127.0.0.1:1\nlisten_adress: typo\n"))
	if err == nil || !strings.Contains(err.Error(), "listen_adress") {
		t.Fatalf("a typo in the configuration must be reported, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"listen without port":     "listen: localhost\n",
		"relative control socket": "control_socket: run/control.sock\n",
		"relative data dir":       "data_dir: data\n",
		"empty listen":            "listen: \"\"\n",
		"malformed yaml":          "listen: [\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body)); err == nil {
				t.Fatalf("%q must be rejected", body)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil || !strings.Contains(err.Error(), "nope.yaml") {
		t.Fatalf("the error must name the file, got %v", err)
	}
}
