package utils

import (
	"crypto/md5" // #nosec G501 -- required only to verify and migrate legacy password rows.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	PasswordBcryptCost = 12
	minPasswordRunes   = 8
	maxPasswordRunes   = 64
	maxBcryptBytes     = 72
	bcryptSHA256Prefix = "$gopherai$bcrypt-sha256$"
)

var ErrIllegalPassword = errors.New("password does not satisfy the storage policy")

// ValidateNewPassword applies only to newly chosen passwords. Legacy users
// must be allowed to authenticate before their old hash can be upgraded.
func ValidateNewPassword(password string) error {
	runes := utf8.RuneCountInString(password)
	if runes < minPasswordRunes || runes > maxPasswordRunes || len(password) > maxBcryptBytes {
		return ErrIllegalPassword
	}
	return nil
}

func HashPassword(password string) (string, error) {
	if err := ValidateNewPassword(password); err != nil {
		return "", err
	}
	return hashVerifiedPassword(password)
}

// VerifyPassword recognizes current bcrypt rows and the exact 32-hex legacy
// MD5 shape. Unknown formats are treated as invalid credentials.
func VerifyPassword(storedHash, password string) (valid bool, needsUpgrade bool) {
	if strings.HasPrefix(storedHash, bcryptSHA256Prefix) {
		hash := strings.TrimPrefix(storedHash, bcryptSHA256Prefix)
		digest := sha256.Sum256([]byte(password))
		if bcrypt.CompareHashAndPassword([]byte(hash), digest[:]) != nil {
			return false, false
		}
		cost, err := bcrypt.Cost([]byte(hash))
		return true, err == nil && cost < PasswordBcryptCost
	}
	if strings.HasPrefix(storedHash, "$2a$") || strings.HasPrefix(storedHash, "$2b$") || strings.HasPrefix(storedHash, "$2y$") {
		if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(password)) != nil {
			return false, false
		}
		cost, err := bcrypt.Cost([]byte(storedHash))
		return true, err == nil && cost < PasswordBcryptCost
	}
	if len(storedHash) != md5.Size*2 {
		return false, false
	}
	decoded, err := hex.DecodeString(storedHash)
	if err != nil || len(decoded) != md5.Size {
		return false, false
	}
	legacy := md5.Sum([]byte(password)) // #nosec G401 -- comparison for a retired storage format.
	if subtle.ConstantTimeCompare(decoded, legacy[:]) != 1 {
		return false, false
	}
	return true, true
}

// HashVerifiedPassword is used after a legacy password has already been
// authenticated. It deliberately does not impose the new minimum length,
// otherwise existing users with short passwords would be locked out.
func HashVerifiedPassword(password string) (string, error) {
	if len(password) > maxBcryptBytes {
		digest := sha256.Sum256([]byte(password))
		hash, err := bcrypt.GenerateFromPassword(digest[:], PasswordBcryptCost)
		if err != nil {
			return "", err
		}
		return bcryptSHA256Prefix + string(hash), nil
	}
	return hashVerifiedPassword(password)
}

func hashVerifiedPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), PasswordBcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}
