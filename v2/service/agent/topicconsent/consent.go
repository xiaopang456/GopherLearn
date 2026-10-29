// Package topicconsent issues and verifies the request-scoped ticket that lets
// a user, rather than a model, authorise which topic's personal state one
// answer may read.
//
// The ticket exists because the selected TopicKeys arrive from the browser, and
// a TopicKey the server merely believes came from its own suggestion list is
// not an authorisation. Everything that makes the choice meaningful - which
// user, which chat session, which request, which question, and which candidates
// were actually offered - is bound into a signature the client cannot forge,
// and every one of those bindings is re-checked before any personal record is
// read.
//
// It is deliberately not a database row. STABILIZE-050 v1 grants "this answer"
// and nothing more, so the grant needs no lifecycle, no revocation path and no
// migration: the next question simply has no ticket. A persistent grant is a
// permissions table, and a permissions table is a much larger promise than this
// task is allowed to make.
package topicconsent

import (
	"GopherAI/common/memoryrecall"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxSelectableTopics is the hard ceiling on how much personal state one answer
// may draw on.
//
// Two is the same bound STABILIZE-049 put on its multi shape: a genuine
// two-subject comparison exists and forcing every question to a single topic
// would structurally lose half the personalisation, while a third topic has no
// observed case behind it and would just be budget nobody asked for.
const MaxSelectableTopics = memoryrecall.TopicConsentMaxSelectable

// MaxCandidates bounds the suggestion list. Beyond a handful the user is being
// asked to do the classification the server just admitted it cannot do.
const MaxCandidates = memoryrecall.TopicConsentMaxCandidates

// DefaultTTL is how long a suggestion stays usable. It is long enough to read
// the candidates and decide, and short enough that a ticket left in a closed
// tab is not an authorisation tomorrow.
const DefaultTTL = 10 * time.Minute

// MinSecretLength refuses a signing key short enough to brute force. The mode
// that needs this package fails startup without one rather than falling back to
// an unsigned ticket.
const MinSecretLength = memoryrecall.TopicConsentMinSecretLength

var (
	// ErrNoTicket means the request carried no consent at all. It is not an
	// error the caller reports to the user: it is ordinary chat without topic
	// personalisation, which is the designed behaviour.
	ErrNoTicket = errors.New("topic consent: no ticket supplied")
	// ErrInvalidTicket covers every failed binding. The reasons are
	// deliberately not distinguished to the caller beyond logging: a client
	// that learns which field failed learns how to search the space.
	ErrInvalidTicket = errors.New("topic consent: ticket is not valid for this request")
	// ErrTooManyTopics is separate because it is a client bug rather than a
	// forgery signal, and the count is safe to report.
	ErrTooManyTopics = errors.New("topic consent: at most two topics may be selected")
	// ErrInvalidSelection means the final catalogue selection is empty or no
	// longer belongs to the authenticated learner. It is checked before a
	// ticket is issued and is never treated as a partial grant.
	ErrInvalidSelection = errors.New("topic consent: selected topics are not valid")
	// ErrNotConfigured means the signer was never built. Failing here is how a
	// misconfigured deployment stops rather than silently accepting tickets.
	ErrNotConfigured = errors.New("topic consent: signer is not configured")
)

// Candidate is one suggested topic as the browser sees it.
//
// It carries no mastery level, no weak point, no note text and no counts. The
// whole point of the suggestion stage is that nothing personal about the topic
// leaves the server until the user has said yes, and a struct that could carry
// a level would eventually carry one.
type Candidate struct {
	TopicKey    string `json:"topicKey"`
	DisplayName string `json:"displayName"`
	// Source names where the suggestion came from, so the UI can explain
	// itself and so a report can separate the channels. It is a provenance
	// label, never a score.
	Source string `json:"candidateSource"`
}

// Candidate sources. They are constants because they are compared, rendered and
// asserted on in tests.
const (
	SourceGradedTopic     = "graded_topic"
	SourceLexicalMatch    = "lexical_match"
	SourceRecentNotes     = "recent_notes"
	SourceCatalogueBrowse = "catalogue_browse"
)

// Claims is the signed payload. Every field is a binding that verification
// re-checks; none of them is advisory.
type Claims struct {
	UserID       int64    `json:"uid"`
	SessionID    string   `json:"sid"`
	RequestID    string   `json:"rid"`
	QuestionHash string   `json:"qh"`
	Candidates   []string `json:"cand"`
	IssuedAt     int64    `json:"iat"`
	ExpiresAt    int64    `json:"exp"`
	Nonce        string   `json:"nonce"`
}

// Signer issues and verifies tickets.
type Signer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

// NewSigner fails closed on a weak or missing secret.
func NewSigner(secret string, ttl time.Duration) (*Signer, error) {
	trimmed := strings.TrimSpace(secret)
	if len(trimmed) < MinSecretLength {
		return nil, fmt.Errorf("%w: signing secret must be at least %d characters", ErrNotConfigured, MinSecretLength)
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > time.Hour {
		return nil, fmt.Errorf("%w: ticket lifetime must not exceed one hour", ErrNotConfigured)
	}
	return &Signer{secret: []byte(trimmed), ttl: ttl, now: time.Now}, nil
}

// UseClock fixes the clock for deterministic tests. Production never calls it.
func (s *Signer) UseClock(now func() time.Time) {
	if s != nil && now != nil {
		s.now = now
	}
}

// TTL reports the configured lifetime so a caller can tell the browser when the
// suggestion stops being usable.
func (s *Signer) TTL() time.Duration {
	if s == nil {
		return 0
	}
	return s.ttl
}

// QuestionHash is the binding between a ticket and the text it was issued for.
//
// It is a hash rather than the question itself so the ticket, which travels
// through the browser, does not carry the learner's words. Whitespace is
// normalised first: a question that only differs by a re-wrapped newline is the
// same question, and treating it as a different one would break the idempotent
// retry the run path depends on.
func QuestionHash(question string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(question), " ")))
	return hex.EncodeToString(sum[:])
}

// Issue signs a ticket for one suggestion list.
func (s *Signer) Issue(userID int64, sessionID, requestID, question string, candidates []Candidate) (string, Claims, error) {
	if s == nil {
		return "", Claims{}, ErrNotConfigured
	}
	if userID <= 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(requestID) == "" ||
		strings.TrimSpace(question) == "" {
		return "", Claims{}, fmt.Errorf("%w: identity fields are required", ErrInvalidTicket)
	}
	if len(candidates) > MaxCandidates {
		return "", Claims{}, fmt.Errorf("%w: at most %d candidates", ErrInvalidTicket, MaxCandidates)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", Claims{}, fmt.Errorf("topic consent: nonce: %w", err)
	}
	issued := s.now().UTC()
	claims := Claims{
		UserID: userID, SessionID: strings.TrimSpace(sessionID), RequestID: strings.TrimSpace(requestID),
		QuestionHash: QuestionHash(question), Candidates: normalizeKeys(candidateKeys(candidates)),
		IssuedAt: issued.Unix(), ExpiresAt: issued.Add(s.ttl).Unix(),
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", Claims{}, fmt.Errorf("topic consent: encode: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + s.sign(encoded), claims, nil
}

// VerifyRequest is what the run path checks a submitted consent against.
type VerifyRequest struct {
	Ticket    string
	Selected  []string
	UserID    int64
	SessionID string
	RequestID string
	Question  string
}

// Verify re-checks every binding and returns the topic keys this answer may
// read. It is the only way a TopicKey from a client becomes readable.
//
// The order matters: the signature is checked before any field is trusted, so a
// forged payload never reaches the comparisons, and the selection subset check
// happens last so that an attacker cannot use its outcome to probe the
// candidate list of a ticket they could not sign.
func (s *Signer) Verify(request VerifyRequest) ([]string, error) {
	if s == nil {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(request.Ticket) == "" {
		return nil, ErrNoTicket
	}
	if len(request.Selected) > MaxSelectableTopics {
		return nil, fmt.Errorf("%w: %d selected", ErrTooManyTopics, len(request.Selected))
	}
	encoded, signature, found := strings.Cut(strings.TrimSpace(request.Ticket), ".")
	if !found {
		return nil, fmt.Errorf("%w: malformed", ErrInvalidTicket)
	}
	if subtle.ConstantTimeCompare([]byte(signature), []byte(s.sign(encoded))) != 1 {
		return nil, fmt.Errorf("%w: signature", ErrInvalidTicket)
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: payload", ErrInvalidTicket)
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims", ErrInvalidTicket)
	}
	if claims.UserID != request.UserID {
		return nil, fmt.Errorf("%w: user", ErrInvalidTicket)
	}
	if claims.SessionID != strings.TrimSpace(request.SessionID) {
		return nil, fmt.Errorf("%w: session", ErrInvalidTicket)
	}
	if claims.RequestID != strings.TrimSpace(request.RequestID) {
		return nil, fmt.Errorf("%w: request", ErrInvalidTicket)
	}
	if claims.QuestionHash != QuestionHash(request.Question) {
		return nil, fmt.Errorf("%w: question", ErrInvalidTicket)
	}
	now := s.now().UTC().Unix()
	if claims.ExpiresAt <= now || claims.IssuedAt > now+60 {
		return nil, fmt.Errorf("%w: expired", ErrInvalidTicket)
	}
	allowed := map[string]struct{}{}
	for _, key := range claims.Candidates {
		allowed[key] = struct{}{}
	}
	selected := normalizeKeys(request.Selected)
	if len(selected) > MaxSelectableTopics {
		return nil, fmt.Errorf("%w: %d distinct selected", ErrTooManyTopics, len(selected))
	}
	for _, key := range selected {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("%w: %q was not offered", ErrInvalidTicket, key)
		}
	}
	return selected, nil
}

func (s *Signer) sign(encoded string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func candidateKeys(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.TopicKey)
	}
	return out
}

// normalizeKeys trims, drops empties and de-duplicates, then sorts.
//
// Sorting is what makes the signature independent of the order the browser sent
// the candidates in, and de-duplication is what stops a client from spending
// its two-topic budget on one topic twice to get a third slot elsewhere.
func normalizeKeys(keys []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(keys))
	for _, raw := range keys {
		key := strings.TrimSpace(raw)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
