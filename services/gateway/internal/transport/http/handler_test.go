// Tests for the gateway edge: local token validation, role gating, correlation
// IDs, rate limiting, and that a valid token reaches Booking as the right user.
//
// Upstream services are stubbed here on purpose. What is under test is the
// gateway's decision to let a request through and what it forwards - not seat
// locking, which is tested where it lives, against a real database
// (AGENTS.md §7).
package http_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	authv1 "github.com/lloyd565/concert-ticket-reservation/proto/auth/v1"
	bookingv1 "github.com/lloyd565/concert-ticket-reservation/proto/booking/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/middleware"
	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/token"
	gatewayhttp "github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/transport/http"
)

const (
	testSecret   = "test-secret-at-least-32-bytes-long!!"
	testIssuer   = "concert-auth"
	testAudience = "concert-api"
	testUserID   = "0192f3c4-5d6e-7f80-9123-456789abcdef"
)

// stubBooking records what the gateway forwarded, so a test can assert on the
// user_id the gateway chose rather than on the one the client asked for.
type stubBooking struct {
	bookingv1.BookingServiceClient
	lastHold *bookingv1.HoldSeatsRequest
	err      error
}

func (s *stubBooking) HoldSeats(_ context.Context, in *bookingv1.HoldSeatsRequest, _ ...grpc.CallOption) (*bookingv1.HoldSeatsResponse, error) {
	s.lastHold = in
	if s.err != nil {
		return nil, s.err
	}
	return &bookingv1.HoldSeatsResponse{
		ReservationId: "0192f3c4-0000-7000-8000-000000000001",
		ExpiresAt:     timestamppb.New(time.Now().Add(10 * time.Minute)),
	}, nil
}

func (s *stubBooking) SeedEvent(context.Context, *bookingv1.SeedEventRequest, ...grpc.CallOption) (*bookingv1.SeedEventResponse, error) {
	return &bookingv1.SeedEventResponse{EventId: "0192f3c4-0000-7000-8000-000000000002", Name: "seeded"}, nil
}

type stubAuth struct {
	authv1.AuthServiceClient
	lastRegister *authv1.RegisterRequest
}

func (s *stubAuth) Register(_ context.Context, in *authv1.RegisterRequest, _ ...grpc.CallOption) (*authv1.RegisterResponse, error) {
	s.lastRegister = in
	return &authv1.RegisterResponse{UserId: testUserID, Email: in.GetEmail(), Role: in.GetRole()}, nil
}

func (s *stubAuth) Login(context.Context, *authv1.LoginRequest, ...grpc.CallOption) (*authv1.TokenPair, error) {
	return nil, status.Error(codes.Unauthenticated, "invalid credentials")
}

// newGateway wires the real middleware and handler around stubbed upstreams.
// rps is generous by default so that unrelated tests are not rate limited.
func newGateway(t *testing.T, auth authv1.AuthServiceClient, booking bookingv1.BookingServiceClient, rps float64, burst int) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	verifier := token.NewVerifier([]byte(testSecret), testIssuer, testAudience)
	limiter := middleware.NewRateLimiter(rps, burst, time.Minute)
	return gatewayhttp.NewHandler(auth, booking, middleware.NewAuthenticator(verifier, log), limiter, log).Routes()
}

// mint builds an access token with whatever properties a test needs to bend.
func mint(t *testing.T, mutate func(*jwt.RegisteredClaims), role, secret string, method jwt.SigningMethod) string {
	t.Helper()
	now := time.Now()
	registered := jwt.RegisteredClaims{
		Subject:   testUserID,
		Issuer:    testIssuer,
		Audience:  jwt.ClaimStrings{testAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
	}
	if mutate != nil {
		mutate(&registered)
	}
	tok := jwt.NewWithClaims(method, struct {
		Role string `json:"role"`
		jwt.RegisteredClaims
	}{Role: role, RegisteredClaims: registered})
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign test token: %v", err)
	}
	return signed
}

func validToken(t *testing.T) string {
	t.Helper()
	return mint(t, nil, "attendee", testSecret, jwt.SigningMethodHS256)
}

func holdRequest(t *testing.T, bearer string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/reservations",
		strings.NewReader(`{"event_id":"0192f3c4-0000-7000-8000-000000000002","seat_ids":["0192f3c4-0000-7000-8000-00000000000a"]}`))
	req.Header.Set("Idempotency-Key", "key-1")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

// TestTokenRejection is the P1 exit criterion: an expired or malformed token
// must not reach Booking.
func TestTokenRejection(t *testing.T) {
	cases := []struct {
		name      string
		bearer    string
		wantCode  string
		omitAuthz bool
	}{
		{name: "no authorization header", omitAuthz: true, wantCode: "unauthenticated"},
		{name: "not a jwt", bearer: "this-is-not-a-token", wantCode: "invalid_token"},
		{name: "two segments only", bearer: "aGVhZGVy.cGF5bG9hZA", wantCode: "invalid_token"},
		{name: "garbage base64 payload", bearer: "eyJhbGciOiJIUzI1NiJ9.!!!!.c2ln", wantCode: "invalid_token"},
		{name: "empty bearer value", bearer: "   ", wantCode: "unauthenticated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			booking := &stubBooking{}
			bearer := tc.bearer
			if tc.omitAuthz {
				bearer = ""
			}
			rec := httptest.NewRecorder()
			newGateway(t, &stubAuth{}, booking, 1000, 1000).ServeHTTP(rec, holdRequest(t, bearer))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if got := errorCode(t, rec.Body.Bytes()); got != tc.wantCode {
				t.Errorf("error code = %q, want %q", got, tc.wantCode)
			}
			if booking.lastHold != nil {
				t.Error("a rejected request reached Booking")
			}
		})
	}
}

// TestExpiredTokenRejected covers the case the whole 15-minute TTL exists for.
func TestExpiredTokenRejected(t *testing.T) {
	expired := mint(t, func(c *jwt.RegisteredClaims) {
		c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
		c.NotBefore = c.IssuedAt
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	}, "attendee", testSecret, jwt.SigningMethodHS256)

	booking := &stubBooking{}
	rec := httptest.NewRecorder()
	newGateway(t, &stubAuth{}, booking, 1000, 1000).ServeHTTP(rec, holdRequest(t, expired))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// A distinct code: the client's remedy is to refresh, not to re-authenticate.
	if got := errorCode(t, rec.Body.Bytes()); got != "token_expired" {
		t.Errorf("error code = %q, want token_expired", got)
	}
	if booking.lastHold != nil {
		t.Error("an expired token reached Booking")
	}
}

// TestForgedTokensRejected covers the attacks that algorithm pinning exists to
// stop, plus a token minted by a system that is not our Auth.
func TestForgedTokensRejected(t *testing.T) {
	cases := []struct {
		name   string
		bearer string
	}{
		{
			name:   "signed with the wrong secret",
			bearer: mint(t, nil, "attendee", "another-secret-at-least-32-bytes-xx!", jwt.SigningMethodHS256),
		},
		{
			// alg=none. Accepting this would mean any claims at all are valid.
			name:   "unsigned",
			bearer: unsignedToken(t),
		},
		{
			name:   "wrong issuer",
			bearer: mint(t, func(c *jwt.RegisteredClaims) { c.Issuer = "somebody-else" }, "attendee", testSecret, jwt.SigningMethodHS256),
		},
		{
			name:   "wrong audience",
			bearer: mint(t, func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"some-other-api"} }, "attendee", testSecret, jwt.SigningMethodHS256),
		},
		{
			name:   "unknown role",
			bearer: mint(t, nil, "superuser", testSecret, jwt.SigningMethodHS256),
		},
		{
			name:   "no subject",
			bearer: mint(t, func(c *jwt.RegisteredClaims) { c.Subject = "" }, "attendee", testSecret, jwt.SigningMethodHS256),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			booking := &stubBooking{}
			rec := httptest.NewRecorder()
			newGateway(t, &stubAuth{}, booking, 1000, 1000).ServeHTTP(rec, holdRequest(t, tc.bearer))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if booking.lastHold != nil {
				t.Error("a forged token reached Booking")
			}
		})
	}
}

// unsignedToken builds an alg=none token by hand: the library will not mint one.
func unsignedToken(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil,
		`{"sub":%q,"role":"admin","iss":%q,"aud":%q,"exp":%d}`,
		testUserID, testIssuer, testAudience, time.Now().Add(time.Hour).Unix()))
	return header + "." + payload + "."
}

// TestValidTokenReachesBooking is the other half of the P1 exit criterion.
func TestValidTokenReachesBooking(t *testing.T) {
	booking := &stubBooking{}
	rec := httptest.NewRecorder()
	newGateway(t, &stubAuth{}, booking, 1000, 1000).ServeHTTP(rec, holdRequest(t, validToken(t)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body.String())
	}
	if booking.lastHold == nil {
		t.Fatal("Booking was never called")
	}
	// The identity Booking records comes from the token, not from the client.
	if got := booking.lastHold.GetUserId(); got != testUserID {
		t.Errorf("forwarded user_id = %q, want %q", got, testUserID)
	}
	if got := booking.lastHold.GetIdempotencyKey(); got != "key-1" {
		t.Errorf("forwarded idempotency key = %q, want key-1", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["reservation_id"] == "" || body["expires_at"] == "" {
		t.Errorf("response is missing reservation fields: %v", body)
	}
}

// TestHoldRequiresIdempotencyKey - the rule holds at the edge too, so a client
// cannot skip it by going through the gateway (AGENTS.md §2 rule 10).
func TestHoldRequiresIdempotencyKey(t *testing.T) {
	booking := &stubBooking{}
	req := holdRequest(t, validToken(t))
	req.Header.Del("Idempotency-Key")

	rec := httptest.NewRecorder()
	newGateway(t, &stubAuth{}, booking, 1000, 1000).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if booking.lastHold != nil {
		t.Error("a keyless hold reached Booking")
	}
}

// TestRoleGating: an attendee token authenticates but must not seed events.
func TestRoleGating(t *testing.T) {
	for _, tc := range []struct {
		role string
		want int
	}{
		{role: "attendee", want: http.StatusForbidden},
		{role: "organizer", want: http.StatusCreated},
		{role: "admin", want: http.StatusCreated},
	} {
		t.Run(tc.role, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/events",
				strings.NewReader(`{"name":"show","starts_at":"2026-01-01T00:00:00Z","sections":["A"],"rows":1,"seats_per_row":1}`))
			req.Header.Set("Authorization", "Bearer "+mint(t, nil, tc.role, testSecret, jwt.SigningMethodHS256))

			rec := httptest.NewRecorder()
			newGateway(t, &stubAuth{}, &stubBooking{}, 1000, 1000).ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

// TestRoleEscalationBlockedAtRegistration: an anonymous caller must not be able
// to register themselves an admin account.
func TestRoleEscalationBlockedAtRegistration(t *testing.T) {
	auth := &stubAuth{}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/register",
		strings.NewReader(`{"email":"a@example.com","password":"correct-horse-battery","role":"admin"}`))

	rec := httptest.NewRecorder()
	newGateway(t, auth, &stubBooking{}, 1000, 1000).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if auth.lastRegister != nil {
		t.Error("an unauthorized privileged registration reached Auth")
	}
}

// TestCorrelationID: every response carries one, a usable client-supplied one
// is reused, and an unusable one is replaced rather than logged.
func TestCorrelationID(t *testing.T) {
	gw := newGateway(t, &stubAuth{}, &stubBooking{}, 1000, 1000)

	t.Run("minted when absent", func(t *testing.T) {
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Header().Get(middleware.CorrelationHeader) == "" {
			t.Error("response carries no correlation ID")
		}
	})

	t.Run("client value reused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set(middleware.CorrelationHeader, "trace-abc-123")
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if got := rec.Header().Get(middleware.CorrelationHeader); got != "trace-abc-123" {
			t.Errorf("correlation ID = %q, want the client's value", got)
		}
	})

	t.Run("log injection attempt replaced", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.Header.Set(middleware.CorrelationHeader, "abc\ndef level=error msg=\"forged\"")
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		got := rec.Header().Get(middleware.CorrelationHeader)
		if strings.ContainsAny(got, "\r\n") || got == "abc\ndef level=error msg=\"forged\"" {
			t.Errorf("correlation ID = %q, want a freshly minted one", got)
		}
	})
}

// TestRateLimiting: a burst is allowed, the request after it is not.
func TestRateLimiting(t *testing.T) {
	// rps is tiny so the bucket cannot refill during the test.
	gw := newGateway(t, &stubAuth{}, &stubBooking{}, 0.01, 3)

	for i := range 3 {
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d within burst: status = %d, want 200", i+1, rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request past burst: status = %d, want 429", rec.Code)
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", got)
	}
}

// TestUpstreamDownIsNotAnInternalError: Auth being unreachable is a 503 for the
// routes that need it, and - crucially - is not an error for the routes that do
// not (ARCHITECTURE.md §3.3).
func TestUpstreamDownIsNotAnInternalError(t *testing.T) {
	booking := &stubBooking{}
	gw := newGateway(t, &stubAuth{}, booking, 1000, 1000)

	// Auth is stubbed to fail on Login the way a dead upstream would.
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", strings.NewReader(`{"email":"a@example.com","password":"x"}`))
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401", rec.Code)
	}

	// The already-issued session keeps working regardless.
	rec = httptest.NewRecorder()
	gw.ServeHTTP(rec, holdRequest(t, validToken(t)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("hold status = %d body = %s, want 201", rec.Code, rec.Body.String())
	}
}

// TestSeatConflictIsAConflict: Booking's Aborted becomes a 409, not a 500. A
// lost race is an expected outcome (FR-3.2).
func TestSeatConflictIsAConflict(t *testing.T) {
	booking := &stubBooking{err: status.Error(codes.Aborted, "one or more seats are no longer available")}
	rec := httptest.NewRecorder()
	newGateway(t, &stubAuth{}, booking, 1000, 1000).ServeHTTP(rec, holdRequest(t, validToken(t)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error
}
