package otelkit

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTPHandler wraps next with automatic server spans and HTTP metrics.
func HTTPHandler(next http.Handler, operation string, opts ...otelhttp.Option) http.Handler {
	return otelhttp.NewHandler(next, operation, opts...)
}

// HTTPClient returns a client whose requests create client spans and inject
// the W3C trace context into outgoing headers.
func HTTPClient(opts ...otelhttp.Option) *http.Client {
	return &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport, opts...)}
}
