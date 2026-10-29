package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/security"
)

const (
	minimumPasswordBytes = 8
	maximumPasswordBytes = 128
)

type AuthService struct {
	repo      repository.Repository
	tokens    *security.TokenManager
	passwords security.Passwords
	clock     func() time.Time
}

// NewAuthService creates the authentication application service. The clock
// parameter is optional and exists to make token expiry tests deterministic.
func NewAuthService(repo repository.Repository, tokens *security.TokenManager, clock func() time.Time) *AuthService {
	if clock == nil {
		clock = time.Now
	}
	return &AuthService{repo: repo, tokens: tokens, passwords: security.Passwords{}, clock: clock}
}

type PublicUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

type AuthResult struct {
	Token string     `json:"token"`
	User  PublicUser `json:"user"`
}

func (s *AuthService) Register(ctx context.Context, email, password, displayName string) (PublicUser, error) {
	normalized, err := normalizeEmail(email)
	if err != nil {
		return PublicUser{}, err
	}
	if err := validatePassword(password); err != nil {
		return PublicUser{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || utf8.RuneCountInString(displayName) > 100 {
		return PublicUser{}, ErrInvalidRequest
	}
	if _, err := s.repo.FindUserByEmail(ctx, normalized); err == nil {
		return PublicUser{}, ErrConflict
	} else if errors.Is(err, repository.ErrNotFound) {
		// The address is available.
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return PublicUser{}, err
	} else {
		return PublicUser{}, ErrInternal
	}

	hash, err := s.passwords.Hash(password)
	if err != nil {
		return PublicUser{}, ErrInternal
	}
	created, err := s.repo.CreateUser(ctx, &domain.User{Email: normalized, DisplayName: displayName, PasswordHash: hash})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return PublicUser{}, ErrConflict
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return PublicUser{}, err
		}
		return PublicUser{}, ErrInternal
	}
	return publicUser(created), nil
}

func (s *AuthService) Login(ctx context.Context, email, password string) (AuthResult, error) {
	normalized, err := normalizeEmail(email)
	if err != nil || password == "" {
		return AuthResult{}, ErrUnauthorized
	}
	user, err := s.repo.FindUserByEmail(ctx, normalized)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return AuthResult{}, ErrUnauthorized
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return AuthResult{}, err
		}
		return AuthResult{}, ErrInternal
	}
	if !s.passwords.Compare(user.PasswordHash, password) {
		return AuthResult{}, ErrUnauthorized
	}
	if s.tokens == nil {
		return AuthResult{}, ErrInternal
	}
	token, err := s.tokens.Issue(user.ID, s.clock())
	if err != nil {
		return AuthResult{}, ErrInternal
	}
	return AuthResult{Token: token, User: publicUser(user)}, nil
}

func (s *AuthService) Me(ctx context.Context, userID string) (PublicUser, error) {
	if strings.TrimSpace(userID) == "" {
		return PublicUser{}, ErrUnauthorized
	}
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return PublicUser{}, ErrUnauthorized
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return PublicUser{}, err
		}
		return PublicUser{}, ErrInternal
	}
	return publicUser(user), nil
}

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 320 || strings.Count(value, "@") != 1 {
		return "", ErrInvalidRequest
	}
	parts := strings.SplitN(value, "@", 2)
	if parts[0] == "" || parts[1] == "" || strings.ContainsAny(value, "\r\n\t ") {
		return "", ErrInvalidRequest
	}
	return value, nil
}

func validatePassword(password string) error {
	length := len([]byte(password))
	if length < minimumPasswordBytes || length > maximumPasswordBytes {
		return ErrInvalidRequest
	}
	return nil
}

func publicUser(user *domain.User) PublicUser {
	return PublicUser{ID: user.ID, Email: user.Email, DisplayName: user.DisplayName}
}
