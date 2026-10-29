package security

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTokenLifetime is used when issuing tokens through a manager created
	// by NewTokenManager.
	DefaultTokenLifetime = 24 * time.Hour

	jwtAlgorithm = "HS256"
	jwtType      = "JWT"
)

var (
	// ErrInvalidTokenManager means that a manager has no usable secret, issuer,
	// or lifetime configuration.
	ErrInvalidTokenManager = errors.New("invalid token manager configuration")
	// ErrInvalidToken is returned for malformed or otherwise invalid JWTs.
	ErrInvalidToken = errors.New("invalid token")
	// ErrInvalidTokenAlgorithm means that the JWT does not use HS256.
	ErrInvalidTokenAlgorithm = errors.New("invalid token algorithm")
	// ErrInvalidTokenSignature means that the JWT signature is absent or does
	// not match the configured secret.
	ErrInvalidTokenSignature = errors.New("invalid token signature")
	// ErrTokenIssuerMismatch means that the issuer is not the configured one.
	ErrTokenIssuerMismatch = errors.New("token issuer mismatch")
	// ErrTokenExpired means that the exp claim is not in the future.
	ErrTokenExpired = errors.New("token expired")
	// ErrInvalidTokenClaims means that required claims are missing or malformed.
	ErrInvalidTokenClaims = errors.New("invalid token claims")
	// ErrInvalidUserID means that a token was requested without a usable user ID.
	ErrInvalidUserID = errors.New("invalid user ID")
	// ErrInvalidIssueTime means that an explicit issue time was not usable.
	ErrInvalidIssueTime = errors.New("invalid issue time")
	// ErrInvalidTokenLifetime means that the configured token lifetime is not
	// positive.
	ErrInvalidTokenLifetime = errors.New("invalid token lifetime")
)

// Claims is the small, fixed claim set accepted by TokenManager. UserID and
// Subject both represent the JWT sub claim; Parse populates both so callers
// can use either service-oriented name. No other claims are emitted or
// accepted by this package.
type Claims struct {
	UserID    string    `json:"-"`
	Subject   string    `json:"-"`
	Issuer    string    `json:"-"`
	IssuedAt  time.Time `json:"-"`
	ExpiresAt time.Time `json:"-"`
}

// MarshalJSON serializes Claims using JWT NumericDate values. It is kept
// strict and emits only sub, iss, iat, and exp.
func (c Claims) MarshalJSON() ([]byte, error) {
	subject, err := c.subjectValue()
	if err != nil {
		return nil, err
	}

	return json.Marshal(jwtClaimsJSON{
		Subject:   subject,
		Issuer:    c.Issuer,
		IssuedAt:  c.IssuedAt.Unix(),
		ExpiresAt: c.ExpiresAt.Unix(),
	})
}

// UnmarshalJSON parses the fixed JWT claim set and rejects unknown, duplicate,
// missing, or incorrectly typed claims.
func (c *Claims) UnmarshalJSON(data []byte) error {
	if c == nil {
		return ErrInvalidTokenClaims
	}
	parsed, err := parseClaimsObject(data)
	if err != nil {
		return err
	}
	*c = *parsed
	return nil
}

// Valid checks the temporal portion of the claims against now. TokenManager
// additionally checks the configured issuer.
func (c Claims) Valid(now time.Time) error {
	if c.Subject == "" && c.UserID == "" {
		return ErrInvalidTokenClaims
	}
	if c.Subject != "" && c.UserID != "" && c.Subject != c.UserID {
		return ErrInvalidTokenClaims
	}
	if c.Issuer == "" || c.IssuedAt.IsZero() || c.ExpiresAt.IsZero() {
		return ErrInvalidTokenClaims
	}
	if c.ExpiresAt.Unix() <= c.IssuedAt.Unix() {
		return ErrInvalidTokenClaims
	}
	if now.IsZero() {
		return ErrInvalidIssueTime
	}
	if c.IssuedAt.Unix() > now.Unix() {
		return ErrInvalidTokenClaims
	}
	if c.ExpiresAt.Unix() <= now.Unix() {
		return ErrTokenExpired
	}
	return nil
}

func (c Claims) subjectValue() (string, error) {
	if c.Subject != "" && c.UserID != "" && c.Subject != c.UserID {
		return "", ErrInvalidTokenClaims
	}
	subject := c.Subject
	if subject == "" {
		subject = c.UserID
	}
	if subject == "" {
		return "", ErrInvalidTokenClaims
	}
	return subject, nil
}

// TokenManager issues and parses signed HS256 JWTs. Its zero value is not
// usable; construct it with NewTokenManager so the secret and issuer are
// supplied by the caller rather than hardcoded.
type TokenManager struct {
	secret   []byte
	issuer   string
	lifetime time.Duration
}

// NewTokenManager creates a token manager configured with secret and issuer.
// It validates configuration before returning, so callers can fail closed at
// startup instead of discovering a missing secret on the first request.
func NewTokenManager(secret, issuer string) (*TokenManager, error) {
	return NewTokenManagerWithLifetime(secret, issuer, DefaultTokenLifetime)
}

// NewTokenManagerWithLifetime creates a manager with an explicit positive
// lifetime. It is useful when an application has a lifetime policy other
// than DefaultTokenLifetime.
func NewTokenManagerWithLifetime(secret, issuer string, lifetime time.Duration) (*TokenManager, error) {
	manager := &TokenManager{secret: []byte(secret), issuer: issuer, lifetime: lifetime}
	if err := manager.validateConfiguration(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *TokenManager) validateConfiguration() error {
	if m == nil || len(m.secret) == 0 || m.issuer == "" {
		return ErrInvalidTokenManager
	}
	if m.lifetime <= 0 {
		return ErrInvalidTokenLifetime
	}
	return nil
}

// Issue creates an HS256 JWT for userID. With no explicit time, the current
// UTC time is used. The variadic form permits deterministic tests and callers
// with a trusted clock while preserving a simple Issue(userID) API.
func (m *TokenManager) Issue(userID any, now ...time.Time) (string, error) {
	if err := m.validateConfiguration(); err != nil {
		return "", err
	}
	if len(now) > 1 {
		return "", ErrInvalidIssueTime
	}

	subject, err := userIDString(userID)
	if err != nil {
		return "", err
	}

	issuedAt := time.Now().UTC()
	if len(now) == 1 {
		issuedAt = now[0]
	}
	if issuedAt.IsZero() {
		return "", ErrInvalidIssueTime
	}
	issuedAt = issuedAt.UTC().Truncate(time.Second)
	if issuedAt.Unix() < 0 {
		return "", ErrInvalidIssueTime
	}

	expiresAt := issuedAt.Add(m.lifetime)
	if !expiresAt.After(issuedAt) || expiresAt.Unix() <= issuedAt.Unix() {
		return "", ErrInvalidTokenLifetime
	}

	header := jwtHeader{Algorithm: jwtAlgorithm, Type: jwtType}
	payload := jwtClaimsJSON{
		Subject:   subject,
		Issuer:    m.issuer,
		IssuedAt:  issuedAt.Unix(),
		ExpiresAt: expiresAt.Unix(),
	}

	headerBytes, err := json.Marshal(header)
	if err != nil {
		return "", ErrInvalidToken
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", ErrInvalidToken
	}

	encodedHeader := base64.RawURLEncoding.EncodeToString(headerBytes)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	input := encodedHeader + "." + encodedPayload
	signature := signHS256(m.secret, []byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Parse verifies and parses a JWT using the current UTC time.
func (m *TokenManager) Parse(token string) (*Claims, error) {
	return m.ParseAt(token, time.Now().UTC())
}

// ParseAt is Parse with an explicit validation time. It is useful for
// deterministic tests and applications with a trusted clock.
func (m *TokenManager) ParseAt(token string, now time.Time) (*Claims, error) {
	if err := m.validateConfiguration(); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, ErrInvalidIssueTime
	}
	if token == "" {
		return nil, ErrInvalidToken
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, ErrInvalidToken
	}

	headerBytes, err := decodeJWTPart(parts[0])
	if err != nil {
		return nil, ErrInvalidToken
	}
	if err := validateHeader(headerBytes); err != nil {
		return nil, err
	}

	signature, err := decodeJWTPart(parts[2])
	if err != nil || len(signature) != sha256.Size {
		return nil, ErrInvalidTokenSignature
	}
	expected := signHS256(m.secret, []byte(parts[0]+"."+parts[1]))
	if !hmac.Equal(signature, expected) {
		return nil, ErrInvalidTokenSignature
	}

	payloadBytes, err := decodeJWTPart(parts[1])
	if err != nil {
		return nil, ErrInvalidToken
	}
	claims, err := parseClaimsObject(payloadBytes)
	if err != nil {
		return nil, err
	}
	if claims.Issuer != m.issuer {
		return nil, ErrTokenIssuerMismatch
	}
	if err := claims.Valid(now.UTC()); err != nil {
		return nil, err
	}
	return claims, nil
}

func signHS256(secret, message []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(message)
	return mac.Sum(nil)
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type jwtClaimsJSON struct {
	Subject   string `json:"sub"`
	Issuer    string `json:"iss"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

func userIDString(value any) (string, error) {
	var result string
	switch id := value.(type) {
	case string:
		result = id
	case []byte:
		result = string(id)
	case int:
		result = strconv.Itoa(id)
	case int8:
		result = strconv.FormatInt(int64(id), 10)
	case int16:
		result = strconv.FormatInt(int64(id), 10)
	case int32:
		result = strconv.FormatInt(int64(id), 10)
	case int64:
		result = strconv.FormatInt(id, 10)
	case uint:
		result = strconv.FormatUint(uint64(id), 10)
	case uint8:
		result = strconv.FormatUint(uint64(id), 10)
	case uint16:
		result = strconv.FormatUint(uint64(id), 10)
	case uint32:
		result = strconv.FormatUint(uint64(id), 10)
	case uint64:
		result = strconv.FormatUint(id, 10)
	default:
		return "", ErrInvalidUserID
	}
	if result == "" {
		return "", ErrInvalidUserID
	}
	return result, nil
}

func decodeJWTPart(encoded string) ([]byte, error) {
	// JWT compact parts use unpadded base64url. Reject alternate alphabets and
	// padding instead of silently accepting a non-canonical representation.
	if encoded == "" || strings.ContainsAny(encoded, "=+/\r\n\t ") {
		return nil, ErrInvalidToken
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrInvalidToken
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, ErrInvalidToken
	}
	return decoded, nil
}

func validateHeader(data []byte) error {
	fields, err := decodeJSONObject(data)
	if err != nil {
		return ErrInvalidToken
	}
	for key := range fields {
		if key != "alg" && key != "typ" {
			return ErrInvalidToken
		}
	}

	algorithm, ok := stringClaim(fields, "alg")
	if !ok {
		return ErrInvalidTokenAlgorithm
	}
	if algorithm != jwtAlgorithm {
		return ErrInvalidTokenAlgorithm
	}
	if rawType, exists := fields["typ"]; exists {
		typ, ok := stringClaimValue(rawType)
		if !ok || typ != jwtType {
			return ErrInvalidToken
		}
	}
	return nil
}

func parseClaimsObject(data []byte) (*Claims, error) {
	fields, err := decodeJSONObject(data)
	if err != nil {
		return nil, ErrInvalidTokenClaims
	}
	for key := range fields {
		switch key {
		case "sub", "iss", "iat", "exp":
		default:
			return nil, ErrInvalidTokenClaims
		}
	}
	if len(fields) != 4 {
		return nil, ErrInvalidTokenClaims
	}

	subject, ok := stringClaim(fields, "sub")
	if !ok || subject == "" {
		return nil, ErrInvalidTokenClaims
	}
	issuer, ok := stringClaim(fields, "iss")
	if !ok || issuer == "" {
		return nil, ErrInvalidTokenClaims
	}
	issuedAt, ok := numericDateClaim(fields, "iat")
	if !ok {
		return nil, ErrInvalidTokenClaims
	}
	expiresAt, ok := numericDateClaim(fields, "exp")
	if !ok || expiresAt <= issuedAt {
		return nil, ErrInvalidTokenClaims
	}

	return &Claims{
		UserID:    subject,
		Subject:   subject,
		Issuer:    issuer,
		IssuedAt:  time.Unix(issuedAt, 0).UTC(),
		ExpiresAt: time.Unix(expiresAt, 0).UTC(),
	}, nil
}

func stringClaim(fields map[string]json.RawMessage, name string) (string, bool) {
	value, ok := fields[name]
	if !ok {
		return "", false
	}
	return stringClaimValue(value)
}

func stringClaimValue(value []byte) (string, bool) {
	var result string
	if err := json.Unmarshal(value, &result); err != nil {
		return "", false
	}
	return result, true
}

func numericDateClaim(fields map[string]json.RawMessage, name string) (int64, bool) {
	value, ok := fields[name]
	if !ok {
		return 0, false
	}
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 {
		return 0, false
	}
	for index, char := range trimmed {
		if (char == '-' && index == 0) || (char >= '0' && char <= '9') {
			continue
		}
		return 0, false
	}
	parsed, err := strconv.ParseInt(string(trimmed), 10, 64)
	return parsed, err == nil
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, ErrInvalidToken
	}

	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, ErrInvalidToken
		}
		key, ok := keyToken.(string)
		if !ok || key == "" {
			return nil, ErrInvalidToken
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrInvalidToken
		}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrInvalidToken
		}
		fields[key] = append(json.RawMessage(nil), value...)
	}

	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, ErrInvalidToken
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidToken
	}
	return fields, nil
}
