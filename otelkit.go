package otelkit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry owns the configured providers. Call Shutdown when the process
// exits. The exported fields are useful in tests and for advanced wiring.
type Telemetry struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *metric.MeterProvider
	LoggerProvider *log.LoggerProvider

	once        sync.Once
	shutdownErr error
}

// Setup installs OpenTelemetry providers and the slog bridge. It is safe to
// call once per process.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	res, err := newResource(cfg)
	if err != nil {
		return nil, err
	}

	traceExp, err := newTraceExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otelkit: trace exporter: %w", err)
	}
	metricExp, err := newMetricExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otelkit: metric exporter: %w", err)
	}
	logExp, err := newLogExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otelkit: log exporter: %w", err)
	}

	traceOpts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if traceExp != nil {
		traceOpts = append(traceOpts, sdktrace.WithBatcher(traceExp))
	}
	tp := sdktrace.NewTracerProvider(traceOpts...)

	metricOpts := []metric.Option{metric.WithResource(res)}
	if metricExp != nil {
		metricOpts = append(metricOpts, metric.WithReader(metric.NewPeriodicReader(metricExp)))
	}
	mp := metric.NewMeterProvider(metricOpts...)

	logOpts := []log.LoggerProviderOption{log.WithResource(res)}
	if logExp != nil {
		logOpts = append(logOpts, log.WithProcessor(log.NewBatchProcessor(logExp)))
	}
	lp := log.NewLoggerProvider(logOpts...)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if cfg.Exporter == ExporterNone {
		handler := contextHandler{Handler: slog.NewJSONHandler(cfg.Writer, &slog.HandlerOptions{Level: slog.LevelInfo})}
		slog.SetDefault(slog.New(handler))
	} else {
		slog.SetDefault(slog.New(otelslog.NewHandler(cfg.ServiceName,
			otelslog.WithLoggerProvider(lp),
			otelslog.WithSource(true),
		)))
	}

	return &Telemetry{TracerProvider: tp, MeterProvider: mp, LoggerProvider: lp}, nil
}

// contextHandler adds trace identifiers to the JSON fallback so local logs
// match the records exported through the OpenTelemetry bridge.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, record)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{Handler: h.Handler.WithGroup(name)}
}

// Shutdown flushes and shuts down all providers. It is safe to call more than
// once.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	t.once.Do(func() {
		t.shutdownErr = errors.Join(
			t.TracerProvider.Shutdown(ctx),
			t.MeterProvider.Shutdown(ctx),
			t.LoggerProvider.Shutdown(ctx),
		)
	})
	return t.shutdownErr
}

// ForceFlush flushes pending telemetry without shutting the providers down.
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	return errors.Join(
		t.TracerProvider.ForceFlush(ctx),
		t.MeterProvider.ForceFlush(ctx),
		t.LoggerProvider.ForceFlush(ctx),
	)
}

func newResource(cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(cfg.Environment))
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("otelkit: resource: %w", err)
	}
	return res, nil
}

func newTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	switch cfg.Exporter {
	case ExporterStdout:
		return stdouttrace.New(stdouttrace.WithWriter(cfg.Writer))
	case ExporterOTLP:
		opts := []otlptracehttp.Option{otlptracehttp.WithTimeout(cfg.Timeout)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		if cfg.Endpoint != "" {
			endpoint, err := signalEndpoint(cfg.Endpoint, "traces")
			if err != nil {
				return nil, err
			}
			opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
		}
		return otlptracehttp.New(ctx, opts...)
	default:
		return nil, nil
	}
}

func newMetricExporter(ctx context.Context, cfg Config) (metric.Exporter, error) {
	switch cfg.Exporter {
	case ExporterStdout:
		return stdoutmetric.New(stdoutmetric.WithWriter(cfg.Writer))
	case ExporterOTLP:
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithTimeout(cfg.Timeout)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		if cfg.Endpoint != "" {
			endpoint, err := signalEndpoint(cfg.Endpoint, "metrics")
			if err != nil {
				return nil, err
			}
			opts = append(opts, otlpmetrichttp.WithEndpointURL(endpoint))
		}
		return otlpmetrichttp.New(ctx, opts...)
	default:
		return nil, nil
	}
}

func newLogExporter(ctx context.Context, cfg Config) (log.Exporter, error) {
	switch cfg.Exporter {
	case ExporterStdout:
		return stdoutlog.New(stdoutlog.WithWriter(cfg.Writer))
	case ExporterOTLP:
		opts := []otlploghttp.Option{otlploghttp.WithTimeout(cfg.Timeout)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
		}
		if cfg.Endpoint != "" {
			endpoint, err := signalEndpoint(cfg.Endpoint, "logs")
			if err != nil {
				return nil, err
			}
			opts = append(opts, otlploghttp.WithEndpointURL(endpoint))
		}
		return otlploghttp.New(ctx, opts...)
	default:
		return nil, nil
	}
}

func signalEndpoint(base, signal string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("otelkit: parse endpoint: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/" + signal
	return u.String(), nil
}
