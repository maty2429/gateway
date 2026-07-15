// Package observability configures structured application logging without
// third-party dependencies.
package observability

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	colorReset  = "\x1b[0m"
	colorGray   = "\x1b[90m"
	colorGreen  = "\x1b[32m"
	colorYellow = "\x1b[33m"
	colorRed    = "\x1b[31m"
	colorBlue   = "\x1b[34m"
)

// NewLogger uses compact, colored output on a development terminal and JSON
// in production, where logs are normally consumed by another process.
func NewLogger(service, environment string) *slog.Logger {
	if environment == "production" {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(NewPrettyHandler(os.Stdout, service, slog.LevelInfo, isTerminal(os.Stdout)))
}

type PrettyHandler struct {
	out     io.Writer
	service string
	level   slog.Level
	color   bool
	mu      *sync.Mutex
	attrs   []slog.Attr
	groups  []string
}

func NewPrettyHandler(out io.Writer, service string, level slog.Level, color bool) *PrettyHandler {
	return &PrettyHandler{out: out, service: service, level: level, color: color, mu: &sync.Mutex{}}
}

func (h *PrettyHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *PrettyHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Time.Format("15:04:05.000"))
	line.WriteString("  ")
	line.WriteString(h.levelText(record.Level))
	line.WriteString("  ")
	if h.color {
		line.WriteString(colorBlue)
	}
	line.WriteString("[")
	line.WriteString(h.service)
	line.WriteString("]")
	if h.color {
		line.WriteString(colorReset)
	}
	line.WriteString("  ")
	line.WriteString(record.Message)

	attributes := make([]slog.Attr, 0, len(h.attrs)+record.NumAttrs())
	attributes = append(attributes, h.attrs...)
	record.Attrs(func(attr slog.Attr) bool { attributes = append(attributes, attr); return true })
	if len(attributes) > 0 {
		line.WriteString("  | ")
		first := true
		for _, attr := range attributes {
			h.appendAttr(&line, strings.Join(h.groups, "."), attr, &first)
		}
	}
	line.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, line.String())
	return err
}

func (h *PrettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *PrettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func (h *PrettyHandler) appendAttr(line *strings.Builder, prefix string, attr slog.Attr, first *bool) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	key := attr.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, child := range attr.Value.Group() {
			h.appendAttr(line, key, child, first)
		}
		return
	}
	if !*first {
		line.WriteByte(' ')
	}
	*first = false
	line.WriteString(key)
	line.WriteByte('=')
	line.WriteString(formatValue(attr.Value))
}

func (h *PrettyHandler) levelText(level slog.Level) string {
	label, color := "INFO ", colorGreen
	switch {
	case level >= slog.LevelError:
		label, color = "ERROR", colorRed
	case level >= slog.LevelWarn:
		label, color = "WARN ", colorYellow
	case level < slog.LevelInfo:
		label, color = "DEBUG", colorGray
	}
	if !h.color {
		return label
	}
	return color + label + colorReset
}

func formatValue(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		text := value.String()
		if text == "" || strings.ContainsAny(text, " \t\r\n") {
			return strconv.Quote(text)
		}
		return text
	case slog.KindTime:
		return value.Time().Format(time.RFC3339Nano)
	case slog.KindDuration:
		return value.Duration().String()
	default:
		return fmt.Sprint(value.Any())
	}
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
