// Package mailer is the outbound email adapter. It sits at the repository layer
// (ARCHITECTURE.md §9): it imports domain, never usecase.
//
// Real delivery is out of scope, exactly as real money is for Payment. The mock
// "sends" by logging the message, and can be told to refuse, which is what makes
// the retry and dead-letter paths demonstrable in a running stack.
package mailer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lloyd565/concert-ticket-reservation/services/notification/internal/domain"
)

// Mode selects how the mock answers.
type Mode string

const (
	// ModeSucceed delivers every message.
	ModeSucceed Mode = "succeed"
	// ModeFail refuses every message: retries run out and the message is
	// dead-lettered.
	ModeFail Mode = "fail"
)

// ParseMode validates a configured mode.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeSucceed, ModeFail:
		return Mode(s), nil
	}
	return "", fmt.Errorf("mail mode must be succeed or fail, got %q", s)
}

// ErrRefused is what the mock returns in ModeFail.
var ErrRefused = errors.New("mock mail provider refused the message")

// Mock is a mail provider that logs instead of sending.
type Mock struct {
	mode Mode
	log  *slog.Logger
}

// NewMock returns a Mock in the given mode.
func NewMock(mode Mode, log *slog.Logger) *Mock { return &Mock{mode: mode, log: log} }

// Send "delivers" a notification by logging it.
//
// The recipient is a user ID, not an address. Addresses live in auth_db, which
// this service may not read (D2), and no event carries one yet.
// ponytail: add the address to the booking events (or an Auth lookup over gRPC)
// when a real provider replaces this mock.
func (m *Mock) Send(ctx context.Context, n domain.Notification) error {
	if m.mode == ModeFail {
		return ErrRefused
	}
	m.log.InfoContext(ctx, "email sent",
		"notification_id", n.ID, "to_user_id", n.UserID, "type", n.Type, "subject", n.Subject, "body", n.Body)
	return nil
}
