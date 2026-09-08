// Package grpc exposes the Auth service over gRPC. It depends on usecase and
// domain only - never on repository (AGENTS.md §4) - and it is the boundary
// where domain errors become status codes and where SQL errors stop.
package grpc

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	authv1 "github.com/lloyd565/concert-ticket-reservation/proto/auth/v1"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/domain"
	"github.com/lloyd565/concert-ticket-reservation/services/auth/internal/usecase"
)

// Server implements authv1.AuthServiceServer.
type Server struct {
	authv1.UnimplementedAuthServiceServer
	accounts *usecase.Accounts
	log      *slog.Logger
}

// NewServer wires a Server.
func NewServer(accounts *usecase.Accounts, log *slog.Logger) *Server {
	return &Server{accounts: accounts, log: log}
}

// Register creates an account.
func (s *Server) Register(ctx context.Context, req *authv1.RegisterRequest) (*authv1.RegisterResponse, error) {
	user, err := s.accounts.Register(ctx, req.GetEmail(), req.GetPassword(), roleToString(req.GetRole()))
	if err != nil {
		return nil, s.fail(ctx, "register", err)
	}
	return &authv1.RegisterResponse{
		UserId: user.ID,
		Email:  user.Email,
		Role:   roleToProto(user.Role),
	}, nil
}

// Login exchanges credentials for a token pair.
func (s *Server) Login(ctx context.Context, req *authv1.LoginRequest) (*authv1.TokenPair, error) {
	session, err := s.accounts.Login(ctx, req.GetEmail(), req.GetPassword())
	if err != nil {
		return nil, s.fail(ctx, "login", err)
	}
	return tokenPair(session), nil
}

// Refresh rotates a refresh token into a new pair.
func (s *Server) Refresh(ctx context.Context, req *authv1.RefreshRequest) (*authv1.TokenPair, error) {
	session, err := s.accounts.Refresh(ctx, req.GetRefreshToken())
	if err != nil {
		return nil, s.fail(ctx, "refresh", err)
	}
	return tokenPair(session), nil
}

// Logout revokes a refresh token.
func (s *Server) Logout(ctx context.Context, req *authv1.LogoutRequest) (*authv1.LogoutResponse, error) {
	revoked, err := s.accounts.Logout(ctx, req.GetRefreshToken())
	if err != nil {
		return nil, s.fail(ctx, "logout", err)
	}
	return &authv1.LogoutResponse{Revoked: revoked}, nil
}

func tokenPair(s usecase.Session) *authv1.TokenPair {
	return &authv1.TokenPair{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		ExpiresIn:    int64(s.ExpiresIn.Seconds()),
		UserId:       s.UserID,
		Role:         roleToProto(s.Role),
	}
}

// fail maps a domain error to a gRPC status. Anything unrecognised is logged
// and reported as a generic Internal: SQL errors never reach a caller
// (AGENTS.md §5).
func (s *Server) fail(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, domain.ErrEmailTaken):
		return status.Error(codes.AlreadyExists, "email already registered")
	case errors.Is(err, domain.ErrInvalidCredentials), errors.Is(err, domain.ErrInvalidRefreshToken):
		// One message for both, and no detail about which half was wrong: the
		// distinction is exactly what an enumeration attack is looking for.
		return status.Error(codes.Unauthenticated, "invalid credentials")
	case errors.Is(err, domain.ErrWeakPassword), errors.Is(err, domain.ErrInvalidRole), errors.Is(err, domain.ErrInvalidInput):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		s.log.ErrorContext(ctx, "auth request failed", "op", op, "error", err)
		return status.Error(codes.Internal, "the request could not be completed")
	}
}

func roleToString(r authv1.Role) string {
	switch r {
	case authv1.Role_ROLE_ATTENDEE:
		return string(domain.RoleAttendee)
	case authv1.Role_ROLE_ORGANIZER:
		return string(domain.RoleOrganizer)
	case authv1.Role_ROLE_ADMIN:
		return string(domain.RoleAdmin)
	default:
		// Unspecified means "caller did not choose"; domain.ParseRole turns the
		// empty string into the least-privileged role.
		return ""
	}
}

func roleToProto(r domain.Role) authv1.Role {
	switch r {
	case domain.RoleAttendee:
		return authv1.Role_ROLE_ATTENDEE
	case domain.RoleOrganizer:
		return authv1.Role_ROLE_ORGANIZER
	case domain.RoleAdmin:
		return authv1.Role_ROLE_ADMIN
	default:
		return authv1.Role_ROLE_UNSPECIFIED
	}
}
