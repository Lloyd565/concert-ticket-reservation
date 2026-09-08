package domain

import "time"

// ReservationStatus tracks a checkout attempt (FR-5.1). Transitions are
// one-way; a terminal status is never left.
type ReservationStatus string

const (
	ReservationPending   ReservationStatus = "pending"
	ReservationConfirmed ReservationStatus = "confirmed"
	ReservationExpired   ReservationStatus = "expired"
	ReservationFailed    ReservationStatus = "failed"
	ReservationRefunded  ReservationStatus = "refunded"
)

var reservationTransitions = map[ReservationStatus]map[ReservationStatus]bool{
	ReservationPending:   {ReservationConfirmed: true, ReservationExpired: true, ReservationFailed: true},
	ReservationConfirmed: {ReservationRefunded: true},
}

// CanTransition reports whether the state machine connects from to to.
func (s ReservationStatus) CanTransition(to ReservationStatus) bool {
	return reservationTransitions[s][to]
}

// Reservation is a time-boxed claim on a set of seats by one user.
type Reservation struct {
	ID      string
	UserID  string
	EventID string
	Status  ReservationStatus
	SeatIDs []string
	// TotalCents is what this reservation is worth, fixed at hold time. It is
	// the amount the saga charges.
	TotalCents     int64
	ExpiresAt      time.Time
	IdempotencyKey string
	CreatedAt      time.Time
	// PaymentPendingSince is set when a payment call returned an UNKNOWN
	// outcome. While it is set, this reservation is owned by the reconciliation
	// job and nothing else may release its seats (D8) - not the sweeper, not a
	// retry of the saga. Nil means no charge is in doubt.
	PaymentPendingSince *time.Time
}

// PaymentOutcomeUnknown reports whether a charge may exist for this reservation
// whose result nobody knows yet.
func (r *Reservation) PaymentOutcomeUnknown() bool { return r.PaymentPendingSince != nil }

// Expired reports whether the checkout window has closed at the given instant.
// Callers pass now explicitly so this stays a pure function of its inputs.
func (r *Reservation) Expired(now time.Time) bool { return !now.Before(r.ExpiresAt) }

// Confirm marks a paid reservation as confirmed.
func (r *Reservation) Confirm() error { return r.transition(ReservationConfirmed) }

// Expire marks a reservation whose checkout window closed unpaid.
func (r *Reservation) Expire() error { return r.transition(ReservationExpired) }

// Fail marks a reservation whose payment was declined.
func (r *Reservation) Fail() error { return r.transition(ReservationFailed) }

// Refund marks a confirmed reservation as refunded.
func (r *Reservation) Refund() error { return r.transition(ReservationRefunded) }

func (r *Reservation) transition(to ReservationStatus) error {
	if !r.Status.CanTransition(to) {
		return TransitionError{Entity: "reservation", From: string(r.Status), To: string(to)}
	}
	r.Status = to
	return nil
}
