// Package upstream dials the internal gRPC services and applies the per-call
// concerns every outbound hop needs: a deadline and a propagated correlation ID
// (ARCHITECTURE.md §6, resilience checklist).
//
// Retries and circuit breakers are the other two items on that checklist and
// are deliberately absent here: they arrive in P5 with the rest of the
// resilience work, and a retry added before Booking's idempotency story is
// complete would be a way to double-book, not a way to be resilient.
package upstream

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/middleware"
)

// CorrelationMetadataKey must match what the services read. gRPC lowercases
// metadata keys, so this stays lowercase.
const CorrelationMetadataKey = "x-correlation-id"

// Dial opens a lazy connection to an internal service.
//
// It does not block on the target being reachable, and that is the point: the
// gateway must start and serve whether or not Auth and Booking are up yet, in
// any order (ARCHITECTURE.md §3.3). Calls made while a target is down fail with
// Unavailable, which the edge turns into a 503 for that route only.
//
// Credentials are insecure because this hop never leaves the compose network.
// It becomes mTLS the moment it does.
//
// Calls are balanced round-robin across every address the name resolves to.
// Booking runs as several replicas behind one DNS name (grpc.NewClient resolves
// a bare host:port through DNS), and gRPC's default policy, pick_first, would
// pin every call to whichever replica answered first: the others would run and
// take no traffic, with nothing visibly wrong.
//
// ponytail: DNS is re-resolved only when a connection fails, so a replica added
// with --scale after the gateway started gets traffic once a connection drops or
// the gateway restarts. Real service discovery when replicas come and go live.
func Dial(addr string, timeout time.Duration) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"round_robin": {}}]}`),
		grpc.WithChainUnaryInterceptor(correlationInterceptor(), timeoutInterceptor(timeout)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// correlationInterceptor copies the request's correlation ID into outgoing
// metadata, so one request produces one traceable ID across every service it
// touches.
func correlationInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if id := middleware.CorrelationID(ctx); id != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, CorrelationMetadataKey, id)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// timeoutInterceptor bounds every outbound call. Without it a stalled upstream
// holds a gateway goroutine and the client's connection open indefinitely,
// which is how one slow dependency becomes a gateway-wide outage.
func timeoutInterceptor(timeout time.Duration) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		// An earlier deadline wins: context.WithTimeout never extends one.
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
