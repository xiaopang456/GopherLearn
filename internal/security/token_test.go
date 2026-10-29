package security

import (
	"strings"
	"testing"
	"time"
)

func TestTokenManagerValidExpiredAndTampered(t *testing.T) {
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	manager, err := NewTokenManagerWithLifetime("test-secret", "gopherlearn-test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := manager.Issue("user-1", base)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := manager.ParseAt(token, base.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if claims.UserID != "user-1" || claims.Issuer != "gopherlearn-test" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
	if _, err := manager.ParseAt(token, base.Add(2*time.Hour)); err != ErrTokenExpired {
		t.Fatalf("expected expiry error, got %v", err)
	}
	parts := strings.Split(token, ".")
	parts[1] = parts[1] + "a"
	if _, err := manager.ParseAt(strings.Join(parts, "."), base.Add(30*time.Minute)); err == nil {
		t.Fatal("tampered token was accepted")
	}
}

func TestTokenManagerRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewTokenManager("", "issuer"); err == nil {
		t.Fatal("empty secret was accepted")
	}
	if _, err := NewTokenManager("secret", ""); err == nil {
		t.Fatal("empty issuer was accepted")
	}
}
