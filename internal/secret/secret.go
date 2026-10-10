// Package secret reads secrets from files, refusing files that other users on the machine can read.
package secret

import (
	"fmt"
	"os"
	"strings"
)

// ReadFile returns the contents of path without surrounding whitespace. The file must not be
// accessible by "others" (mode & 0007 == 0) and must not be empty. Errors never include the content.
func ReadFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("reading secret %s: %w", path, err)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		return "", fmt.Errorf("secret %s is readable by other users (mode %04o): run chmod 600 %s", path, fi.Mode().Perm(), path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading secret %s: %w", path, err)
	}
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", fmt.Errorf("secret %s is empty", path)
	}
	return s, nil
}
