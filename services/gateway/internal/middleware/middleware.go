// Package middleware holds the gateway's edge concerns: correlation IDs,
// per-IP and per-user rate limiting and local access-token validation. These
// are the three things that belong at the entrance and nowhere else
// (ARCHITECTURE.md §3.1).
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/lloyd565/concert-ticket-reservation/services/gateway/internal/token"
)

// CorrelationHeader is the header a client may use to supply its own request
// ID, and the header every response carries back.
const CorrelationHeader = "X-Correlation-ID"

// maxCorrelationIDLen bounds a client-supplied ID. Unbounded, it is a way to
// write arbitrarily large attacker-chosen strings into the log pipeline.
const maxCorrelationIDLen = 128

type (
	correlationKey struct{}
	claimsKey      struct{}
)

// CorrelationID returns the request's correlation ID, or "".
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}

// ClaimsFrom returns the validated claims attached by Authenticate. The bool is
// false on an unauthenticated route.
func ClaimsFrom(ctx context.Context) (token.Claims, bool) {
	c, ok := ctx.Value(claimsKey{}).(token.Claims)
	return c, ok
}

// Correlate gives every request an ID, reusing the client's when it supplied a
// usable one so a trace that starts in the browser stays one trace.
//
// A client-supplied value is sanitised, not trusted: it ends up in structured
// logs across four services, and a value containing newlines or control
// characters is log injection - forged log lines that look like they came from
// somewhere else.
func Correlate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeCorrelationID(r.Header.Get(CorrelationHeader))
		if id == "" {
			id = uuid.Must(uuid.NewV7()).String()
		}
		// Echoed back so a client can quote it in a bug report, and set before
		// the handler runs so it is present even on a panic or an error path.
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), correlationKey{}, id)))
	})
}

func sanitizeCorrelationID(raw string) string {
	if len(raw) == 0 || len(raw) > maxCorrelationIDLen {
		return ""
	}
	for _, c := range raw {
		// Printable ASCII, minus space. Anything else - control characters,
		// newlines, multi-byte sequences - and we mint our own instead.
		if c <= ' ' || c > '~' {
			return ""
		}
	}
	return raw
}

// RateLimiter keeps a token bucket per client IP (Middleware) and per
// authenticated user (PerUser).
//
// ponytail: in-process, so the limit is per gateway replica - two replicas
// allow twice the rate. Move the buckets into Redis when the gateway is
// actually replicated; until then a shared store buys nothing and adds a
// dependency on the request path.
type RateLimiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	limit    rate.Limit
	burst    int
	idleFor  time.Duration
}

type visitor struct {
	limiter *rate.Limiter
	seen    time.Time
}

// NewRateLimiter returns a limiter allowing rps requests per second per IP with
// the given burst. idleFor is how long an idle IP's bucket is kept; evicting
// too eagerly hands a fresh full burst to anyone who pauses.
func NewRateLimiter(rps float64, burst int, idleFor time.Duration) *RateLimiter {
	return &RateLimiter{
		visitors: make(map[string]*visitor),
		limit:    rate.Limit(rps),
		burst:    burst,
		idleFor:  idleFor,
	}
}

// Run evicts idle buckets until ctx is cancelled. Without it the map is an
// unbounded memory leak keyed by attacker-chosen addresses.
func (l *RateLimiter) Run(ctx context.Context) {
	ticker := time.NewTicker(l.idleFor)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			l.mu.Lock()
			for ip, v := range l.visitors {
				if now.Sub(v.seen) > l.idleFor {
					delete(l.visitors, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

// Middleware rejects requests from an IP that is over its budget.
func (l *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests from this address")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// PerUser rejects requests from an authenticated user who is over their budget
// (NFR-5.3). It must sit behind Authenticator.Require, which is what puts the
// user on the context.
//
// The per-IP limit cannot see an account: one user spread across many addresses
// gets a fresh bucket at each, which is how a script hoards seats at on-sale.
// Both limits apply to an authenticated request, and either can refuse it.
func (l *RateLimiter) PerUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := ClaimsFrom(r.Context())
		// The prefix keeps an account from ever sharing a bucket with an address.
		if ok && !l.allow("user:"+claims.UserID) {
			w.Header().Set("Retry-After", "1")
			WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests from this account")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (l *RateLimiter) allow(key string) bool {
	l.mu.Lock()
	v, ok := l.visitors[key]
	if !ok {
		v = &visitor{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.visitors[key] = v
	}
	v.seen = time.Now()
	l.mu.Unlock()
	return v.limiter.Allow()
}

// clientIP is the rate-limit key.
//
// It is RemoteAddr and deliberately not X-Forwarded-For: XFF is client-supplied
// and honouring it unconditionally means anyone can defeat the limit by varying
// a header. Behind a real load balancer this needs a trusted-proxy list before
// XFF can be read - which is a deployment concern, not a code one, and there is
// no proxy in front of this gateway today.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Authenticator validates access tokens at the edge.
type Authenticator struct {
	verifier *token.Verifier
	log      *slog.Logger
}

// NewAuthenticator wires an Authenticator.
func NewAuthenticator(verifier *token.Verifier, log *slog.Logger) *Authenticator {
	return &Authenticator{verifier: verifier, log: log}
}

// Require returns middleware that rejects any request without a valid access
// token, and - when roles is non-empty - without one of the listed roles.
//
// No call to Auth happens here or anywhere on this path. See the package
// comment on internal/token for exactly what that does and does not cover.
func (a *Authenticator) Require(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				WriteError(w, http.StatusUnauthorized, "unauthenticated", "a bearer access token is required")
				return
			}
			claims, err := a.verifier.Verify(raw)
			switch {
			case err == nil:
			case errors.Is(err, token.ErrTokenExpired):
				// A distinct code, because the client's remedy is specific:
				// call /v1/auth/refresh, do not re-prompt for a password.
				WriteError(w, http.StatusUnauthorized, "token_expired", "the access token has expired")
				return
			default:
				// Logged at debug, not returned: telling a caller which check
				// failed is free reconnaissance.
				a.log.DebugContext(r.Context(), "rejected access token", "error", err)
				WriteError(w, http.StatusUnauthorized, "invalid_token", "the access token is not valid")
				return
			}
			if len(allowed) > 0 && !allowed[claims.Role] {
				// Authenticated but not permitted - 403, not 401: retrying with
				// the same credentials will never work.
				WriteError(w, http.StatusForbidden, "forbidden", "this role may not perform that action")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims)))
		})
	}
}

// Optional attaches claims when a valid token is present and does nothing when
// it is not. Used where a route's behaviour depends on the caller but does not
// require one - registration, where only an admin may set a privileged role.
func (a *Authenticator) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw, ok := bearerToken(r); ok {
			if claims, err := a.verifier.Verify(raw); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	// Scheme is case-insensitive per RFC 7235; the token is not.
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", false
	}
	raw := strings.TrimSpace(header[7:])
	return raw, raw != ""
}
