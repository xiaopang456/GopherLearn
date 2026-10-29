package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/security"
)

func newTestAuth(t *testing.T) (*AuthService, *repository.InMemoryRepository) {
	t.Helper()
	store := repository.NewInMemoryRepository()
	manager, err := security.NewTokenManagerWithLifetime("test-secret", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	return NewAuthService(store, manager, clock), store
}

func TestAuthRegisterLoginAndPasswordProtection(t *testing.T) {
	auth, store := newTestAuth(t)
	ctx := context.Background()
	user, err := auth.Register(ctx, "  Person@Example.COM ", "correct horse", "Learner")
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "person@example.com" {
		t.Fatalf("email was not normalized: %#v", user)
	}
	stored, err := store.FindUserByEmail(ctx, "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == "" || stored.PasswordHash == "correct horse" || strings.Contains(stored.PasswordHash, "correct horse") {
		t.Fatal("password was stored in a reversible or plaintext form")
	}
	result, err := auth.Login(ctx, "PERSON@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if result.Token == "" || result.User.ID != user.ID {
		t.Fatalf("unexpected login result: %#v", result)
	}
	if _, err := auth.Login(ctx, "person@example.com", "wrong password"); err != ErrUnauthorized {
		t.Fatalf("wrong password error = %v", err)
	}
}

func TestAuthPasswordByteLimits(t *testing.T) {
	auth, _ := newTestAuth(t)
	if _, err := auth.Register(context.Background(), "a@example.com", "short", "A"); err != ErrInvalidRequest {
		t.Fatalf("short password error = %v", err)
	}
	if _, err := auth.Register(context.Background(), "a@example.com", strings.Repeat("x", 129), "A"); err != ErrInvalidRequest {
		t.Fatalf("long password error = %v", err)
	}
	if _, err := auth.Register(context.Background(), "utf8@example.com", strings.Repeat("中", 4), "A"); err != nil {
		t.Fatalf("8-byte UTF-8 password should be accepted: %v", err)
	}
	if _, err := auth.Register(context.Background(), "max@example.com", strings.Repeat("x", 128), "A"); err != nil {
		t.Fatalf("128-byte password should be accepted: %v", err)
	}
}
