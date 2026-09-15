// Package http is the gateway's edge: REST in, gRPC out (ARCHITECTURE.md §4).
//
// It holds no business logic and no database. Its whole job is to decide
// whether a request may proceed, give it an identity, and hand it to the
// service that owns the data.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/lloyd565/concert-ticket-reservation/proto/auth/v1"
	bookingv1 "github.com/lloyd565/concert-ticket-reservation/proto/booking/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/middleware"
)

// Roles the gateway authorizes on. They mirror auth_db.users.role; the gateway
// reads them out of the token and never asks anybody to confirm them.
const (
	roleAttendee  = "attendee"
	roleOrganizer = "organizer"
	roleAdmin     = "admin"
)

// maxBodyBytes caps a request body. The gateway forwards bodies it has parsed,
// so an unbounded body is an unbounded allocation at the front door.
const maxBodyBytes = 64 << 10

// Handler routes REST requests to the internal gRPC services.
type Handler struct {
	auth    authv1.AuthServiceClient
	booking bookingv1.BookingServiceClient
	authn   *middleware.Authenticator
	limiter *middleware.RateLimiter
	log     *slog.Logger
}

// NewHandler wires a Handler.
func NewHandler(auth authv1.AuthServiceClient, booking bookingv1.BookingServiceClient, authn *middleware.Authenticator, limiter *middleware.RateLimiter, log *slog.Logger) *Handler {
	return &Handler{auth: auth, booking: booking, authn: authn, limiter: limiter, log: log}
}

// Routes returns the gateway's mux wrapped in the edge middleware.
//
// Order matters: correlate first so that everything downstream - including a
// rate-limit rejection - is traceable, then rate limit so that a flood is
// dropped before it costs a signature verification, then authenticate.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// Liveness and readiness (NFR-4.4). Readiness deliberately does NOT probe
	// Auth or Booking: the gateway is ready when it can accept and validate
	// requests, and reporting it unready because Auth is down would take the
	// gateway out of rotation for exactly the outage local validation exists to
	// survive (ARCHITECTURE.md §3.3).
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		middleware.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		middleware.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Public: these are how a caller gets a token in the first place, so
	// requiring one would be circular. Registration takes optional auth because
	// only an admin may create a non-attendee account.
	mux.Handle("POST /v1/auth/register", h.authn.Optional(http.HandlerFunc(h.register)))
	mux.HandleFunc("POST /v1/auth/login", h.login)
	mux.HandleFunc("POST /v1/auth/refresh", h.refresh)
	mux.HandleFunc("POST /v1/auth/logout", h.logout)

	// Authenticated. Seeding an event is an organizer action; holding seats is
	// open to any signed-in role. Each is also limited per user, which can only
	// happen after authentication: that is where the user becomes known.
	mux.Handle("POST /v1/events", h.authn.Require(roleOrganizer, roleAdmin)(h.limiter.PerUser(http.HandlerFunc(h.seedEvent))))
	mux.Handle("POST /v1/reservations", h.authn.Require()(h.limiter.PerUser(http.HandlerFunc(h.holdSeats))))
	mux.Handle("POST /v1/reservations/{id}/pay", h.authn.Require()(h.limiter.PerUser(http.HandlerFunc(h.payReservation))))

	return middleware.Correlate(h.limiter.Middleware(mux))
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// register creates an account.
//
// Role escalation is stopped here rather than in Auth because the gateway is
// the component that knows who the caller is: Auth receives a role field and
// trusts it, so the gateway must never forward one it did not authorize.
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decode(w, r, &req) {
		return
	}
	switch req.Role {
	case "", roleAttendee:
	case roleOrganizer, roleAdmin:
		claims, ok := middleware.ClaimsFrom(r.Context())
		if !ok || claims.Role != roleAdmin {
			middleware.WriteError(w, http.StatusForbidden, "forbidden", "only an admin may register a privileged account")
			return
		}
	default:
		// Rejected rather than quietly downgraded to attendee: a caller that
		// asked for a role we do not recognise should be told, not surprised.
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", "role must be attendee, organizer or admin")
		return
	}
	res, err := h.auth.Register(r.Context(), &authv1.RegisterRequest{
		Email:    req.Email,
		Password: req.Password,
		Role:     roleToProto(req.Role),
	})
	if err != nil {
		h.fail(w, r, "auth", err)
		return
	}
	middleware.WriteJSON(w, http.StatusCreated, map[string]any{
		"user_id": res.GetUserId(),
		"email":   res.GetEmail(),
		"role":    roleFromProto(res.GetRole()),
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decode(w, r, &req) {
		return
	}
	pair, err := h.auth.Login(r.Context(), &authv1.LoginRequest{Email: req.Email, Password: req.Password})
	if err != nil {
		h.fail(w, r, "auth", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, tokenPairBody(pair))
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decode(w, r, &req) {
		return
	}
	// The one operation in the system that genuinely needs Auth to be up: a
	// refresh token is opaque and can only be checked against the table Auth
	// owns. This is a call to Auth by definition, not an extra hop the gateway
	// chose to add.
	pair, err := h.auth.Refresh(r.Context(), &authv1.RefreshRequest{RefreshToken: req.RefreshToken})
	if err != nil {
		h.fail(w, r, "auth", err)
		return
	}
	middleware.WriteJSON(w, http.StatusOK, tokenPairBody(pair))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.auth.Logout(r.Context(), &authv1.LogoutRequest{RefreshToken: req.RefreshToken})
	if err != nil {
		h.fail(w, r, "auth", err)
		return
	}
	// The caller's access token keeps working until it expires - the gateway
	// does not consult a revocation list. revoked=true means the session cannot
	// be renewed, which is what actually ends it.
	middleware.WriteJSON(w, http.StatusOK, map[string]any{"revoked": res.GetRevoked()})
}

type seedRequest struct {
	Name        string    `json:"name"`
	StartsAt    time.Time `json:"starts_at"`
	Sections    []string  `json:"sections"`
	Rows        int32     `json:"rows"`
	SeatsPerRow int32     `json:"seats_per_row"`
}

func (h *Handler) seedEvent(w http.ResponseWriter, r *http.Request) {
	var req seedRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.booking.SeedEvent(r.Context(), &bookingv1.SeedEventRequest{
		Name:        req.Name,
		StartsAt:    timestamppb.New(req.StartsAt),
		Sections:    req.Sections,
		Rows:        req.Rows,
		SeatsPerRow: req.SeatsPerRow,
	})
	if err != nil {
		h.fail(w, r, "booking", err)
		return
	}
	seats := make([]map[string]string, 0, len(res.GetSeats()))
	for _, s := range res.GetSeats() {
		seats = append(seats, map[string]string{
			"id": s.GetId(), "section": s.GetSection(), "row": s.GetRow(),
			"number": s.GetNumber(), "status": s.GetStatus(),
		})
	}
	middleware.WriteJSON(w, http.StatusCreated, map[string]any{
		"event_id":  res.GetEventId(),
		"name":      res.GetName(),
		"starts_at": res.GetStartsAt().AsTime().UTC().Format(time.RFC3339),
		"seats":     seats,
	})
}

type holdRequest struct {
	EventID string   `json:"event_id"`
	SeatIDs []string `json:"seat_ids"`
}

// holdSeats forwards a hold to Booking.
//
// Note what is absent: no seat state is read, cached or decided here. The
// gateway establishes who the caller is and gets out of the way; the claim
// itself happens in one transaction inside Booking, which is the only place it
// can be correct (D1).
func (h *Handler) holdSeats(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		middleware.WriteError(w, http.StatusBadRequest, "idempotency_key_required", "the Idempotency-Key header is required")
		return
	}
	var req holdRequest
	if !decode(w, r, &req) {
		return
	}
	// The subject comes from the validated token, never from the body: a caller
	// must not be able to hold seats in someone else's name.
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		middleware.WriteError(w, http.StatusUnauthorized, "unauthenticated", "a bearer access token is required")
		return
	}
	res, err := h.booking.HoldSeats(r.Context(), &bookingv1.HoldSeatsRequest{
		EventId:        req.EventID,
		SeatIds:        req.SeatIDs,
		UserId:         claims.UserID,
		IdempotencyKey: key,
	})
	if err != nil {
		h.fail(w, r, "booking", err)
		return
	}
	middleware.WriteJSON(w, http.StatusCreated, map[string]any{
		"reservation_id": res.GetReservationId(),
		"expires_at":     res.GetExpiresAt().AsTime().UTC().Format(time.RFC3339),
		"total_cents":    res.GetTotalCents(),
	})
}

// payReservation forwards a payment to Booking, which orchestrates the saga
// (ARCHITECTURE.md §6, D7).
//
// Booking answers with what happened rather than with an error, and this
// translates those three outcomes into the three status codes that mean them.
// The 202 is the one that matters: it says the request was accepted and the
// outcome is not settled yet. Reporting that as a 5xx would tell the customer
// their payment failed when the money may have moved, and invite a retry that
// looks to them like paying twice (D8).
func (h *Handler) payReservation(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		middleware.WriteError(w, http.StatusBadRequest, "idempotency_key_required", "the Idempotency-Key header is required")
		return
	}
	// The subject comes from the validated token, never from the body: a
	// reservation ID is not a capability, and Booking checks ownership against
	// whatever this says.
	claims, ok := middleware.ClaimsFrom(r.Context())
	if !ok {
		middleware.WriteError(w, http.StatusUnauthorized, "unauthenticated", "a bearer access token is required")
		return
	}
	res, err := h.booking.PayReservation(r.Context(), &bookingv1.PayReservationRequest{
		ReservationId:  r.PathValue("id"),
		UserId:         claims.UserID,
		IdempotencyKey: key,
	})
	if err != nil {
		h.fail(w, r, "booking", err)
		return
	}

	body := map[string]any{
		"reservation_id": res.GetReservationId(),
		"status":         res.GetStatus(),
	}
	switch res.GetStatus() {
	case "pending":
		// Accepted, not settled. The seats are still held and the
		// reconciliation job owns the reservation from here.
		body["message"] = "the payment outcome is not yet known; this reservation is being reconciled"
		middleware.WriteJSON(w, http.StatusAccepted, body)
	case "failed":
		body["error"] = "payment_declined"
		body["message"] = res.GetDeclineReason()
		middleware.WriteJSON(w, http.StatusPaymentRequired, body)
	default:
		tickets := make([]map[string]string, 0, len(res.GetTickets()))
		for _, t := range res.GetTickets() {
			tickets = append(tickets, map[string]string{"id": t.GetId(), "seat_id": t.GetSeatId(), "qr_code": t.GetQrCode()})
		}
		body["booking_id"] = res.GetBookingId()
		body["tickets"] = tickets
		middleware.WriteJSON(w, http.StatusOK, body)
	}
}

// fail maps a gRPC status from an upstream service back to an HTTP status.
//
// Unavailable is the interesting one: it means the upstream is down, and the
// honest answer is 503 rather than a 500 that looks like a bug in the request.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, upstream string, err error) {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.InvalidArgument:
		middleware.WriteError(w, http.StatusBadRequest, "invalid_request", st.Message())
	case codes.Unauthenticated:
		middleware.WriteError(w, http.StatusUnauthorized, "unauthenticated", st.Message())
	case codes.PermissionDenied:
		middleware.WriteError(w, http.StatusForbidden, "forbidden", st.Message())
	case codes.NotFound:
		middleware.WriteError(w, http.StatusNotFound, "not_found", st.Message())
	case codes.AlreadyExists:
		middleware.WriteError(w, http.StatusConflict, "already_exists", st.Message())
	case codes.Aborted:
		// A lost race for a seat. Expected under contention, not a fault.
		middleware.WriteError(w, http.StatusConflict, "conflict", st.Message())
	case codes.FailedPrecondition:
		// The reservation is in a state the request does not apply to - past
		// its checkout window, or already settled. The caller's mistake, not
		// the server's.
		middleware.WriteError(w, http.StatusConflict, "conflict", st.Message())
	case codes.Unavailable:
		h.log.WarnContext(r.Context(), "upstream unavailable", "upstream", upstream, "error", err)
		middleware.WriteError(w, http.StatusServiceUnavailable, "upstream_unavailable", upstream+" is unavailable")
	case codes.DeadlineExceeded:
		h.log.WarnContext(r.Context(), "upstream timed out", "upstream", upstream, "error", err)
		middleware.WriteError(w, http.StatusGatewayTimeout, "upstream_timeout", upstream+" did not respond in time")
	default:
		h.log.ErrorContext(r.Context(), "upstream call failed", "upstream", upstream, "code", st.Code().String(), "error", err)
		middleware.WriteError(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
	}
}

// decode reads a bounded JSON body, reporting a 400 and returning false when it
// cannot. Unknown fields are rejected: silently ignoring a misspelled field is
// how a client believes it set a role it did not set.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			middleware.WriteError(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body is too large")
			return false
		}
		middleware.WriteError(w, http.StatusBadRequest, "invalid_body", "request body is not valid JSON")
		return false
	}
	return true
}

func tokenPairBody(p *authv1.TokenPair) map[string]any {
	return map[string]any{
		"access_token":  p.GetAccessToken(),
		"refresh_token": p.GetRefreshToken(),
		"token_type":    "Bearer",
		"expires_in":    p.GetExpiresIn(),
		"user_id":       p.GetUserId(),
		"role":          roleFromProto(p.GetRole()),
	}
}

func roleToProto(role string) authv1.Role {
	switch role {
	case roleAttendee:
		return authv1.Role_ROLE_ATTENDEE
	case roleOrganizer:
		return authv1.Role_ROLE_ORGANIZER
	case roleAdmin:
		return authv1.Role_ROLE_ADMIN
	default:
		// Unspecified, including an unrecognised string: Auth decides what an
		// unset role means, and rejects one it does not know.
		return authv1.Role_ROLE_UNSPECIFIED
	}
}

func roleFromProto(role authv1.Role) string {
	switch role {
	case authv1.Role_ROLE_ATTENDEE:
		return roleAttendee
	case authv1.Role_ROLE_ORGANIZER:
		return roleOrganizer
	case authv1.Role_ROLE_ADMIN:
		return roleAdmin
	default:
		return ""
	}
}
