package usecase

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/booking/internal/domain"
)

// SeedSpec describes a rectangular test seat map: one seat map per section,
// rows numbered 1..Rows, seats numbered 1..SeatsPerRow.
type SeedSpec struct {
	Name        string
	StartsAt    time.Time
	Sections    []string
	Rows        int
	SeatsPerRow int
}

// Seeder creates demo events and their seat maps (NFR-6.2).
type Seeder struct {
	tx     TxManager
	events EventRepository
}

// NewSeeder wires a Seeder.
func NewSeeder(tx TxManager, events EventRepository) *Seeder {
	return &Seeder{tx: tx, events: events}
}

// Seed creates one event and its seat map in a single transaction, returning
// the event and every seat created.
func (s *Seeder) Seed(ctx context.Context, spec SeedSpec) (domain.Event, []domain.Seat, error) {
	if spec.Name == "" {
		return domain.Event{}, nil, fmt.Errorf("seed: name required: %w", domain.ErrInvalidInput)
	}
	if len(spec.Sections) == 0 || spec.Rows < 1 || spec.SeatsPerRow < 1 {
		return domain.Event{}, nil, fmt.Errorf("seed: need at least one section, row and seat: %w", domain.ErrInvalidInput)
	}
	if spec.StartsAt.IsZero() {
		spec.StartsAt = time.Now().UTC().Add(30 * 24 * time.Hour)
	}

	ev := domain.Event{
		ID:       uuid.Must(uuid.NewV7()).String(),
		Name:     spec.Name,
		StartsAt: spec.StartsAt.UTC(),
	}
	seats := make([]domain.Seat, 0, len(spec.Sections)*spec.Rows*spec.SeatsPerRow)
	for _, section := range spec.Sections {
		for row := 1; row <= spec.Rows; row++ {
			for num := 1; num <= spec.SeatsPerRow; num++ {
				seats = append(seats, domain.Seat{
					ID:      uuid.Must(uuid.NewV7()).String(),
					EventID: ev.ID,
					Section: section,
					Row:     strconv.Itoa(row),
					Number:  strconv.Itoa(num),
					Status:  domain.SeatAvailable,
				})
			}
		}
	}

	// Event and seats commit together: an event with a half-written seat map is
	// not a state anything downstream should have to handle.
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.events.CreateEvent(ctx, ev); err != nil {
			return err
		}
		return s.events.CreateSeats(ctx, seats)
	})
	if err != nil {
		return domain.Event{}, nil, err
	}
	return ev, seats, nil
}
