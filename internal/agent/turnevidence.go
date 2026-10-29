package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// This file carries the one turn evidence object: a single, per-turn registry
// that answers "which citations exist", "which records has this turn already
// shown", and "which document versions may be read back" for every tool of the
// turn.
//
// It is a faithful restoration of the reference project's split authorities,
// merged into one value on purpose. The reference kept a document evidence set
// (stable citation numbers, content dedup, read-back eligibility) in its tool
// package and a memory evidence registry (G/S/E/C references over the
// learner's own records) in its agent package, created once per run and shared
// by the context builder, the tools and the final citation check. Keeping two
// instances here - one per window, one per tool package - is exactly how a
// citation the model legally saw gets refused by whichever registry never
// issued it, so there is one.
//
// The token budget is deliberately absent. contextwindow.Evidence is this
// turn's shared allowance for evidence bodies, installed on the same context;
// a tool that charges it honours the same pool as its neighbours. This
// registry owns identity, not money.

// The memory evidence kinds. The letter a reference carries declares the kind,
// so "the reference does not exist" and "the reference exists and is a
// different kind" stay two distinguishable failures. A stated preference
// cannot support "we decided X last time", and the kind is what makes that
// checkable instead of hoped for.
const (
	// MemoryEvidenceGlobal is stable profile: teaching preferences, long-running
	// goals, standing prohibitions.
	MemoryEvidenceGlobal = "global"
	// MemoryEvidenceTopic is this learner's own understanding of one subject.
	MemoryEvidenceTopic = "topic_semantic"
	// MemoryEvidenceEpisodic is something that happened in an earlier session.
	MemoryEvidenceEpisodic = "episodic"
	// MemoryEvidenceCurrent is a message the learner already sent in this
	// conversation.
	MemoryEvidenceCurrent = "current_message"
)

// memoryEvidencePrefix is the letter a reference to this kind carries.
func memoryEvidencePrefix(kind string) string {
	switch kind {
	case MemoryEvidenceGlobal:
		return "G"
	case MemoryEvidenceTopic:
		return "S"
	case MemoryEvidenceEpisodic:
		return "E"
	case MemoryEvidenceCurrent:
		return "C"
	default:
		return ""
	}
}

// memoryKindForPrefix resolves the kind behind one reference letter.
func memoryKindForPrefix(prefix string) (string, bool) {
	switch strings.ToUpper(prefix) {
	case "G":
		return MemoryEvidenceGlobal, true
	case "S":
		return MemoryEvidenceTopic, true
	case "E":
		return MemoryEvidenceEpisodic, true
	case "C":
		return MemoryEvidenceCurrent, true
	default:
		return "", false
	}
}

// MemoryEvidenceEntry is one citable piece of evidence with both halves of its
// identity: the request-scoped label the model saw, and the server-side record
// it stands for. InternalID, Version, Owner and Status never leave the server:
// a model that could name them could ask for them.
type MemoryEvidenceEntry struct {
	Ref        string
	Kind       string
	InternalID string
	Version    uint64
	Owner      string
	Status     string
	Source     string
	Subject    string
	// MessageID is set only for current-conversation evidence.
	MessageID string
}

// MemoryRefError reports the two ways a memory citation can be wrong that a
// server can decide on its own.
var (
	// ErrUnknownMemoryRef is a reference this turn never issued: out of range,
	// invented, or carried over from a turn that has ended.
	ErrUnknownMemoryRef = fmt.Errorf("unknown memory evidence reference")
	// ErrMemoryKindMismatch is a reference that exists but was cited as the
	// wrong kind - most consequentially, a preference cited as a past decision.
	ErrMemoryKindMismatch = fmt.Errorf("memory evidence reference cited as the wrong kind")
)

// TurnEvidence is one turn's citation and evidence authority.
//
// It lives on the turn's own stack, is created once by the harness before any
// tool runs, and is reachable from the context. A shared instance across turns
// would let one learner's citations leak into another's transcript and would
// let a repeat turn believe it had already shown records it has not.
type TurnEvidence struct {
	mu sync.Mutex

	// Document half: stable citations and dedup, as the reference project's
	// shared evidence set owned them.
	maxTokens        int
	usedTokens       int
	nextCitation     int
	citations        map[string]int
	contentEmitted   map[string]bool
	allowedDocuments map[string]struct{}
	// budget is the single writable evidence authority for this turn.
	budget *contextwindow.Evidence

	// Memory half: request-scoped references over the learner's own records.
	counter         int
	entries         map[string]MemoryEvidenceEntry
	order           []string
	issued          map[string]struct{}
	identityLabel   map[string]string
	memoryAttempted bool

	// Grounding: every retrieval result shown to the model this turn, with the
	// citation number each carries. The citation check and the repair builder
	// read this instead of keeping their own copy.
	grounded []GroundedSource
}

// NewTurnEvidence returns an empty registry for one turn, with the document
// half's token ceiling reported alongside the budget it mirrors. The ceiling is
// informational here - charging stays with contextwindow.Evidence - and exists
// so a report can show the two figures came from the same configuration.
func NewTurnEvidence(maxTokens int) *TurnEvidence {
	if maxTokens < 0 {
		maxTokens = 0
	}
	return &TurnEvidence{
		maxTokens:        maxTokens,
		citations:        make(map[string]int),
		contentEmitted:   make(map[string]bool),
		allowedDocuments: make(map[string]struct{}),
		entries:          make(map[string]MemoryEvidenceEntry),
		issued:           make(map[string]struct{}),
		identityLabel:    make(map[string]string),
	}
}

// AttachBudget binds registry metadata to the turn's sole writable evidence authority.
func (e *TurnEvidence) AttachBudget(budget *contextwindow.Evidence) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.budget = budget
}

// issuedRecordKey is the identity two memory tools compare against. Kind is
// part of it because a topic note and an episode may share an id in two
// different tables, and version is part of it because a record the learner
// edited between one call and the next really is something the model has not
// read.
func issuedRecordKey(kind, id string, version uint64) string {
	return fmt.Sprintf("%s|%s|%d", kind, strings.TrimSpace(id), version)
}

// IssuedRecord reports whether this turn has already handed the model this
// exact record, under any label and from any component.
func (e *TurnEvidence) IssuedRecord(kind, id string, version uint64) bool {
	if e == nil || strings.TrimSpace(id) == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, exists := e.issued[issuedRecordKey(kind, id, version)]
	return exists
}

// Register issues the next reference for one piece of evidence and returns it.
// A kind without a prefix registers nothing: an unknown kind has no letter the
// citation check could resolve.
func (e *TurnEvidence) Register(entry MemoryEvidenceEntry) string {
	if e == nil || memoryEvidencePrefix(entry.Kind) == "" {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counter++
	entry.Ref = fmt.Sprintf("%s%d", memoryEvidencePrefix(entry.Kind), e.counter)
	e.entries[entry.Ref] = entry
	e.order = append(e.order, entry.Ref)
	if strings.TrimSpace(entry.InternalID) != "" {
		key := issuedRecordKey(entry.Kind, entry.InternalID, entry.Version)
		e.issued[key] = struct{}{}
		e.identityLabel[key] = entry.Ref
	}
	return entry.Ref
}

// Lookup resolves one reference exactly as the model wrote it.
//
// The number and slot-less letter identify the entry; the letter is the
// model's claim about its kind. Resolving on the identity first is what makes
// a wrong letter a kind mismatch rather than an unknown reference.
func (e *TurnEvidence) Lookup(ref string) (MemoryEvidenceEntry, error) {
	if e == nil {
		return MemoryEvidenceEntry{}, ErrUnknownMemoryRef
	}
	prefix, number, ok := splitMemoryRef(ref)
	if !ok {
		return MemoryEvidenceEntry{}, ErrUnknownMemoryRef
	}
	kind, known := memoryKindForPrefix(prefix)
	if !known {
		return MemoryEvidenceEntry{}, ErrUnknownMemoryRef
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, existing := range e.entries {
		_, existingNumber, existingOK := splitMemoryRef(existing.Ref)
		if !existingOK || existingNumber != number {
			continue
		}
		if existing.Kind != kind {
			return existing, fmt.Errorf("%w: %s is %s", ErrMemoryKindMismatch, ref, existing.Kind)
		}
		return existing, nil
	}
	return MemoryEvidenceEntry{}, fmt.Errorf("%w: %s", ErrUnknownMemoryRef, ref)
}

// splitMemoryRef parses a reference into its kind letter and number. It
// accepts the same shapes the citation pattern extracts: [G1], [S12].
func splitMemoryRef(ref string) (string, int, bool) {
	ref = strings.TrimSpace(strings.Trim(ref, "[]"))
	if len(ref) < 2 {
		return "", 0, false
	}
	number, err := strconv.Atoi(ref[1:])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	return strings.ToUpper(ref[:1]), number, true
}

// Entries returns the registered memory evidence in issue order.
func (e *TurnEvidence) Entries() []MemoryEvidenceEntry {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]MemoryEvidenceEntry, 0, len(e.order))
	for _, ref := range e.order {
		out = append(out, e.entries[ref])
	}
	return out
}

// HasLongTermEvidence reports whether the turn holds any topic or episodic
// evidence - the two kinds that come from a search or a read-back rather than
// from the window.
func (e *TurnEvidence) HasLongTermEvidence() bool {
	for _, entry := range e.Entries() {
		if entry.Kind == MemoryEvidenceTopic || entry.Kind == MemoryEvidenceEpisodic {
			return true
		}
	}
	return false
}

// Attempted reports whether any memory search or read-back ran at all this
// turn. It is set by the tools through RecordMemoryAttempt.
func (e *TurnEvidence) Attempted() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.memoryAttempted
}

// LegalRefSummary is the list a repair call is given: every reference this
// turn actually issued, with its kind and subject and nothing else.
func (e *TurnEvidence) LegalRefSummary() string {
	entries := e.Entries()
	if len(entries) == 0 {
		return "(this turn holds no memory evidence at all)"
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		subject := strings.TrimSpace(entry.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		lines = append(lines, fmt.Sprintf("[%s] kind=%s subject=%s", entry.Ref, entry.Kind, subject))
	}
	return strings.Join(lines, "\n")
}

// Document citation half.
//
// The identity of a document citation is the same string the knowledge tools
// build: collection/document plus chunk or range, at a version. A citation the
// search issued in round one keeps its number in round three, so an answer
// that cites [2] in the third round cites the same passage it named there.

// CitationFor returns the stable citation number for one evidence identity,
// issuing the next number when the identity is new.
func (e *TurnEvidence) CitationFor(identity string) int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.citationForLocked(identity)
}

func (e *TurnEvidence) citationForLocked(identity string) int {
	if citation, exists := e.citations[identity]; exists {
		return citation
	}
	e.nextCitation++
	e.citations[identity] = e.nextCitation
	return e.nextCitation
}

// HasCitation reports whether this identity was ever issued a number.
func (e *TurnEvidence) HasCitation(identity string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, exists := e.citations[identity]
	return exists
}

// KnownCitations returns every citation number this turn has issued.
func (e *TurnEvidence) KnownCitations() map[int]struct{} {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	known := make(map[int]struct{}, len(e.citations))
	for _, citation := range e.citations {
		known[citation] = struct{}{}
	}
	return known
}

// BodySent reports whether this exact display unit already reached the model
// in this turn. A repeated unit keeps its citation and drops its body: the
// text is still in front of the model, in the earlier message.
func (e *TurnEvidence) BodySent(key string) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	budget := e.budget
	e.mu.Unlock()
	if budget == nil {
		return false
	}
	return budget.BodySent(key)
}

// MarkBodySent records that this display unit reached the model. Only a unit
// whose text actually went out may be marked, or a later call would read an
// omission as a quotation.
func (e *TurnEvidence) MarkBodySent(key string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	budget := e.budget
	e.mu.Unlock()
	if budget != nil {
		budget.MarkBodySent(key)
	}
}

// AllowDocument records that one document version was surfaced to the model by
// search, making it eligible for a follow-up read. Only versions that passed
// here may be read back: the model cannot name a document id into eligibility.
func (e *TurnEvidence) AllowDocument(documentID string, version int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.allowedDocuments[documentVersionKey(documentID, version)] = struct{}{}
}

// DocumentAllowed reports whether one document version was surfaced by search
// this turn.
func (e *TurnEvidence) DocumentAllowed(documentID string, version int) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, exists := e.allowedDocuments[documentVersionKey(documentID, version)]
	return exists
}

func documentVersionKey(documentID string, version int) string {
	return fmt.Sprintf("%s:v%d", boundedEvidenceText(documentID, 128), version)
}

// RecordMemoryAttempt notes that a long-term-memory tool ran this turn, whether
// or not it returned anything. The final check needs it to tell "the model
// cited memory without searching" from "the tools were never available".
func (e *TurnEvidence) RecordMemoryAttempt() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.memoryAttempted = true
}

// RetainCitedSources is how the turn's grounding set is narrowed to what the
// answer actually cited. The harness holds the collected sources and hands the
// whole slice in; the survivor order is the collection order.
//
// It lives here rather than as bare harness state because the citation check,
// the repair message builder and the final source list all need the same
// narrowed view, and a second copy of the set is how they drift.
func (e *TurnEvidence) RetainCitedSources(retain func(key string) bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	kept := make([]GroundedSource, 0, len(e.grounded))
	for _, source := range e.grounded {
		if retain(source.Key) {
			kept = append(kept, source)
		}
	}
	e.grounded = kept
}

// GroundedSource is one retrieval result the turn has actually shown the
// model, together with the citation number it carries. Key is the identity the
// citation check compares a model-written [n] against - not the number itself,
// but the evidence the number stands for.
type GroundedSource struct {
	// Citation is the stable number issued for Key.
	Citation int
	// Key is the evidence identity, built by the tool that produced it.
	Key string
	// Source is the citation record attached to the turn's answer.
	Source domain.Source
}

// AddGroundedSource records one shown retrieval result under its citation.
func (e *TurnEvidence) AddGroundedSource(source GroundedSource) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, existing := range e.grounded {
		if existing.Key == source.Key {
			return
		}
	}
	e.grounded = append(e.grounded, source)
}

// GroundedSources returns every retrieval result shown this turn, in show
// order.
func (e *TurnEvidence) GroundedSources() []GroundedSource {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]GroundedSource(nil), e.grounded...)
}

// UsedTokens reports what the document half has recorded as charged, mirroring
// the allowance the tools actually spend against. It exists for receipts.
func (e *TurnEvidence) UsedTokens() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	budget := e.budget
	e.mu.Unlock()
	if budget == nil {
		return 0
	}
	return budget.Used()
}

// ChargeTokens records spent tokens on the document half. It reports whether
// the charge fit under the ceiling; a failed charge spends nothing.
func (e *TurnEvidence) ChargeTokens(tokens int) bool {
	if e == nil {
		return true
	}
	e.mu.Lock()
	budget := e.budget
	e.mu.Unlock()
	if budget == nil {
		return false
	}
	return budget.TryCharge(tokens)
}

// RemainingTokens reports what the document half may still record.
func (e *TurnEvidence) RemainingTokens() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	budget := e.budget
	e.mu.Unlock()
	if budget == nil {
		return 0
	}
	return budget.Remaining()
}

// MaxTokens reports the ceiling the document half was built with.
func (e *TurnEvidence) MaxTokens() int {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.maxTokens
}

func boundedEvidenceText(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return strings.TrimSpace(string(runes[:maxRunes]))
}

// The context plumbing. The registry is installed next to the turn's token
// allowance, and a tool that ignores it degrades to per-call behaviour exactly
// as a tool that ignores the allowance degrades to its own cap.

type turnEvidenceContextKey struct{}

// WithTurnEvidence hands one turn's registry to everything downstream of ctx.
func WithTurnEvidence(ctx context.Context, evidence *TurnEvidence) context.Context {
	return context.WithValue(ctx, turnEvidenceContextKey{}, evidence)
}

// TurnEvidenceFrom returns the registry this turn installed, or nil when there
// is none - which is the answer outside an agent turn, not a failure.
func TurnEvidenceFrom(ctx context.Context) *TurnEvidence {
	if ctx == nil {
		return nil
	}
	evidence, _ := ctx.Value(turnEvidenceContextKey{}).(*TurnEvidence)
	return evidence
}
