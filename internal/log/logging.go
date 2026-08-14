// Package log provides helpers for creating a configured slog.Logger.
//
// When a log file path is not provided, logs are written to stdout for
// non-error levels and to stderr for errors (so stderr can be used for
// error redirection while keeping normal logs on stdout).
package log

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

const maxLogFileBytes int64 = 16 << 20

type boundedFile struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	mode     os.FileMode
	size     int64
	maxBytes int64
}

func openBoundedFile(path string, mode os.FileMode, maxBytes int64) (*boundedFile, error) {
	if maxBytes <= 0 {
		return nil, errors.New("bounded log file size must be positive")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, mode)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &boundedFile{
		file: file, path: path, mode: mode, size: info.Size(), maxBytes: maxBytes,
	}, nil
}

// OpenBoundedFile opens a durable append-only session log. Recovery restarts
// retain the previous failure, while a fixed-size wrap prevents unattended
// trace logging from consuming the machine's disk.
func OpenBoundedFile(path string, mode os.FileMode) (io.WriteCloser, error) {
	return openBoundedFile(path, mode, maxLogFileBytes)
}

func (f *boundedFile) Write(payload []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return 0, os.ErrClosed
	}
	originalLength := len(payload)
	if int64(len(payload)) > f.maxBytes {
		payload = payload[len(payload)-int(f.maxBytes):]
	}
	if f.size+int64(len(payload)) > f.maxBytes {
		if err := f.file.Close(); err != nil {
			f.file = nil
			return 0, err
		}
		file, err := os.OpenFile(
			f.path, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_RDWR, f.mode)
		if err != nil {
			f.file = nil
			return 0, err
		}
		f.file = file
		f.size = 0
	}
	written, err := f.file.Write(payload)
	f.size += int64(written)
	if err != nil {
		return written, err
	}
	if written != len(payload) {
		return written, io.ErrShortWrite
	}
	return originalLength, nil
}

func (f *boundedFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

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

// SetupLogger builds a slog.Logger with console and optional file handlers.
func SetupLogger(logLevel, logFile string) (*slog.Logger, []io.Closer, error) {
	level := ParseLevel(logLevel)
	var handlers []slog.Handler

	stdoutHandler := &colorHandler{w: os.Stdout, level: level}
	handlers = append(handlers, LevelFilter{pass: func(l slog.Level) bool { return l < slog.LevelError }, h: stdoutHandler})

	stderrHandler := &colorHandler{w: os.Stderr, level: slog.LevelError}
	handlers = append(handlers, LevelFilter{pass: func(l slog.Level) bool { return l >= slog.LevelError }, h: stderrHandler})
	var closeFiles []io.Closer
	if logFile != "" {
		f, err := OpenBoundedFile(logFile, 0o644)
		if err != nil {
			return nil, nil, err
		}
		closeFiles = append(closeFiles, f)
		handlers = append(handlers, slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))
	}
	logger := slog.New(MultiHandler{hs: handlers})
	slog.SetDefault(logger)
	return logger, closeFiles, nil
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

type colorHandler struct {
	w     io.Writer
	level slog.Leveler
}

func (h *colorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *colorHandler) Handle(_ context.Context, r slog.Record) error {
	buf := strings.Builder{}

	buf.WriteString("\033[90m")
	buf.WriteString(r.Time.Format("2006-01-02T15:04:05.000000Z07:00"))
	buf.WriteString("\033[0m ")

	var color string
	switch {
	case r.Level >= slog.LevelError:
		color = "\033[31m"
	case r.Level >= slog.LevelWarn:
		color = "\033[33m"
	case r.Level >= slog.LevelInfo:
		color = "\033[32m"
	case r.Level >= slog.LevelDebug:
		color = "\033[34m"
	case r.Level >= LevelTrace:
		color = "\033[35m"
	default:
		color = "\033[0m"
	}
	buf.WriteString(color)
	fmt.Fprintf(&buf, "%5s", r.Level.String())
	buf.WriteString("\033[0m")

	buf.WriteString(" ")
	buf.WriteString(r.Message)

	r.Attrs(func(a slog.Attr) bool {
		buf.WriteString(" \033[90m")
		buf.WriteString(a.Key)
		buf.WriteString("=\033[0m")
		buf.WriteString(a.Value.String())
		return true
	})

	buf.WriteString("\n")
	_, err := h.w.Write([]byte(buf.String()))
	return err
}

func (h *colorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *colorHandler) WithGroup(name string) slog.Handler {
	return h
}
