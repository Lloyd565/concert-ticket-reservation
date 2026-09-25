package domain

import "time"

// Event is the thing seats belong to. P0 carries only what the seat map needs;
// venue, organizer and on-sale gating arrive with the organizer flow.
type Event struct {
	ID       string
	Name     string
	StartsAt time.Time
}
