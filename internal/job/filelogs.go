package job

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/dymmer-code/dymploy/internal/runner"
)

// ErrBadID is returned for an id that could not be a job id (and could escape the log directory).
var ErrBadID = errors.New("invalid job id")

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// FileLogs keeps each job's log as a JSON-lines file, one entry per line, written as it arrives so
// that a crash loses at most the line being written. A job's log stops growing at MaxBytes.
type FileLogs struct {
	dir      string
	maxBytes int64
	mu       sync.Mutex
	full     map[ID]bool // jobs whose log reached the limit
}

// NewFileLogs stores logs under dir (mode 0750, files 0600). maxBytes <= 0 means unlimited.
func NewFileLogs(dir string, maxBytes int64) (*FileLogs, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating the log directory: %w", err)
	}
	return &FileLogs{dir: dir, maxBytes: maxBytes, full: map[ID]bool{}}, nil
}

func (l *FileLogs) path(id ID) (string, error) {
	if !safeID.MatchString(string(id)) {
		return "", fmt.Errorf("%w: %q", ErrBadID, id)
	}
	return filepath.Join(l.dir, string(id)+".jsonl"), nil
}

// Append implements Logs.
func (l *FileLogs) Append(id ID, e Entry) error {
	p, err := l.path(id)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.full[id] {
		return nil // the truncation marker is already in the file
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if l.maxBytes > 0 {
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if fi.Size()+int64(len(raw)) > l.maxBytes {
			// Say it once, then stop: the marker is small and may take the file slightly over the limit.
			raw, _ = json.Marshal(Entry{Seq: e.Seq, Time: e.Time, Stream: runner.System,
				Text: fmt.Sprintf("[log truncated: the %d byte limit per job was reached]", l.maxBytes)})
			raw = append(raw, '\n')
			l.full[id] = true
		}
	}
	_, err = f.Write(raw)
	return err
}

// Read implements Logs.
func (l *FileLogs) Read(id ID, after int64, limit int) ([]Entry, error) {
	p, err := l.path(id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return out, fmt.Errorf("log of %s: %w", id, err)
		}
		if e.Seq > after {
			out = append(out, e)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, sc.Err()
}

// Delete implements Logs.
func (l *FileLogs) Delete(id ID) error {
	p, err := l.path(id)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.full, id)
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
