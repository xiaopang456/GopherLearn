// Package security contains small, transport-independent security primitives.
package security

import (
	"crypto/sha256"
	"errors"

	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrPasswordMismatch is returned by error-returning comparison helpers
	// when a password does not match a hash.
	ErrPasswordMismatch = errors.New("password does not match")
	// ErrInvalidPasswordHash is returned when the stored hash cannot be parsed.
	ErrInvalidPasswordHash = errors.New("invalid password hash")
	// ErrInvalidPasswordCost is returned when a bcrypt cost is outside the
	// range supported by the bcrypt implementation.
	ErrInvalidPasswordCost = errors.New("invalid password cost")
	// ErrPasswordHasherUnavailable is returned when a nil or invalid hasher is
	// used.
	ErrPasswordHasherUnavailable = errors.New("password hasher unavailable")
	// ErrInvalidPasswordInput is returned for an input type other than string
	// or []byte. It intentionally does not include the supplied value.
	ErrInvalidPasswordInput = errors.New("invalid password input")
)

// DefaultPasswordCost is the cost used by the package-level helpers and the
// zero value of Passwords.
const DefaultPasswordCost = bcrypt.DefaultCost

const passwordDerivationPrefix = "gopherlearn-bcrypt-v1:\x00"

// Passwords is the service-facing bcrypt password helper. Its zero value is
// ready to use. Hash and Compare accept either string or []byte plaintext;
// Compare returns false for a malformed hash, a mismatch, or an unsupported
// input type so callers cannot accidentally expose bcrypt's internal errors.
type Passwords struct {
	cost int
}

// NewPasswords creates a Passwords helper. With no argument, or with a cost
// of zero, bcrypt's current default cost is used. Invalid costs return nil;
// callers that need an error can use NewPasswordsWithError.
func NewPasswords(cost ...int) *Passwords {
	passwords, err := NewPasswordsWithError(cost...)
	if err != nil {
		return nil
	}
	return passwords
}

// NewPasswordsWithError creates a Passwords helper and reports an invalid
// bcrypt cost without disclosing any password data.
func NewPasswordsWithError(cost ...int) (*Passwords, error) {
	if len(cost) > 1 {
		return nil, ErrInvalidPasswordCost
	}

	selectedCost := bcrypt.DefaultCost
	if len(cost) == 1 && cost[0] != 0 {
		selectedCost = cost[0]
	}
	if selectedCost < bcrypt.MinCost || selectedCost > bcrypt.MaxCost {
		return nil, ErrInvalidPasswordCost
	}

	return &Passwords{cost: selectedCost}, nil
}

// Hash accepts a string or []byte plaintext and returns its bcrypt encoded
// hash. The plaintext is never included in an error.
func (p Passwords) Hash(plaintext any) (string, error) {
	password, err := passwordBytes(plaintext)
	if err != nil {
		return "", err
	}
	password = bcryptInput(password)

	cost := p.cost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return "", ErrPasswordHasherUnavailable
	}

	hashed, err := bcrypt.GenerateFromPassword(password, cost)
	if err != nil {
		return "", errors.New("password hashing failed")
	}
	return string(hashed), nil
}

// HashBytes is an explicit []byte-returning variant for integrations that
// store bcrypt hashes as binary data. Hash is the canonical string API.
func (p Passwords) HashBytes(plaintext []byte) ([]byte, error) {
	hashed, err := p.Hash(plaintext)
	if err != nil {
		return nil, err
	}
	return []byte(hashed), nil
}

// Compare reports whether plaintext matches hash. hash and plaintext may be
// either string or []byte. It deliberately returns only a boolean; use
// CompareError when the caller needs to distinguish mismatch from malformed
// stored data.
func (p Passwords) Compare(hash, plaintext any) bool {
	return p.CompareError(hash, plaintext) == nil
}

// CompareError compares plaintext with a bcrypt hash and returns a safe
// sentinel error on mismatch or malformed hash.
func (p Passwords) CompareError(hash, plaintext any) error {
	encodedHash, err := passwordBytes(hash)
	if err != nil {
		return ErrInvalidPasswordHash
	}
	password, err := passwordBytes(plaintext)
	if err != nil {
		return err
	}
	password = bcryptInput(password)

	cost := p.cost
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		return ErrPasswordHasherUnavailable
	}

	err = bcrypt.CompareHashAndPassword(encodedHash, password)
	if err == nil {
		return nil
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return ErrPasswordMismatch
	}
	return ErrInvalidPasswordHash
}

// Verify is an error-returning alias for CompareError.
func (p Passwords) Verify(hash, plaintext any) error {
	return p.CompareError(hash, plaintext)
}

// passwordBytes accepts only the two deliberately supported password forms.
// It copies []byte inputs before handing them to bcrypt so callers can safely
// reuse or change their input after this function returns.
func passwordBytes(value any) ([]byte, error) {
	switch input := value.(type) {
	case string:
		return []byte(input), nil
	case []byte:
		return append([]byte(nil), input...), nil
	default:
		return nil, ErrInvalidPasswordInput
	}
}

// bcryptInput preserves short password compatibility while hashing longer
// contract-allowed values through a domain-separated digest. bcrypt itself has
// a 72-byte ceiling, so the digest path prevents silent truncation.
func bcryptInput(password []byte) []byte {
	if len(password) <= 72 {
		return password
	}
	digest := sha256.Sum256(append([]byte(passwordDerivationPrefix), password...))
	return digest[:]
}
