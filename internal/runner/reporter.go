package runner

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// Stream says where a line of the log came from.
type Stream string

// The streams of a job's log.
const (
	// Stdout and Stderr are the output of the work itself.
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
	// System is the runner talking about the execution: pulling an image, starting a container,
	// copying an artifact… It is what lets a log tell the story, not just the output.
	System Stream = "system"
)

// Reporter is how a runner narrates an execution. All methods are safe for concurrent use and
// lines are delivered in the order they were completed, across streams.
type Reporter interface {
	// Stdout and Stderr are line-oriented writers for the output of the work.
	Stdout() io.Writer
	Stderr() io.Writer
	// System records a step or event of the execution itself.
	System(msg string)
}

// MaxLine is the longest line delivered; longer ones are split so that memory stays bounded.
const MaxLine = 16 << 10

// LineReporter is a Reporter that splits writes into lines and hands each complete line to emit,
// immediately and in order. Close delivers what is left of unfinished lines.
type LineReporter struct {
	mu     sync.Mutex
	emit   func(Stream, string)
	out    *lineWriter
	err    *lineWriter
	closed bool
}

// NewLineReporter returns a reporter that calls emit once per line. emit is called with the
// reporter's lock held, which is what keeps the order between streams: it should be quick.
func NewLineReporter(emit func(Stream, string)) *LineReporter {
	r := &LineReporter{emit: emit}
	r.out = &lineWriter{r: r, stream: Stdout}
	r.err = &lineWriter{r: r, stream: Stderr}
	return r
}

// Stdout implements Reporter.
func (r *LineReporter) Stdout() io.Writer { return r.out }

// Stderr implements Reporter.
func (r *LineReporter) Stderr() io.Writer { return r.err }

// System implements Reporter.
func (r *LineReporter) System(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		r.emit(System, line)
	}
}

// Close delivers any unfinished line. It is safe to call more than once.
func (r *LineReporter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, w := range []*lineWriter{r.out, r.err} {
		if len(w.buf) > 0 {
			r.emit(w.stream, string(bytes.TrimSuffix(w.buf, []byte("\r"))))
			w.buf = nil
		}
	}
}

type lineWriter struct {
	r      *LineReporter
	stream Stream
	buf    []byte
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	n := len(p)
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		switch {
		case i >= 0 && i <= MaxLine:
			w.r.emit(w.stream, string(bytes.TrimSuffix(w.buf[:i], []byte("\r"))))
			w.buf = w.buf[i+1:]
		case len(w.buf) > MaxLine:
			w.r.emit(w.stream, string(w.buf[:MaxLine]))
			w.buf = w.buf[MaxLine:]
		default:
			return n, nil
		}
	}
}
