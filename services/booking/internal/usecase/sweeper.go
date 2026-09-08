package usecase

import (
	"context"
	"log/slog"
	"time"
)

// DefaultSweepInterval bounds how long an abandoned seat stays unavailable
// after its window closes. It is the accuracy limit of the P0 expiry mechanism;
// Redis TTLs remove that limit in P3 (ARCHITECTURE.md §5.2).
const DefaultSweepInterval = 5 * time.Second

// Sweeper releases seats whose checkout window has closed.
//
// This is what makes an abandoned checkout self-healing with no user or
// operator action (PRD §4.1 step 8, NFR-1.2). It is a single SQL statement, so
// a reservation is never left expired while its seats stay held.
type Sweeper struct {
	seats    SeatRepository
	interval time.Duration
	log      *slog.Logger
}

// NewSweeper wires a Sweeper. A zero interval falls back to DefaultSweepInterval.
func NewSweeper(seats SeatRepository, interval time.Duration, log *slog.Logger) *Sweeper {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	return &Sweeper{seats: seats, interval: interval, log: log}
}

// Run sweeps until ctx is cancelled. Intended to be started in its own
// goroutine at boot.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepOnce(ctx)
		}
	}
}

// SweepOnce runs a single sweep. Exported so tests can drive expiry
// deterministically instead of waiting on the ticker.
func (s *Sweeper) SweepOnce(ctx context.Context) {
	freed, err := s.seats.ReleaseExpiredHolds(ctx)
	if err != nil {
		// A failed sweep is not fatal: the next tick retries, and the seats stay
		// held (unavailable) meanwhile rather than being wrongly freed.
		s.log.ErrorContext(ctx, "sweep expired holds", "error", err)
		return
	}
	if freed > 0 {
		s.log.InfoContext(ctx, "released expired holds", "seats", freed)
	}
}
