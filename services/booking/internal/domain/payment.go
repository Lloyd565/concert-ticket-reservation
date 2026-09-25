package domain

// PaymentStatus is what Payment says happened to a charge, as this service
// understands it. It is a domain concept rather than a transport one so the
// gRPC adapter can implement the port without importing usecase (AGENTS.md §4).
type PaymentStatus string

const (
	// PaymentPending means Payment has a charge under this key whose outcome it
	// does not know yet. It is the answer that keeps a reservation waiting
	// rather than resolving it either way.
	PaymentPending   PaymentStatus = "pending"
	PaymentSucceeded PaymentStatus = "succeeded"
	// PaymentDeclined is the provider definitively refusing. Compensatable.
	PaymentDeclined PaymentStatus = "declined"
	// PaymentFailed is the provider rejecting the request itself. Also
	// definitive, also compensatable, and treated identically to a decline by
	// the saga - what differs is only what the customer is told.
	PaymentFailed PaymentStatus = "failed"
	// PaymentNoCharge means Payment has never seen this idempotency key: no
	// charge was ever created. Only reconciliation can observe it, and it is
	// the one answer that makes releasing the seats safe after a timeout.
	PaymentNoCharge PaymentStatus = "no_charge"
)

// Settled reports whether the outcome will not change on its own.
func (s PaymentStatus) Settled() bool { return s != PaymentPending }

// PaymentResult is Payment's answer about one charge.
type PaymentResult struct {
	ChargeID      string
	Status        PaymentStatus
	AmountCents   int64
	DeclineReason string
}
