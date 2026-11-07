// Package logging adds wide-event request logging on top of log/slog.
//
// Instead of scattering log lines through handlers, the middleware collects
// request and business context and emits one canonical event per request when
// the handler returns.
package logging

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"
)

const defaultMessage = "request_completed"

// Config configures request logging middleware.
type Config struct {
	// Logger receives the canonical event. When nil, slog.Default() is used
	// at emit time, so it follows a logger installed by otelkit.Setup.
	Logger *slog.Logger

	// Message is the default event message. Defaults to "request_completed".
	Message string

	// Fields are added to every event, for example a service name.
	Fields []slog.Attr

	// HealthySampleRate samples successful requests. Values <= 0 or >= 1 keep
	// every request. Errors, 5xx responses, slow requests and KeepPaths are
	// always kept.
	HealthySampleRate float64

	// SlowThreshold keeps events at or above this duration. Zero disables it.
	SlowThreshold time.Duration

	// KeepPaths keeps every request whose path starts with one of these
	// prefixes, regardless of HealthySampleRate.
	KeepPaths []string

	// SkipPaths skips logging for requests whose path starts with one of
	// these prefixes, for example "/healthz".
	SkipPaths []string
}

// Middleware returns a handler that emits one canonical log event per request.
// Place it inside otelkit.HTTPHandler so the server span is active and the
// event carries trace_id and span_id.
func Middleware(next http.Handler, cfg Config) http.Handler {
	return newMiddleware(next, cfg, rand.Float64)
}

type middleware struct {
	next      http.Handler
	cfg       Config
	randFloat func() float64
}

func newMiddleware(next http.Handler, cfg Config, randFloat func() float64) *middleware {
	if cfg.Message == "" {
		cfg.Message = defaultMessage
	}
	cfg.Fields = append([]slog.Attr(nil), cfg.Fields...)
	cfg.KeepPaths = append([]string(nil), cfg.KeepPaths...)
	cfg.SkipPaths = append([]string(nil), cfg.SkipPaths...)
	return &middleware{next: next, cfg: cfg, randFloat: randFloat}
}

func (m *middleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if hasPathPrefix(r.URL.Path, m.cfg.SkipPaths) {
		m.next.ServeHTTP(w, r)
		return
	}

	start := time.Now()
	ev := newEvent(m.cfg.Message, m.cfg.Fields)
	ev.add(slog.String("http.method", r.Method))
	ev.add(slog.String("http.path", r.URL.Path))
	if id := r.Header.Get("X-Request-ID"); id != "" {
		ev.add(slog.String("request_id", id))
	}

	ctx := context.WithValue(r.Context(), contextKey{}, ev)
	req := r.WithContext(ctx)
	rw := &responseWriter{ResponseWriter: w}

	defer func() {
		rec := recover()

		status := rw.status
		if status == 0 {
			status = http.StatusOK
			if rec != nil {
				status = http.StatusInternalServerError
			}
		}
		route := req.Pattern
		if route == "" {
			route = r.URL.Path
		}
		duration := time.Since(start)

		if rec != nil {
			ev.setError(fmt.Errorf("panic: %v", rec))
		}

		err := ev.getError()
		outcome := outcomeFor(status, err)
		message, attrs, _ := ev.finish(
			slog.String("http.route", route),
			slog.Int("http.status", status),
			slog.Int("http.bytes", rw.bytes),
			slog.Float64("duration_ms", float64(duration.Microseconds())/1000),
			slog.String("outcome", outcome),
		)

		if m.keep(status, duration, r.URL.Path, err) {
			logger := m.cfg.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.LogAttrs(ctx, levelFor(outcome), message, attrs...)
		}

		if rec != nil {
			panic(rec)
		}
	}()

	m.next.ServeHTTP(rw, req)
}

func (m *middleware) keep(status int, duration time.Duration, path string, err error) bool {
	if err != nil || status >= http.StatusInternalServerError {
		return true
	}
	if m.cfg.SlowThreshold > 0 && duration >= m.cfg.SlowThreshold {
		return true
	}
	if hasPathPrefix(path, m.cfg.KeepPaths) {
		return true
	}
	rate := m.cfg.HealthySampleRate
	if rate <= 0 || rate >= 1 {
		return true
	}
	return m.randFloat() < rate
}

func outcomeFor(status int, err error) string {
	if err != nil || status >= http.StatusInternalServerError {
		return "error"
	}
	if status >= http.StatusBadRequest {
		return "client_error"
	}
	return "success"
}

func levelFor(outcome string) slog.Level {
	switch outcome {
	case "error":
		return slog.LevelError
	case "client_error":
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

func hasPathPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix != "" && strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Add appends slog-style key/value pairs to the current request event.
func Add(ctx context.Context, args ...any) {
	if ev := eventFromContext(ctx); ev != nil {
		ev.addArgs(args)
	}
}

// AddAttrs appends typed slog attributes to the current request event.
func AddAttrs(ctx context.Context, attrs ...slog.Attr) {
	if ev := eventFromContext(ctx); ev != nil {
		for _, attr := range attrs {
			ev.add(attr)
		}
	}
}

// Error records err on the current request event. The final event carries an
// error group with the error type and message.
func Error(ctx context.Context, err error) {
	if ev := eventFromContext(ctx); ev != nil {
		ev.setError(err)
	}
}

// SetMessage overrides the final event message for the current request.
func SetMessage(ctx context.Context, message string) {
	if ev := eventFromContext(ctx); ev != nil {
		ev.setMessage(message)
	}
}

// responseWriter captures status and size while keeping the optional
// interfaces of the underlying writer reachable.
type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush implements http.Flusher for streaming responses.
func (w *responseWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack implements http.Hijacker for WebSocket and raw connections.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// ReadFrom keeps io.Copy on the fast path when the underlying writer supports
// it, and still counts the copied bytes.
func (w *responseWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		w.bytes += int(n)
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytes += int(n)
	return n, err
}
