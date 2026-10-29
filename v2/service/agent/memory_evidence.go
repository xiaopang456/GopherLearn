package agent

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MemoryEvidenceKind is which of the four evidence types one reference stands
// for.
//
// They are separate kinds rather than one bag because they license different
// sentences. A stated teaching preference cannot support "we decided X last
// time", and a message from this conversation cannot support "you said that
// three weeks ago". The kind is what makes that checkable instead of hoped for.
type MemoryEvidenceKind string

const (
	// MemoryEvidenceGlobal is global semantic memory: the stable profile,
	// teaching preferences, long-running goals and explicit prohibitions.
	MemoryEvidenceGlobal MemoryEvidenceKind = "global"
	// MemoryEvidenceTopic is topic semantic memory: this learner's own
	// understanding, confusion, analogies and pitfalls on one subject.
	MemoryEvidenceTopic MemoryEvidenceKind = "topic_semantic"
	// MemoryEvidenceEpisodic is Episodic Memory: what happened, was decided or
	// was concluded in an earlier session.
	MemoryEvidenceEpisodic MemoryEvidenceKind = "episodic"
	// MemoryEvidenceCurrent is a message the learner already sent in this
	// conversation.
	MemoryEvidenceCurrent MemoryEvidenceKind = "current_message"
)

// Prefix is the letter a reference to this kind carries.
func (k MemoryEvidenceKind) Prefix() string {
	switch k {
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

func memoryKindForPrefix(prefix string) (MemoryEvidenceKind, bool) {
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

// SupportsHistory reports whether this kind may stand behind a claim about
// something that happened in an earlier session.
//
// Only Episodic can. A preference is not a history, and this is the rule
// BUG-AGENT-152 exists because nothing enforced: both read paths built a
// plausible past out of the learner's stated goals.
func (k MemoryEvidenceKind) SupportsHistory() bool { return k == MemoryEvidenceEpisodic }

// MemoryEvidenceEntry is one citable piece of evidence, with both halves of its
// identity: the request-scoped label the model saw, and the server-side record
// it stands for.
type MemoryEvidenceEntry struct {
	Ref  string             `json:"ref"`
	Kind MemoryEvidenceKind `json:"kind"`
	// InternalID, Version, Owner, Status and Source never leave the server. A
	// model that could name them could ask for them.
	InternalID string    `json:"internal_id"`
	Version    uint64    `json:"version"`
	Owner      int64     `json:"owner"`
	Status     string    `json:"status"`
	Source     string    `json:"source"`
	Subject    string    `json:"subject"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	// MessageID is set only for current-conversation evidence.
	MessageID uint `json:"message_id,omitempty"`
}

// MemorySearchCall is one tool invocation.
type MemorySearchCall struct {
	Sequence int      `json:"sequence"`
	Query    string   `json:"query"`
	Kinds    []string `json:"kinds"`
	Temporal string   `json:"temporal"`
	TopK     int      `json:"top_k"`

	Status string `json:"status"`
	Reason string `json:"reason"`

	// Refs are the labels this call handed the model, paired with the records
	// they stood for.
	Refs []MemoryEvidenceRef `json:"refs"`

	// SuppressedHits is how many eligible candidates this call dropped only
	// because the assembled window had already shown them. It is what makes
	// "nothing new" and "nothing recorded" two distinguishable outcomes in the
	// trace, the same way status and reason distinguish them for the model.
	SuppressedHits    int      `json:"suppressed_hits"`
	SourceQuotes      int      `json:"source_quotes"`
	ObservationTokens int      `json:"observation_tokens"`
	Truncated         bool     `json:"truncated"`
	Degraded          bool     `json:"degraded"`
	EmbeddingCalls    int      `json:"embedding_calls"`
	ReloadRejections  int      `json:"reload_rejections"`
	Channels          []string `json:"channels"`
	Reasons           []string `json:"reasons"`
	LatencyMS         int64    `json:"latency_ms"`
}

// TopicMemoryReadCall is one topic_memory_read invocation.
//
// It is a separate record from MemorySearchCall, and deliberately so. Every
// cost report in this series counts "how many general memory searches did this
// turn buy", and a narrow key read that spends no embedding and no vector query
// folded into that number would make the two indistinguishable in exactly the
// artefact that exists to tell them apart.
type TopicMemoryReadCall struct {
	Sequence int `json:"sequence"`
	// Keys are the authoritative TopicKeys this call read, and they come from
	// server state. The model cannot supply one and cannot see one.
	Keys   []string `json:"keys"`
	Status string   `json:"status"`
	Reason string   `json:"reason"`
	// Refs are the labels this call handed the model.
	Refs []MemoryEvidenceRef `json:"refs"`
	// SuppressedHits is how many eligible records were dropped only because
	// another tool in this same run had already issued them.
	SuppressedHits    int  `json:"suppressed_hits"`
	ObservationTokens int  `json:"observation_tokens"`
	Truncated         bool `json:"truncated"`
	Degraded          bool `json:"degraded"`
	EligibleCount     int  `json:"eligible_count,omitempty"`
	ReturnedCount     int  `json:"returned_count,omitempty"`
	OmittedCount      int  `json:"omitted_count,omitempty"`
	Complete          bool `json:"complete,omitempty"`
	// Scope and NextAction are the two v4 routing facts the model saw. They are
	// retained in the server trace so an evaluation can prove the observation
	// was coherent instead of inferring that from the final answer.
	Scope      string `json:"scope,omitempty"`
	NextAction string `json:"next_action,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
}

// MemoryEvidenceRef is the ref-to-record pairing recorded per call.
type MemoryEvidenceRef struct {
	Ref             string `json:"ref"`
	Kind            string `json:"kind"`
	InternalID      string `json:"internal_id"`
	Version         uint64 `json:"version"`
	QuoteMessageIDs []uint `json:"quote_message_ids,omitempty"`
}

// The two ways a citation can be wrong that a server can decide on its own.
var (
	// ErrUnknownMemoryRef is a reference this run never issued: out of range,
	// invented, or carried over from a run that has ended.
	ErrUnknownMemoryRef = errors.New("unknown memory evidence reference")
	// ErrMemoryKindMismatch is a reference that exists but was cited as the
	// wrong kind - most consequentially, a preference cited as a past decision.
	ErrMemoryKindMismatch = errors.New("memory evidence reference cited as the wrong kind")
)

// MemoryEvidenceRegistry is one AgentRun's evidence authority.
//
// It is deliberately not rag.Source and shares no structure with it. A document
// citation is a public artefact - a number the answer prints, a file name the
// learner recognises, a line range they can open. A memory reference is a
// request-scoped label over a private record: it never carries a database id,
// and the next run may not reuse it. Putting the two in one list would let a
// sentence the learner wrote about themselves be rendered in the UI as a source
// from their uploaded notes.
//
// Numbers are unique across all four kinds and the letter declares the kind, so
// "the reference does not exist" and "the reference exists and is a different
// kind" are two distinguishable failures. Numbering each kind from one would
// collapse the second into the first, and the second is the one that catches a
// preference being passed off as a past decision.
type MemoryEvidenceRegistry struct {
	mu      sync.Mutex
	counter int
	entries map[string]MemoryEvidenceEntry
	order   []string
	calls   []MemorySearchCall
	reads   []TopicMemoryReadCall
	// issued is every record this run has already put in front of the model,
	// keyed by kind, id and version, whichever component put it there.
	//
	// It is here rather than in either tool because it is the one fact both
	// tools need and neither owns. STABILIZE-040 registers two memory tools in
	// one run, and "has the model already been shown this row" has to have the
	// same answer in both of them regardless of which ran first.
	issued map[string]struct{}
	// identityLabel is the reverse of issued: the label each identity was
	// issued under. BUG-AGENT-217 needs it because a governed re-search hands
	// the tool locally numbered labels, and the turn's answer - which label
	// this identity already carries - lives here, not in the tool.
	identityLabel map[string]string
	// episodeCard is the atomic per-turn EpisodeID/version -> card-prefix
	// mapping. It is separate from identityLabel because synopsis, decision,
	// open-loop and correction assertions all share one card number.
	episodeCard map[string]string
}

// NewMemoryEvidenceRegistry returns an empty registry for one run.
func NewMemoryEvidenceRegistry() *MemoryEvidenceRegistry {
	return &MemoryEvidenceRegistry{entries: map[string]MemoryEvidenceEntry{},
		issued: map[string]struct{}{}, identityLabel: map[string]string{}, episodeCard: map[string]string{}}
}

// issuedRecordKey is the identity two memory tools compare against.
//
// Kind is part of it because a topic note and an episode may share an id in two
// different tables, and version is part of it because a note the learner edited
// between one tool call and the next really is something the model has not
// read. Dropping either would make the dedup delete a record it was never asked
// to.
func issuedRecordKey(kind MemoryEvidenceKind, id string, version uint64) string {
	return fmt.Sprintf("%s|%s|%d", kind, strings.TrimSpace(id), version)
}

// IssuedRecord reports whether this run has already handed the model this exact
// record, under any label and from any component.
func (r *MemoryEvidenceRegistry) IssuedRecord(kind MemoryEvidenceKind, id string, version uint64) bool {
	if r == nil || strings.TrimSpace(id) == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.issued[issuedRecordKey(kind, id, version)]
	return exists
}

// Register issues the next reference for one piece of evidence and returns it.
func (r *MemoryEvidenceRegistry) Register(entry MemoryEvidenceEntry) string {
	if r == nil || entry.Kind.Prefix() == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	entry.Ref = fmt.Sprintf("%s%d", entry.Kind.Prefix(), r.counter)
	if r.entries == nil {
		r.entries = map[string]MemoryEvidenceEntry{}
	}
	r.entries[entry.Ref] = entry
	r.order = append(r.order, entry.Ref)
	if strings.TrimSpace(entry.InternalID) != "" {
		if r.issued == nil {
			r.issued = map[string]struct{}{}
		}
		r.issued[issuedRecordKey(entry.Kind, entry.InternalID, entry.Version)] = struct{}{}
		if r.identityLabel == nil {
			r.identityLabel = map[string]string{}
		}
		r.identityLabel[issuedRecordKey(entry.Kind, entry.InternalID, entry.Version)] = entry.Ref
	}
	return entry.Ref
}

// GetOrAllocateEpisodeCard returns the one card prefix for an Episode identity
// in this turn. Lookup and allocation are one critical section: two governed
// searches cannot both observe a missing card and mint different prefixes.
//
// The key is EpisodeID plus version, not an assertion ID. All slots of one
// Episode therefore share one card while their own suffixes remain distinct.
func (r *MemoryEvidenceRegistry) GetOrAllocateEpisodeCard(episodeID string, version uint64) string {
	if r == nil || strings.TrimSpace(episodeID) == "" {
		return ""
	}
	key := fmt.Sprintf("%s|%d", strings.TrimSpace(episodeID), version)
	r.mu.Lock()
	defer r.mu.Unlock()
	if card, ok := r.episodeCard[key]; ok {
		return card
	}
	r.counter++
	card := fmt.Sprintf("E%d", r.counter)
	if r.episodeCard == nil {
		r.episodeCard = map[string]string{}
	}
	r.episodeCard[key] = card
	return card
}

// EpisodeCard returns the stable card prefix for an Episode identity.
func (r *MemoryEvidenceRegistry) EpisodeCard(episodeID string, version uint64) (string, bool) {
	if r == nil {
		return "", false
	}
	key := fmt.Sprintf("%s|%d", strings.TrimSpace(episodeID), version)
	r.mu.Lock()
	defer r.mu.Unlock()
	card, ok := r.episodeCard[key]
	return card, ok
}

// LabelFor reports the label this turn already issued for one identity.
//
// BUG-AGENT-217: a governed re-search presents episodes the turn has seen
// before under locally numbered labels, and the tool must reuse the label the
// turn already gave them instead of the search's own numbering. The mapping
// lives here because the registry is what a citation resolves against.
func (r *MemoryEvidenceRegistry) LabelFor(kind MemoryEvidenceKind,
	internalID string, version uint64) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	label, ok := r.identityLabel[issuedRecordKey(kind, internalID, version)]
	return label, ok
}

// AllocateCardNumber advances the shared counter and returns the new number.
//
// A governed re-search that surfaces an episode the turn has never seen needs a
// card number no other kind has claimed; the counter is the one source of
// numbers the whole registry shares, so allocating through it keeps every
// future auto-issued reference collision-free. A number allocated here and
// never registered leaves a gap, which is harmless: stability is the property
// that matters, not density.
func (r *MemoryEvidenceRegistry) AllocateCardNumber() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	return r.counter
}

// RegisterAs records evidence under a reference the caller has already decided
// on, and refuses anything else.
//
// Register issues its own numbers, which is right when the registry is what
// names a card. It is wrong for assertion slots: there the governor has already
// put [E1.d1] in front of the model, and a registry that quietly renumbered it
// to E7 would leave the model reading one vocabulary and the citation check
// holding another - the model's own label unresolvable, and an E7 it never saw
// citable. So this takes the label as given and validates it instead:
//
//   - the shape must be a slot reference, [E1.d1] and not [E1], because a
//     governed turn shows no whole cards;
//   - the letter must be the kind's own letter, so the ref cannot claim to be
//     evidence of a type it is not;
//   - the label must be free, so two assertions can never share one reference.
//
// A conflict is an error rather than an overwrite: the alternative is a
// reference that resolves to a record other than the one the model read.
func (r *MemoryEvidenceRegistry) RegisterAs(ref string, entry MemoryEvidenceEntry) error {
	if r == nil {
		return fmt.Errorf("no evidence registry")
	}
	prefix := entry.Kind.Prefix()
	if prefix == "" {
		return fmt.Errorf("%s has no evidence kind", ref)
	}
	trimmed := strings.TrimSpace(strings.Trim(ref, "[]"))
	parts := memorySlotRefPattern.FindStringSubmatch(trimmed)
	if parts == nil {
		return fmt.Errorf("%q is not a slot reference", ref)
	}
	if parts[1] != prefix {
		return fmt.Errorf("%q is not a %s reference", ref, entry.Kind)
	}
	number, err := strconv.Atoi(parts[2])
	if err != nil || number <= 0 {
		return fmt.Errorf("%q has no card number", ref)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]MemoryEvidenceEntry{}
	}
	if existing, taken := r.entries[trimmed]; taken {
		// Issuing the same slot to the same memory again is the same issue,
		// not a second one.
		//
		// A learner who asks about an earlier session twice gets two searches,
		// and the two searches usually return some of the same episodes -
		// which is the retrieval working, not a collision. Refusing that put an
		// error out of the memory tool, and a tool that fails while running is
		// an infrastructure fault: one perfectly ordinary second question voided
		// a whole measured round. The rehearsal that found it reproduced the
		// shape STABILIZE-062 had been trying to explain for three versions.
		//
		// A real collision - the same slot pointing at a different memory - is
		// still refused, because that is the case where a citation would resolve
		// to something the learner was never shown.
		if existing.InternalID == entry.InternalID && existing.Kind == entry.Kind &&
			existing.Version == entry.Version {
			return nil
		}
		return fmt.Errorf("%s is already issued for %s", trimmed, existing.InternalID)
	}
	entry.Ref = trimmed
	r.entries[entry.Ref] = entry
	r.order = append(r.order, entry.Ref)
	// Auto-issued references are numbered from the same counter, so it has to
	// clear every card number a governed turn has used. Otherwise a later G1
	// would collide with this turn's E1 cards on the number Lookup resolves by.
	if number > r.counter {
		r.counter = number
	}
	if strings.TrimSpace(entry.InternalID) != "" {
		if r.issued == nil {
			r.issued = map[string]struct{}{}
		}
		r.issued[issuedRecordKey(entry.Kind, entry.InternalID, entry.Version)] = struct{}{}
		if r.identityLabel == nil {
			r.identityLabel = map[string]string{}
		}
		r.identityLabel[issuedRecordKey(entry.Kind, entry.InternalID, entry.Version)] = trimmed
	}
	return nil
}

// Lookup resolves one reference exactly as the model wrote it.
func (r *MemoryEvidenceRegistry) Lookup(ref string) (MemoryEvidenceEntry, error) {
	if r == nil {
		return MemoryEvidenceEntry{}, ErrUnknownMemoryRef
	}
	prefix, number, slot, ok := splitMemoryRef(ref)
	if !ok {
		return MemoryEvidenceEntry{}, ErrUnknownMemoryRef
	}
	kind, known := memoryKindForPrefix(prefix)
	if !known {
		return MemoryEvidenceEntry{}, ErrUnknownMemoryRef
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// The number and slot identify the entry; the letter is the model's claim
	// about its kind. Resolving on the identity first is what makes a wrong
	// letter a kind mismatch rather than an unknown reference. The slot has to
	// match exactly: citing [E1.o1] when only [E1.d1] was issued is a claim
	// about an assertion this turn never showed, not a mistyped card.
	for _, existing := range r.entries {
		_, existingNumber, existingSlot, _ := splitMemoryRef(existing.Ref)
		if existingNumber != number || existingSlot != slot {
			continue
		}
		if existing.Kind != kind {
			return existing, fmt.Errorf("%w: %s is %s", ErrMemoryKindMismatch, ref, existing.Kind)
		}
		return existing, nil
	}
	return MemoryEvidenceEntry{}, fmt.Errorf("%w: %s", ErrUnknownMemoryRef, ref)
}

// Entries returns the registered evidence in issue order.
func (r *MemoryEvidenceRegistry) Entries() []MemoryEvidenceEntry {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MemoryEvidenceEntry, 0, len(r.order))
	for _, ref := range r.order {
		out = append(out, r.entries[ref])
	}
	return out
}

// RefsOfKind lists the references of one kind, in issue order.
func (r *MemoryEvidenceRegistry) RefsOfKind(kind MemoryEvidenceKind) []string {
	out := []string{}
	for _, entry := range r.Entries() {
		if entry.Kind == kind {
			out = append(out, entry.Ref)
		}
	}
	return out
}

// HasLongTermEvidence reports whether the run holds any topic or episodic
// evidence - the two kinds that come from a search rather than from the window.
func (r *MemoryEvidenceRegistry) HasLongTermEvidence() bool {
	for _, entry := range r.Entries() {
		if entry.Kind == MemoryEvidenceTopic || entry.Kind == MemoryEvidenceEpisodic {
			return true
		}
	}
	return false
}

// Record appends one search call.
func (r *MemoryEvidenceRegistry) Record(call MemorySearchCall) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	call.Sequence = len(r.calls) + 1
	r.calls = append(r.calls, call)
}

// RecordTopicRead appends one topic_memory_read call.
func (r *MemoryEvidenceRegistry) RecordTopicRead(call TopicMemoryReadCall) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	call.Sequence = len(r.reads) + 1
	r.reads = append(r.reads, call)
}

// TopicReads returns a copy of the topic reads this run performed.
func (r *MemoryEvidenceRegistry) TopicReads() []TopicMemoryReadCall {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TopicMemoryReadCall, len(r.reads))
	copy(out, r.reads)
	return out
}

// Calls returns a copy of what the run recorded.
func (r *MemoryEvidenceRegistry) Calls() []MemorySearchCall {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MemorySearchCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// HasEvidence reports whether any search returned at least one card.
//
// The runner needs it to answer a question the citation machinery cannot: an
// answer built on the learner's own recorded past has no document and no
// citation number, and the grounding guard would otherwise replace it with a
// refusal for lacking the one kind of evidence it can count.
// EvidenceCount is how many references this run has issued so far.
//
// The progress ledger uses the delta across one tool call to decide whether the
// call added anything, which is a stronger question than "did it return cards":
// a card the model has already been shown is not new evidence.
func (r *MemoryEvidenceRegistry) EvidenceCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.order)
}

func (r *MemoryEvidenceRegistry) HasEvidence() bool {
	for _, call := range r.Calls() {
		if len(call.Refs) > 0 {
			return true
		}
	}
	for _, read := range r.TopicReads() {
		if len(read.Refs) > 0 {
			return true
		}
	}
	return false
}

// Attempted reports whether any memory tool ran at all this turn.
func (r *MemoryEvidenceRegistry) Attempted() bool {
	return len(r.Calls()) > 0 || len(r.TopicReads()) > 0
}

// SearchedAndFoundNothing reports that every search this run performed came
// back empty or unavailable.
//
// It is the state in which an answer must say so plainly rather than assemble a
// plausible history out of the profile, which is exactly what BUG-AGENT-152
// recorded both read paths doing.
// The last clause is what STABILIZE-039 added. Under agent_memory_tool_v2 the
// window can already hold topic evidence before any search runs, so a search
// that legitimately returns nothing new - because every hit was already shown -
// must not turn a correct [S#] citation into "cited evidence that does not
// exist". Under every earlier mode the clause changes nothing: S and E are
// issued only by memory_search, and never without also landing in that call's
// Refs, so HasLongTermEvidence is already implied by the loop above.
// STABILIZE-040 adds the topic reads to both halves of it. Under v3 a turn may
// consult long-term memory without memory_search running at all, and a run that
// asked for the current topic, was told there is none, and then cited [S1]
// anyway is exactly the state this predicate exists to catch.
func (r *MemoryEvidenceRegistry) SearchedAndFoundNothing() bool {
	calls := r.Calls()
	reads := r.TopicReads()
	if len(calls) == 0 && len(reads) == 0 {
		return false
	}
	for _, call := range calls {
		if len(call.Refs) > 0 {
			return false
		}
	}
	for _, read := range reads {
		if len(read.Refs) > 0 {
			return false
		}
	}
	return !r.HasLongTermEvidence()
}

// TotalToolLatency is the summed tool latency, for cost reporting.
func (r *MemoryEvidenceRegistry) TotalToolLatency() time.Duration {
	var total time.Duration
	for _, call := range r.Calls() {
		total += time.Duration(call.LatencyMS) * time.Millisecond
	}
	for _, read := range r.TopicReads() {
		total += time.Duration(read.LatencyMS) * time.Millisecond
	}
	return total
}

// memoryRefPattern matches a memory citation and nothing else. Document
// citations are bare numbers - [1], [2] - so the two vocabularies cannot
// collide, and neither can be parsed as the other.
//
// The optional dotted tail is an assertion slot: [E1.d1] is the first decision
// inside the first Episode card. A mode that governs assertions shows the model
// slots and never whole cards, so an extractor that only understood [E1] would
// read a governed answer as citing nothing at all - and "cited nothing" is the
// one outcome the citation check treats as harmless.
var memoryRefPattern = regexp.MustCompile(`\[([GSECgsec])([0-9]{1,3})(\.[a-zA-Z][0-9]{0,3})?\]`)

// memorySlotRefPattern is what an explicitly issued slot reference must look
// like before it can enter the registry.
var memorySlotRefPattern = regexp.MustCompile(`^([GSEC])([0-9]{1,3})\.([a-z][0-9]{0,3})$`)

// splitMemoryRef parses a reference into its kind letter, card number and slot.
// The slot is empty for a whole-card reference, and two references are the same
// record only when both the number and the slot agree: [E1] and [E1.d1] are
// different claims about different things.
func splitMemoryRef(ref string) (string, int, string, bool) {
	ref = strings.TrimSpace(strings.Trim(ref, "[]"))
	if len(ref) < 2 {
		return "", 0, "", false
	}
	rest := ref[1:]
	slot := ""
	if dot := strings.Index(rest, "."); dot >= 0 {
		slot = strings.ToLower(rest[dot+1:])
		rest = rest[:dot]
		if slot == "" {
			return "", 0, "", false
		}
	}
	number, err := strconv.Atoi(rest)
	if err != nil || number <= 0 {
		return "", 0, "", false
	}
	return strings.ToUpper(ref[:1]), number, slot, true
}

// CitationFallbackReason is the closed vocabulary for why an answer ended in
// the safe fallback. It is written at the point the fallback is taken, never
// derived from error text, so a future reader can tell apart a model that
// invented a reference twice from a model that repaired correctly but ran out
// of budget before it could. The zero value is CitationFallbackNone: SafeFallback
// is false on every outcome that carries it.
type CitationFallbackReason string

const (
	// CitationFallbackNone is the zero value. SafeFallback is always false.
	CitationFallbackNone CitationFallbackReason = "none"
	// CitationFallbackNoBudget: the runner reached the citation gate but had
	// no model-call budget left for a repair. Distinct from a transport
	// failure and from a model that repaired badly: the system refused, not
	// the model.
	CitationFallbackNoBudget CitationFallbackReason = "no_budget_for_repair"
	// CitationFallbackTransportFailure: the repair stream could not be opened
	// or read; the runner fell back because it never heard back.
	CitationFallbackTransportFailure CitationFallbackReason = "repair_transport_failure"
	// CitationFallbackEmptyOrAbstain: the repair came back empty or with the
	// abstention sentinel MEMORY_EVIDENCE_INSUFFICIENT; the runner refused to
	// substitute it for an answer.
	CitationFallbackEmptyOrAbstain CitationFallbackReason = "repair_empty_or_abstain"
	// CitationFallbackReturnedInvalid: the repair produced text, but a second
	// citation check still failed; one more repair is not offered, by design.
	CitationFallbackReturnedInvalid CitationFallbackReason = "repair_returned_invalid"
)

// MemoryCitationOutcome is what the server-side check concluded about one
// answer. Every field is counted separately because they call for different
// fixes: an invented reference is a model failure, a missing citation may be a
// prompt failure, and laundering is neither and cannot be seen from here.
type MemoryCitationOutcome struct {
	// Cited are the references the answer actually used, in order.
	Cited []string `json:"cited"`
	// Invalid are references this run never issued.
	Invalid []string `json:"invalid_memory_refs"`
	// KindMismatch are references that exist and were cited as the wrong kind.
	KindMismatch []string `json:"kind_mismatch"`
	// MissingCitation is set when the run holds long-term evidence and the
	// answer cites none of it. It is recorded and never gated: a search that
	// returned only neighbouring subjects should end in "nothing recorded",
	// and that answer legitimately cites nothing.
	MissingCitation bool `json:"missing_memory_citation"`
	// NoEvidenceButCited is set when every search came back empty and the
	// answer still cites long-term memory.
	NoEvidenceButCited bool `json:"no_evidence_but_cited"`
	RepairAttempted    bool `json:"repair_attempted"`
	RepairSucceeded    bool `json:"repair_succeeded"`
	SafeFallback       bool `json:"safe_fallback"`
	// FallbackReason names the path that took the fallback, in the closed
	// vocabulary above. Empty whenever SafeFallback is false.
	FallbackReason CitationFallbackReason `json:"citation_fallback_reason,omitempty"`
	// BudgetRemainingBeforeRepair is how many model calls the budget still held
	// when the citation gate decided whether a repair was affordable. Zero
	// when no repair was attempted. Useful to tell apart "the budget was
	// already gone" from "the model used it on something else first".
	BudgetRemainingBeforeRepair int `json:"budget_remaining_before_repair"`
}

// Failed reports whether the outcome is one the server must not send.
func (o MemoryCitationOutcome) Failed() bool {
	return len(o.Invalid) > 0 || len(o.KindMismatch) > 0 || o.NoEvidenceButCited
}

// Reasons is the stable enumeration for logs and reports. It never carries
// answer text or a database id.
func (o MemoryCitationOutcome) Reasons() []string {
	reasons := []string{}
	if len(o.Invalid) > 0 {
		reasons = append(reasons, "invalid_memory_ref")
	}
	if len(o.KindMismatch) > 0 {
		reasons = append(reasons, "kind_mismatch")
	}
	if o.NoEvidenceButCited {
		reasons = append(reasons, "no_evidence_but_cited")
	}
	if o.MissingCitation {
		reasons = append(reasons, "missing_memory_citation")
	}
	sort.Strings(reasons)
	return reasons
}

// CheckMemoryCitations is the deterministic half of the contract.
//
// It decides exactly what a server can decide: whether a reference exists,
// whether it was cited as the kind it actually is, and whether an answer cited
// long-term memory in a run where every search came back empty. It deliberately
// does not try to decide whether the cited text supports the sentence it is
// attached to - a legal reference proves the source is legitimate, not that the
// natural language follows from it. That judgement belongs to the anonymous
// judge, and pretending to make it here would be the more dangerous error.
func CheckMemoryCitations(registry *MemoryEvidenceRegistry, answer string) MemoryCitationOutcome {
	outcome := MemoryCitationOutcome{Cited: []string{}, Invalid: []string{}, KindMismatch: []string{}}
	seen := map[string]struct{}{}
	for _, match := range memoryRefPattern.FindAllStringSubmatch(answer, -1) {
		ref := strings.ToUpper(match[1]) + match[2] + strings.ToLower(match[3])
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		outcome.Cited = append(outcome.Cited, ref)
		_, err := registry.Lookup(ref)
		switch {
		case errors.Is(err, ErrMemoryKindMismatch):
			outcome.KindMismatch = append(outcome.KindMismatch, ref)
		case err != nil:
			outcome.Invalid = append(outcome.Invalid, ref)
		}
	}
	longTermCited := false
	for _, ref := range outcome.Cited {
		prefix, _, _, ok := splitMemoryRef(ref)
		if ok && (prefix == "S" || prefix == "E") {
			longTermCited = true
		}
	}
	if registry.HasLongTermEvidence() && !longTermCited {
		outcome.MissingCitation = true
	}
	if registry.SearchedAndFoundNothing() && longTermCited {
		outcome.NoEvidenceButCited = true
	}
	return outcome
}

// LegalRefSummary is the list a repair call is given: every reference this run
// actually issued, with its kind and subject and nothing else.
func (r *MemoryEvidenceRegistry) LegalRefSummary() string {
	entries := r.Entries()
	if len(entries) == 0 {
		return "(this run holds no memory evidence at all)"
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
