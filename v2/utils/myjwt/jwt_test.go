package myjwt

import (
	"GopherAI/config"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestMain(m *testing.M) {
	// The token helpers read the process-wide configuration. This repository
	// ships no config/config.toml, so the test installs the settings it needs
	// instead of depending on a file that is not there. The values are the
	// ones the assertions below depend on: a signing key, an expiry and the
	// issuer and subject a foreign token must not carry.
	if err := config.Use(&config.Config{JwtConfig: config.JwtConfig{
		ExpireDuration: 1,
		Issuer:         "gopherlearn-test",
		Subject:        "gopherlearn-test-user",
		Key:            "test-only-jwt-secret-with-at-least-32-bytes",
	}}); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestGenerateAndParseTokenPreservesIdentity(t *testing.T) {
	token, err := GenerateToken(42, "10000000001")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := ParseToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.ID != 42 || claims.Username != "10000000001" {
		t.Fatalf("identity changed: %+v", claims)
	}
}

func TestParseTokenRejectsExpiredToken(t *testing.T) {
	claims := Claims{ID: 42, Username: "user", RegisteredClaims: jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(config.GetConfig().Key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(token); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestParseTokenRejectsUnexpectedSigningMethod(t *testing.T) {
	claims := Claims{ID: 42, Username: "user", RegisteredClaims: jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString([]byte(config.GetConfig().Key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(token); err == nil {
		t.Fatal("expected HS384 token to be rejected")
	}
}

func TestParseTokenRejectsUnexpectedIssuerOrSubject(t *testing.T) {
	claims := Claims{ID: 42, Username: "user", RegisteredClaims: jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)), Issuer: "other", Subject: "other",
	}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(config.GetConfig().Key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseToken(token); err == nil {
		t.Fatal("expected foreign issuer and subject to be rejected")
	}
}
