package utils

import (
	"crypto/md5"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestPasswordHashAndVerification(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(hash, "5f4dcc3b5aa765d61d8327deb882cf99") {
		t.Fatal("new password was stored as legacy MD5")
	}
	valid, upgrade := VerifyPassword(hash, "correct horse battery staple")
	if !valid || upgrade {
		t.Fatalf("current bcrypt verification valid=%v upgrade=%v", valid, upgrade)
	}
	if valid, _ := VerifyPassword(hash, "wrong password"); valid {
		t.Fatal("wrong password matched bcrypt hash")
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil || cost != PasswordBcryptCost {
		t.Fatalf("bcrypt cost=%d err=%v", cost, err)
	}
}

func TestLegacyPasswordRequiresUpgrade(t *testing.T) {
	legacy := fmt.Sprintf("%x", md5.Sum([]byte("legacy-pass"))) // #nosec G401 -- migration fixture.
	valid, upgrade := VerifyPassword(legacy, "legacy-pass")
	if !valid || !upgrade {
		t.Fatalf("legacy verification valid=%v upgrade=%v", valid, upgrade)
	}
	if valid, _ := VerifyPassword(legacy, "wrong"); valid {
		t.Fatal("wrong password matched legacy hash")
	}
	if valid, _ := VerifyPassword("not-a-supported-hash", "legacy-pass"); valid {
		t.Fatal("unknown password format was accepted")
	}
}

func TestLongLegacyPasswordCanUpgradeThroughSHA256Prehash(t *testing.T) {
	password := strings.Repeat("legacy-long-password-", 8)
	legacy := fmt.Sprintf("%x", md5.Sum([]byte(password))) // #nosec G401 -- migration fixture.
	valid, upgrade := VerifyPassword(legacy, password)
	if !valid || !upgrade {
		t.Fatalf("legacy verification valid=%v upgrade=%v", valid, upgrade)
	}
	hash, err := HashVerifiedPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, bcryptSHA256Prefix) {
		t.Fatalf("long legacy hash prefix=%q", hash)
	}
	valid, upgrade = VerifyPassword(hash, password)
	if !valid || upgrade {
		t.Fatalf("migrated long password valid=%v upgrade=%v", valid, upgrade)
	}
}

func TestLowCostBcryptRequiresUpgrade(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("legacy-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	valid, upgrade := VerifyPassword(string(hash), "legacy-pass")
	if !valid || !upgrade {
		t.Fatalf("low-cost bcrypt valid=%v upgrade=%v", valid, upgrade)
	}
}

func TestNewPasswordPolicy(t *testing.T) {
	for _, password := range []string{"", "1234567", strings.Repeat("a", 65), strings.Repeat("界", 25)} {
		if err := ValidateNewPassword(password); err == nil {
			t.Fatalf("password with %d bytes unexpectedly accepted", len(password))
		}
	}
	for _, password := range []string{"12345678", strings.Repeat("a", 64), "安全密码-12345"} {
		if err := ValidateNewPassword(password); err != nil {
			t.Fatalf("password %q rejected: %v", password, err)
		}
	}
	if _, err := HashVerifiedPassword("short"); err != nil {
		t.Fatalf("verified legacy short password cannot be upgraded: %v", err)
	}
}

func TestSecureRandomDigits(t *testing.T) {
	value, err := SecureRandomDigits(32)
	if err != nil {
		t.Fatal(err)
	}
	if len(value) != 32 {
		t.Fatalf("length=%d", len(value))
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			t.Fatalf("non-digit %q", char)
		}
	}
	if _, err := SecureRandomDigits(0); err == nil {
		t.Fatal("zero random length accepted")
	}
}
