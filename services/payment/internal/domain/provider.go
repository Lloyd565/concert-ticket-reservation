package domain

// ProviderOutcome is what the payment provider said. It has exactly two
// values, and the missing third one is the point: "we don't know" is not an
// outcome the provider can report - it is the absence of one, and it arrives
// as a transport error rather than as a value here.
type ProviderOutcome string

const (
	ProviderApproved ProviderOutcome = "approved"
	ProviderDeclined ProviderOutcome = "declined"
)

// ProviderResult is a settled answer from the provider.
//
// It lives in domain rather than in usecase so that the provider adapter can
// implement the port without importing usecase, which the layering rules forbid
// (AGENTS.md §4).
type ProviderResult struct {
	Outcome ProviderOutcome
	// Ref is the provider's own identifier for the movement of money. It is
	// what a later refund is issued against.
	Ref string
	// Reason is human-readable and only set on a decline.
	Reason string
}
