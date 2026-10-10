package runner

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

type entry struct {
	stream Stream
	text   string
}

func collect() (*LineReporter, func() []entry) {
	var mu sync.Mutex
	var got []entry
	r := NewLineReporter(func(s Stream, line string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, entry{s, line})
	})
	return r, func() []entry { mu.Lock(); defer mu.Unlock(); return append([]entry(nil), got...) }
}

func TestLinesAreEmittedAsTheyAreCompleted(t *testing.T) {
	r, got := collect()
	r.Stdout().Write([]byte("one\ntw"))
	if g := got(); len(g) != 1 || g[0] != (entry{Stdout, "one"}) {
		t.Fatalf("a completed line must be emitted at once, the partial one held back: %v", g)
	}
	r.Stdout().Write([]byte("o\nthree\n"))
	if g := got(); len(g) != 3 || g[1].text != "two" || g[2].text != "three" {
		t.Fatalf("%v", g)
	}
}

func TestTheOrderBetweenStreamsIsKept(t *testing.T) {
	r, got := collect()
	r.Stdout().Write([]byte("a\n"))
	r.Stderr().Write([]byte("b\n"))
	r.System("c")
	r.Stdout().Write([]byte("d\n"))
	want := []entry{{Stdout, "a"}, {Stderr, "b"}, {System, "c"}, {Stdout, "d"}}
	g := got()
	if fmt.Sprint(g) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", g, want)
	}
}

func TestCloseFlushesPartialLines(t *testing.T) {
	r, got := collect()
	r.Stdout().Write([]byte("no newline at the end"))
	r.Stderr().Write([]byte("nor here"))
	if len(got()) != 0 {
		t.Fatal("nothing is complete yet")
	}
	r.Close()
	g := got()
	if len(g) != 2 || g[0].text != "no newline at the end" || g[1].text != "nor here" {
		t.Fatalf("%v", g)
	}
	r.Close() // idempotent
	if len(got()) != 2 {
		t.Fatal("a second Close must not repeat lines")
	}
}

func TestCarriageReturnsAreStripped(t *testing.T) {
	r, got := collect()
	r.Stdout().Write([]byte("windows\r\nstyle\r\n"))
	if g := got(); g[0].text != "windows" || g[1].text != "style" {
		t.Fatalf("%q", g)
	}
}

func TestOverlongLinesAreSplit(t *testing.T) {
	r, got := collect()
	r.Stdout().Write([]byte(strings.Repeat("x", MaxLine*2+10) + "\n"))
	g := got()
	if len(g) != 3 || len(g[0].text) != MaxLine || len(g[1].text) != MaxLine || len(g[2].text) != 10 {
		lens := []int{}
		for _, e := range g {
			lens = append(lens, len(e.text))
		}
		t.Fatalf("a line without end must not grow without limit, got lengths %v", lens)
	}
}

func TestWritesReportFullLength(t *testing.T) {
	r, _ := collect()
	n, err := r.Stdout().Write([]byte("hello\nwor"))
	if n != 9 || err != nil {
		t.Fatalf("io.Writer contract: %d %v", n, err)
	}
}

func TestConcurrentWritersDoNotTearLines(t *testing.T) {
	r, got := collect()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				fmt.Fprintf(r.Stdout(), "writer-%d line-%d\n", w, i)
			}
		}(w)
	}
	wg.Wait()
	g := got()
	if len(g) != 800 {
		t.Fatalf("got %d lines", len(g))
	}
	for _, e := range g {
		if !strings.HasPrefix(e.text, "writer-") || !strings.Contains(e.text, " line-") {
			t.Fatalf("torn line %q", e.text)
		}
	}
}

func TestEmptyLinesArePreserved(t *testing.T) {
	r, got := collect()
	r.Stdout().Write([]byte("a\n\nb\n"))
	if g := got(); len(g) != 3 || g[1].text != "" {
		t.Fatalf("%v", g)
	}
}
