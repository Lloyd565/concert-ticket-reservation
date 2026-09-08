// Package grpc exposes the Booking service over gRPC, which is how the gateway
// reaches it (ARCHITECTURE.md §4: gRPC internally, REST at the edge).
//
// This is a second door onto the same rooms. Both this package and the REST
// handler call the identical usecase methods; no seat-locking logic is
// duplicated, reimplemented or reached around here. Adding a transport must not
// be able to change what a hold does.
package grpc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	bookingv1 "github.com/lloyd565/concert-ticket-reservation/proto/booking/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/usecase"
)

// Server implements bookingv1.BookingServiceServer.
type Server struct {
	bookingv1.UnimplementedBookingServiceServer
	holder *usecase.Holder
	seeder *usecase.Seeder
	log    *slog.Logger
}

// NewServer wires a Server.
func NewServer(holder *usecase.Holder, seeder *usecase.Seeder, log *slog.Logger) *Server {
	return &Server{holder: holder, seeder: seeder, log: log}
}

// SeedEvent creates a demo event and its seat map (NFR-6.2).
func (s *Server) SeedEvent(ctx context.Context, req *bookingv1.SeedEventRequest) (*bookingv1.SeedEventResponse, error) {
	ev, seats, err := s.seeder.Seed(ctx, usecase.SeedSpec{
		Name:        req.GetName(),
		StartsAt:    req.GetStartsAt().AsTime(),
		Sections:    req.GetSections(),
		Rows:        int(req.GetRows()),
		SeatsPerRow: int(req.GetSeatsPerRow()),
	})
	if err != nil {
		return nil, s.fail(ctx, "seed_event", err)
	}
	out := make([]*bookingv1.Seat, 0, len(seats))
	for _, seat := range seats {
		out = append(out, &bookingv1.Seat{
			Id:      seat.ID,
			Section: seat.Section,
			Row:     seat.Row,
			Number:  seat.Number,
			Status:  string(seat.Status),
		})
	}
	return &bookingv1.SeedEventResponse{
		EventId:  ev.ID,
		Name:     ev.Name,
		StartsAt: timestamppb.New(ev.StartsAt),
		Seats:    out,
	}, nil
}

// HoldSeats claims seats for a user.
//
// user_id comes from the request message rather than from anything this service
// reads off the wire: the gateway has already validated the access token and
// overwritten the field with the token's subject. Booking is not the component
// that decides who the caller is.
func (s *Server) HoldSeats(ctx context.Context, req *bookingv1.HoldSeatsRequest) (*bookingv1.HoldSeatsResponse, error) {
	if req.GetIdempotencyKey() == "" {
		// Same rule as the REST door (AGENTS.md §2 rule 10): a mutating call
		// without a key has no safe retry.
		return nil, status.Error(codes.InvalidArgument, "idempotency key is required")
	}
	reservationID, expiresAt, err := s.holder.HoldSeats(ctx, req.GetEventId(), req.GetSeatIds(), req.GetUserId(), req.GetIdempotencyKey())
	if err != nil {
		return nil, s.fail(ctx, "hold_seats", err)
	}
	return &bookingv1.HoldSeatsResponse{
		ReservationId: reservationID,
		ExpiresAt:     timestamppb.New(expiresAt.UTC()),
	}, nil
}

// fail maps a domain error to a gRPC status, mirroring the REST handler's
// mapping so that the two doors cannot disagree about what a lost race means.
// Anything unrecognised is logged and reported as Internal: SQL errors never
// reach a caller (AGENTS.md §5).
func (s *Server) fail(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, domain.ErrBackstopTripped):
		// Correct answer, alarming cause: the database had to stop a double
		// claim the locking path should already have prevented.
		s.log.ErrorContext(ctx, "D13 backstop tripped - the seat locking path let a double claim through", "error", err)
		return status.Error(codes.Aborted, "one or more seats are no longer available")
	case errors.Is(err, domain.ErrSeatUnavailable):
		// A lost race is an expected outcome, not a server fault (FR-3.2).
		// Aborted is the gRPC code the gateway maps back to 409.
		return status.Error(codes.Aborted, "one or more seats are no longer available")
	case errors.Is(err, domain.ErrSeatNotFound):
		return status.Error(codes.NotFound, "one or more seats do not exist for this event")
	case errors.Is(err, domain.ErrDuplicateRequest):
		return status.Error(codes.Aborted, "an identical request is already in flight")
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrNoSeatsRequested), errors.Is(err, domain.ErrInvalidTransition):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		s.log.ErrorContext(ctx, "booking request failed", "op", op, "error", err)
		return status.Error(codes.Internal, "the request could not be completed")
	}
}
