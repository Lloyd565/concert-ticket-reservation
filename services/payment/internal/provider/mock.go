// Package provider adapts an external payment provider to the usecase.Provider
// port. It is an outbound adapter and sits at the same layer as repository:
// it imports domain and may import drivers, and it never imports usecase
// (AGENTS.md §4).
//
// Real money is out of scope (ARCHITECTURE.md §7), so the only implementation
// is a mock. The interface is not speculative generality - it is what makes the
// saga's failure paths testable at all, since the failure this system most
// needs to survive is one no real provider will produce on demand.
package provider

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
)

// Mode is how the mock answers.
type Mode string

const (
	// ModeSucceed approves everything.
	ModeSucceed Mode = "succeed"
	// ModeDecline refuses everything, definitively.
	ModeDecline Mode = "decline"
	// ModeHang never answers. The caller's deadline is what ends the call, so
	// this is how the unknown-outcome case - the one D8 exists for, and the
	// hardest one to reproduce against a real provider - is reached on demand.
	ModeHang Mode = "hang"
)

// ParseMode validates a configured mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeSucceed, ModeDecline, ModeHang:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("unknown payment provider mode %q (want succeed, decline or hang)", s)
	}
}

// Mock is a fault-injecting payment provider.
type Mock struct {
	mode atomic.Value // Mode

	// latency simulates a provider that is slow but not broken.
	latency time.Duration

	// Real providers are idempotent by key: a retry after a timeout returns the
	// original result instead of moving money twice. The retry logic in the
	// charge use case is only safe because of that property, so the mock has to
	// have it too - otherwise the tests would pass against a mock that is more
	// forgiving than the thing it stands in for.
	//
	// ponytail: in memory, so this record dies with the Payment process and a
	// real provider's does not. In the running stack, a crash between approval
	// and settle followed by a re-drive mints a second approval that nothing
	// flags. Tests run in one process and are unaffected; persist it if the
	// stack is ever used for crash testing.
	mu      sync.Mutex
	results map[string]domain.ProviderResult
}

// NewMock returns a Mock in the given mode.
func NewMock(mode Mode, latency time.Duration) *Mock {
	m := &Mock{latency: latency, results: make(map[string]domain.ProviderResult)}
	m.mode.Store(mode)
	return m
}

// SetMode changes how the mock answers. Safe to call while calls are in flight,
// which is what lets a test flip a hanging provider back to healthy and watch
// reconciliation converge.
func (m *Mock) SetMode(mode Mode) { m.mode.Store(mode) }

// Mode returns the current mode.
func (m *Mock) Mode() Mode { return m.mode.Load().(Mode) }

// Charge takes money, or does not, according to the current mode.
func (m *Mock) Charge(ctx context.Context, idempotencyKey string, amountCents int64) (domain.ProviderResult, error) {
	return m.call(ctx, "ch", idempotencyKey, amountCents)
}

// Refund gives money back, or does not, according to the current mode.
func (m *Mock) Refund(ctx context.Context, idempotencyKey, chargeRef string, amountCents int64) (domain.ProviderResult, error) {
	if chargeRef == "" {
		// A real provider cannot refund against nothing either. Surfacing it as
		// a decline rather than a panic keeps the failure inside the domain.
		return domain.ProviderResult{Outcome: domain.ProviderDeclined, Reason: "no provider reference to refund against"}, nil
	}
	return m.call(ctx, "re", idempotencyKey, amountCents)
}

func (m *Mock) call(ctx context.Context, prefix, idempotencyKey string, amountCents int64) (domain.ProviderResult, error) {
	if prior, ok := m.replay(idempotencyKey); ok {
		return prior, nil
	}

	switch m.Mode() {
	case ModeHang:
		// Block until the caller gives up. Returning ErrProviderUnavailable is
		// the contract: the caller must read it as unknown, not as failed.
		<-ctx.Done()
		return domain.ProviderResult{}, fmt.Errorf("%w: %v", domain.ErrProviderUnavailable, ctx.Err())
	case ModeDecline:
		if err := m.sleep(ctx); err != nil {
			return domain.ProviderResult{}, err
		}
		// A decline is not recorded against the key: it moved no money, so a
		// later retry is free to get a different answer once the mode changes.
		return domain.ProviderResult{Outcome: domain.ProviderDeclined, Reason: "card declined by issuer"}, nil
	default:
		if err := m.sleep(ctx); err != nil {
			return domain.ProviderResult{}, err
		}
		res := domain.ProviderResult{
			Outcome: domain.ProviderApproved,
			Ref:     prefix + "_" + uuid.Must(uuid.NewV7()).String(),
		}
		m.remember(idempotencyKey, res)
		return res, nil
	}
}

// sleep simulates provider latency and respects the caller's deadline.
func (m *Mock) sleep(ctx context.Context) error {
	if m.latency <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", domain.ErrProviderUnavailable, ctx.Err())
	case <-time.After(m.latency):
		return nil
	}
}

func (m *Mock) replay(key string) (domain.ProviderResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, ok := m.results[key]
	return res, ok
}

func (m *Mock) remember(key string, res domain.ProviderResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[key] = res
}
