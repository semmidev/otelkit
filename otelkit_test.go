package otelkit

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

func restoreGlobals(t *testing.T) {
	t.Helper()
	oldTP := otel.GetTracerProvider()
	oldMP := otel.GetMeterProvider()
	oldLP := otel.GetLoggerProvider()
	oldLogger := slog.Default()
	t.Cleanup(func() {
		otel.SetTracerProvider(oldTP)
		otel.SetMeterProvider(oldMP)
		otel.SetLoggerProvider(oldLP)
		slog.SetDefault(oldLogger)
	})
}

func TestSetupNone(t *testing.T) {
	restoreGlobals(t)

	var buf bytes.Buffer
	tel, err := Setup(context.Background(), Config{
		ServiceName: "test-service",
		Exporter:    ExporterNone,
		Writer:      &buf,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if tel.TracerProvider == nil || tel.MeterProvider == nil || tel.LoggerProvider == nil {
		t.Fatal("Setup() returned nil providers")
	}

	t.Run("given the none exporter when logging then JSON goes to the writer", func(t *testing.T) {
		slog.Info("hello-none", "answer", 42)
		if got := buf.String(); !strings.Contains(got, `"msg":"hello-none"`) {
			t.Fatalf("log output = %q, want JSON message", got)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tel.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
}

func TestSetupStdout(t *testing.T) {
	restoreGlobals(t)

	var buf bytes.Buffer
	tel, err := Setup(context.Background(), Config{
		ServiceName: "test-service",
		Exporter:    ExporterStdout,
		Writer:      &buf,
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	ctx := context.Background()
	_, span := tel.TracerProvider.Tracer("test").Start(ctx, "hello-span")
	span.End()

	counter, err := tel.MeterProvider.Meter("test").Int64Counter("requests.total")
	if err != nil {
		t.Fatalf("Int64Counter() error = %v", err)
	}
	counter.Add(ctx, 1)

	slog.Info("hello-log", "answer", 42)

	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := tel.ForceFlush(flushCtx); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}
	if err := tel.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	out := buf.String()
	for _, want := range []string{"test-service", "hello-span", "requests.total", "hello-log"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestNewResource(t *testing.T) {
	t.Run("given service metadata when building a resource then attributes are set", func(t *testing.T) {
		res, err := newResource(Config{
			ServiceName:    "checkout",
			ServiceVersion: "1.2.3",
			Environment:    "test",
		})
		if err != nil {
			t.Fatalf("newResource() error = %v", err)
		}

		attrs := map[string]string{}
		for _, kv := range res.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		if attrs["service.name"] != "checkout" {
			t.Errorf("service.name = %q, want checkout", attrs["service.name"])
		}
		if attrs["service.version"] != "1.2.3" {
			t.Errorf("service.version = %q, want 1.2.3", attrs["service.version"])
		}
		if attrs["deployment.environment.name"] != "test" {
			t.Errorf("deployment.environment.name = %q, want test", attrs["deployment.environment.name"])
		}
	})
}
