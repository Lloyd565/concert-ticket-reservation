// Package grpc exposes the Payment service over gRPC, which is how Booking
// reaches it (ARCHITECTURE.md §4.1: the one synchronous service-to-service
// hop). It depends on usecase and domain only - never on repository or on the
// provider adapter (AGENTS.md §4) - and it is the boundary where domain errors
// become status codes and where SQL and provider errors stop.
package grpc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentv1 "github.com/lloyd565/concert-ticket-reservation/proto/payment/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/usecase"
)

// Server implements paymentv1.PaymentServiceServer.
type Server struct {
	paymentv1.UnimplementedPaymentServiceServer
	charger  *usecase.Charger
	refunder *usecase.Refunder
	log      *slog.Logger
}

// NewServer wires a Server.
func NewServer(charger *usecase.Charger, refunder *usecase.Refunder, log *slog.Logger) *Server {
	return &Server{charger: charger, refunder: refunder, log: log}
}

// Charge takes money for a reservation, at most once per idempotency key.
func (s *Server) Charge(ctx context.Context, req *paymentv1.ChargeRequest) (*paymentv1.ChargeResponse, error) {
	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency key is required")
	}
	ch, err := s.charger.Charge(ctx, usecase.ChargeInput{
		ReservationID:  req.GetReservationId(),
		UserID:         req.GetUserId(),
		AmountCents:    req.GetAmountCents(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, s.fail(ctx, "charge", err)
	}
	return &paymentv1.ChargeResponse{Charge: toProtoCharge(ch)}, nil
}

// GetCharge reports what happened to the charge under an idempotency key.
//
// A declined charge is a successful answer to this question, not an error: the
// caller is asking what happened, and "it was declined" is what happened.
func (s *Server) GetCharge(ctx context.Context, req *paymentv1.GetChargeRequest) (*paymentv1.GetChargeResponse, error) {
	ch, err := s.charger.GetCharge(ctx, req.GetIdempotencyKey())
	if err != nil {
		return nil, s.fail(ctx, "get_charge", err)
	}
	return &paymentv1.GetChargeResponse{Charge: toProtoCharge(ch)}, nil
}

// Refund gives money back against a succeeded charge, at most once per key.
func (s *Server) Refund(ctx context.Context, req *paymentv1.RefundRequest) (*paymentv1.RefundResponse, error) {
	if req.GetIdempotencyKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency key is required")
	}
	rf, err := s.refunder.Refund(ctx, usecase.RefundInput{
		ChargeID:       req.GetChargeId(),
		AmountCents:    req.GetAmountCents(),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, s.fail(ctx, "refund", err)
	}
	return &paymentv1.RefundResponse{Refund: toProtoRefund(rf)}, nil
}

// fail maps a domain error to a gRPC status. Anything unrecognised is logged
// and reported as Internal: SQL errors never reach a caller (AGENTS.md §5).
func (s *Server) fail(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, domain.ErrProviderUnavailable):
		// The single most important mapping in this file. Unavailable means
		// "the outcome is unknown" - Booking reads it as the D8 case and leaves
		// its reservation pending. Reporting it as any code that reads like a
		// definite failure would invite the caller to release seats for a
		// charge that may have succeeded.
		s.log.WarnContext(ctx, "payment provider did not answer; outcome unknown", "op", op, "error", err)
		return status.Error(codes.Unavailable, "the payment outcome is unknown; resolve it by idempotency key")
	case errors.Is(err, domain.ErrChargeNotFound):
		return status.Error(codes.NotFound, "no charge exists for this key")
	case errors.Is(err, domain.ErrRefundNotFound):
		return status.Error(codes.NotFound, "no refund exists for this key")
	case errors.Is(err, domain.ErrChargeNotRefundable):
		return status.Error(codes.FailedPrecondition, "this charge took no money and cannot be refunded")
	case errors.Is(err, domain.ErrAmountMismatch), errors.Is(err, domain.ErrRefundExceedsCharge):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrInvalidTransition):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		s.log.ErrorContext(ctx, "payment request failed", "op", op, "error", err)
		return status.Error(codes.Internal, "the request could not be completed")
	}
}

func toProtoCharge(c domain.Charge) *paymentv1.Charge {
	return &paymentv1.Charge{
		Id:             c.ID,
		ReservationId:  c.ReservationID,
		UserId:         c.UserID,
		AmountCents:    c.AmountCents,
		Status:         chargeStatusToProto(c.Status),
		ProviderRef:    c.ProviderRef,
		IdempotencyKey: c.IdempotencyKey,
		DeclineReason:  c.DeclineReason,
	}
}

func toProtoRefund(r domain.Refund) *paymentv1.Refund {
	return &paymentv1.Refund{
		Id:             r.ID,
		ChargeId:       r.ChargeID,
		AmountCents:    r.AmountCents,
		Status:         refundStatusToProto(r.Status),
		ProviderRef:    r.ProviderRef,
		IdempotencyKey: r.IdempotencyKey,
	}
}

func chargeStatusToProto(s domain.ChargeStatus) paymentv1.ChargeStatus {
	switch s {
	case domain.ChargePending:
		return paymentv1.ChargeStatus_CHARGE_STATUS_PENDING
	case domain.ChargeSucceeded:
		return paymentv1.ChargeStatus_CHARGE_STATUS_SUCCEEDED
	case domain.ChargeDeclined:
		return paymentv1.ChargeStatus_CHARGE_STATUS_DECLINED
	case domain.ChargeFailed:
		return paymentv1.ChargeStatus_CHARGE_STATUS_FAILED
	default:
		return paymentv1.ChargeStatus_CHARGE_STATUS_UNSPECIFIED
	}
}

func refundStatusToProto(s domain.RefundStatus) paymentv1.RefundStatus {
	switch s {
	case domain.RefundPending:
		return paymentv1.RefundStatus_REFUND_STATUS_PENDING
	case domain.RefundSucceeded:
		return paymentv1.RefundStatus_REFUND_STATUS_SUCCEEDED
	case domain.RefundFailed:
		return paymentv1.RefundStatus_REFUND_STATUS_FAILED
	default:
		return paymentv1.RefundStatus_REFUND_STATUS_UNSPECIFIED
	}
}
