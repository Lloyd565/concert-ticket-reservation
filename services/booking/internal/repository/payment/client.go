// Package payment adapts the Payment service's gRPC contract to the
// usecase.PaymentClient port. It is an outbound adapter: it imports domain and
// the generated stubs, and never imports usecase (AGENTS.md §4).
//
// Everything on the resilience checklist for an outbound call is applied here,
// in one place: a deadline, a circuit breaker, and a propagated correlation ID.
// Retries are deliberately absent - see Charge.
package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	paymentv1 "github.com/lloyd565/concert-ticket-reservation/proto/payment/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/breaker"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/logging"
)

// Client calls the Payment service.
type Client struct {
	rpc     paymentv1.PaymentServiceClient
	breaker *breaker.Breaker
	timeout time.Duration
}

// Dial opens a lazy connection to Payment.
//
// Lazy on purpose: Booking must start and serve seat maps and holds whether or
// not Payment is up (ARCHITECTURE.md §3.3). Only the pay step needs it, and
// that step has a defined behaviour when it is unreachable.
//
// Credentials are insecure because this hop never leaves the compose network.
// It becomes mTLS the moment it does.
func Dial(addr string, timeout time.Duration, br *breaker.Breaker) (*Client, *grpc.ClientConn, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(correlationInterceptor()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial payment at %s: %w", addr, err)
	}
	return NewClient(paymentv1.NewPaymentServiceClient(conn), timeout, br), conn, nil
}

// NewClient wraps an existing gRPC client. Exported so tests can drive the
// adapter against an in-process server.
func NewClient(rpc paymentv1.PaymentServiceClient, timeout time.Duration, br *breaker.Breaker) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if br == nil {
		br = breaker.New(0, 0)
	}
	return &Client{rpc: rpc, breaker: br, timeout: timeout}
}

// Charge asks Payment to take money.
//
// There is no retry here, and that is a decision rather than an omission. The
// safe place to retry a charge is inside Payment, where the idempotency key is
// already being passed through to the provider (FR-4.4). Retrying from this
// side would only add attempts that hit the same unreachable service, delay the
// answer the customer is waiting for, and widen the window in which the
// reservation's checkout deadline passes.
//
// An unreachable Payment produces domain.ErrPaymentOutcomeUnknown, never a
// failure. A charge may exist; only reconciliation may decide (D8).
func (c *Client) Charge(ctx context.Context, req ChargeRequest) (domain.PaymentResult, error) {
	var out domain.PaymentResult
	err := c.breaker.Do(func() error {
		callCtx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()

		resp, err := c.rpc.Charge(callCtx, &paymentv1.ChargeRequest{
			ReservationId:  req.ReservationID,
			UserId:         req.UserID,
			AmountCents:    req.AmountCents,
			IdempotencyKey: req.IdempotencyKey,
		})
		if err != nil {
			return err
		}
		out = toResult(resp.GetCharge())
		return nil
	})
	if err != nil {
		return domain.PaymentResult{}, classify(err)
	}
	return out, nil
}

// GetCharge asks what happened to the charge under an idempotency key.
//
// NotFound is not an error here: a key Payment has never seen means no charge
// was ever created, which is the one answer that makes releasing the seats
// safe. Collapsing it into a generic failure would lose exactly the distinction
// reconciliation exists to make.
func (c *Client) GetCharge(ctx context.Context, idempotencyKey string) (domain.PaymentResult, error) {
	var out domain.PaymentResult
	err := c.breaker.Do(func() error {
		callCtx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()

		resp, err := c.rpc.GetCharge(callCtx, &paymentv1.GetChargeRequest{IdempotencyKey: idempotencyKey})
		if status.Code(err) == codes.NotFound {
			out = domain.PaymentResult{Status: domain.PaymentNoCharge}
			return nil
		}
		if err != nil {
			return err
		}
		out = toResult(resp.GetCharge())
		return nil
	})
	if err != nil {
		return domain.PaymentResult{}, classify(err)
	}
	return out, nil
}

// Refund gives money back against a charge.
func (c *Client) Refund(ctx context.Context, chargeID string, amountCents int64, idempotencyKey string) error {
	err := c.breaker.Do(func() error {
		callCtx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()

		_, err := c.rpc.Refund(callCtx, &paymentv1.RefundRequest{
			ChargeId:       chargeID,
			AmountCents:    amountCents,
			IdempotencyKey: idempotencyKey,
		})
		return err
	})
	if err != nil {
		return classify(err)
	}
	return nil
}

// ChargeRequest mirrors usecase.ChargeRequest. It is redeclared here rather
// than imported because an adapter may not import usecase (AGENTS.md §4); the
// wiring in main converts between the two.
type ChargeRequest struct {
	ReservationID  string
	UserID         string
	AmountCents    int64
	IdempotencyKey string
}

// classify turns a transport error into the domain's vocabulary.
//
// This is the most consequential function in the package. Every failure that
// leaves the outcome unknown - a deadline, an unreachable service, a tripped
// breaker, a cancelled context - must come out as ErrPaymentOutcomeUnknown, and
// the saga must never be given a reason to believe a charge did not happen when
// it might have (D8). Only a definite answer from Payment produces a value, and
// that path does not come through here.
func classify(err error) error {
	if errors.Is(err, breaker.ErrOpen) {
		// The breaker is open, so this call was never made - but an earlier one
		// under the same key may have been. Unknown, not failed.
		return fmt.Errorf("payment call not attempted: %w: %w", domain.ErrPaymentOutcomeUnknown, err)
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.FailedPrecondition:
		// Payment rejected the request itself. Nothing was charged and nothing
		// will be: a malformed request is not going to become well-formed on a
		// retry, so this one is safe to report as a definite failure.
		return fmt.Errorf("payment rejected the request: %w: %v", domain.ErrInvalidInput, err)
	default:
		// Everything else - Unavailable, DeadlineExceeded, Internal, Unknown -
		// leaves the outcome in doubt. Fail towards "we do not know".
		return fmt.Errorf("payment call did not complete: %w: %v", domain.ErrPaymentOutcomeUnknown, err)
	}
}

func toResult(c *paymentv1.Charge) domain.PaymentResult {
	return domain.PaymentResult{
		ChargeID:      c.GetId(),
		Status:        fromProtoStatus(c.GetStatus()),
		AmountCents:   c.GetAmountCents(),
		DeclineReason: c.GetDeclineReason(),
	}
}

func fromProtoStatus(s paymentv1.ChargeStatus) domain.PaymentStatus {
	switch s {
	case paymentv1.ChargeStatus_CHARGE_STATUS_SUCCEEDED:
		return domain.PaymentSucceeded
	case paymentv1.ChargeStatus_CHARGE_STATUS_DECLINED:
		return domain.PaymentDeclined
	case paymentv1.ChargeStatus_CHARGE_STATUS_FAILED:
		return domain.PaymentFailed
	default:
		// PENDING and UNSPECIFIED both mean "no settled answer". Treating an
		// unrecognised status as pending keeps a future enum value from being
		// silently read as a decline, which would release seats.
		return domain.PaymentPending
	}
}

// correlationInterceptor copies the request's correlation ID into outgoing
// metadata, so one request produces one traceable ID across every service it
// touches (NFR-4.1).
func correlationInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if id := logging.CorrelationID(ctx); id != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, logging.MetadataKey, id)
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
