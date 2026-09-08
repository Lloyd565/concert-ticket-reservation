// Package breaker is a circuit breaker for one outbound dependency
// (ARCHITECTURE.md §10, resilience checklist).
//
// It exists because a dependency that is down answers slowly, and every request
// that waits on it holds a goroutine, a connection and a customer. Past a point,
// continuing to call a broken service converts its outage into ours and adds
// load that keeps it broken. The breaker turns "wait five seconds then fail"
// into "fail now", and probes occasionally to notice recovery.
//
// It is deliberately not a library dependency. This is the whole of the logic,
// it is forty lines, and having it here means the state machine is readable
// beside the saga that depends on its exact semantics.
package breaker

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned instead of calling through while the breaker is open.
//
// Callers of a payment dependency must map this to an UNKNOWN outcome, not to a
// failure: the breaker opening says nothing about whether an earlier attempt
// moved money (D8).
var ErrOpen = errors.New("circuit breaker open: dependency is failing")

// State is the breaker's position in its cycle.
type State string

const (
	// StateClosed passes calls through and counts consecutive failures.
	StateClosed State = "closed"
	// StateOpen fails fast without calling.
	StateOpen State = "open"
	// StateHalfOpen lets exactly one call through as a probe. Exactly one: a
	// recovering service must not be hit by the full backlog the moment it
	// answers once.
	StateHalfOpen State = "half-open"
)

// Breaker guards one dependency. The zero value is not usable; use New.
type Breaker struct {
	threshold int
	cooldown  time.Duration
	now       func() time.Time

	mu          sync.Mutex
	state       State
	failures    int
	openedAt    time.Time
	probeInFlig bool
}

// New returns a closed breaker that opens after threshold consecutive failures
// and probes again after cooldown. Zero or negative values fall back to 5
// failures and 30 seconds.
func New(threshold int, cooldown time.Duration) *Breaker {
	if threshold <= 0 {
		threshold = 5
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now, state: StateClosed}
}

// State reports the breaker's current position, for metrics and tests.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()
	return b.state
}

// Do runs fn unless the breaker is open, in which case it returns ErrOpen
// without calling.
//
// What counts as a failure is the caller's decision, expressed by the error fn
// returns: this breaker trips on any non-nil error. The payment client passes
// only transport failures through it - a declined card is a working dependency
// giving a correct answer, and counting it as a failure would open the breaker
// on a busy night of expired credit cards.
func (b *Breaker) Do(fn func() error) error {
	if err := b.allow(); err != nil {
		return err
	}
	err := fn()
	b.record(err == nil)
	return err
}

// allow decides whether this call may proceed.
func (b *Breaker) allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()
	switch b.state {
	case StateOpen:
		return ErrOpen
	case StateHalfOpen:
		if b.probeInFlig {
			// Another caller is already probing. Everyone else keeps failing
			// fast until that probe answers.
			return ErrOpen
		}
		b.probeInFlig = true
		return nil
	default:
		return nil
	}
}

// record folds one outcome into the state machine.
func (b *Breaker) record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	wasProbe := b.state == StateHalfOpen
	b.probeInFlig = false

	if success {
		// One success closes a half-open breaker and resets a closed one. A
		// count that survived an intervening success would open the breaker on
		// failures that are not consecutive, which is a different (and worse)
		// policy than the one documented.
		b.state = StateClosed
		b.failures = 0
		return
	}

	b.failures++
	if wasProbe || b.failures >= b.threshold {
		// A failed probe re-opens immediately: the dependency answered, and the
		// answer was that it is still broken.
		b.state = StateOpen
		b.openedAt = b.now()
	}
}

// refresh moves an open breaker to half-open once the cooldown has elapsed.
// Callers must hold the lock.
func (b *Breaker) refresh() {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.cooldown {
		b.state = StateHalfOpen
		b.probeInFlig = false
	}
}
