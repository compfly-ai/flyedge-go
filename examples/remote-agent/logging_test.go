package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrettyLogKeepsContentReadableWithoutTerminalInjection(t *testing.T) {
	var out bytes.Buffer
	h := &prettyHandler{out: &out, mu: &sync.Mutex{}}
	r := slog.NewRecord(time.Date(2026, 10, 26, 11, 48, 16, 0, time.UTC), slog.LevelInfo, "invocation.input", 0)
	r.Add("content", "first line\n\x1b[2Jsecond line\u202e", "invocation", "704fc2578ac44f2b962cd5a95cf4fced", "session", "signed-session-value")
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	result := out.String()
	if strings.Contains(result, "\x1b") || strings.ContainsRune(result, '\u202e') {
		t.Fatalf("terminal control escaped protection: %q", result)
	}
	if !strings.Contains(result, "    │ first line\n    │ ") || strings.Contains(result, "signed-session-value") {
		t.Fatalf("not compact, readable content: %q", result)
	}
}
