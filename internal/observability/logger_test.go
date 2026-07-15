package observability

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPrettyHandlerProducesCompactReadableLine(t *testing.T) {
	var output bytes.Buffer
	handler := NewPrettyHandler(&output, "gateway", slog.LevelInfo, false)
	record := slog.NewRecord(time.Date(2026, 7, 14, 19, 1, 2, 345000000, time.UTC), slog.LevelInfo, "Request processed", 0)
	record.Add("method", "GET", "path", "/healthz", "status", 200)
	if err := handler.Handle(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	line := output.String()
	for _, expected := range []string{"19:01:02.345", "INFO", "[gateway]", "Request processed", "method=GET", "path=/healthz", "status=200"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("missing %q in %q", expected, line)
		}
	}
	if strings.Contains(line, "\x1b[") {
		t.Fatalf("unexpected ANSI sequence: %q", line)
	}
}
