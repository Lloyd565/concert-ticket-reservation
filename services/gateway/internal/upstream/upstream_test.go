package upstream_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"

	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/upstream"
)

// TestDialSpreadsCallsAcrossReplicas: the gateway must reach every Booking
// replica its target resolves to. gRPC's default policy pins every call to one
// of them, and nothing visible breaks when that happens - the other replicas
// just sit idle - which is why this is a test and not only a comment.
func TestDialSpreadsCallsAcrossReplicas(t *testing.T) {
	var hits [2]atomic.Int64
	addrs := make([]resolver.Address, 0, len(hits))
	for i := range hits {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		srv := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			hits[i].Add(1)
			return handler(ctx, req)
		}))
		healthpb.RegisterHealthServer(srv, health.NewServer())
		go func() { _ = srv.Serve(lis) }()
		t.Cleanup(srv.Stop)
		addrs = append(addrs, resolver.Address{Addr: lis.Addr().String()})
	}

	// Stands in for Docker's DNS answering "booking" with one address per
	// replica.
	r := manual.NewBuilderWithScheme("replicas")
	r.InitialState(resolver.State{Addresses: addrs})
	resolver.Register(r)

	conn, err := upstream.Dial(r.Scheme()+":///booking", time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := healthpb.NewHealthClient(conn)
	deadline := time.Now().Add(5 * time.Second)
	for hits[0].Load() == 0 || hits[1].Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("calls never reached both replicas: %d and %d", hits[0].Load(), hits[1].Load())
		}
		if _, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatalf("health check: %v", err)
		}
	}
}
