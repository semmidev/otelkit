package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/semmidev/otelkit"
	"github.com/semmidev/otelkit/logging"
	"github.com/semmidev/otelkit/openobserve"
)

// This example simulates three microservices in one process. Each service
// listens on its own port and talks to the others over HTTP, so trace context
// propagates through the same otelhttp instrumentation used in production.
//
//	gateway :8080 -> orders :8082 -> users :8081
//	gateway :8080 -> users  :8081
func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telemetry, err := otelkit.Setup(ctx, otelkit.Config{
		ServiceName:    env("OTEL_SERVICE_NAME", "otelkit-example"),
		ServiceVersion: "1.0.0",
		Environment:    env("DEPLOYMENT_ENVIRONMENT", "development"),
		Exporter:       otelkit.Exporter(env("OTEL_EXPORTER", string(otelkit.ExporterOTLP))),
		Endpoint:       os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Timeout:        5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("init telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := telemetry.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown telemetry", "error", err)
		}
	}()

	client := otelkit.HTTPClient()
	meter := otel.Meter("example-api")
	requests, _ := meter.Int64Counter("http.requests",
		metric.WithDescription("Requests handled by route"))
	ordersCreated, _ := meter.Int64Counter("orders.created",
		metric.WithDescription("Orders created"))
	orderLatency, _ := meter.Float64Histogram("orders.latency",
		metric.WithDescription("Order processing latency"),
		metric.WithUnit("ms"))

	obs, err := newOpenObserve()
	if err != nil {
		slog.Warn("OpenObserve API disabled", "reason", err)
	}

	usersURL := env("USERS_URL", "http://localhost:8081")
	ordersURL := env("ORDERS_URL", "http://localhost:8082")

	servers := []*server{
		start("gateway", env("GATEWAY_ADDR", ":8080"), gatewayHandler(client, usersURL, ordersURL, obs, requests)),
		start("users", env("USERS_ADDR", ":8081"), usersHandler()),
		start("orders", env("ORDERS_ADDR", ":8082"), ordersHandler(client, usersURL, requests, ordersCreated, orderLatency)),
	}

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	for _, s := range servers {
		if err := s.shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		}
	}
	return errors.Join(errs...)
}

type server struct {
	name string
	srv  *http.Server
}

func start(name, addr string, handler http.Handler) *server {
	s := &server{
		name: name,
		srv: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	go func() {
		slog.Info("listening", "service", name, "addr", addr)
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server", "service", name, "error", err)
		}
	}()
	return s
}

func (s *server) shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func gatewayHandler(client *http.Client, usersURL, ordersURL string, obs *openobserve.Client, requests metric.Int64Counter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(r.Context(), w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(r.Context(), 1, metric.WithAttributes(
			attribute.String("service", "gateway"),
			attribute.String("route", "/api/users"),
		))
		proxy(w, r, client, http.MethodGet, usersURL+"/users")
	})

	mux.HandleFunc("POST /api/orders", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(r.Context(), 1, metric.WithAttributes(
			attribute.String("service", "gateway"),
			attribute.String("route", "/api/orders"),
		))
		proxy(w, r, client, http.MethodPost, ordersURL+"/orders")
	})

	mux.HandleFunc("POST /api/logs", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if obs == nil {
			writeError(ctx, w, http.StatusServiceUnavailable, "OPENOBSERVE_URL is not configured")
			return
		}

		var row map[string]any
		if err := json.NewDecoder(r.Body).Decode(&row); err != nil {
			logging.Error(ctx, err)
			writeError(ctx, w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		row["_timestamp"] = time.Now().UnixMicro()

		stream := env("OPENOBSERVE_STREAM", "example_logs")
		logging.Add(ctx, "stream", stream, "rows", 1)
		res, err := obs.IngestJSON(ctx, stream, []map[string]any{row})
		if err != nil {
			logging.Error(ctx, err)
			trace.SpanFromContext(ctx).RecordError(err)
			writeError(ctx, w, http.StatusBadGateway, "ingest failed")
			return
		}
		writeJSON(ctx, w, http.StatusOK, res)
	})

	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if obs == nil {
			writeError(ctx, w, http.StatusServiceUnavailable, "OPENOBSERVE_URL is not configured")
			return
		}

		sql := r.URL.Query().Get("sql")
		if sql == "" {
			sql = "SELECT * FROM example_logs ORDER BY _timestamp DESC LIMIT 20"
		}
		logging.Add(ctx, "sql", sql)
		res, err := obs.Search(ctx, sql, openobserve.SearchOptions{Size: 20})
		if err != nil {
			logging.Error(ctx, err)
			trace.SpanFromContext(ctx).RecordError(err)
			writeError(ctx, w, http.StatusBadGateway, "search failed")
			return
		}
		logging.Add(ctx, "hits", res.Total, "took_ms", res.Took)
		writeJSON(ctx, w, http.StatusOK, res)
	})

	// Health checks are noise; keep them out of traces and wide events.
	return otelkit.HTTPHandler(logging.Middleware(mux, logging.Config{
		Fields:    []slog.Attr{slog.String("service", "gateway")},
		SkipPaths: []string{"/healthz"},
	}), "gateway",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }))
}

func usersHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		logging.Add(r.Context(), "users_returned", 2)
		writeJSON(r.Context(), w, http.StatusOK, []user{
			{ID: 1, Name: "Budi"},
			{ID: 2, Name: "Siti"},
		})
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id < 1 {
			logging.Add(r.Context(), "invalid_user_id", r.PathValue("id"))
			writeError(r.Context(), w, http.StatusBadRequest, "invalid user id")
			return
		}
		logging.Add(r.Context(), "user_id", id)
		writeJSON(r.Context(), w, http.StatusOK, user{ID: id, Name: "Budi"})
	})
	return otelkit.HTTPHandler(logging.Middleware(mux, logging.Config{
		Fields: []slog.Attr{slog.String("service", "users")},
	}), "users")
}

func ordersHandler(client *http.Client, usersURL string, requests, created metric.Int64Counter, latency metric.Float64Histogram) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		requests.Add(ctx, 1, metric.WithAttributes(
			attribute.String("service", "orders"),
			attribute.String("route", "POST /orders"),
		))

		start := time.Now()
		u, err := loadUser(ctx, client, usersURL)
		if err != nil {
			logging.SetMessage(ctx, "order_failed")
			logging.Error(ctx, err)
			trace.SpanFromContext(ctx).RecordError(err)
			writeError(ctx, w, http.StatusBadGateway, "users service unavailable")
			return
		}

		time.Sleep(15 * time.Millisecond)
		latencyMs := float64(time.Since(start).Milliseconds())
		latency.Record(ctx, latencyMs, metric.WithAttributes(attribute.String("service", "orders")))
		created.Add(ctx, 1, metric.WithAttributes(attribute.String("service", "orders")))

		logging.Add(ctx, "user_id", u.ID, "order_id", "ord-1", "latency_ms", latencyMs)
		logging.SetMessage(ctx, "order_created")
		writeJSON(ctx, w, http.StatusCreated, map[string]any{
			"order_id":   "ord-1",
			"user_id":    u.ID,
			"status":     "confirmed",
			"latency_ms": latencyMs,
		})
	})
	return otelkit.HTTPHandler(logging.Middleware(mux, logging.Config{
		Fields: []slog.Attr{slog.String("service", "orders")},
	}), "orders")
}

type user struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// loadUser shows a manual span around a downstream call. The client span from
// otelkit.HTTPClient is nested inside it, and the users server span inside that.
func loadUser(ctx context.Context, client *http.Client, usersURL string) (user, error) {
	ctx, span := otel.Tracer("orders").Start(ctx, "load-user")
	defer span.End()

	logging.Add(ctx, "upstream", "users")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usersURL+"/users/1", http.NoBody)
	if err != nil {
		return user{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return user{}, err
	}
	defer resp.Body.Close()

	var u user
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return user{}, err
	}
	return u, nil
}

func proxy(w http.ResponseWriter, r *http.Request, client *http.Client, method, target string) {
	ctx := r.Context()
	logging.Add(ctx, "upstream", target)

	req, err := http.NewRequestWithContext(ctx, method, target, r.Body)
	if err != nil {
		logging.Error(ctx, err)
		writeError(ctx, w, http.StatusInternalServerError, "build upstream request")
		return
	}
	req.Header = r.Header.Clone()

	resp, err := client.Do(req)
	if err != nil {
		logging.Error(ctx, err)
		trace.SpanFromContext(ctx).RecordError(err)
		writeError(ctx, w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer resp.Body.Close()

	logging.Add(ctx, "upstream_status", resp.StatusCode)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		logging.Error(ctx, err)
	}
}

func newOpenObserve() (*openobserve.Client, error) {
	baseURL := os.Getenv("OPENOBSERVE_URL")
	if baseURL == "" {
		return nil, errors.New("OPENOBSERVE_URL is not set")
	}
	return openobserve.New(openobserve.Config{
		BaseURL:  baseURL,
		Org:      env("OPENOBSERVE_ORG", "default"),
		Username: os.Getenv("OPENOBSERVE_USER"),
		Password: os.Getenv("OPENOBSERVE_PASSWORD"),
		Token:    os.Getenv("OPENOBSERVE_TOKEN"),
	})
}

func writeJSON(ctx context.Context, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logging.Error(ctx, err)
	}
}

func writeError(ctx context.Context, w http.ResponseWriter, status int, message string) {
	writeJSON(ctx, w, status, map[string]string{"error": message})
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
