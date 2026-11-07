package otelkit

import (
	"context"
	"net"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"
)

func TestGRPCInstrumentation(t *testing.T) {
	exp := withInMemoryTracer(t)

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(GRPCServerOption())
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		GRPCClientOption(),
	)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	t.Run("given a healthy server when checking then client and server spans are recorded", func(t *testing.T) {
		res, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatalf("Check() error = %v", err)
		}
		if res.Status != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("status = %v, want SERVING", res.Status)
		}

		var client, server int
		for _, span := range exp.GetSpans() {
			switch span.SpanKind {
			case trace.SpanKindClient:
				client++
			case trace.SpanKindServer:
				server++
			default:
			}
		}
		if client != 1 || server != 1 {
			t.Fatalf("client spans = %d, server spans = %d, want 1 and 1", client, server)
		}
	})
}
