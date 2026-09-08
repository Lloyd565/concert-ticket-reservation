//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/provider"
	"github.com/lloyd565/concert-ticket-reservation/services/payment/internal/usecase"
)

func newChargeInput() usecase.ChargeInput {
	return usecase.ChargeInput{
		ReservationID:  uuid.Must(uuid.NewV7()).String(),
		UserID:         uuid.Must(uuid.NewV7()).String(),
		AmountCents:    5000,
		IdempotencyKey: "key-" + uuid.Must(uuid.NewV7()).String(),
	}
}

func TestChargeSucceeds(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	if ch.Status != domain.ChargeSucceeded {
		t.Fatalf("want succeeded, got %s", ch.Status)
	}
	if ch.ProviderRef == "" {
		t.Fatal("a succeeded charge must carry the reference a refund is issued against")
	}

	status, ref := chargeRow(t, ctx, in.IdempotencyKey)
	if status != string(domain.ChargeSucceeded) || ref == nil {
		t.Fatalf("database disagrees: status=%s ref=%v", status, ref)
	}
}

// TestChargeIsIdempotent is FR-4.3: a retry never double-charges.
func TestChargeIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	first, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("first charge: %v", err)
	}
	second, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("replayed charge: %v", err)
	}
	if first.ID != second.ID || first.ProviderRef != second.ProviderRef {
		t.Fatalf("replay produced a second charge: %+v then %+v", first, second)
	}
	if n := countCharges(t, ctx, in.ReservationID); n != 1 {
		t.Fatalf("want 1 charge row after a replay, got %d", n)
	}
}

// TestConcurrentChargesUnderOneKeyProduceOneCharge is the check-then-act race in
// this service. Both callers read "no charge for this key" before either
// writes; only the UNIQUE index stops them both taking the money.
func TestConcurrentChargesUnderOneKeyProduceOneCharge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	const callers = 8
	results := make([]domain.Charge, callers)
	errs := make([]error, callers)

	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	for i := range callers {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = f.charger.Charge(ctx, in)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].ID != results[0].ID {
			t.Fatalf("caller %d got a different charge: %s vs %s", i, results[i].ID, results[0].ID)
		}
	}
	if n := countCharges(t, ctx, in.ReservationID); n != 1 {
		t.Fatalf("want exactly 1 charge row for %d concurrent callers, got %d", callers, n)
	}
}

func TestChargeDeclines(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeDecline)
	in := newChargeInput()

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		// A decline is an answer, not an error: the caller asked what happened
		// and was told.
		t.Fatalf("a decline must be reported as a value, not an error: %v", err)
	}
	if ch.Status != domain.ChargeDeclined {
		t.Fatalf("want declined, got %s", ch.Status)
	}
	if ch.DeclineReason == "" {
		t.Fatal("a decline must carry a reason the customer can be told")
	}
	if status, _ := chargeRow(t, ctx, in.IdempotencyKey); status != string(domain.ChargeDeclined) {
		t.Fatalf("database disagrees: %s", status)
	}
}

// TestChargeStaysPendingWhenTheProviderHangs is FR-4.5 and the Payment half of
// D8.
//
// The provider never answers. The row must already exist, and must still say
// pending: it is the only evidence the attempt happened, and rewriting it as
// failed would turn "we do not know" into "it did not happen" one layer below
// the saga that must not believe that.
func TestChargeStaysPendingWhenTheProviderHangs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeHang)
	in := newChargeInput()

	_, err := f.charger.Charge(ctx, in)
	if !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("want ErrProviderUnavailable, got %v", err)
	}

	status, ref := chargeRow(t, ctx, in.IdempotencyKey)
	if status != string(domain.ChargePending) {
		t.Fatalf("an unanswered charge must stay pending, got %s", status)
	}
	if ref != nil {
		t.Fatalf("an unsettled charge must carry no provider reference, got %v", *ref)
	}

	// The stored charge is what GetCharge reports, which is what makes
	// Booking's reconciliation converge: pending means ask again, not failed.
	got, err := f.charger.GetCharge(ctx, in.IdempotencyKey)
	if err != nil {
		t.Fatalf("get charge: %v", err)
	}
	if got.Status != domain.ChargePending {
		t.Fatalf("want pending from GetCharge, got %s", got.Status)
	}
}

// TestChargeResumesAnUnsettledCharge is the recovery FR-4.5 exists to enable:
// the process died between writing the row and settling it, and a later call
// under the same key finishes the job rather than starting a second charge.
func TestChargeResumesAnUnsettledCharge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeHang)
	in := newChargeInput()

	if _, err := f.charger.Charge(ctx, in); !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("want ErrProviderUnavailable, got %v", err)
	}

	// The provider comes back.
	f.provider.SetMode(provider.ModeSucceed)

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("resumed charge: %v", err)
	}
	if ch.Status != domain.ChargeSucceeded {
		t.Fatalf("want succeeded after the provider recovered, got %s", ch.Status)
	}
	if n := countCharges(t, ctx, in.ReservationID); n != 1 {
		t.Fatalf("recovery must resume the original charge, not create a second: got %d rows", n)
	}
}

// TestGetChargeReportsUnknownKeys: no charge under this key means no money
// moved. It is the one answer that lets Booking release seats after a timeout,
// so it must be distinguishable from every other outcome.
func TestGetChargeReportsUnknownKeys(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)

	_, err := f.charger.GetCharge(ctx, "never-used-"+uuid.Must(uuid.NewV7()).String())
	if !errors.Is(err, domain.ErrChargeNotFound) {
		t.Fatalf("want ErrChargeNotFound, got %v", err)
	}
}

// TestChargeRejectsAKeyReplayedWithADifferentAmount: returning the original
// charge would answer a question the caller did not ask, and charging again
// would be worse.
func TestChargeRejectsAKeyReplayedWithADifferentAmount(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	if _, err := f.charger.Charge(ctx, in); err != nil {
		t.Fatalf("first charge: %v", err)
	}
	in.AmountCents = 9900
	if _, err := f.charger.Charge(ctx, in); !errors.Is(err, domain.ErrAmountMismatch) {
		t.Fatalf("want ErrAmountMismatch, got %v", err)
	}
	if n := countCharges(t, ctx, in.ReservationID); n != 1 {
		t.Fatalf("want 1 charge row, got %d", n)
	}
}

func TestChargeValidatesInput(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)

	cases := map[string]func(*usecase.ChargeInput){
		"no idempotency key": func(in *usecase.ChargeInput) { in.IdempotencyKey = "" },
		"zero amount":        func(in *usecase.ChargeInput) { in.AmountCents = 0 },
		"negative amount":    func(in *usecase.ChargeInput) { in.AmountCents = -1 },
		"bad reservation id": func(in *usecase.ChargeInput) { in.ReservationID = "not-a-uuid" },
		"bad user id":        func(in *usecase.ChargeInput) { in.UserID = "not-a-uuid" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := newChargeInput()
			mutate(&in)
			if _, err := f.charger.Charge(ctx, in); !errors.Is(err, domain.ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
		})
	}
}

// TestRefundIsIdempotent is FR-4.6. A refund is as dangerous to repeat as a
// charge.
func TestRefundIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	refundIn := usecase.RefundInput{ChargeID: ch.ID, AmountCents: ch.AmountCents, IdempotencyKey: "refund-" + in.IdempotencyKey}
	first, err := f.refunder.Refund(ctx, refundIn)
	if err != nil {
		t.Fatalf("refund: %v", err)
	}
	if first.Status != domain.RefundSucceeded || first.ProviderRef == "" {
		t.Fatalf("want a settled refund with a provider reference, got %+v", first)
	}
	second, err := f.refunder.Refund(ctx, refundIn)
	if err != nil {
		t.Fatalf("replayed refund: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("replay produced a second refund: %s then %s", first.ID, second.ID)
	}

	var refunds int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM refunds WHERE charge_id = $1`, ch.ID).Scan(&refunds); err != nil {
		t.Fatalf("count refunds: %v", err)
	}
	if refunds != 1 {
		t.Fatalf("want 1 refund row after a replay, got %d", refunds)
	}
}

// TestRefundRefusesAChargeThatTookNoMoney: refunding a declined charge would be
// a no-op the caller mistakes for a real refund; refunding a pending one would
// be a payout against money that has not arrived.
func TestRefundRefusesAChargeThatTookNoMoney(t *testing.T) {
	ctx := context.Background()

	declined := newFixture(t, provider.ModeDecline)
	in := newChargeInput()
	ch, err := declined.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	_, err = declined.refunder.Refund(ctx, usecase.RefundInput{ChargeID: ch.ID, AmountCents: ch.AmountCents, IdempotencyKey: "r1-" + in.IdempotencyKey})
	if !errors.Is(err, domain.ErrChargeNotRefundable) {
		t.Fatalf("want ErrChargeNotRefundable for a declined charge, got %v", err)
	}

	hung := newFixture(t, provider.ModeHang)
	in2 := newChargeInput()
	if _, err := hung.charger.Charge(ctx, in2); !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("want ErrProviderUnavailable, got %v", err)
	}
	pending, err := hung.charger.GetCharge(ctx, in2.IdempotencyKey)
	if err != nil {
		t.Fatalf("get charge: %v", err)
	}
	_, err = hung.refunder.Refund(ctx, usecase.RefundInput{ChargeID: pending.ID, AmountCents: pending.AmountCents, IdempotencyKey: "r2-" + in2.IdempotencyKey})
	if !errors.Is(err, domain.ErrChargeNotRefundable) {
		t.Fatalf("want ErrChargeNotRefundable for a pending charge, got %v", err)
	}
}

func TestRefundRefusesMoreThanWasCharged(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}
	_, err = f.refunder.Refund(ctx, usecase.RefundInput{ChargeID: ch.ID, AmountCents: ch.AmountCents + 1, IdempotencyKey: "over-" + in.IdempotencyKey})
	if !errors.Is(err, domain.ErrRefundExceedsCharge) {
		t.Fatalf("want ErrRefundExceedsCharge, got %v", err)
	}
}

// TestRefundStaysPendingWhenTheProviderHangs mirrors the charge case: an
// unanswered refund is unknown, not failed.
func TestRefundStaysPendingWhenTheProviderHangs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, provider.ModeSucceed)
	in := newChargeInput()

	ch, err := f.charger.Charge(ctx, in)
	if err != nil {
		t.Fatalf("charge: %v", err)
	}

	f.provider.SetMode(provider.ModeHang)
	key := "refund-hang-" + in.IdempotencyKey
	if _, err := f.refunder.Refund(ctx, usecase.RefundInput{ChargeID: ch.ID, AmountCents: ch.AmountCents, IdempotencyKey: key}); !errors.Is(err, domain.ErrProviderUnavailable) {
		t.Fatalf("want ErrProviderUnavailable, got %v", err)
	}

	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM refunds WHERE idempotency_key = $1`, key).Scan(&status); err != nil {
		t.Fatalf("read refund: %v", err)
	}
	if status != string(domain.RefundPending) {
		t.Fatalf("an unanswered refund must stay pending, got %s", status)
	}

	// And it resumes rather than starting a second refund.
	f.provider.SetMode(provider.ModeSucceed)
	rf, err := f.refunder.Refund(ctx, usecase.RefundInput{ChargeID: ch.ID, AmountCents: ch.AmountCents, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("resumed refund: %v", err)
	}
	if rf.Status != domain.RefundSucceeded {
		t.Fatalf("want succeeded after the provider recovered, got %s", rf.Status)
	}
}
