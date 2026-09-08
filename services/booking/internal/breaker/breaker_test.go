package breaker

import (
	"errors"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

// clock lets the test move time without sleeping through a cooldown.
func (b *Breaker) setClock(now func() time.Time) { b.now = now }

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := New(3, time.Minute)

	for i := range 2 {
		if err := b.Do(func() error { return errBoom }); !errors.Is(err, errBoom) {
			t.Fatalf("call %d: want the underlying error through a closed breaker, got %v", i, err)
		}
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("want the breaker still closed under the threshold, got %s", got)
	}

	if err := b.Do(func() error { return errBoom }); !errors.Is(err, errBoom) {
		t.Fatalf("third call: want the underlying error, got %v", err)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("want the breaker open at the threshold, got %s", got)
	}

	// Open means the dependency is not called at all - that is the entire
	// point, so assert on the call not happening rather than on the error.
	called := false
	if err := b.Do(func() error { called = true; return nil }); !errors.Is(err, ErrOpen) {
		t.Fatalf("want ErrOpen, got %v", err)
	}
	if called {
		t.Fatal("an open breaker called through to the dependency")
	}
}

func TestBreakerResetsOnSuccess(t *testing.T) {
	b := New(3, time.Minute)

	_ = b.Do(func() error { return errBoom })
	_ = b.Do(func() error { return errBoom })
	_ = b.Do(func() error { return nil })
	// Two more failures must not open it: the counter is consecutive failures,
	// not total ones.
	_ = b.Do(func() error { return errBoom })
	_ = b.Do(func() error { return errBoom })

	if got := b.State(); got != StateClosed {
		t.Fatalf("want the breaker closed after an intervening success, got %s", got)
	}
}

func TestBreakerProbesAfterCooldownAndCloses(t *testing.T) {
	now := time.Now()
	b := New(1, 30*time.Second)
	b.setClock(func() time.Time { return now })

	_ = b.Do(func() error { return errBoom })
	if got := b.State(); got != StateOpen {
		t.Fatalf("want open, got %s", got)
	}

	now = now.Add(31 * time.Second)
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("want half-open after the cooldown, got %s", got)
	}

	// Exactly one probe gets through; everyone else keeps failing fast, so a
	// recovering dependency is not hit by the whole backlog at once.
	probes := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Do(func() error {
			probes++
			// While this probe is in flight the breaker must refuse others.
			if err := b.Do(func() error { probes++; return nil }); !errors.Is(err, ErrOpen) {
				t.Errorf("want a second concurrent probe refused, got %v", err)
			}
			return nil
		})
	}()
	<-done

	if probes != 1 {
		t.Fatalf("want exactly 1 probe through a half-open breaker, got %d", probes)
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("want the breaker closed after a successful probe, got %s", got)
	}
}

func TestBreakerReopensOnFailedProbe(t *testing.T) {
	now := time.Now()
	b := New(1, 30*time.Second)
	b.setClock(func() time.Time { return now })

	_ = b.Do(func() error { return errBoom })
	now = now.Add(31 * time.Second)

	// The probe answers, and the answer is that the dependency is still broken.
	// It re-opens immediately rather than needing to hit the threshold again.
	if err := b.Do(func() error { return errBoom }); !errors.Is(err, errBoom) {
		t.Fatalf("want the probe error through, got %v", err)
	}
	if got := b.State(); got != StateOpen {
		t.Fatalf("want the breaker open again after a failed probe, got %s", got)
	}
}
