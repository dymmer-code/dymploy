package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, mode os.FileMode, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask must not hide the mode under test
		t.Fatal(err)
	}
	return p
}

func TestReadTrimsTheTrailingNewline(t *testing.T) {
	got, err := ReadFile(write(t, 0o600, "abc123\n"))
	if err != nil || got != "abc123" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestGroupReadableIsAllowed(t *testing.T) {
	if _, err := ReadFile(write(t, 0o640, "x")); err != nil {
		t.Fatalf("0640 is the usual mode for a secret shared with the service group: %v", err)
	}
}

func TestWorldAccessIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o604, 0o602, 0o666} {
		_, err := ReadFile(write(t, mode, "x"))
		if err == nil || !strings.Contains(err.Error(), "chmod") {
			t.Fatalf("mode %o must be refused with a hint, got %v", mode, err)
		}
	}
}

func TestEmptySecretIsRefused(t *testing.T) {
	for _, body := range []string{"", "\n", "  \n"} {
		if _, err := ReadFile(write(t, 0o600, body)); err == nil {
			t.Fatalf("%q must be refused", body)
		}
	}
}

func TestMissingFileNamesThePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent")
	if _, err := ReadFile(p); err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("got %v", err)
	}
}

func TestErrorsNeverContainTheSecret(t *testing.T) {
	_, err := ReadFile(write(t, 0o644, "top-secret-value"))
	if err == nil || strings.Contains(err.Error(), "top-secret-value") {
		t.Fatalf("got %v", err)
	}
}
