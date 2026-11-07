package otelkit

import (
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
)

// GRPCServerOption instruments a gRPC server with server spans, metrics and
// trace context extraction.
func GRPCServerOption(opts ...otelgrpc.Option) grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler(opts...))
}

// GRPCClientOption instruments a gRPC client with client spans, metrics and
// trace context injection.
func GRPCClientOption(opts ...otelgrpc.Option) grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler(opts...))
}
