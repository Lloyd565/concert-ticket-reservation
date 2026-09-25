// Package http exposes the Booking service over REST. It depends on usecase and
// domain only - never on repository (AGENTS.md §4) - and it is the boundary
// where domain errors become status codes and where infrastructure errors stop.
//
// Routing uses net/http's method-and-path patterns (Go 1.22+). chi buys nothing
// at this size; introduce it when middleware ordering actually needs it.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/usecase"
)

// Handler serves the Booking REST API.
type Handler struct {
	holder *usecase.Holder
	seeder *usecase.Seeder
	saga   *usecase.Saga
	log    *slog.Logger
}

// NewHandler wires a Handler.
func NewHandler(holder *usecase.Holder, seeder *usecase.Seeder, saga *usecase.Saga, log *slog.Logger) *Handler {
	return &Handler{holder: holder, seeder: seeder, saga: saga, log: log}
}

// Routes returns the service's mux, including liveness and readiness (NFR-4.4).
func (h *Handler) Routes(ready func() error) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		if err := ready(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /events/seed", h.seedEvent)
	mux.HandleFunc("POST /reservations", h.holdSeats)
	mux.HandleFunc("POST /reservations/{id}/pay", h.payReservation)
	return mux
}

type seedRequest struct {
	Name        string    `json:"name"`
	StartsAt    time.Time `json:"starts_at"`
	Sections    []string  `json:"sections"`
	Rows        int       `json:"rows"`
	SeatsPerRow int       `json:"seats_per_row"`
}

type seatView struct {
	ID      string `json:"id"`
	Section string `json:"section"`
	Row     string `json:"row"`
	Number  string `json:"number"`
	Status  string `json:"status"`
}

// seedEvent creates a demo event and seat map so the service is demoable
// without an organizer flow (NFR-6.2).
func (h *Handler) seedEvent(w http.ResponseWriter, r *http.Request) {
	var req seedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}
	ev, seats, err := h.seeder.Seed(r.Context(), usecase.SeedSpec{
		Name:        req.Name,
		StartsAt:    req.StartsAt,
		Sections:    req.Sections,
		Rows:        req.Rows,
		SeatsPerRow: req.SeatsPerRow,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views := make([]seatView, 0, len(seats))
	for _, s := range seats {
		views = append(views, seatView{ID: s.ID, Section: s.Section, Row: s.Row, Number: s.Number, Status: string(s.Status)})
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"event_id":  ev.ID,
		"name":      ev.Name,
		"starts_at": ev.StartsAt,
		"seats":     views,
	})
}

type holdRequest struct {
	EventID string   `json:"event_id"`
	UserID  string   `json:"user_id"`
	SeatIDs []string `json:"seat_ids"`
}

// holdSeats claims seats for a user. Requires an Idempotency-Key header: a
// mutating endpoint without one has no safe retry (AGENTS.md §2 rule 10).
func (h *Handler) holdSeats(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "the Idempotency-Key header is required")
		return
	}
	var req holdRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}

	held, err := h.holder.HoldSeats(r.Context(), req.EventID, req.SeatIDs, req.UserID, key)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The expiry is explicit in the response so the client can show the
	// checkout countdown without guessing (FR-3.3), and the total so it knows
	// what it is about to be charged.
	writeJSON(w, http.StatusCreated, map[string]any{
		"reservation_id": held.ReservationID,
		"expires_at":     held.ExpiresAt.UTC().Format(time.RFC3339),
		"total_cents":    held.TotalCents,
	})
}

type payRequest struct {
	UserID string `json:"user_id"`
}

// payReservation drives the saga (PRD §4.1 step 5). Requires an
// Idempotency-Key header like every other mutating endpoint (AGENTS.md §2 rule
// 10); the charge itself is made idempotent by the reservation ID, which is a
// stronger scope than any key the client can choose - see
// usecase.ChargeIdempotencyKey.
func (h *Handler) payReservation(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Idempotency-Key") == "" {
		writeError(w, http.StatusBadRequest, "idempotency_key_required", "the Idempotency-Key header is required")
		return
	}
	var req payRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return
	}

	conf, err := h.saga.Pay(r.Context(), r.PathValue("id"), req.UserID, r.Header.Get("Idempotency-Key"))
	if errors.Is(err, domain.ErrPaymentDeclined) {
		// A settled answer, not a server fault. The seats are already back on
		// sale by the time this is written.
		writeJSON(w, http.StatusPaymentRequired, map[string]any{
			"reservation_id": conf.ReservationID,
			"status":         string(conf.Status),
			"error":          "payment_declined",
			"message":        conf.DeclineReason,
		})
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}

	tickets := make([]map[string]string, 0, len(conf.Tickets))
	for _, t := range conf.Tickets {
		tickets = append(tickets, map[string]string{"id": t.ID, "seat_id": t.SeatID, "qr_code": t.QRCode})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reservation_id": conf.ReservationID,
		"status":         string(conf.Status),
		"booking_id":     conf.Booking.ID,
		"tickets":        tickets,
	})
}

// fail maps a domain error to a status code. Anything unrecognised is logged
// and reported as a generic 500: SQL errors never reach a client (AGENTS.md §5).
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrBackstopTripped):
		// Correct answer, alarming cause: the database had to stop a double
		// claim the locking path should already have prevented. Logged at error
		// level even though the client sees an ordinary conflict.
		h.log.ErrorContext(r.Context(), "D13 backstop tripped - the seat locking path let a double claim through", "error", err)
		writeError(w, http.StatusConflict, "seat_unavailable", "one or more seats are no longer available")
	case errors.Is(err, domain.ErrHoldsUnavailable):
		// 503, not 409, and the difference matters to the caller: 409 says the
		// seat is gone, this says we cannot tell and will not guess (D12). The
		// seats may be perfectly free; what is missing is the lock that hands
		// one of them to exactly one person, so the only safe answer is to
		// refuse and let them retry.
		h.log.ErrorContext(r.Context(), "hold store unreachable; refusing holds rather than taking them unlocked", "error", err)
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "holds_unavailable", "seat holds are temporarily unavailable; please retry")
	case errors.Is(err, domain.ErrSeatUnavailable):
		// A lost race is an expected outcome, not a server fault (FR-3.2).
		writeError(w, http.StatusConflict, "seat_unavailable", "one or more seats are no longer available")
	case errors.Is(err, domain.ErrSeatNotFound):
		writeError(w, http.StatusNotFound, "seat_not_found", "one or more seats do not exist for this event")
	case errors.Is(err, domain.ErrDuplicateRequest):
		writeError(w, http.StatusConflict, "duplicate_request", "an identical request is already in flight")
	case errors.Is(err, domain.ErrPaymentOutcomeUnknown):
		// 202, not 5xx. The request was accepted, the seats are still held, and
		// reconciliation will settle it either way - reporting a failure here
		// would tell the customer their payment did not happen when it may
		// well have (D8).
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":  "pending",
			"message": "the payment outcome is not yet known; this reservation is being reconciled",
		})
	case errors.Is(err, domain.ErrNotReservationOwner):
		writeError(w, http.StatusForbidden, "forbidden", "this reservation belongs to another user")
	case errors.Is(err, domain.ErrReservationNotPayable):
		writeError(w, http.StatusConflict, "reservation_not_payable", "this reservation can no longer be paid for")
	case errors.Is(err, domain.ErrReservationNotFound):
		writeError(w, http.StatusNotFound, "reservation_not_found", "reservation not found")
	case errors.Is(err, domain.ErrConfirmUnrecoverable):
		writeError(w, http.StatusConflict, "confirm_failed", "the reservation could not be confirmed")
	case errors.Is(err, domain.ErrInvalidInput), errors.Is(err, domain.ErrNoSeatsRequested), errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		h.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}
