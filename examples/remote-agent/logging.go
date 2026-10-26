package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"unicode"
)

func newExampleLogger(format, color string) (*slog.Logger, error) {
	if color != "auto" && color != "always" && color != "never" {
		return nil, fmt.Errorf("invalid -color %q: use auto, always, or never", color)
	}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, nil)), nil
	case "pretty":
		stat, err := os.Stderr.Stat()
		colored := color == "always" || (color == "auto" && err == nil && stat.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == "")
		return slog.New(&prettyHandler{out: os.Stderr, mu: &sync.Mutex{}, color: colored}), nil
	default:
		return nil, fmt.Errorf("invalid -log-format %q: use pretty, text, or json", format)
	}
}

type prettyHandler struct {
	out   io.Writer
	mu    *sync.Mutex
	color bool
	attrs []slog.Attr
	group string
}

func (h *prettyHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		a.Key = h.group + a.Key
		clone.attrs = append(clone.attrs, a)
	}
	return &clone
}
func (h *prettyHandler) WithGroup(name string) slog.Handler {
	clone := *h
	if name != "" {
		clone.group += name + "."
	}
	return &clone
}
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

// Preserve readable paragraphs while escaping terminal commands and bidi controls.
func terminalText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
func (h *prettyHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := append([]slog.Attr(nil), h.attrs...)
	r.Attrs(func(a slog.Attr) bool { a.Key = h.group + a.Key; attrs = append(attrs, a); return true })
	fields := map[string]string{}
	for _, a := range attrs {
		fields[a.Key] = a.Value.Resolve().String()
	}
	label, tone := "EVENT", "36"
	switch r.Message {
	case "connection.ready":
		label, tone = "READY", "32"
	case "invocation.started":
		label, tone = "INVOKE", "36"
	case "invocation.input":
		label, tone = "INPUT", "36"
	case "tool.requested":
		label, tone = "TOOL REQUEST", "35"
	case "tool.called":
		label, tone = "TOOL CALL", "35"
	case "tool.result":
		label, tone = "TOOL RESULT", "35"
	case "tool.governed_result":
		label, tone = "TOOL CHECKED", "35"
	case "policy.allowed":
		label, tone = "POLICY OK", "90"
	case "policy.denied":
		label, tone = "DENIED", "33"
	case "invocation.output":
		label, tone = "OUTPUT", "32"
	case "invocation.completed":
		label, tone = "DONE", "32"
	case "invocation.failed":
		label, tone = "FAILED", "31"
	}
	heading := fmt.Sprintf("%-12s", label)
	if h.color {
		heading = "\x1b[" + tone + "m" + heading + "\x1b[0m"
	}
	var b strings.Builder
	if r.Message == "invocation.started" || r.Message == "connection.ready" {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "%s  %s", r.Time.Format("15:04:05.000"), heading)
	if id := fields["invocation"]; id != "" {
		fmt.Fprintf(&b, "  %s", terminalText(shortID(id)))
	}
	for _, a := range attrs {
		switch a.Key {
		case "content", "invocation":
			continue
		case "run", "session", "purpose":
			if r.Message != "invocation.started" {
				continue
			}
		}
		value := a.Value.Resolve().String()
		if value == "" {
			continue
		}
		if a.Key == "session" || a.Key == "connection" {
			value = shortID(value)
		}
		fmt.Fprintf(&b, "  %s=%s", terminalText(a.Key), strings.ReplaceAll(terminalText(value), "\n", "\\n"))
	}
	b.WriteByte('\n')
	if content, ok := fields["content"]; ok {
		var pretty bytes.Buffer
		if json.Indent(&pretty, []byte(content), "", "  ") == nil {
			content = pretty.String()
		}
		for _, line := range strings.Split(terminalText(content), "\n") {
			fmt.Fprintf(&b, "    │ %s\n", line)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}
