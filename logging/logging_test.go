package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func decodeEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func TestMiddlewareEvent(t *testing.T) {
	t.Run("given a routed request when the handler returns then one canonical event is emitted", func(t *testing.T) {
		var buf bytes.Buffer
		mux := http.NewServeMux()
		mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
			Add(r.Context(), "user_id", 42)
			w.WriteHeader(http.StatusCreated)
			if _, err := w.Write([]byte("ok")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
		})

		handler := Middleware(mux, Config{Logger: newTestLogger(&buf)})
		req := httptest.NewRequest(http.MethodGet, "/users/42", http.NoBody)
		req.Header.Set("X-Request-ID", "req-1")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		ev := events[0]
		want := map[string]any{
			"msg":         "request_completed",
			"http.method": "GET",
			"http.path":   "/users/42",
			"http.route":  "GET /users/{id}",
			"http.status": float64(http.StatusCreated),
			"http.bytes":  float64(2),
			"outcome":     "success",
			"user_id":     float64(42),
			"request_id":  "req-1",
		}
		for key, value := range want {
			if ev[key] != value {
				t.Errorf("%s = %v, want %v", key, ev[key], value)
			}
		}
		if _, ok := ev["duration_ms"]; !ok {
			t.Error("duration_ms is missing")
		}
	})
}

func TestAddReplacesDuplicateAndAddAttrs(t *testing.T) {
	t.Run("given repeated keys when the handler adds them then the last value wins", func(t *testing.T) {
		var buf bytes.Buffer
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Add(r.Context(), "key", "first")
			Add(r.Context(), "key", "second")
			AddAttrs(r.Context(), slog.Int("count", 3))
			w.WriteHeader(http.StatusNoContent)
		}), Config{Logger: newTestLogger(&buf)})

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", http.NoBody))

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		ev := events[0]
		if ev["key"] != "second" {
			t.Errorf("key = %v, want second", ev["key"])
		}
		if ev["count"] != float64(3) {
			t.Errorf("count = %v, want 3", ev["count"])
		}
		if got := strings.Count(buf.String(), `"key"`); got != 1 {
			t.Errorf("key occurrences = %d, want 1", got)
		}
	})
}

func TestMiddlewareError(t *testing.T) {
	t.Run("given a handler error when healthy traffic is sampled out then the event is kept", func(t *testing.T) {
		var buf bytes.Buffer
		handler := newMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Error(r.Context(), errors.New("boom"))
			http.Error(w, "boom", http.StatusBadGateway)
		}), Config{Logger: newTestLogger(&buf), HealthySampleRate: 0.1}, func() float64 { return 0.9 })

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/fail", http.NoBody))

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		ev := events[0]
		if ev["level"] != "ERROR" {
			t.Errorf("level = %v, want ERROR", ev["level"])
		}
		if ev["outcome"] != "error" {
			t.Errorf("outcome = %v, want error", ev["outcome"])
		}
		if ev["http.status"] != float64(http.StatusBadGateway) {
			t.Errorf("http.status = %v, want %d", ev["http.status"], http.StatusBadGateway)
		}
		errObject, ok := ev["error"].(map[string]any)
		if !ok {
			t.Fatalf("error = %v, want object", ev["error"])
		}
		if errObject["message"] != "boom" {
			t.Errorf("error.message = %v, want boom", errObject["message"])
		}
		if errObject["type"] != "*errors.errorString" {
			t.Errorf("error.type = %v, want *errors.errorString", errObject["type"])
		}
	})
}

func TestMiddlewareClientError(t *testing.T) {
	t.Run("given a 404 when the handler returns then the event is a warning", func(t *testing.T) {
		var buf bytes.Buffer
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}), Config{Logger: newTestLogger(&buf)})

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", http.NoBody))

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		if events[0]["level"] != "WARN" {
			t.Errorf("level = %v, want WARN", events[0]["level"])
		}
		if events[0]["outcome"] != "client_error" {
			t.Errorf("outcome = %v, want client_error", events[0]["outcome"])
		}
	})
}

func TestSampling(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		rand   float64
		status int
		sleep  time.Duration
		path   string
		want   int
	}{
		{
			name:   "given healthy traffic below the sample rate when sampled out then no event is emitted",
			cfg:    Config{HealthySampleRate: 0.1},
			rand:   0.5,
			status: http.StatusOK,
			path:   "/ok",
			want:   0,
		},
		{
			name:   "given healthy traffic above the sample rate when sampled in then one event is emitted",
			cfg:    Config{HealthySampleRate: 0.9},
			rand:   0.5,
			status: http.StatusOK,
			path:   "/ok",
			want:   1,
		},
		{
			name:   "given a server error when sampled out then the event is kept",
			cfg:    Config{HealthySampleRate: 0.1},
			rand:   0.5,
			status: http.StatusInternalServerError,
			path:   "/fail",
			want:   1,
		},
		{
			name:   "given a slow request when sampled out then the event is kept",
			cfg:    Config{HealthySampleRate: 0.1, SlowThreshold: time.Millisecond},
			rand:   0.5,
			status: http.StatusOK,
			sleep:  2 * time.Millisecond,
			path:   "/slow",
			want:   1,
		},
		{
			name:   "given a keep path when sampled out then the event is kept",
			cfg:    Config{HealthySampleRate: 0.1, KeepPaths: []string{"/admin"}},
			rand:   0.5,
			status: http.StatusOK,
			path:   "/admin/users",
			want:   1,
		},
		{
			name:   "given a skip path when sampled out then nothing is logged",
			cfg:    Config{HealthySampleRate: 0.1, SkipPaths: []string{"/healthz"}},
			rand:   0.5,
			status: http.StatusOK,
			path:   "/healthz",
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.cfg.Logger = newTestLogger(&buf)
			handler := newMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.sleep > 0 {
					time.Sleep(tt.sleep)
				}
				w.WriteHeader(tt.status)
			}), tt.cfg, func() float64 { return tt.rand })

			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, tt.path, http.NoBody))

			if got := len(decodeEvents(t, &buf)); got != tt.want {
				t.Fatalf("events = %d, want %d\noutput:\n%s", got, tt.want, buf.String())
			}
		})
	}
}

func TestMiddlewarePanic(t *testing.T) {
	t.Run("given a panic when the handler panics then the event is emitted and the panic is rethrown", func(t *testing.T) {
		var buf bytes.Buffer
		handler := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}), Config{Logger: newTestLogger(&buf)})

		var recovered any
		func() {
			defer func() { recovered = recover() }()
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panic", http.NoBody))
		}()
		if recovered != "boom" {
			t.Fatalf("recover() = %v, want boom", recovered)
		}

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		ev := events[0]
		if ev["level"] != "ERROR" {
			t.Errorf("level = %v, want ERROR", ev["level"])
		}
		if ev["outcome"] != "error" {
			t.Errorf("outcome = %v, want error", ev["outcome"])
		}
		if ev["http.status"] != float64(http.StatusInternalServerError) {
			t.Errorf("http.status = %v, want %d", ev["http.status"], http.StatusInternalServerError)
		}
		if errObject, ok := ev["error"].(map[string]any); !ok || errObject["message"] != "panic: boom" {
			t.Errorf("error = %v, want panic: boom", ev["error"])
		}
	})
}

func TestSetMessage(t *testing.T) {
	t.Run("given a per-request message when the handler sets it then the event uses it", func(t *testing.T) {
		var buf bytes.Buffer
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			SetMessage(r.Context(), "order_created")
			w.WriteHeader(http.StatusCreated)
		}), Config{Logger: newTestLogger(&buf)})

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/orders", http.NoBody))

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		if events[0]["msg"] != "order_created" {
			t.Errorf("msg = %v, want order_created", events[0]["msg"])
		}
	})
}

func TestAddBadKey(t *testing.T) {
	t.Run("given an odd argument when the handler adds it then it is recorded as !BADKEY", func(t *testing.T) {
		var buf bytes.Buffer
		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			Add(r.Context(), "key")
			w.WriteHeader(http.StatusNoContent)
		}), Config{Logger: newTestLogger(&buf)})

		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", http.NoBody))

		events := decodeEvents(t, &buf)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		if _, ok := events[0]["!BADKEY"]; !ok {
			t.Errorf("event = %v, want !BADKEY", events[0])
		}
	})
}

func TestContextHelpersWithoutEvent(t *testing.T) {
	t.Run("given a context without an event when helpers are called then they are no-ops", func(t *testing.T) {
		ctx := context.Background()
		Add(ctx, "key", "value")
		AddAttrs(ctx, slog.String("key", "value"))
		Error(ctx, errors.New("boom"))
		SetMessage(ctx, "message")
	})
}

type captureHandler struct {
	slog.Handler
	spanContext trace.SpanContext
}

func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	h.spanContext = trace.SpanContextFromContext(ctx)
	return h.Handler.Handle(ctx, record)
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{Handler: h.Handler.WithGroup(name)}
}

func TestMiddlewarePassesTraceContext(t *testing.T) {
	t.Run("given a span in the request context when the event is emitted then the logger receives the span", func(t *testing.T) {
		var buf bytes.Buffer
		capture := &captureHandler{Handler: slog.NewJSONHandler(&buf, nil)}
		want := trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID{1},
			SpanID:     trace.SpanID{2},
			TraceFlags: trace.FlagsSampled,
		})
		ctx := trace.ContextWithSpanContext(context.Background(), want)
		req := httptest.NewRequest(http.MethodGet, "/trace", http.NoBody).WithContext(ctx)

		handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), Config{Logger: slog.New(capture)})
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if capture.spanContext.TraceID() != want.TraceID() {
			t.Errorf("trace ID = %s, want %s", capture.spanContext.TraceID(), want.TraceID())
		}
	})
}
