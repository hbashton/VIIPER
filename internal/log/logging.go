// Package log provides helpers for creating a configured slog.Logger.
//
// When a log file path is not provided, logs are written to stdout for
// non-error levels and to stderr for errors (so stderr can be used for
// error redirection while keeping normal logs on stdout).
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// LevelTrace defines a custom slog level below Debug for very verbose output.
const LevelTrace slog.Level = -8

func ParseLevel(s string) slog.Level {
	switch s {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "info", "":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// MultiHandler fans out records to multiple handlers.
type MultiHandler struct{ hs []slog.Handler }

func (m MultiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.hs {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}
func (m MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.hs {
		_ = h.Handle(ctx, r)
	}
	return nil
}
func (m MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		out[i] = h.WithAttrs(attrs)
	}
	return MultiHandler{hs: out}
}
func (m MultiHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(m.hs))
	for i, h := range m.hs {
		out[i] = h.WithGroup(name)
	}
	return MultiHandler{hs: out}
}

// LevelFilter delegates to an underlying handler but filters which levels are
// passed to it using the provided predicate.
type LevelFilter struct {
	pass func(slog.Level) bool
	h    slog.Handler
}

func (f LevelFilter) Enabled(ctx context.Context, level slog.Level) bool {
	if !f.pass(level) {
		return false
	}
	return f.h.Enabled(ctx, level)
}

func (f LevelFilter) Handle(ctx context.Context, r slog.Record) error {
	if !f.pass(r.Level) {
		return nil
	}
	return f.h.Handle(ctx, r)
}

func (f LevelFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return LevelFilter{pass: f.pass, h: f.h.WithAttrs(attrs)}
}
func (f LevelFilter) WithGroup(name string) slog.Handler {
	return LevelFilter{pass: f.pass, h: f.h.WithGroup(name)}
}

// SetupLogger builds a slog.Logger with console and optional file handlers.
func SetupLogger(logLevel, logFile string) (*slog.Logger, []io.Closer, error) {
	level := ParseLevel(logLevel)
	var handlers []slog.Handler

	if logFile == "" {
		stdoutHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
		handlers = append(handlers, LevelFilter{pass: func(l slog.Level) bool { return l < slog.LevelError }, h: stdoutHandler})

		stderrHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})
		handlers = append(handlers, LevelFilter{pass: func(l slog.Level) bool { return l >= slog.LevelError }, h: stderrHandler})
	} else {
		handlers = append(handlers, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	var closeFiles []io.Closer
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, nil, err
		}
		closeFiles = append(closeFiles, f)
		handlers = append(handlers, slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))
	}
	logger := slog.New(MultiHandler{hs: handlers})
	return logger, closeFiles, nil
}
