package otelkit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "given no service name when validating then it fails",
			cfg:     Config{},
			wantErr: "ServiceName is required",
		},
		{
			name:    "given an unknown exporter when validating then it fails",
			cfg:     Config{ServiceName: "svc", Exporter: "jaeger"},
			wantErr: "unknown exporter",
		},
		{
			name:    "given a negative timeout when validating then it fails",
			cfg:     Config{ServiceName: "svc", Exporter: ExporterNone, Timeout: -time.Second},
			wantErr: "Timeout must not be negative",
		},
		{
			name:    "given an endpoint without a scheme when validating then it fails",
			cfg:     Config{ServiceName: "svc", Exporter: ExporterOTLP, Endpoint: "localhost:4318"},
			wantErr: "http or https",
		},
		{
			name:    "given an endpoint without a host when validating then it fails",
			cfg:     Config{ServiceName: "svc", Exporter: ExporterOTLP, Endpoint: "http:///v1/traces"},
			wantErr: "include a host",
		},
		{
			name: "given a complete config when validating then it succeeds",
			cfg: Config{
				ServiceName: "svc",
				Exporter:    ExporterOTLP,
				Endpoint:    "http://localhost:4318",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestConfigWithDefaults(t *testing.T) {
	t.Run("given an empty config when applying defaults then OTLP, 10s and stdout are used", func(t *testing.T) {
		cfg := Config{}.withDefaults()
		if cfg.Exporter != ExporterOTLP {
			t.Errorf("Exporter = %q, want %q", cfg.Exporter, ExporterOTLP)
		}
		if cfg.Timeout != 10*time.Second {
			t.Errorf("Timeout = %v, want 10s", cfg.Timeout)
		}
		if cfg.Writer == nil {
			t.Error("Writer = nil, want os.Stdout")
		}
	})

	t.Run("given explicit values when applying defaults then they are preserved", func(t *testing.T) {
		var buf bytes.Buffer
		cfg := Config{Exporter: ExporterNone, Timeout: time.Second, Writer: &buf}.withDefaults()
		if cfg.Exporter != ExporterNone || cfg.Timeout != time.Second || cfg.Writer != &buf {
			t.Fatalf("withDefaults() changed explicit values: %+v", cfg)
		}
	})
}

func TestSignalEndpoint(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		signal string
		want   string
	}{
		{
			name:   "given a collector base URL when building a trace endpoint then /v1/traces is appended",
			base:   "http://localhost:4318",
			signal: "traces",
			want:   "http://localhost:4318/v1/traces",
		},
		{
			name:   "given an OpenObserve base path when building a log endpoint then /v1/logs is appended",
			base:   "http://openobserve:5080/api/default/",
			signal: "logs",
			want:   "http://openobserve:5080/api/default/v1/logs",
		},
		{
			name:   "given a base URL with a path when building a metric endpoint then the path is preserved",
			base:   "https://example.com/otel",
			signal: "metrics",
			want:   "https://example.com/otel/v1/metrics",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := signalEndpoint(tt.base, tt.signal)
			if err != nil {
				t.Fatalf("signalEndpoint() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("signalEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}
