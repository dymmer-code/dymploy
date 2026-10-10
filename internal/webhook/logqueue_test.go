package webhook

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/dymmer-code/dymploy/internal/provider"
)

func TestLogQueueAcceptsAndSaysSo(t *testing.T) {
	var buf bytes.Buffer
	q := LogQueue{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if err := q.Enqueue(context.Background(), provider.Event{Provider: "github", FullName: "a/b", Ref: "refs/heads/main"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not implemented yet") || !strings.Contains(buf.String(), "a/b") {
		t.Fatalf("the placeholder must be loud about not running anything:\n%s", buf.String())
	}
}
