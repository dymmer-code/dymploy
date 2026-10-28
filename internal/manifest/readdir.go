package manifest

import (
	"fmt"
	"os"
	"path/filepath"
)

// ReadDir returns the manifest of a working copy, or a message for a job's log when it cannot be used. The
// file comes from the repository: it must be a plain file of it, not a link to something else.
func ReadDir(dir string) (*Manifest, string) {
	fsys := os.DirFS(dir)
	name, err := Find(fsys)
	if err != nil {
		return nil, err.Error()
	}
	full := filepath.Join(dir, name)
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, name + " must be a regular file"
	}
	if fi.Size() > MaxSize {
		return nil, fmt.Sprintf("%s is too large (the limit is %d bytes)", name, MaxSize)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, "reading " + name + ": " + err.Error()
	}
	m, err := Parse(data)
	if err != nil {
		return nil, name + ": " + err.Error()
	}
	if err := m.CheckFiles(fsys); err != nil {
		return nil, name + ": " + err.Error()
	}
	return m, ""
}
