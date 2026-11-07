// Package otelkit wires OpenTelemetry providers, exporters, propagation and
// logging with a small, backend-agnostic API.
//
// It is a bootstrap kit, not a replacement for the OpenTelemetry API. Use
// otel.Tracer, otel.Meter and slog directly in application code.
package otelkit

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

// Exporter selects where telemetry is sent.
type Exporter string

const (
	// ExporterOTLP sends traces, metrics and logs over OTLP/HTTP.
	// Endpoint can point at an OpenTelemetry Collector or directly at
	// OpenObserve (for example http://openobserve:5080/api/default).
	ExporterOTLP Exporter = "otlp"

	// ExporterStdout writes telemetry to Config.Writer. Useful for local
	// development and tests.
	ExporterStdout Exporter = "stdout"

	// ExporterNone installs no-op providers. Logs still go to Config.Writer
	// as JSON so the application remains observable locally.
	ExporterNone Exporter = "none"
)

// Config configures the telemetry bootstrap.
//
// Endpoint is a base URL. otelkit appends /v1/traces, /v1/metrics and
// /v1/logs for OTLP. Leave it empty to let the standard OTEL_EXPORTER_*
// environment variables drive the exporter.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string

	Exporter Exporter
	Endpoint string
	Headers  map[string]string
	Timeout  time.Duration

	// Writer receives stdout exporters and the JSON log fallback.
	// Defaults to os.Stdout.
	Writer io.Writer
}

func (c Config) withDefaults() Config {
	if c.Exporter == "" {
		c.Exporter = ExporterOTLP
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Writer == nil {
		c.Writer = os.Stdout
	}
	return c
}

func (c Config) validate() error {
	if strings.TrimSpace(c.ServiceName) == "" {
		return errors.New("otelkit: ServiceName is required")
	}
	switch c.Exporter {
	case ExporterOTLP, ExporterStdout, ExporterNone:
	default:
		return fmt.Errorf("otelkit: unknown exporter %q (want %q, %q or %q)",
			c.Exporter, ExporterOTLP, ExporterStdout, ExporterNone)
	}
	if c.Timeout < 0 {
		return errors.New("otelkit: Timeout must not be negative")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return fmt.Errorf("otelkit: invalid Endpoint: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("otelkit: Endpoint must use http or https, got %q", u.Scheme)
		}
		if u.Host == "" {
			return errors.New("otelkit: Endpoint must include a host")
		}
	}
	return nil
}
