package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The projector: one durable pass from a finished conversation to its episodic
// memory card. It loads a target-frozen view of the session, asks the generator
// for a typed outcome, applies the two-layer semantic boundary, reconciles
// assertion identities, and saves through optimistic concurrency keyed on the
// source watermark.

var (
	errProjectionSourceNotFound = errors.New("episode projection source not found")
	errProjectionTargetMissing  = errors.New("episode projection target message missing")
)

// newEpisodeUUID issues the card's id. It is opaque random hex: an id that
// encoded its owner or its position would be a handle someone could infer.
func newEpisodeUUID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic("service: episode id entropy unavailable: " + err.Error())
	}
	return "ep-" + hex.EncodeToString(buffer)
}

// ProjectionEvidence is a target-frozen view of one session. The summary is
// only used when its boundary does not exceed the target; Messages always
// contains the complete rows after that boundary and through the claimed
// target.
type ProjectionEvidence struct {
	UserID          string
	Session         domain.Session
	Summary         *domain.SessionSummary
	SummaryMessages []domain.Message
	Messages        []domain.Message
	Existing        *domain.Episode
	TargetMessageID string
	TargetMessageAt time.Time
}

// Validate re-checks the invariants the projector depends on, independently of
// how the view was assembled.
func (e ProjectionEvidence) Validate() error {
	if strings.TrimSpace(e.UserID) == "" || strings.TrimSpace(e.Session.ID) == "" ||
		e.Session.UserID != e.UserID || strings.TrimSpace(e.TargetMessageID) == "" || e.TargetMessageAt.IsZero() {
		return errProjectionSourceNotFound
	}
	if e.Summary != nil {
		if e.Summary.UserID != e.UserID || e.Summary.SessionID != e.Session.ID ||
			domain.CompareMessageIDs(e.Summary.CoveredMessageID, e.TargetMessageID) > 0 {
			return fmt.Errorf("invalid episode summary source")
		}
		for _, message := range e.SummaryMessages {
			if message.SessionID != e.Session.ID || domain.CompareMessageIDs(message.ID, e.Summary.CoveredMessageID) > 0 {
				return fmt.Errorf("invalid episode summary evidence message")
			}
		}
	}
	afterID := ""
	if e.Summary != nil {
		afterID = e.Summary.CoveredMessageID
	}
	previous := afterID
	for _, message := range e.Messages {
		if message.SessionID != e.Session.ID ||
			(previous != "" && domain.CompareMessageIDs(message.ID, previous) <= 0) ||
			domain.CompareMessageIDs(message.ID, e.TargetMessageID) > 0 {
			return fmt.Errorf("invalid episode message source")
		}
		previous = message.ID
	}
	if domain.CompareMessageIDs(afterID, e.TargetMessageID) < 0 {
		if len(e.Messages) == 0 || e.Messages[len(e.Messages)-1].ID != e.TargetMessageID {
			return errProjectionTargetMissing
		}
	}
	return nil
}

// EvidenceLoader loads the target-frozen session view.
type EvidenceLoader interface {
	Load(ctx context.Context, userID, sessionID, targetMessageID string) (ProjectionEvidence, error)
}

// RepositoryEvidenceLoader loads the projection's evidence from the same
// repository the conversation is stored in, so a projection can never see a
// session, message or summary through a different door than the turn that
// wrote it.
type RepositoryEvidenceLoader struct {
	repo            repository.Repository
	maxMessageCount int
}

// NewRepositoryEvidenceLoader builds the loader. The message cap has the same
// floor and ceiling as the configuration that names it.
func NewRepositoryEvidenceLoader(repo repository.Repository, maxMessageCount int) (*RepositoryEvidenceLoader, error) {
	if repo == nil {
		return nil, fmt.Errorf("episode evidence repository is required")
	}
	if maxMessageCount <= 0 {
		maxMessageCount = 4096
	}
	if maxMessageCount < 32 || maxMessageCount > 20000 {
		return nil, fmt.Errorf("episode evidence message limit must be between 32 and 20000")
	}
	return &RepositoryEvidenceLoader{repo: repo, maxMessageCount: maxMessageCount}, nil
}

// Load assembles the frozen view. Every read is owner-scoped: a session or
// message that belongs to somebody else is a miss, not a permission question.
func (l *RepositoryEvidenceLoader) Load(ctx context.Context, userID, sessionID, targetMessageID string) (ProjectionEvidence, error) {
	sessionID = strings.TrimSpace(sessionID)
	if l == nil || l.repo == nil || strings.TrimSpace(userID) == "" || sessionID == "" || strings.TrimSpace(targetMessageID) == "" {
		return ProjectionEvidence{}, errProjectionSourceNotFound
	}
	view := ProjectionEvidence{UserID: userID, TargetMessageID: targetMessageID}
	session, err := l.repo.GetSession(ctx, userID, sessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ProjectionEvidence{}, errProjectionSourceNotFound
		}
		return ProjectionEvidence{}, err
	}
	view.Session = *session

	messages, err := l.repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ProjectionEvidence{}, errProjectionSourceNotFound
		}
		return ProjectionEvidence{}, err
	}
	var targetIndex = -1
	for index, message := range messages {
		if message.ID == targetMessageID {
			targetIndex = index
			break
		}
	}
	if targetIndex < 0 {
		return ProjectionEvidence{}, errProjectionTargetMissing
	}
	view.TargetMessageAt = messages[targetIndex].CreatedAt

	summary, err := l.repo.GetSessionSummary(ctx, userID, sessionID)
	switch {
	case err == nil:
		// The summary is used only when its boundary does not exceed the
		// target: a summary that claims coverage past the claimed message
		// describes a conversation this projection has not seen.
		if domain.CompareMessageIDs(summary.CoveredMessageID, targetMessageID) <= 0 {
			view.Summary = summary
		}
	case errors.Is(err, repository.ErrNotFound):
		// No summary yet, which is the normal state of a young session.
	default:
		return ProjectionEvidence{}, err
	}

	afterID := ""
	if view.Summary != nil {
		afterID = view.Summary.CoveredMessageID
		// A summary boundary this session cannot locate means the summary's
		// coverage is undecidable, so it is not used as compressed context and
		// its provenance is not recorded: the projection reads the complete
		// messages instead, which is always safe, only less compressed.
		boundaryFound := false
		for _, message := range messages {
			if message.ID == afterID {
				boundaryFound = true
				break
			}
		}
		if !boundaryFound {
			view.Summary = nil
			afterID = ""
		}
	}
	for _, message := range messages {
		if afterID != "" && domain.CompareMessageIDs(message.ID, afterID) <= 0 {
			continue
		}
		if domain.CompareMessageIDs(message.ID, targetMessageID) > 0 {
			continue
		}
		view.Messages = append(view.Messages, message)
	}
	if len(view.Messages) > l.maxMessageCount {
		return ProjectionEvidence{}, fmt.Errorf("episode projection source exceeds %d complete messages", l.maxMessageCount)
	}

	existing, err := l.repo.GetEpisodeBySession(ctx, userID, sessionID)
	switch {
	case err == nil:
		view.Existing = existing
	case errors.Is(err, repository.ErrEpisodeNotFound):
	default:
		return ProjectionEvidence{}, err
	}
	return view, nil
}

// ProjectionRepository is the persistence seam the projector writes through.
type ProjectionRepository interface {
	GetEpisodeBySession(ctx context.Context, userID, sessionID string) (*domain.Episode, error)
	SaveEpisode(ctx context.Context, episode *domain.Episode, expectedVersion uint64) error
}

// Projector runs one projection pass.
type Projector struct {
	loader    EvidenceLoader
	generator Generator
	repo      ProjectionRepository
	// verifier is the second semantic layer. It is required at construction and
	// there is no setter, so a projector cannot exist in an unverified state.
	verifier SupportVerifier
}

// NewProjector refuses to build a projector that cannot verify.
//
// The verifier is a constructor argument rather than a setter because of what
// happened when it was a setter in the original project: the interface existed,
// tests injected fakes through it, and production never called it - so the
// second semantic layer was reachable only from a test while production passed
// every assertion straight through. Making it required moves the guarantee from
// "remember to call this" to "this does not compile otherwise".
func NewProjector(loader EvidenceLoader, generator Generator, repo ProjectionRepository, verifier SupportVerifier) (*Projector, error) {
	if loader == nil || generator == nil || repo == nil {
		return nil, fmt.Errorf("episode projector dependencies are required")
	}
	if verifier == nil {
		return nil, fmt.Errorf("the episode projector needs a support verifier: without one every " +
			"assertion a model produces would be stored unchecked")
	}
	return &Projector{loader: loader, generator: generator, repo: repo, verifier: verifier}, nil
}

// Project runs one pass for the claimed target. A repeat at a watermark already
// covered returns what is there without asking the model again - including an
// abstained row, which is the direct payoff of persisting an abstention rather
// than writing nothing.
func (p *Projector) Project(ctx context.Context, userID, sessionID, targetMessageID string) (*domain.Episode, error) {
	if p == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(targetMessageID) == "" {
		return nil, fmt.Errorf("invalid episode projection request")
	}
	projectionCtx := modelcall.WithPurpose(ctx, modelcall.PurposeEpisodeProjection)
	evidence, err := p.loader.Load(projectionCtx, userID, sessionID, targetMessageID)
	if err != nil {
		return nil, err
	}
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	existing := evidence.Existing
	if existing == nil {
		existing, err = p.repo.GetEpisodeBySession(projectionCtx, userID, sessionID)
		if err != nil && !errors.Is(err, repository.ErrEpisodeNotFound) {
			return nil, err
		}
		if errors.Is(err, repository.ErrEpisodeNotFound) {
			existing = nil
		}
	}
	if existing != nil && domain.CompareMessageIDs(existing.SourceLastMessageID, targetMessageID) >= 0 {
		return existing, nil
	}
	outcome, err := p.generator.Generate(projectionCtx, evidence)
	if err != nil {
		return nil, err
	}
	if outcome.Kind == OutcomeAbstained {
		return p.saveAbstention(projectionCtx, userID, sessionID, targetMessageID, evidence, existing, outcome.Abstain)
	}
	if outcome.Kind != OutcomeDocument {
		return nil, fmt.Errorf("episode projection returned an unknown outcome %q", outcome.Kind)
	}
	document := outcome.Document

	// The first, deterministic layer, applied here rather than only inside one
	// generator. Whatever produced this document, an assertion may only cite a
	// user message from this session; anything else refuses the projection.
	if err := EnforceEpisodeEvidenceBoundary(&document, evidence, evidence.Messages); err != nil {
		return nil, err
	}

	// The second semantic layer, before identities are issued and before
	// anything is persisted. Trimming here rather than after Save means a
	// refused assertion never gets an identity a later re-projection could
	// inherit.
	trimmed, removed, err := p.verifySupport(projectionCtx, document, evidence)
	if err != nil {
		return nil, err
	}
	document = trimmed
	if document.AssertionCount() == 0 {
		// Everything the model proposed was removed. That is a different fact
		// from "the model proposed nothing", and the reason says which.
		reason := domain.AbstainNoDurableAssertion
		if removed > 0 {
			reason = domain.AbstainAllAssertionsTrimmed
		}
		return p.saveAbstention(projectionCtx, userID, sessionID, targetMessageID, evidence,
			existing, AbstainRecord{Reason: reason, ConsideredUserMessages: countEpisodeUserMessages(evidence.Messages)})
	}

	// Stable identity is settled here, before anything is persisted: an id the
	// model echoed must be one this server issued, and a re-projection inherits
	// identities by fact rather than by position, so a card that moved an entry
	// keeps the entry's recorded identity.
	prior := []domain.Assertion(nil)
	if existing != nil {
		prior = domain.AssertionsOf(*existing)
	}
	document, err = domain.ReconcileDocumentIdentities(prior, existingEpisodeIDFor(existing), document)
	if err != nil {
		return nil, fmt.Errorf("reconcile episode assertions: %w", err)
	}
	document = datedEpisodeDocument(document, episodeMessageTimes(evidence))

	episode := &domain.Episode{
		ID: newEpisodeUUID(), UserID: userID, SessionID: sessionID,
		Title: titleForEpisode(evidence.Session.Title, document), Document: document,
		SearchText: searchTextForEpisode(document), TopicKeys: canonicalEpisodeTopicKeys(document),
		SourceLastMessageID: targetMessageID, LastMessageAt: evidence.TargetMessageAt,
		Status: domain.EpisodeStatusActive, IndexStatus: domain.EpisodeIndexPending,
		// An abstained row that becomes memory clears its reason; a row cannot
		// both be memory and be an explanation for the absence of memory.
		AbstainReason: "",
	}
	if evidence.Summary != nil {
		episode.SourceSummaryVersion = evidence.Summary.Version
		episode.SourceSummaryWatermark = evidence.Summary.CoveredMessageID
	}
	expectedVersion := uint64(0)
	if existing != nil {
		episode.ID = existing.ID
		expectedVersion = existing.Version
	}
	if err := p.repo.SaveEpisode(projectionCtx, episode, expectedVersion); err != nil {
		return nil, err
	}
	return episode, nil
}

func existingEpisodeIDFor(existing *domain.Episode) string {
	if existing == nil {
		return ""
	}
	return existing.ID
}

// verifySupport runs the second semantic layer over every assertion and returns
// the document with the unsupported ones removed. The rule is one rule: an
// assertion survives only when something says it is entailed. not_entailed
// removes it, uncertain removes it, and a verifier that errors or cannot be
// reached removes it. A check that keeps the assertion whenever it cannot
// answer is not a check.
func (p *Projector) verifySupport(ctx context.Context, document domain.EpisodeDocument, evidence ProjectionEvidence) (domain.EpisodeDocument, int, error) {
	if p.verifier == nil {
		return domain.EpisodeDocument{}, 0, fmt.Errorf("episode projector has no support verifier")
	}
	bodies := map[string]string{}
	for _, group := range [][]domain.Message{evidence.SummaryMessages, evidence.Messages} {
		for _, message := range group {
			if isUserMessage(message) {
				bodies[message.ID] = message.Content
			}
		}
	}
	removed := 0
	for _, group := range document.AssertionGroups() {
		kept := (*group.Entries)[:0]
		for _, entry := range *group.Entries {
			// Only the learner's own messages, and only the ones this assertion
			// cites. The verifier never sees assistant text, the synopsis, or a
			// sibling assertion.
			cited := make([]string, 0, len(entry.EvidenceMessageIDs))
			for _, id := range entry.EvidenceMessageIDs {
				if body, ok := bodies[id]; ok {
					cited = append(cited, body)
				}
			}
			verdict, err := p.verifier.Verify(ctx, SupportRequest{
				AssertionKind: group.Kind, AssertionText: entry.Text, UserMessages: cited})
			if err != nil || verdict != SupportEntailed {
				removed++
				continue
			}
			kept = append(kept, entry)
		}
		*group.Entries = kept
	}
	return document, removed, nil
}

// episodeMessageTimes indexes every message this projection saw, so an
// assertion can be dated from the evidence it cites rather than from the card.
func episodeMessageTimes(evidence ProjectionEvidence) map[string]time.Time {
	out := make(map[string]time.Time, len(evidence.Messages)+len(evidence.SummaryMessages))
	for _, group := range [][]domain.Message{evidence.SummaryMessages, evidence.Messages} {
		for _, message := range group {
			out[message.ID] = message.CreatedAt.UTC()
		}
	}
	return out
}

// datedEpisodeDocument stamps each assertion with the time of its latest
// evidence. The model never supplies a time: a decision is made once everything
// supporting it has been said, and two decisions inside one session would
// otherwise share the card's timestamp and become unorderable.
func datedEpisodeDocument(document domain.EpisodeDocument, times map[string]time.Time) domain.EpisodeDocument {
	stamp := func(entries []domain.EpisodeEvidence) []domain.EpisodeEvidence {
		out := make([]domain.EpisodeEvidence, 0, len(entries))
		for _, entry := range entries {
			latest, known := time.Time{}, false
			for _, id := range entry.EvidenceMessageIDs {
				at, ok := times[id]
				if !ok {
					continue
				}
				if !known || at.After(latest) {
					latest, known = at, true
				}
			}
			if known {
				entry.OccurredAt = latest
			}
			out = append(out, entry)
		}
		return out
	}
	for _, group := range document.AssertionGroups() {
		*group.Entries = stamp(*group.Entries)
	}
	return document
}

func titleForEpisode(sessionTitle string, document domain.EpisodeDocument) string {
	title := strings.TrimSpace(sessionTitle)
	if title == "" && len(document.Topics) > 0 {
		title = strings.TrimSpace(document.Topics[0].Label)
	}
	if title == "" {
		title = strings.TrimSpace(document.Synopsis)
	}
	return truncateEpisodeRunes(title, domain.EpisodeTitleMaxRunes)
}

func searchTextForEpisode(document domain.EpisodeDocument) string {
	parts := []string{strings.TrimSpace(document.Synopsis)}
	for _, topic := range document.Topics {
		parts = append(parts, strings.TrimSpace(topic.Label), strings.TrimSpace(topic.Key))
	}
	for _, entries := range [][]domain.EpisodeEvidence{document.Decisions, document.OpenLoops, document.UserCorrections} {
		for _, entry := range entries {
			parts = append(parts, strings.TrimSpace(entry.Text))
		}
	}
	return truncateEpisodeRunes(strings.Join(parts, "\n"), domain.EpisodeSearchTextMaxRunes)
}

func truncateEpisodeRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit]))
}

// saveAbstention records that this session was read and held nothing. It
// reuses the session's existing row when there is one, so a session that was
// memory and is being re-projected into nothing does not leave two rows, and a
// session that abstains twice does not either. The row keeps its watermark, so
// the next repeat at the same watermark short-circuits without calling a model.
func (p *Projector) saveAbstention(ctx context.Context, userID, sessionID, targetMessageID string,
	evidence ProjectionEvidence, existing *domain.Episode, record AbstainRecord) (*domain.Episode, error) {
	if !record.Reason.Valid() {
		return nil, fmt.Errorf("episode abstention needs a known reason, got %q", record.Reason)
	}
	episode := &domain.Episode{
		ID: newEpisodeUUID(), UserID: userID, SessionID: sessionID,
		Title: episodeAbstainTitle,
		// An abstained row carries no assertions, and its synopsis names what
		// happened rather than summarising a session nobody is remembering.
		Document: domain.EpisodeDocument{
			Synopsis:      episodeAbstainSynopsis,
			Topics:        []domain.EpisodeTopic{},
			SourceHandles: []string{fmt.Sprintf("abstain:%s:%s", sessionID, targetMessageID)},
		},
		// Fixed text, not the learner's words. The row is an operational
		// record, and SearchText on an abstained row is never indexed: the index
		// write path refuses anything that is not active.
		SearchText:          episodeAbstainSynopsis,
		TopicKeys:           []string{},
		SourceLastMessageID: targetMessageID,
		LastMessageAt:       evidence.TargetMessageAt,
		Status:              domain.EpisodeStatusAbstained,
		IndexStatus:         domain.EpisodeIndexIndexed,
		AbstainReason:       record.Reason,
	}
	if evidence.Summary != nil {
		episode.SourceSummaryVersion = evidence.Summary.Version
		episode.SourceSummaryWatermark = evidence.Summary.CoveredMessageID
	}
	expectedVersion := uint64(0)
	if existing != nil {
		episode.ID = existing.ID
		expectedVersion = existing.Version
	}
	if err := p.repo.SaveEpisode(ctx, episode, expectedVersion); err != nil {
		return nil, err
	}
	return episode, nil
}

// episodeAbstainTitle and episodeAbstainSynopsis are what an operator sees in a
// listing. Fixed text, never the learner's own words: an abstained row is an
// operational record, and the one thing it must not do is carry content out of
// a session nobody decided to remember.
const (
	episodeAbstainTitle    = "（未保存：无长期内容）"
	episodeAbstainSynopsis = "本次会话没有需要长期保存的内容。"
)
