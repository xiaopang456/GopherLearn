package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"GopherAI/model"

	"github.com/google/uuid"
)

// Session-finalize candidate extraction.
//
// When a session is finished - the learner closed it, or the server's idle
// fallback fired - this module reads the session one last time and proposes
// long-term memory candidates for the learner to confirm on the memory page.
// It is the write entry the third phase adds alongside manual input and the
// in-conversation "please remember" tool.
//
// The session whiteboard is the clue, not the evidence. The board says which
// parts of the conversation already held up as current facts during the
// session, and every entry carries the message ids it came from. The evidence
// itself is always the raw messages: this module reads them back by those
// references, and a candidate is only as good as the references it survives.
// What the program checks is what it can check as a fact about the messages -
// that a cited message exists, belongs to this session, has the role the
// candidate needs, and that an adopted proposal of the assistant's rests on a
// learner's message that accepted it after the proposal was made. Whether a
// sentence is a lasting preference or a one-turn request is the model's
// judgement, and the program does not try to read it out of a string.
//
// The output is a proposal, never a memory. Candidates are handed to the
// draft entry (GovernanceService.ProposeDraft) and stay pending until the
// learner confirms them; nothing here writes active memory, touches quiz
// mastery, or decides an entry's fate alone.

// The scope a candidate can land in, and the vocabularies that come with
// each. The global profile class is the model package's own enum - the one the
// drafts and the confirmed memories already carry - so a candidate and the row
// it becomes share one vocabulary end to end. The topic kind is fixed here:
// the two kinds are the words the memory page shows, not free model output.
const (
	// FinalizeKindUnderstanding is the learner's own understanding of a topic,
	// including confusions, analogies and pitfalls (我的理解).
	FinalizeKindUnderstanding = "understanding"
	// FinalizeKindSelfReport is what the learner says they know, kept apart
	// from graded mastery, which is computed from scored attempts only.
	FinalizeKindSelfReport = "self_report"
)

func ValidFinalizeKind(kind string) bool {
	switch kind {
	case FinalizeKindUnderstanding, FinalizeKindSelfReport:
		return true
	}
	return false
}

// Skip reasons. They are a fixed set so a report can count them, and they
// separate the two ways a candidate fails: the source did not hold up
// (validation), and the matter was already covered (duplicate).
const (
	// FinalizeSkipEvidenceMissing says a cited message is not part of what
	// this extraction could see: it does not exist in the session, or it was
	// never in the extraction's input. This is the discovery boundary of the
	// plan's §6.1 replay pair.
	FinalizeSkipEvidenceMissing = "evidence_not_visible"
	// FinalizeSkipEvidenceRole says the cited message exists but is not the
	// role the candidate needs: an evidence id that is not the learner's
	// message, or an assistant id that is not the assistant's.
	FinalizeSkipEvidenceRole = "evidence_role_invalid"
	// FinalizeSkipAdoptionUnproven says the candidate credits an assistant
	// proposal the learner never explicitly took on: assistant words with no
	// learner's message after them. A proposal nobody agreed to is not a
	// fact about the learner.
	FinalizeSkipAdoptionUnproven = "adoption_unproven"
	// FinalizeSkipInvalidShape says the proposal failed the field rules the
	// program owns: empty text, over the length limits, an unknown class or
	// kind, or an impossible validity.
	FinalizeSkipInvalidShape = "invalid_shape"
	// FinalizeSkipDuplicateExisting says the same content is already an
	// active memory in the range the candidate belongs to. Checked by hash,
	// so it costs no model call.
	FinalizeSkipDuplicateExisting = "duplicate_existing"
	// FinalizeSkipDuplicateKnown says this fingerprint is already known from
	// an earlier trigger of the same session: the draft is pending, confirmed
	// or ignored, and a repeat must not propose it twice.
	FinalizeSkipDuplicateKnown = "duplicate_known"
	// FinalizeSkipDuplicateRun says one extraction produced the same
	// candidate twice; the first wins.
	FinalizeSkipDuplicateRun = "duplicate_run"
	// FinalizeSkipSemanticDuplicate says the relation judgement found the
	// same meaning already covered by an active entry of the range: the
	// candidate is skipped outright and the existing entry stands, which is
	// the plan's "同一意思" row. The learner never sees a card for it, and
	// nothing is written.
	FinalizeSkipSemanticDuplicate = "semantic_duplicate"
	// FinalizeSkipStatementEvidenceRequired says a proposal citing several
	// messages did not name the one that supports its current value. With
	// more than one utterance in play the program cannot know which one the
	// value came from, and guessing is what stamped an updated value with a
	// replaced utterance's time - so the proposal is refused and the model
	// is asked to name its primary evidence.
	FinalizeSkipStatementEvidenceRequired = "statement_evidence_required"
	// The draft entry's own skip reasons, passed through when Handoff hands a
	// candidate to the real ProposeDraft and the entry decides against
	// creating a card. They are terminal decisions made by the same rules the
	// manual and "please remember" paths obey, which is what makes the
	// extraction and those paths share one dedup boundary.
	FinalizeSkipDraftDuplicateExisting = "draft_duplicate_existing"
	FinalizeSkipDraftIgnoredSource     = "draft_ignored_source"
)

// FinalizeCursor is the progress of one session's finalize work: the newest
// message id the extraction has processed.
//
// The trigger keeps one cursor per session and passes it back on every run.
// A run whose cursor already covers the session's newest message has nothing
// new to process and proposes nothing, which is what makes a repeated trigger
// of the same progress harmless. A run that ends with candidates whose
// relation judgement failed returns the cursor unchanged, so the next trigger
// re-runs the same progress and the fingerprint keeps one candidate.
type FinalizeCursor struct {
	SessionID        string
	CoveredMessageID uint
}

// SessionMessage is one raw message of the session, with the facts the
// program validates against: its id, its role, and when it was said.
type SessionMessage struct {
	ID        uint
	IsUser    bool
	Content   string
	CreatedAt time.Time
}

// SessionEvidenceRef is one source reference the program has validated. The
// time is the message's own time - the 原话时间 every later comparison uses -
// read from the stored message and never taken from the model.
type SessionEvidenceRef struct {
	MessageID uint      `json:"message_id"`
	Role      string    `json:"role"`
	At        time.Time `json:"at"`
	// Statement marks the reference the statement time follows: the message
	// that directly supports the candidate's current value. The other
	// references remain as context, which is what the confirmation page
	// shows them as.
	Statement bool `json:"statement,omitempty"`
}

// SessionExtractionInput is what one extraction run hands the proposer.
//
// Visible is the only set a citation may name. It is the union of the board's
// own references (the clue) and a bounded window of the learner's recent
// messages, which is the plan §6.1 read-back: a fact the board missed is not
// lost while its message is still recent. A message outside Visible was never
// in front of the proposer, so a proposal citing it is refused rather than
// believed.
type SessionExtractionInput struct {
	UserID    int64
	SessionID string
	// Board is the decoded session whiteboard. It may be empty: a session
	// without a board still extracts from the recent window.
	Board model.SessionWhiteboardDocument
	// Visible maps every message id the proposer may cite to the message.
	Visible map[uint]SessionMessage
	// Topics is the learner's topic catalogue, for the proposer to reuse a
	// key from. It carries no memory content and no scores.
	Topics []Topic
	// ExistingValues are the learner's confirmed current values that look
	// directly relevant to this session - a small bounded set. They are
	// context for one question only: does the new statement replace the old
	// value from now on, or adjust it for this one time? They are not
	// evidence: every candidate still has to cite the learner's own messages,
	// and only the learner's confirmation writes anything.
	ExistingValues []SessionExistingValue
}

// SessionExistingValue is one confirmed current value shown to the proposer
// for comparison. It carries no message ids and cannot be cited as evidence.
type SessionExistingValue struct {
	ID      string
	Subject string
	Content string
}

// SessionRawProposal is one candidate as the proposer returned it, before
// the program has believed any of it.
type SessionRawProposal struct {
	Scope       model.MemoryScope
	Class       model.MemoryProfileClass // global scope only; the profile slot
	Kind        string                   // topic scope only
	TopicKey    string                   // topic scope: the proposed topic, resolved or pending
	Subject     string
	Content     string
	Durability  model.MemoryDurability
	ValidUntil  *time.Time
	EvidenceIDs []uint // the learner's own words the candidate rests on
	// TopicUnique says the proposer judged that exactly one catalogue topic
	// clearly applies to this note. It is the model's explicit answer to the
	// ambiguity question, and the program prefills only on it (or when the
	// learner named the topic) - a silent pick among several plausible
	// topics leaves the attribution to the learner.
	TopicUnique bool
	// StatementEvidenceID names the evidence message that directly supports
	// the candidate's current value, and it has to be one of the
	// EvidenceIDs. A proposal that cites several messages must name it: with
	// more than one utterance in play, the 原话时间 has to come from the
	// message that carries the value, and a proposal that leaves the choice
	// to the program is refused rather than guessed - that guess is what
	// stamped an updated value with a replaced utterance's time. A
	// proposal citing exactly one message may leave it unset, because there
	// is only one message the time could come from.
	StatementEvidenceID uint
	// AssistantIDs are the assistant's messages that proposed what the
	// learner took on. They are recorded apart from the evidence, never in
	// its place.
	AssistantIDs []uint
}

// SessionCandidateProposer is the model seam of the extraction.
//
// The production implementation calls the structured extraction model and
// arrives with the real-model verification of the plan's batch 3. Local
// validation runs against deterministic stand-ins, which is how this round
// stays zero-cost.
type SessionCandidateProposer interface {
	Propose(ctx context.Context, input *SessionExtractionInput) ([]SessionRawProposal, error)
}

// SessionFinalizeService runs one finalize extraction.
//
// Every store behind it is a narrow interface, so the module carries no
// database and no queue of its own: the finalize trigger wires the real
// repositories when this is integrated, and the stand-in tests wire fakes.
type SessionFinalizeService struct {
	boards     FinalizeBoardSource
	messages   FinalizeMessageSource
	catalogue  FinalizeTopicCatalogue
	known      FinalizeFingerprintSource
	globalMind GlobalMemorySnapshotSource
	topicMind  FinalizeTopicEntrySource
	proposer   SessionCandidateProposer
	relations  GlobalRelationResolver
	// topicRelations judges a topic-scope candidate against the active notes
	// of its own topic - the same resolver the draft edit path uses, so the
	// extraction and the edit share one judgement. Optional: without it the
	// topic conflict query alone decides at the draft entry.
	topicRelations TopicRelationResolver
	now            func() time.Time

	// RecentMessageWindow is how many of the session's most recent messages
	// - the learner's and the assistant's alike - one extraction keeps
	// visible beyond the board's own references. It is the bounded read-back
	// of plan §6.1: large enough to cover a preference the board missed, and
	// to keep an adopted proposal's two sides together; small enough that
	// the input stays a bounded cost. The number is an implementation
	// parameter of this round and is recorded in the batch report.
	RecentMessageWindow int
	// existingValues supplies the bounded confirmed values the proposer sees
	// as comparison context. Optional: without it the extraction request
	// carries no old values.
	existingValues FinalizeExistingValueSource
	// ExistingValueLimit bounds how many old values reach one request.
	ExistingValueLimit int
	// RelationTargets bounds how many active entries a topic note's relation
	// judgement may be given. The global profile does not use it: extraction
	// compares the same snapshot the draft entry does (up to the global hard
	// limit), so the two layers cannot disagree about which old value is in
	// range.
	RelationTargets int
}

// FinalizeBoardSource reads one session's whiteboard, scoped to its owner.
// v2/dao/session.Repository satisfies it as-is.
type FinalizeBoardSource interface {
	GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error)
}

// FinalizeMessageSource loads the raw messages of one session.
//
// The production adapter wraps the message DAO and must resolve the session
// through its owner: the finalize trigger passes a (user, session) pair it
// has already checked, and the adapter keeps that check by never returning
// messages for a session the user does not own.
//
// The two reads answer the two reasons the extraction opens a session: the
// messages the watermark has not covered yet, read oldest first in batches
// so a long session is covered across triggers instead of one unbounded
// scan that always stops at the same bound; and the whiteboard's own
// references, which may point at messages an earlier run already covered -
// the clue has to stay readable regardless of the watermark, but it is
// bounded by the board's own structure.
type FinalizeMessageSource interface {
	LoadUnprocessedMessages(ctx context.Context, userID int64, sessionID string, afterID uint) ([]SessionMessage, error)
	LoadSessionMessagesByID(ctx context.Context, userID int64, sessionID string, messageIDs []uint) ([]SessionMessage, error)
}

// FinalizeTopicCatalogue lists the learner's topics, derived from quiz
// progress and existing topic notes the same way the memory page derives
// them. v2 TopicCatalogue.List satisfies it.
type FinalizeTopicCatalogue interface {
	ListTopics(ctx context.Context, userID int64) ([]Topic, error)
}

// FinalizeFingerprintSource reports the candidate fingerprints already known
// for one session, from any draft state. The production adapter answers from
// the drafts the session produced (pending, confirmed and ignored alike), so
// a repeated trigger of the same source proposes nothing twice.
type FinalizeFingerprintSource interface {
	KnownCandidateFingerprints(ctx context.Context, userID int64, sessionID string) (map[string]struct{}, error)
}

// FinalizeExistingValueSource selects a small bounded set of the learner's
// confirmed current values that are directly relevant to the session's own
// words. Relevance is the source's business; the extraction only needs a
// short list to compare a new statement against.
type FinalizeExistingValueSource interface {
	RelevantExistingValues(ctx context.Context, userID int64, sessionText string, limit int) ([]SessionExistingValue, error)
}

// FinalizeTopicEntrySource lists the active topic notes of one topic. The
// production adapter wraps the explicit repository the same way the global
// snapshot source wraps its own queries.
type FinalizeTopicEntrySource interface {
	ListActiveTopicEntries(ctx context.Context, userID int64, topicKey string, limit int) ([]model.LongTermMemory, error)
}

func NewSessionFinalizeService(
	boards FinalizeBoardSource,
	messages FinalizeMessageSource,
	catalogue FinalizeTopicCatalogue,
	known FinalizeFingerprintSource,
	globalMind GlobalMemorySnapshotSource,
	topicMind FinalizeTopicEntrySource,
	proposer SessionCandidateProposer,
	relations GlobalRelationResolver,
) (*SessionFinalizeService, error) {
	if boards == nil || messages == nil || proposer == nil {
		return nil, fmt.Errorf("session finalize service requires board and message sources and a proposer")
	}
	if known == nil {
		return nil, fmt.Errorf("session finalize service requires a fingerprint source")
	}
	return &SessionFinalizeService{
		boards: boards, messages: messages, catalogue: catalogue, known: known,
		globalMind: globalMind, topicMind: topicMind,
		proposer: proposer, relations: relations,
		now:                 func() time.Time { return time.Now() },
		RecentMessageWindow: 50,
		ExistingValueLimit:  5,
		RelationTargets:     10,
	}, nil
}

// UseExistingValues attaches the source of the confirmed old values shown to
// the proposer as comparison context.
func (s *SessionFinalizeService) UseExistingValues(source FinalizeExistingValueSource) error {
	if source == nil {
		return fmt.Errorf("existing value source is required")
	}
	s.existingValues = source
	return nil
}

// UseTopicRelations attaches the topic-scope semantic judgement. It is the
// same resolver type the draft edit path uses, so extraction and edit share
// one judgement per topic. Optional: without it, topic candidates propose
// without an extraction-time judgement and the draft entry's conflict query
// plus the edit-time judgement decide.
func (s *SessionFinalizeService) UseTopicRelations(resolver TopicRelationResolver) error {
	if resolver == nil {
		return fmt.Errorf("topic relation resolver is required")
	}
	s.topicRelations = resolver
	return nil
}

// FinalizeRequest is one trigger of the finalize extraction.
type FinalizeRequest struct {
	UserID int64
	Cursor FinalizeCursor
}

// FinalizeSkip is a proposal the program refused, with the reason it refused
// it. Refusals are reported, not swallowed: the plan's acceptance asks for
// the boundary each failure sits at, and a reason is that boundary.
type FinalizeSkip struct {
	Reason       string
	Subject      string
	EvidenceIDs  []uint
	AssistantIDs []uint
}

// SessionMemoryCandidate is one validated candidate, ready for the draft
// entry and for the learner's confirmation page.
//
// Every field the learner will see is here: the title and body, the range it
// belongs to, the source session, the message ids and the time of the words
// it came from, and - when the candidate rests on a proposal of the
// assistant's that the learner took on - the assistant's own words, recorded
// apart from the learner's.
type SessionMemoryCandidate struct {
	ID       string
	UserID   int64
	Scope    model.MemoryScope
	Class    model.MemoryProfileClass // global scope: the profile slot the fact belongs to
	Kind     string                   // topic scope: understanding or self_report
	TopicKey string
	// TopicPending says the topic could not be uniquely resolved: several
	// topics could fit, or none could. The candidate still proposes, and the
	// confirmation flow asks the learner where it belongs.
	TopicPending bool
	Subject      string
	Content      string
	Tags         []string
	Durability   model.MemoryDurability
	ValidUntil   *time.Time
	// Evidence is the learner's own words the candidate rests on. When the
	// candidate is an adopted proposal, this is the learner's acceptance.
	Evidence []SessionEvidenceRef
	// AssistantUtterances is the proposal the learner accepted, when there
	// was one. It is never a substitute for Evidence.
	AssistantUtterances []SessionEvidenceRef
	// StatementTime is the 原话发生时间 every later新旧 comparison uses, as
	// the plan's time rule requires. It is the time of the evidence message
	// that directly supports the candidate's current value - not of the
	// older utterances the candidate also cites as context, so a value that
	// replaced an earlier one never carries the earlier utterance's time.
	StatementTime time.Time
	// Fingerprint identifies the candidate across triggers of the same
	// session: the same content from the same session is one candidate,
	// whatever the trigger that produced it.
	Fingerprint string
	// Relation is the semantic judgement against the in-range active
	// entries, and RelatedOld are the entries the judgement saw.
	Relation   GlobalRelationResult
	RelatedOld []model.LongTermMemory
	// Cursor is the progress this candidate was extracted at.
	Cursor FinalizeCursor
}

// IdempotencyKey is the draft-entry key of the candidate. It is content
// addressed, not trigger addressed: re-running the same session proposes the
// same key, so the draft upsert keeps one card instead of one card per
// trigger.
func (c SessionMemoryCandidate) IdempotencyKey() string {
	return "finalize:" + c.Fingerprint
}

// DraftInput maps the candidate onto the draft entry's input shape, provenance
// included: the profile class, the message references, the statement time and
// the source session ride with the candidate into the draft, and batch 1's
// confirmation carries them onto the confirmed memory. This is the full 来源
// contract the draft table now holds.
func (c SessionMemoryCandidate) DraftInput() ExplicitMemoryInput {
	var statementTime *time.Time
	if !c.StatementTime.IsZero() {
		local := c.StatementTime.In(time.Local)
		statementTime = &local
	}
	return ExplicitMemoryInput{
		Scope: c.Scope, Subject: c.Subject, Content: c.Content, Tags: c.Tags,
		TopicKey: c.TopicKey, Durability: c.Durability, ValidUntil: c.ValidUntil,
		IdempotencyKey:    c.IdempotencyKey(),
		ProfileClass:      c.Class,
		EvidenceMessages:  EvidenceRefStrings(c.Evidence),
		AssistantMessages: EvidenceRefStrings(c.AssistantUtterances),
		StatementTime:     statementTime,
		SourceSession:     c.Cursor.SessionID,
	}
}

// EvidenceRefStrings renders validated message references the way the draft
// and memory provenance columns carry them: "m<id>" per message, the format
// the fixed-candidate contract fixed.
func EvidenceRefStrings(refs []SessionEvidenceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	result := make([]string, 0, len(refs))
	for _, ref := range refs {
		result = append(result, fmt.Sprintf("m%d", ref.MessageID))
	}
	return result
}

func evidenceRefIDs(refs []SessionEvidenceRef) []uint {
	if len(refs) == 0 {
		return nil
	}
	result := make([]uint, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref.MessageID)
	}
	return result
}

func assistantRefIDs(refs []SessionEvidenceRef) []uint {
	return evidenceRefIDs(refs)
}

// DraftRequest is the full call the finalize handoff makes into the draft
// entry. The run id is derived from the cursor, not invented per trigger, so
// a repeated trigger of the same progress stays one proposal end to end.
func (c SessionMemoryCandidate) DraftRequest() DraftRequest {
	return DraftRequest{
		UserID: c.UserID, SessionID: c.Cursor.SessionID,
		RunID: FinalizeRunID(c.Cursor), Input: c.DraftInput(),
	}
}

// FinalizeRunID is the run id finalize work uses at the draft entry.
func FinalizeRunID(cursor FinalizeCursor) string {
	id := "fin-" + strconv.FormatUint(uint64(cursor.CoveredMessageID), 10)
	if utf8.RuneCountInString(id) > 36 {
		id = id[:36]
	}
	return id
}

// EvidenceJSON renders the message-level references the confirmation page
// expands into the original words. The draft table has no column for it yet;
// this is the structure the integration note asks the draft to carry. The
// statement reference is named apart, so the page can show which words the
// candidate's value rests on and which are context.
func (c SessionMemoryCandidate) EvidenceJSON() (string, error) {
	statementID := uint(0)
	for _, ref := range c.Evidence {
		if ref.Statement {
			statementID = ref.MessageID
			break
		}
	}
	payload := map[string]any{
		"scope": c.Scope, "class": c.Class, "kind": c.Kind,
		"statement_time":       c.StatementTime.Format(time.RFC3339),
		"statement_message_id": statementID,
		"evidence":             c.Evidence, "assistant_utterances": c.AssistantUtterances,
		"fingerprint": c.Fingerprint,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// FinalizeResult is what one trigger produced.
type FinalizeResult struct {
	SessionID string
	// Cursor is the progress this run reached. Extract advances it when
	// nothing is waiting for a judgement; the watermark rule itself - the
	// cursor moves only once every candidate is persisted or explicitly
	// skipped - is Handoff's, which is the call the integration trigger
	// makes.
	Cursor FinalizeCursor
	// Proposals are the validated candidates, in the order the proposer
	// returned them.
	Proposals []SessionMemoryCandidate
	// Skipped are the refusals, with reasons.
	Skipped []FinalizeSkip
	// RetryPending are the candidates whose relation judgement could not be
	// completed. They are kept for the next trigger, not proposed.
	RetryPending []SessionMemoryCandidate
	// NoNewContent says the cursor already covered the session's newest
	// message: nothing was proposed and the proposer was not called.
	NoNewContent bool
	// ProposerRejected says the extraction model answered with the reject
	// tool: it looked at the same words and judged that nothing there is
	// durable. That is a terminal answer and the watermark advances, but it
	// is recorded apart from "the model returned nothing usable" - the two
	// used to look identical in the run output, and that is exactly the
	// evidence the real-model rounds could not recover.
	ProposerRejected bool
	// BoardMissing says the session has no whiteboard yet. Extraction ran
	// from the recent window alone.
	BoardMissing bool
}

// ErrFinalizeNothingToRemember is the proposer's terminal "nothing here"
// answer: the model saw the words and rejected them. It is not a failure - it
// is a decision, and the watermark may move past it - but it is kept apart
// from a protocol failure so the run output can tell the two apart.
var ErrFinalizeNothingToRemember = errors.New("extraction model rejected the session")

// Extract runs one finalize extraction over a finished session.
func (s *SessionFinalizeService) Extract(ctx context.Context, request FinalizeRequest) (FinalizeResult, error) {
	if s == nil || s.proposer == nil || s.known == nil {
		return FinalizeResult{}, fmt.Errorf("session finalize service is not initialized")
	}
	if request.UserID <= 0 || strings.TrimSpace(request.Cursor.SessionID) == "" {
		return FinalizeResult{}, fmt.Errorf("invalid finalize request")
	}
	sessionID := strings.TrimSpace(request.Cursor.SessionID)
	result := FinalizeResult{SessionID: sessionID, Cursor: FinalizeCursor{SessionID: sessionID, CoveredMessageID: request.Cursor.CoveredMessageID}}

	// 未处理的已完成消息：从水位之后按批读，读到本轮预算为止。水位的
	// 语义因此是真正的处理进度——一个两千条之后还有新消息的会话，由下一
	// 次触发接着读，而不是被同一个"最早两千条"的界挡住。
	unprocessed, err := s.messages.LoadUnprocessedMessages(ctx, request.UserID, sessionID, request.Cursor.CoveredMessageID)
	if err != nil {
		return FinalizeResult{}, fmt.Errorf("load unprocessed messages: %w", err)
	}
	newest := uint(0)
	for _, message := range unprocessed {
		if message.ID > newest {
			newest = message.ID
		}
	}
	// Nothing new since the last trigger: no proposer call, no proposals,
	// the cursor stays where it was. This is the "no new content" branch the
	// plan's repeated-trigger rule rests on.
	if newest == 0 {
		result.NoNewContent = true
		return result, nil
	}

	board, boardMissing, err := s.loadBoard(ctx, request.UserID, sessionID)
	if err != nil {
		return FinalizeResult{}, err
	}
	result.BoardMissing = boardMissing

	// 白板线索指向的原话单独按引用读：线索可能指向本轮之前已覆盖的消息，
	// 不受水位限制，但集合由白板自身的引用结构决定大小。
	clue, err := s.messages.LoadSessionMessagesByID(ctx, request.UserID, sessionID, board.EvidenceMessageIDs())
	if err != nil {
		return FinalizeResult{}, fmt.Errorf("load board clue messages: %w", err)
	}

	input := &SessionExtractionInput{
		UserID: request.UserID, SessionID: sessionID,
		// The whiteboard is the clue the extraction is organised around: the
		// proposer has to see it, or a session whose board already held a
		// fact would reach the model as a plain message list. It stays a
		// clue only - every candidate is still validated against the
		// utterances before it can propose.
		Board:   board,
		Visible: visibleMessages(clue, unprocessed, s.RecentMessageWindow),
		Topics:  s.loadTopics(ctx, request.UserID),
	}
	// The board's user-origin message ids: a learner-first decision the
	// board already attributed to the learner. The adoption check uses it to
	// separate "the learner decided, the assistant restated" from "the
	// assistant proposed, nobody agreed".
	userBoardMessages := userStatementMessageIDs(board)
	// The board's assistant_accepted entries are the engine's own record of
	// which assistant proposal the learner took on, and with which learner
	// message. Extraction re-reads that pair instead of trusting the order of
	// the messages alone.
	acceptedByAssistant := boardAcceptedProposals(board, input.Visible)
	// A small set of confirmed current values, selected from the session's own
	// words, so the proposer can tell a new standing value from a one-off
	// adjustment. Losing this context degrades the judgement, not the run.
	if s.existingValues != nil {
		if values, valueErr := s.existingValues.RelevantExistingValues(ctx, request.UserID, sessionText(input.Visible), s.ExistingValueLimit); valueErr == nil {
			input.ExistingValues = values
		}
	}

	known, err := s.known.KnownCandidateFingerprints(ctx, request.UserID, sessionID)
	if err != nil {
		return FinalizeResult{}, fmt.Errorf("load known candidate fingerprints: %w", err)
	}

	raw, err := s.proposer.Propose(ctx, input)
	if err != nil {
		if errors.Is(err, ErrFinalizeNothingToRemember) {
			// A terminal decision, not a failure: the model looked at the
			// same words and judged that nothing is durable. The run records
			// the decision and the watermark moves past the progress.
			result.ProposerRejected = true
			result.Cursor.CoveredMessageID = newest
			return result, nil
		}
		// The proposer failed before returning anything: the run fails, the
		// cursor stays, and the next trigger retries the same progress.
		return FinalizeResult{}, fmt.Errorf("propose finalize candidates: %w", err)
	}

	ranges := s.loadRanges(ctx, request.UserID)
	// The coverage this run reports to every candidate it accepts: the
	// session's newest message, not the progress the trigger handed in.
	coverage := FinalizeCursor{SessionID: sessionID, CoveredMessageID: newest}
	accepted := make(map[string]struct{}, len(raw))
	for _, proposal := range raw {
		skip := s.validateShape(proposal)
		if skip != "" {
			result.Skipped = append(result.Skipped, FinalizeSkip{Reason: skip, Subject: proposal.Subject, EvidenceIDs: proposal.EvidenceIDs, AssistantIDs: proposal.AssistantIDs})
			continue
		}
		evidence, assistant, learnerFirstContent, skip := validateEvidence(proposal, input.Visible, userBoardMessages, acceptedByAssistant)
		if skip != "" {
			result.Skipped = append(result.Skipped, FinalizeSkip{Reason: skip, Subject: proposal.Subject, EvidenceIDs: proposal.EvidenceIDs, AssistantIDs: proposal.AssistantIDs})
			continue
		}
		if learnerFirstContent != "" {
			// 学习者先说、助手只是复述：卡片记录学习者原话。模型候选里超出
			// 这句原话的内容都是助手新增的，没有学习者采纳证据，不能进卡。
			proposal.Content = learnerFirstContent
		}
		candidate := s.buildCandidate(request.UserID, coverage, proposal, evidence, assistant, input.Topics, input.Visible)
		fingerprint := candidate.Fingerprint
		if _, seen := known[fingerprint]; seen {
			result.Skipped = append(result.Skipped, FinalizeSkip{Reason: FinalizeSkipDuplicateKnown, Subject: candidate.Subject, EvidenceIDs: proposal.EvidenceIDs, AssistantIDs: proposal.AssistantIDs})
			continue
		}
		if _, seen := accepted[fingerprint]; seen {
			result.Skipped = append(result.Skipped, FinalizeSkip{Reason: FinalizeSkipDuplicateRun, Subject: candidate.Subject, EvidenceIDs: proposal.EvidenceIDs, AssistantIDs: proposal.AssistantIDs})
			continue
		}
		entries, hashSkip := s.inRangeEntries(ctx, request.UserID, candidate, ranges)
		if hashSkip == FinalizeSkipDuplicateExisting {
			result.Skipped = append(result.Skipped, FinalizeSkip{Reason: FinalizeSkipDuplicateExisting, Subject: candidate.Subject, EvidenceIDs: proposal.EvidenceIDs, AssistantIDs: proposal.AssistantIDs})
			continue
		}
		relation, related, retry, err := s.reviewRelation(ctx, candidate, entries)
		if err != nil {
			return FinalizeResult{}, fmt.Errorf("review finalize relation: %w", err)
		}
		if retry {
			candidate.Relation, candidate.RelatedOld = relation, related
			result.RetryPending = append(result.RetryPending, candidate)
			continue
		}
		if relation.Relation == GlobalRelationDuplicate {
			// The same meaning is already covered by an active entry of the
			// range: the plan's "同一意思" row. The candidate is skipped
			// outright and the existing entry stands - the learner never sees
			// a card for it, and nothing is written. The judgement itself is
			// the record of why.
			result.Skipped = append(result.Skipped, FinalizeSkip{Reason: FinalizeSkipSemanticDuplicate, Subject: candidate.Subject, EvidenceIDs: proposal.EvidenceIDs, AssistantIDs: proposal.AssistantIDs})
			continue
		}
		candidate.Relation, candidate.RelatedOld = relation, related
		accepted[fingerprint] = struct{}{}
		result.Proposals = append(result.Proposals, candidate)
	}

	// The cursor advances only when nothing is waiting for a judgement. A
	// run with retry-pending candidates leaves the cursor where it was, so
	// the next trigger re-processes the same messages and the fingerprint
	// keeps the retry from proposing twice.
	if len(result.RetryPending) == 0 {
		result.Cursor.CoveredMessageID = newest
	}
	return result, nil
}

func (s *SessionFinalizeService) loadBoard(ctx context.Context, userID int64, sessionID string) (model.SessionWhiteboardDocument, bool, error) {
	stored, err := s.boards.GetWhiteboard(ctx, userID, sessionID)
	if err != nil {
		if errors.Is(err, model.ErrWhiteboardNotFound) {
			return model.SessionWhiteboardDocument{}, true, nil
		}
		return model.SessionWhiteboardDocument{}, false, fmt.Errorf("load session whiteboard: %w", err)
	}
	document, err := model.DecodeSessionWhiteboardDocument(stored.Content)
	if err != nil {
		// A board that cannot be decoded is not a clue, but the messages are
		// still there. Extraction continues from the window alone rather
		// than losing the session's candidates to an unreadable board.
		return model.SessionWhiteboardDocument{}, true, nil
	}
	return document, false, nil
}

func (s *SessionFinalizeService) loadTopics(ctx context.Context, userID int64) []Topic {
	if s.catalogue == nil {
		return nil
	}
	topics, err := s.catalogue.ListTopics(ctx, userID)
	if err != nil {
		// A catalogue failure does not fail the extraction: the candidates
		// simply propose without a prefill, and the confirmation flow asks
		// where each belongs. It is the same fallback the draft tool uses.
		return nil
	}
	return topics
}

// finalizeRanges holds the active entries of the ranges one run compares
// against, loaded once for the whole run.
type finalizeRanges struct {
	global    []model.LongTermMemory
	globalErr bool
	topic     map[string][]model.LongTermMemory
}

func (s *SessionFinalizeService) loadRanges(ctx context.Context, userID int64) finalizeRanges {
	ranges := finalizeRanges{topic: make(map[string][]model.LongTermMemory)}
	if s.globalMind != nil {
		entries, err := s.globalMind.ListAllActive(ctx, userID, model.MemoryScopeGlobal, 0, GlobalMemoryHardLimit+1)
		if err != nil {
			ranges.globalErr = true
		} else {
			ranges.global = entries
		}
	}
	return ranges
}

// inRangeEntries returns the active entries the candidate is compared
// against: the global profile for a global candidate, the active notes of
// its own topic for a topic candidate. It also runs the free duplicate check:
// content that is already an active memory of the range is skipped without a
// model call.
func (s *SessionFinalizeService) inRangeEntries(ctx context.Context, userID int64, candidate SessionMemoryCandidate, ranges finalizeRanges) ([]model.LongTermMemory, string) {
	var entries []model.LongTermMemory
	if candidate.Scope == model.MemoryScopeGlobal {
		if ranges.globalErr {
			return nil, ""
		}
		// 全局范围的比较集与草稿入口一致：入口侧读取 GlobalMemoryHardLimit+1
		// 条、在容量内把全部生效条目交给判定器。提取侧若用更小的窗口截前
		// 若干条，同一事项的旧值排到窗口外时这里会漏关联，两层对同一张卡
		// 给出不同结论。类别仍然不构成比较边界；不同事项由判定返回
		// independent。
		entries = boundedEntries(ranges.global, GlobalMemoryHardLimit)
	} else {
		if candidate.TopicKey == "" {
			// A pending-topic candidate has no range yet. The duplicate check
			// and the relation judgement both happen after the learner places
			// it, which is the plan's rule: a topic-undetermined candidate is
			// checked in its final home.
			return nil, ""
		}
		if s.topicMind == nil {
			return nil, ""
		}
		loaded, err := s.topicMind.ListActiveTopicEntries(ctx, userID, candidate.TopicKey, s.RelationTargets)
		if err != nil {
			// The comparison is best-effort the same way the conflict
			// snapshot is: a failed listing must not lose the candidate, and
			// the draft entry re-checks conflicts when the learner confirms.
			return nil, ""
		}
		entries = loaded
	}
	contentHash := model.MemoryContentHash(candidate.Content)
	for _, entry := range entries {
		if entry.ContentHash == contentHash {
			return entries, FinalizeSkipDuplicateExisting
		}
	}
	return entries, ""
}

// reviewRelation judges the candidate against its range. With no related
// entries there is nothing to judge and the candidate proposes as new; with
// entries the scope's resolver decides - the global classifier for the
// profile, the topic resolver for a topic note, which is the same resolver
// the draft edit path uses. A resolver failure keeps the candidate retryable
// instead of proposing it unjudged; a scope whose resolver is not wired
// proposes without a judgement and lets the draft entry's conflict rules
// decide.
func (s *SessionFinalizeService) reviewRelation(ctx context.Context, candidate SessionMemoryCandidate, entries []model.LongTermMemory) (GlobalRelationResult, []model.LongTermMemory, bool, error) {
	if len(entries) == 0 {
		return GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "no related entries in range"}, nil, false, nil
	}
	if candidate.Scope == model.MemoryScopeTopic {
		if s.topicRelations == nil {
			return GlobalRelationResult{Relation: GlobalRelationIndependent, Reason: "topic relation resolver not configured"}, entries, false, nil
		}
		relation, err := s.topicRelations.Classify(ctx, candidate.DraftInput(), entries)
		if err != nil {
			return GlobalRelationResult{}, entries, true, nil
		}
		// The two scopes share one relation vocabulary: the topic enum's
		// values are the global enum's values, so the candidate carries one
		// result shape regardless of where it lands.
		return GlobalRelationResult{Relation: GlobalRelation(relation.Relation), TargetMemoryID: relation.TargetMemoryID, Reason: relation.Reason}, entries, false, nil
	}
	if s.relations == nil {
		return GlobalRelationResult{}, entries, true, nil
	}
	relation, err := s.relations.Classify(ctx, candidate.DraftInput(), entries)
	if err != nil {
		return GlobalRelationResult{}, entries, true, nil
	}
	return relation, entries, false, nil
}

func boundedEntries(entries []model.LongTermMemory, limit int) []model.LongTermMemory {
	if len(entries) <= limit {
		return entries
	}
	return entries[:limit]
}

// userStatementMessageIDs collects the messages the board attributes to the
// learner (origin=user) across goal, constraints, plan and open items. These
// are the statements the learner already made in their own words; the
// adoption check treats one of them as the origin of a candidate the
// assistant merely restated.
func userStatementMessageIDs(board model.SessionWhiteboardDocument) map[uint]struct{} {
	ids := make(map[uint]struct{})
	add := func(entries []model.SessionWhiteboardEntry) {
		for _, entry := range entries {
			if entry.Origin != model.SessionWhiteboardOriginUser {
				continue
			}
			for _, id := range entry.SourceMessageIDs {
				ids[id] = struct{}{}
			}
		}
	}
	add(board.Constraints)
	add(board.Plan)
	add(board.OpenItems)
	// The goal carries no origin field: it is the learner's own goal by
	// construction, so its source messages are their words.
	if board.Goal != nil {
		for _, id := range board.Goal.SourceMessageIDs {
			ids[id] = struct{}{}
		}
	}
	return ids
}

// boardAcceptedProposal is one adoption the board recorded: the assistant
// message that made the proposal, the learner message after it that took the
// proposal on, and the confirmed content the board kept.
type boardAcceptedProposal struct {
	AcceptanceID uint
	Text         string
	Value        string
}

// boardAcceptedProposals reads the board's assistant_accepted entries into
// the pair the whiteboard engine recorded. A plain ID list cannot be combined
// into adoption pairs: the entry must name exactly one assistant message and
// exactly one learner message after it. With two proposals in one entry, the
// list alone cannot say which one the learner chose, so the entry yields no
// pair and the candidate is refused as insufficient evidence. Two entries
// that pair the same proposal with different acceptances or content are
// ambiguous for the same reason.
func boardAcceptedProposals(board model.SessionWhiteboardDocument, visible map[uint]SessionMessage) map[uint]boardAcceptedProposal {
	accepted := make(map[uint]boardAcceptedProposal)
	ambiguous := make(map[uint]struct{})
	add := func(entries []model.SessionWhiteboardEntry) {
		for _, entry := range entries {
			if entry.Origin != model.SessionWhiteboardOriginAssistantAccepted {
				continue
			}
			assistantIDs := make([]uint, 0, len(entry.SourceMessageIDs))
			learnerIDs := make([]uint, 0, len(entry.SourceMessageIDs))
			complete := true
			for _, id := range entry.SourceMessageIDs {
				message, found := visible[id]
				if !found {
					// A cited message we cannot see leaves the pair
					// unverifiable; it must not be guessed from the rest.
					complete = false
					break
				}
				if message.IsUser {
					learnerIDs = append(learnerIDs, id)
				} else {
					assistantIDs = append(assistantIDs, id)
				}
			}
			if !complete || len(assistantIDs) != 1 {
				continue
			}
			proposalID := assistantIDs[0]
			acceptances := make([]uint, 0, len(learnerIDs))
			for _, learnerID := range learnerIDs {
				if learnerID > proposalID {
					acceptances = append(acceptances, learnerID)
				}
			}
			if len(acceptances) != 1 {
				continue
			}
			if _, seen := ambiguous[proposalID]; seen {
				continue
			}
			pair := boardAcceptedProposal{AcceptanceID: acceptances[0], Text: entry.Text, Value: entry.Value}
			if existing, recorded := accepted[proposalID]; recorded {
				if existing != pair {
					delete(accepted, proposalID)
					ambiguous[proposalID] = struct{}{}
				}
				continue
			}
			accepted[proposalID] = pair
		}
	}
	add(board.Constraints)
	add(board.Plan)
	add(board.OpenItems)
	return accepted
}

// visibleMessages assembles the set the proposer may cite: every message the
// board cites, loaded by reference, plus a bounded window of the most recent
// unprocessed messages on both sides. The window is what keeps a fact the
// board missed extractable while its message is still unprocessed - the plan
// §6.1 read-back - and what keeps an adopted proposal's two sides in one
// input when the board did not carry the acceptance.
func visibleMessages(clue []SessionMessage, unprocessed []SessionMessage, window int) map[uint]SessionMessage {
	visible := make(map[uint]SessionMessage)
	for _, message := range clue {
		visible[message.ID] = message
	}
	if window > 0 && len(unprocessed) > 0 {
		ordered := append([]SessionMessage(nil), unprocessed...)
		sort.Slice(ordered, func(left, right int) bool { return ordered[left].ID < ordered[right].ID })
		if len(ordered) > window {
			ordered = ordered[len(ordered)-window:]
		}
		for _, message := range ordered {
			visible[message.ID] = message
		}
	}
	return visible
}

// validateShape checks the fields the program owns, before any source is
// believed. The class and kind vocabularies are fixed here because they are
// the words the memory page shows; the length limits are the memory's own;
// the durability rules are the memory shape rules the draft entry enforces
// again when the learner confirms.
func (s *SessionFinalizeService) validateShape(proposal SessionRawProposal) string {
	proposal.Scope = model.MemoryScope(strings.ToLower(strings.TrimSpace(string(proposal.Scope))))
	proposal.Subject = strings.TrimSpace(proposal.Subject)
	proposal.Content = strings.TrimSpace(proposal.Content)
	switch proposal.Scope {
	case model.MemoryScopeGlobal:
		// The profile class is the model package's enum, the same one the
		// draft and memory rows carry, and a global candidate must name its
		// slot - the class-scoped comparison has nothing to compare without
		// it.
		if !proposal.Class.Valid() {
			return FinalizeSkipInvalidShape
		}
	case model.MemoryScopeTopic:
		if proposal.Class != "" {
			// The provenance rule the model package enforces again at the
			// draft entry: a topic note has no profile slot.
			return FinalizeSkipInvalidShape
		}
		if strings.TrimSpace(proposal.Kind) == "" || !ValidFinalizeKind(proposal.Kind) {
			return FinalizeSkipInvalidShape
		}
	default:
		return FinalizeSkipInvalidShape
	}
	if proposal.Subject == "" || proposal.Content == "" ||
		utf8.RuneCountInString(proposal.Subject) > model.MemorySubjectMaxRunes ||
		utf8.RuneCountInString(proposal.Content) > model.MemoryContentMaxRunes {
		return FinalizeSkipInvalidShape
	}
	durability := model.MemoryDurability(strings.ToLower(strings.TrimSpace(string(proposal.Durability))))
	if !durability.Valid() {
		return FinalizeSkipInvalidShape
	}
	if proposal.ValidUntil != nil {
		local := proposal.ValidUntil.In(time.Local)
		proposal.ValidUntil = &local
	}
	// The same shape probe the explicit-write path uses, so a candidate that
	// passes here cannot fail there on scope, durability or validity. A
	// topic-scope proposal without a resolved key is probed under a
	// placeholder key: the one rule being deferred is the topic's presence,
	// which the pending-draft relaxation at the draft entry covers - every
	// other rule still applies here.
	topicKey := strings.TrimSpace(proposal.TopicKey)
	if proposal.Scope == model.MemoryScopeTopic && topicKey == "" {
		topicKey = "pending-validation"
	}
	probe := model.LongTermMemory{
		ID: "validation", UserID: 1, Scope: proposal.Scope, Subject: proposal.Subject, Content: proposal.Content,
		ContentHash: model.MemoryContentHash(proposal.Content), StructuredJSON: `{}`, Tags: model.StringList{"learning"},
		TopicKey: topicKey, Durability: durability, ValidUntil: proposal.ValidUntil,
		ProfileClass: proposal.Class,
		SourceRefs:   model.StringList{"validation"}, Confidence: 1, Importance: 8, Status: model.LongTermMemoryActive,
		UserConfirmed: true, CreatedAt: s.now(),
	}
	if err := probe.Validate(); err != nil {
		return FinalizeSkipInvalidShape
	}
	return ""
}

// validateEvidence checks the source of one proposal against the messages
// this extraction could see. It is the provenance boundary: the model says
// what the candidate is about, and the program says whether the words it
// rests on exist, belong to the learner, and - for an adopted proposal -
// include an acceptance that came after the proposal. It returns the evidence
// refs, the assistant refs, the content the card must carry (set only for the
// learner-first shape), and a skip reason.
func validateEvidence(proposal SessionRawProposal, visible map[uint]SessionMessage, userBoardMessages map[uint]struct{}, acceptedByAssistant map[uint]boardAcceptedProposal) ([]SessionEvidenceRef, []SessionEvidenceRef, string, string) {
	if len(proposal.EvidenceIDs) == 0 {
		return nil, nil, "", FinalizeSkipEvidenceMissing
	}
	// A candidate citing more messages than an entry may cite is describing
	// a stretch of history rather than one matter, which is a shape failure
	// - the same limit the whiteboard applies to an entry's evidence.
	if len(proposal.EvidenceIDs) > model.SessionWhiteboardEvidenceLimit {
		return nil, nil, "", FinalizeSkipInvalidShape
	}
	evidenceIDs, ok := dedupeIDs(proposal.EvidenceIDs, model.SessionWhiteboardEvidenceLimit)
	if !ok || len(evidenceIDs) == 0 {
		return nil, nil, "", FinalizeSkipEvidenceMissing
	}
	evidence := make([]SessionEvidenceRef, 0, len(evidenceIDs))
	for _, id := range evidenceIDs {
		message, found := visible[id]
		if !found {
			return nil, nil, "", FinalizeSkipEvidenceMissing
		}
		if !message.IsUser {
			return nil, nil, "", FinalizeSkipEvidenceRole
		}
		if strings.TrimSpace(message.Content) == "" {
			return nil, nil, "", FinalizeSkipEvidenceMissing
		}
		evidence = append(evidence, SessionEvidenceRef{MessageID: id, Role: model.SessionWhiteboardOriginUser, At: message.CreatedAt})
	}
	assistantIDs, _ := dedupeIDs(proposal.AssistantIDs, model.SessionWhiteboardEvidenceLimit)
	assistant := make([]SessionEvidenceRef, 0, len(assistantIDs))
	for _, id := range assistantIDs {
		message, found := visible[id]
		if !found {
			return nil, nil, "", FinalizeSkipEvidenceMissing
		}
		if message.IsUser {
			return nil, nil, "", FinalizeSkipEvidenceRole
		}
		assistant = append(assistant, SessionEvidenceRef{MessageID: id, Role: model.SessionWhiteboardOriginAssistant, At: message.CreatedAt})
	}
	learnerFirstContent := ""
	restatementOnly := false
	if len(assistant) > 0 {
		// Acceptance must point at the proposal this candidate cites. The
		// board records the pair - the assistant's proposal and the learner's
		// message that took it on - and extraction re-reads the same pair:
		// with two proposals in play, "go with the second one" must not let
		// the first one through. A proposal the board did not record as
		// accepted is not an adoption, however the later message reads. The
		// message also has to read like an acceptance; the board's record and
		// the learner's words have to agree.
		earliest := uint(0)
		for _, ref := range assistant {
			if earliest == 0 || ref.MessageID < earliest {
				earliest = ref.MessageID
			}
		}
		accepted := true
		for _, ref := range assistant {
			pair, recorded := acceptedByAssistant[ref.MessageID]
			if !recorded {
				accepted = false
				break
			}
			matched := false
			for _, ev := range evidence {
				if ev.MessageID != pair.AcceptanceID {
					continue
				}
				if ev.MessageID > ref.MessageID {
					if message, found := visible[ev.MessageID]; found && learnerAcceptedProposal(message.Content) {
						matched = true
					}
				}
				break
			}
			if !matched {
				accepted = false
				break
			}
			// 草稿必须与白板确认的那条记录一致：同一事项、同一个值、不
			// 冲突的适用范围。引用了正确的消息 ID 但正文写成别的值，等于
			// 把用户没选的提议写进草稿。
			if !learnerDraftMatchesAdoption(pair.Text, pair.Value, proposal.Content, proposal.Subject) {
				accepted = false
				break
			}
		}
		// Learner-first: the learner stated the decision and the assistant
		// only restated it afterwards. The statement evidence then predates
		// every cited assistant message, and the message must be one the
		// board already attributes to the learner (origin=user). Time order
		// and that attribution prove the learner spoke first; they do not
		// prove the candidate's content is what the learner said. The
		// statement has to support the content, and the card is built from
		// the part of the statement that supports this one matter: the
		// assistant only restated it, so anything else the assistant (or the
		// message's other clauses) added - a new matter, value or condition,
		// or a one-off operation - has no learner adoption and must not reach
		// the card. The restatement is not an adoption either, so it is
		// dropped from the assistant refs below: it may not enter
		// assistant_messages and may not name a topic.
		if !accepted {
			statementID := proposal.StatementEvidenceID
			if statementID == 0 && len(evidence) == 1 {
				statementID = evidence[0].MessageID
			}
			learnerFirst := false
			if statementID != 0 && statementID < earliest {
				if _, ok := userBoardMessages[statementID]; ok {
					statement, found := visible[statementID]
					if found {
						supported := learnerStatementSupportedContent(strings.TrimSpace(statement.Content), proposal.Content)
						if supported != "" && utf8.RuneCountInString(supported) <= model.MemoryContentMaxRunes {
							learnerFirst = true
							restatementOnly = true
							learnerFirstContent = supported
						}
					}
				}
			}
			if !learnerFirst {
				return nil, nil, "", FinalizeSkipAdoptionUnproven
			}
		}
	}
	// The statement reference is the one the 原话时间 follows. A proposal
	// that names a message outside its own evidence set is incoherent about
	// where its value came from, which is a shape failure. A proposal that
	// cites several messages must name one: the program cannot know which
	// utterance the value came from, and guessing is exactly the failure
	// the statement-time rule exists to remove. A single-evidence proposal
	// needs no name - there is only one message the time could come from.
	if proposal.StatementEvidenceID != 0 {
		found := false
		for i := range evidence {
			if evidence[i].MessageID == proposal.StatementEvidenceID {
				evidence[i].Statement = true
				found = true
				break
			}
		}
		if !found {
			return nil, nil, "", FinalizeSkipInvalidShape
		}
	} else if len(evidence) > 1 {
		return nil, nil, "", FinalizeSkipStatementEvidenceRequired
	} else {
		evidence[0].Statement = true
	}
	if restatementOnly {
		// 助手只是复述学习者先说的话，学习者没有明确采纳这条助手内容。它不
		// 是被采纳的助手原话：不进入 assistant_messages，也不能作为主题
		// 点名证据。
		assistant = nil
	}
	return evidence, assistant, learnerFirstContent, ""
}

// minLearnerStatementCoverage is the share of the candidate content's
// character pairs that the kept parts of the learner's statement must
// support. It is the same coverage rule the live draft tool applies to a
// draft built from the learner's current message
// (agenttool.draftContentFromCurrentMessage): a restatement loses only its
// leading subject, while an assistant invention loses the sentence it was
// built from.
const minLearnerStatementCoverage = 0.6

// minLearnerClauseSupport is the share of a clause's character pairs that
// must appear in the candidate content before the clause counts as being
// about this matter. It keeps a one-off operation that shares a stray pair
// (say the same number, or a common word) out of a standing decision's card.
const minLearnerClauseSupport = 0.3

// learnerStatementSupportedContent returns the parts of the learner's
// statement that support this one candidate matter, or "" when the statement
// does not support it at all. The learner-first shape uses it to build the
// card: the statement's clauses are kept only when they talk about the
// candidate, so a long-term decision does not drag a one-off operation from
// the same message into the card, and an assistant-added matter has no
// clause to stand on. The kept clauses must still cover most of the
// candidate's content, which is what separates "the learner said this" from
// "the model wrote something loosely related".
func learnerStatementSupportedContent(statement, content string) string {
	contentPairs := learnerStatementPairs(content)
	if len(contentPairs) == 0 {
		return ""
	}
	clauses := learnerStatementClauses(statement)
	kept := make([]learnerClause, 0, len(clauses))
	covered := make(map[string]struct{}, len(contentPairs))
	for _, clause := range clauses {
		pairs := learnerStatementPairs(clause.text)
		if len(pairs) == 0 {
			continue
		}
		supported := 0
		for pair := range pairs {
			if _, ok := contentPairs[pair]; ok {
				supported++
			}
		}
		if float64(supported)/float64(len(pairs)) < minLearnerClauseSupport {
			continue
		}
		// 字符相似度只负责找出可能的候选片段；最终还要核对值本身和适用
		// 范围。同一事项的临时值（今天先用 1.22）与长期值（以后都用 1.23）
		// 用词很接近，但值和范围都不同，不能进同一张卡。
		if !learnerClauseAgreesOnValue(clause.text, content) || learnerScopeConflict(clause.text, content) {
			continue
		}
		kept = append(kept, clause)
		for pair := range pairs {
			if _, ok := contentPairs[pair]; ok {
				covered[pair] = struct{}{}
			}
		}
	}
	if len(kept) == 0 || float64(len(covered))/float64(len(contentPairs)) < minLearnerStatementCoverage {
		return ""
	}
	var builder strings.Builder
	for index, clause := range kept {
		builder.WriteString(clause.text)
		if index+1 < len(kept) {
			builder.WriteString("，")
		} else if learnerSentenceEnd(clause.suffix) {
			builder.WriteString(clause.suffix)
		}
	}
	return builder.String()
}

// learnerClause is one clause of the statement: its trimmed text and the
// separator that followed it ("" for the last clause). The separator is kept
// so the selected spans read like the learner's original message.
type learnerClause struct {
	text   string
	suffix string
}

// learnerStatementClauses splits the statement on sentence and clause
// punctuation. A separator between two digits is part of a number ("1.23",
// "1,000"), not a break; the whiteboard's version numbers must survive the
// split or the clause that supports the candidate would be torn in two.
func learnerStatementClauses(text string) []learnerClause {
	runes := []rune(text)
	clauses := make([]learnerClause, 0, 4)
	appendClause := func(from, to int) {
		body := strings.TrimSpace(string(runes[from:to]))
		if body == "" {
			return
		}
		suffix := ""
		if to < len(runes) {
			suffix = string(runes[to])
		}
		clauses = append(clauses, learnerClause{text: body, suffix: suffix})
	}
	start := 0
	for index := 0; index < len(runes); index++ {
		separator := runes[index]
		if !learnerClauseSeparator(separator) {
			continue
		}
		if (separator == '.' || separator == ',') && index > 0 && index+1 < len(runes) &&
			unicode.IsDigit(runes[index-1]) && unicode.IsDigit(runes[index+1]) {
			continue
		}
		appendClause(start, index)
		start = index + 1
	}
	appendClause(start, len(runes))
	return clauses
}

func learnerClauseSeparator(r rune) bool {
	switch r {
	case '，', ',', '。', '.', '；', ';', '！', '!', '？', '?', '\n', '\r':
		return true
	}
	return false
}

// learnerSentenceEnd reports whether the separator ends a sentence rather
// than a clause. A selected span keeps that terminator; a trailing comma
// would just look like a cut-off sentence.
func learnerSentenceEnd(separator string) bool {
	switch separator {
	case "。", "！", "？", ".", "!", "?":
		return true
	}
	return false
}

// learnerValueTokens is the set of numbers and versions a text states.
// "1.23", "224" and "7.4" are values; a clause that states a different
// version from the candidate is about a different value, no matter how
// similar the wording is.
func learnerValueTokens(text string) map[string]struct{} {
	runes := []rune(text)
	values := make(map[string]struct{})
	for index := 0; index < len(runes); {
		if !unicode.IsDigit(runes[index]) {
			index++
			continue
		}
		start := index
		for index < len(runes) && (unicode.IsDigit(runes[index]) || runes[index] == '.') {
			index++
		}
		token := strings.Trim(string(runes[start:index]), ".")
		if token != "" {
			values[token] = struct{}{}
		}
	}
	return values
}

// learnerClauseAgreesOnValue reports whether every value in the clause is the
// candidate's value. A clause that states a number the candidate does not is
// not talking about the candidate's value, even when the words around it
// match.
func learnerClauseAgreesOnValue(clause, content string) bool {
	contentValues := learnerValueTokens(content)
	for value := range learnerValueTokens(clause) {
		if _, ok := contentValues[value]; !ok {
			return false
		}
	}
	return true
}

type learnerScopeClass int

const (
	learnerScopeUnspecified learnerScopeClass = iota
	learnerScopeStanding
	learnerScopeTemporary
)

// The markers are deliberately small: "先" and "都" are ordinary words in
// explanations, so only unambiguous temporal scope words count. Start-time
// phrases are stripped first: "从今天起长期用" starts a standing decision
// today - the date says when it begins, not how long it lasts - while a bare
// "今天用一次" is the one-off. A conflict needs a classified clause and a
// classified candidate; an unmarked side neither proves nor disproves the
// scope.
var (
	learnerStartMarkers     = []string{"从今天起", "从今天开始", "从今日起", "自今天起", "从今以后", "从现在起", "即日起", "今天起", "今天开始", "从明天起", "从明天开始"}
	learnerStandingMarkers  = []string{"以后", "从此", "长期", "一直", "每次", "永远"}
	learnerTemporaryMarkers = []string{"今天", "这次", "本次", "暂时", "临时", "只用一次", "只这一次", "仅此一次"}
)

func learnerScopeOf(text string) learnerScopeClass {
	folded := strings.ToLower(text)
	started := false
	for _, marker := range learnerStartMarkers {
		if strings.Contains(folded, marker) {
			started = true
			folded = strings.ReplaceAll(folded, marker, " ")
		}
	}
	for _, marker := range learnerTemporaryMarkers {
		if strings.Contains(folded, marker) {
			return learnerScopeTemporary
		}
	}
	for _, marker := range learnerStandingMarkers {
		if strings.Contains(folded, marker) {
			return learnerScopeStanding
		}
	}
	if started {
		// "从今天起使用 Go 1.23" names when a standing decision starts; the
		// date is the start time, not the duration. Without a temporary
		// marker the decision is standing.
		return learnerScopeStanding
	}
	return learnerScopeUnspecified
}

func learnerScopeConflict(clause, content string) bool {
	clauseScope, contentScope := learnerScopeOf(clause), learnerScopeOf(content)
	return clauseScope != learnerScopeUnspecified && contentScope != learnerScopeUnspecified && clauseScope != contentScope
}

// minAdoptionMatterCoverage is the share of the adopted record's character
// pairs the draft's subject and content must carry. The board entry is a
// summary, so the floor is lower than the learner-first content rule; the
// value and scope checks carry the exact part of the match.
const minAdoptionMatterCoverage = 0.35

// learnerDraftMatchesAdoption reports whether the draft records the adoption
// the board confirmed: the same matter, the same values, and a compatible
// scope. The message IDs prove which proposal was accepted; they do not prove
// what the draft says, so the content is checked against the record itself.
func learnerDraftMatchesAdoption(entryText, entryValue, content, subject string) bool {
	confirmed := strings.TrimSpace(entryText + "\n" + entryValue)
	draft := strings.TrimSpace(subject + "\n" + content)
	if confirmed == "" || draft == "" {
		return false
	}
	confirmedValues := learnerValueTokens(confirmed)
	draftValues := learnerValueTokens(draft)
	// A value in the record must be in the draft, and the draft cannot add a
	// value the adoption never covered: an adopted 7.2 must not be written as
	// 7.4, and a 7.4 must not be smuggled in beside it.
	for value := range confirmedValues {
		if _, ok := draftValues[value]; !ok {
			return false
		}
	}
	for value := range draftValues {
		if _, ok := confirmedValues[value]; !ok {
			return false
		}
	}
	if learnerScopeConflict(confirmed, draft) {
		return false
	}
	confirmedPairs := learnerStatementPairs(confirmed)
	if len(confirmedPairs) == 0 {
		return false
	}
	draftPairs := learnerStatementPairs(draft)
	covered := 0
	for pair := range confirmedPairs {
		if _, ok := draftPairs[pair]; ok {
			covered++
		}
	}
	return float64(covered)/float64(len(confirmedPairs)) >= minAdoptionMatterCoverage
}

// The acceptance cues are the learner's words that point back at the
// proposal or commit to it. Order alone is not adoption: "that's it for
// today", "let's talk about something else", and "I'll think about it" all
// come after the proposal too. Rejection cues win over an acceptance cue in
// the same message ("可以，但先不用").
var (
	learnerAcceptanceCues = []string{
		"就按", "按你说的", "按你刚说", "照你说的", "照你刚说", "听你的",
		"就这样", "就这么", "就这个", "那就这样", "照做",
		"采用", "同意", "赞成", "没问题",
		"记吧", "记下", "记住", "记下来", "定下来",
	}
	learnerRejectionCues = []string{
		"不用", "不要", "不需要", "不同意", "不赞成", "不采用",
		"别记", "算了", "再说吧", "先不", "不行", "取消", "暂时不",
	}
)

// learnerAcceptedProposal reports whether a later learner message takes the
// assistant proposal on. It is a cue check, not a semantic proof: it only
// admits messages that point back at the proposal or commit to it, so an
// ordinary closing, a topic switch, or a non-committal reply cannot ride on
// message order alone.
func learnerAcceptedProposal(message string) bool {
	text := strings.ToLower(strings.TrimSpace(message))
	if text == "" {
		return false
	}
	for _, cue := range learnerRejectionCues {
		if strings.Contains(text, cue) {
			return false
		}
	}
	for _, cue := range learnerAcceptanceCues {
		if strings.Contains(text, cue) {
			return true
		}
	}
	return false
}

// learnerStatementPairs is the set of adjacent character pairs a text is made
// of, with spacing, punctuation, symbols and case folded away. The live draft
// tool's content check uses the same shape, so the two entry points agree on
// what "the learner's own words" means.
func learnerStatementPairs(text string) map[string]struct{} {
	folded := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(text))
	runes := []rune(folded)
	if len(runes) == 0 {
		return nil
	}
	pairs := make(map[string]struct{}, len(runes))
	if len(runes) == 1 {
		pairs[string(runes)] = struct{}{}
		return pairs
	}
	for index := 0; index+1 < len(runes); index++ {
		pairs[string(runes[index:index+2])] = struct{}{}
	}
	return pairs
}

func dedupeIDs(ids []uint, limit int) ([]uint, bool) {
	if len(ids) == 0 || len(ids) > limit {
		return nil, false
	}
	seen := make(map[uint]struct{}, len(ids))
	result := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			return nil, false
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result, true
}

// buildCandidate assembles the validated proposal into the candidate the
// draft entry and the confirmation page receive. The topic is resolved here:
// an exact catalogue match prefills, everything else stays pending for the
// learner to place, because a topic the program guessed wrong is harder to
// fix after saving than before.
func (s *SessionFinalizeService) buildCandidate(userID int64, coverage FinalizeCursor, proposal SessionRawProposal,
	evidence, assistant []SessionEvidenceRef, topics []Topic, visible map[uint]SessionMessage) SessionMemoryCandidate {
	scope := model.MemoryScope(strings.ToLower(strings.TrimSpace(string(proposal.Scope))))
	subject := strings.TrimSpace(proposal.Subject)
	content := strings.TrimSpace(proposal.Content)
	durability := model.MemoryDurability(strings.ToLower(strings.TrimSpace(string(proposal.Durability))))
	topicKey := ""
	topicPending := false
	if scope == model.MemoryScopeTopic {
		key, matched := Resolve(topics, proposal.TopicKey)
		if matched && (proposal.TopicUnique || topicNamedInEvidence(key, topics, evidence, assistant, visible)) {
			topicKey = key
		} else {
			// Either the catalogue has no unique home, the model did not
			// declare the topic unique, or the learner never named it. The
			// candidate still proposes; the confirmation flow asks the learner
			// where it belongs. The prefill is a convenience, and a
			// convenience that guesses is worse than no prefill: several
			// topics can be plausible, and the learner's own words (or the
			// model's explicit "unique" answer) are the only evidence of which
			// one it is.
			topicKey = ""
			topicPending = true
		}
	}
	tags := GenerateQueryTags(subject+"\n"+content, topicKey)
	// The statement time comes from the reference marked as the value's
	// direct support. The earliest-time fallback only exists for a candidate
	// that arrived without any marked reference, which the validation above
	// no longer produces - it is kept because the caller may pass evidence
	// refs of its own making.
	statement := time.Time{}
	for _, ref := range evidence {
		if ref.Statement {
			statement = ref.At
			break
		}
	}
	if statement.IsZero() {
		for _, ref := range evidence {
			if statement.IsZero() || ref.At.Before(statement) {
				statement = ref.At
			}
		}
	}
	candidate := SessionMemoryCandidate{
		ID: uuid.NewString(), UserID: userID, Scope: scope,
		Class: proposal.Class, Kind: strings.TrimSpace(proposal.Kind),
		TopicKey: topicKey, TopicPending: topicPending,
		Subject: subject, Content: content, Tags: tags,
		Durability: durability, ValidUntil: proposal.ValidUntil,
		Evidence: evidence, AssistantUtterances: assistant,
		StatementTime: statement, Cursor: coverage,
	}
	candidate.Fingerprint = CandidateFingerprint(candidate.UserID, coverage.SessionID, candidate.Scope, candidate.TopicKey, candidate.Subject, candidate.Content)
	return candidate
}

// sessionText joins the learner's own visible words for relevance selection.
func sessionText(visible map[uint]SessionMessage) string {
	ids := make([]uint, 0, len(visible))
	for id, message := range visible {
		if message.IsUser {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	var builder strings.Builder
	for _, id := range ids {
		builder.WriteString(visible[id].Content)
		builder.WriteString("\n")
	}
	return builder.String()
}

// selectRelevantExistingValues picks the confirmed values that share the
// session's own tags - a bounded, deterministic relevance. Entries without a
// tag match are left out: the context is supposed to be a short list of
// directly related old values, not the whole profile.
func selectRelevantExistingValues(rows []model.LongTermMemory, sessionText string, limit int) []SessionExistingValue {
	if limit < 1 {
		return nil
	}
	wanted := GenerateQueryTags(sessionText, "")
	if len(wanted) == 0 {
		return nil
	}
	type scored struct {
		value SessionExistingValue
		score int
	}
	candidates := make([]scored, 0, len(rows))
	for _, row := range rows {
		if !row.UserConfirmed || row.Status != model.LongTermMemoryActive {
			continue
		}
		score := 0
		for _, tag := range row.Tags {
			for _, want := range wanted {
				if tag == want {
					score++
				}
			}
		}
		if score == 0 {
			continue
		}
		candidates = append(candidates, scored{
			value: SessionExistingValue{ID: row.ID, Subject: row.Subject, Content: row.Content},
			score: score,
		})
	}
	sort.SliceStable(candidates, func(left, right int) bool { return candidates[left].score > candidates[right].score })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	values := make([]SessionExistingValue, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, candidate.value)
	}
	return values
}

// topicNamedInEvidence reports whether the learner named the topic in the
// words the candidate rests on - or accepted an assistant proposal that named
// it. The topic's catalogue key and display name are the identities a name can
// take; a topic nobody named is left to the learner to choose, however exact
// the model's proposed key looks.
func topicNamedInEvidence(key string, topics []Topic, evidence, assistant []SessionEvidenceRef, visible map[uint]SessionMessage) bool {
	needles := []string{strings.ToLower(strings.TrimSpace(key))}
	for _, topic := range topics {
		if topic.Key != key {
			continue
		}
		if name := strings.ToLower(strings.TrimSpace(topic.Name)); name != "" && name != needles[0] {
			needles = append(needles, name)
		}
		break
	}
	named := func(refs []SessionEvidenceRef) bool {
		for _, ref := range refs {
			message, ok := visible[ref.MessageID]
			if !ok {
				continue
			}
			text := strings.ToLower(message.Content)
			for _, needle := range needles {
				if topicNameAppears(text, needle) {
					return true
				}
			}
		}
		return false
	}
	return named(evidence) || named(assistant)
}

// topicNameAppears reports whether the needle names the topic in the text.
//
// An ASCII-only needle must appear as a whole word: "rag" is inside
// "storage", and a substring match would let an unrelated word name the
// topic - the learner who writes "storage 层" has not named RAG. A needle
// that carries CJK content is matched as a phrase, because those scripts
// have no word delimiters; the byte before/after an ASCII run inside them is
// a boundary either way.
func topicNameAppears(text, needle string) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	if needle == "" {
		return false
	}
	if !asciiOnly(needle) {
		return strings.Contains(text, needle)
	}
	for offset := 0; offset <= len(text)-len(needle); {
		position := strings.Index(text[offset:], needle)
		if position < 0 {
			return false
		}
		start := offset + position
		end := start + len(needle)
		beforeOK := start == 0 || !asciiNameRune(text[start-1])
		afterOK := end == len(text) || !asciiNameRune(text[end])
		if beforeOK && afterOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func asciiOnly(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func asciiNameRune(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9')
}

// CandidateFingerprint identifies one candidate within one session: the same
// matter from the same session is the same candidate, whatever trigger
// produced it. The session is part of the key on purpose - the same sentence
// said again in a later session is a new expression for the learner to
// decide on, and suppressing it there is the draft entry's rule, not this
// module's.
func CandidateFingerprint(userID int64, sessionID string, scope model.MemoryScope, topicKey, subject, content string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\x00%s\x00%s", userID, sessionID, scope, topicKey, subject, content)
	return hex.EncodeToString(h.Sum(nil))
}

// DraftProposer is the narrow face of the draft entry the handoff calls.
// *GovernanceService satisfies it as-is; the integration wires it directly.
// The skip reason is the entry's own decision - a pending card the candidate
// coalesces into, a source-ignored retry, or content the active memory or a
// semantic judgement already covers.
type DraftProposer interface {
	ProposeDraft(ctx context.Context, request DraftRequest) (*model.MemoryDraft, bool, string, error)
}

// FailedProposal is a candidate whose draft write did not complete.
type FailedProposal struct {
	CandidateID string
	Err         error
}

// HandoffResult is what one finalize trigger left behind, including the
// draft-entry side of the handoff.
type HandoffResult struct {
	FinalizeResult
	// Persisted carries the draft id each accepted proposal produced, in
	// proposal order. A proposal the draft entry merged into an existing
	// card reports that card's id - persistence, not creation, is what
	// counts.
	Persisted []string
	// Failed are the candidates whose draft write errored. They are not
	// terminal: nothing decided their fate yet.
	Failed []FailedProposal
	// Advanced says the watermark moved. Cursor is safe to store either
	// way: it is held at the progress the trigger handed in unless every
	// candidate reached a terminal state, so storing it unconditionally is
	// always correct.
	Advanced bool
}

// Handoff extracts one finished session and hands every accepted candidate
// to the draft entry, and it owns the watermark rule: the progress advances
// only when every candidate reached a terminal state - persisted at the
// draft entry, or explicitly skipped with a reason here.
//
// A failed draft write does not stop the handoff: the rest of the run still
// persists, and the cursor simply stays where it was. The next trigger
// re-runs the same progress, the fingerprint keeps what already persisted
// from proposing twice, and the failed candidate gets its second try. That
// is the same recoverable failure the repeated-trigger rule exists for: a
// candidate whose fate is undecided is never silently advanced past.
func (s *SessionFinalizeService) Handoff(ctx context.Context, request FinalizeRequest, drafts DraftProposer) (HandoffResult, error) {
	if drafts == nil {
		return HandoffResult{}, fmt.Errorf("handoff requires the draft entry")
	}
	result, err := s.Extract(ctx, request)
	if err != nil {
		return HandoffResult{}, err
	}
	outcome := HandoffResult{FinalizeResult: result}
	for _, candidate := range result.Proposals {
		draft, created, skipReason, err := drafts.ProposeDraft(ctx, candidate.DraftRequest())
		if err != nil {
			outcome.Failed = append(outcome.Failed, FailedProposal{CandidateID: candidate.ID, Err: err})
			continue
		}
		// A non-empty skip reason is the draft entry's own decision - the
		// same dedup and conflict rules the manual and "please remember"
		// paths obey. The candidate reached a terminal state without a card:
		// recorded as an explicit skip, the watermark moves past it.
		if skipReason != "" {
			reason := FinalizeSkipDraftDuplicateExisting
			if skipReason == SkipIgnoredSource {
				reason = FinalizeSkipDraftIgnoredSource
			}
			outcome.Skipped = append(outcome.Skipped, FinalizeSkip{
				Reason: reason, Subject: candidate.Subject,
				EvidenceIDs: evidenceRefIDs(candidate.Evidence), AssistantIDs: assistantRefIDs(candidate.AssistantUtterances),
			})
			continue
		}
		id := ""
		if draft != nil {
			id = draft.ID
		}
		_ = created
		outcome.Persisted = append(outcome.Persisted, id)
	}
	outcome.Advanced = len(outcome.Failed) == 0 && len(result.RetryPending) == 0 && !result.NoNewContent
	if !outcome.Advanced {
		// Hold the watermark at the progress the trigger handed in: the
		// cursor the caller stores must never run past a candidate whose
		// fate is still undecided.
		outcome.Cursor = FinalizeCursor{SessionID: request.Cursor.SessionID, CoveredMessageID: request.Cursor.CoveredMessageID}
	}
	return outcome, nil
}
