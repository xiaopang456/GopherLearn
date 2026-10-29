package episode

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	episodedao "GopherAI/dao/episode"
	"github.com/google/uuid"
)

type ProjectionRepository interface {
	GetBySession(context.Context, int64, string) (*model.EpisodeMemory, error)
	Save(context.Context, *model.EpisodeMemory, uint64) error
}

// GovernanceIndex is the smallest read-only question the projector has to ask
// of assertion governance: which assertions in this card must survive a
// re-projection.
//
// It is an interface here rather than a repository dependency because the
// projector must keep working, unchanged, in every mode that has no governance
// at all. When none is attached the old behaviour stands exactly as measured;
// when one is attached the projection fails closed instead of orphaning a
// relation.
type GovernanceIndex interface {
	// GovernedAssertions returns the assertions of one Episode that carry a
	// current or replaced state, together with those another assertion names as
	// its replacement. Both are things a relation points at, and a relation
	// pointing at nothing is invisible.
	GovernedAssertions(ctx context.Context, userID int64, episodeID string) (map[string]struct{}, error)
}

type Projector struct {
	loader     EvidenceLoader
	generator  Generator
	repository ProjectionRepository
	// governance is optional and set only by a mode that governs assertion
	// time. Nil means no governance exists to orphan.
	governance GovernanceIndex
	// verifier is the second semantic layer. It is required at construction and
	// there is no setter, so a projector cannot exist in an unverified state.
	verifier SupportVerifier
}

// NewProjector refuses to build a projector that cannot verify.
//
// The verifier is a constructor argument rather than a setter because of what
// happened when it was a setter: the interface existed, tests injected fakes
// through it, and main.go never called it - so the second semantic layer was
// reachable only from a test while production passed every assertion straight
// through. A boundary that is off by default is not a boundary.
//
// Making it required moves the guarantee from "remember to call this" to "this
// does not compile otherwise", which is the only version of it that survives a
// future assembly change nobody reviews closely.
func NewProjector(loader EvidenceLoader, generator Generator,
	repository ProjectionRepository, verifier SupportVerifier) (*Projector, error) {
	if loader == nil || generator == nil || repository == nil {
		return nil, fmt.Errorf("episode projector dependencies are required")
	}
	if verifier == nil {
		return nil, fmt.Errorf(
			"the episode projector needs a support verifier: without one every " +
				"assertion a model produces would be stored unchecked")
	}
	return &Projector{loader: loader, generator: generator,
		repository: repository, verifier: verifier}, nil
}

// UseGovernanceIndex makes re-projection fail closed on orphaned relations.
//
// Only a mode that governs assertion time attaches one. Attaching it to the
// shipped default would change what a projection does in a mode whose results
// are already published, and would do it for relations that mode never writes.
func (p *Projector) UseGovernanceIndex(index GovernanceIndex) {
	if p == nil {
		return
	}
	p.governance = index
}

func (p *Projector) Project(ctx context.Context, userID int64, sessionID string, targetMessageID uint) (*model.EpisodeMemory, error) {
	if p == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" || targetMessageID == 0 {
		return nil, fmt.Errorf("invalid episode projection request")
	}
	evidence, err := p.loader.Load(ctx, userID, sessionID, targetMessageID)
	if err != nil {
		return nil, err
	}
	if err := evidence.Validate(); err != nil {
		return nil, err
	}
	existing := evidence.Existing
	if existing == nil {
		existing, err = p.repository.GetBySession(ctx, userID, sessionID)
		if err != nil && !errors.Is(err, episodedao.ErrEpisodeNotFound) {
			return nil, err
		}
		if errors.Is(err, episodedao.ErrEpisodeNotFound) {
			existing = nil
		}
	}
	// A repeat at a watermark already covered returns what is there without
	// asking the model again. This now covers abstained rows too, which is the
	// direct payoff of persisting an abstention rather than writing nothing:
	// without the row, every retry would re-run the model to re-discover that
	// the session holds nothing.
	if existing != nil && existing.SourceLastMessageID >= targetMessageID {
		return existing, nil
	}
	outcome, err := p.generator.Generate(ctx, evidence)
	if err != nil {
		return nil, err
	}
	if outcome.Kind == OutcomeAbstained {
		return p.saveAbstention(ctx, userID, sessionID, targetMessageID, evidence,
			existing, outcome.Abstain)
	}
	if outcome.Kind != OutcomeDocument {
		return nil, fmt.Errorf("episode projection returned an unknown outcome %q", outcome.Kind)
	}
	document := outcome.Document

	// The first, deterministic layer, applied here rather than only inside one
	// Generator. Whatever produced this document, an assertion may only cite a
	// message from this session; anything else refuses the projection.
	if err := EnforceEvidenceBoundary(&document, evidence, evidence.Messages); err != nil {
		return nil, err
	}

	// The second semantic layer, before identities are issued and before
	// anything is persisted. Trimming here rather than after Save means a
	// refused assertion never gets an identity a relation could later point at.
	trimmed, removed, err := p.verifySupport(ctx, document, evidence)
	if err != nil {
		return nil, err
	}
	document = trimmed
	if document.AssertionCount() == 0 {
		// Everything the model proposed was removed. That is a different fact
		// from "the model proposed nothing", and the reason says which.
		reason := model.AbstainNoDurableAssertion
		if removed > 0 {
			reason = model.AbstainAllAssertionsTrimmed
		}
		return p.saveAbstention(ctx, userID, sessionID, targetMessageID, evidence,
			existing, AbstainRecord{Reason: reason,
				ConsideredUserMessages: countUserMessages(evidence.Messages)})
	}
	// Stable identity is settled here, before anything is persisted.
	//
	// v1.1 built the reconciler and left it where only tests could reach it, so
	// production went on writing documents whose assertions were identified by
	// array position. A relation recorded against such an assertion silently
	// re-points at whatever moves into that slot, which is worse than losing
	// it: it keeps rendering, and it is now about a different fact.
	document, err = p.reconcileIdentities(ctx, existing, document, evidence)
	if err != nil {
		return nil, err
	}
	sourceGroups := make([]model.EpisodeSourceRefs, 0, len(evidence.Messages))
	for _, message := range evidence.Messages {
		if len(message.EpisodeSources) > 0 {
			sourceGroups = append(sourceGroups, message.EpisodeSources)
		}
	}
	sourceEpisodes, err := model.MergeEpisodeSourceRefs(sourceGroups...)
	if err != nil {
		return nil, fmt.Errorf("merge episode provenance: %w", err)
	}

	episode := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		Title: titleForEpisode(evidence.Session.Title, document), Document: document,
		SearchText: searchTextForEpisode(document), TopicKeys: canonicalTopicKeys(document),
		SourceEpisodes:      sourceEpisodes,
		SourceLastMessageID: targetMessageID, LastMessageAt: evidence.TargetMessageAt,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
		// An abstained row that becomes memory clears its reason; a row cannot
		// both be memory and be an explanation for the absence of memory.
		AbstainReason: "",
	}
	if episodeSensitivePattern.MatchString(episode.Title + "\n" + episode.SearchText) {
		return nil, ErrEpisodeSensitive
	}
	expectedVersion := uint64(0)
	if evidence.Summary != nil {
		episode.SourceSummaryVersion = evidence.Summary.Version
		episode.SourceSummaryWatermark = evidence.Summary.CoveredMessageID
	}
	if existing != nil {
		episode.ID = existing.ID
		expectedVersion = existing.Version
	}
	if err := p.repository.Save(ctx, episode, expectedVersion); err != nil {
		return nil, err
	}
	return episode, nil
}

// reconcileIdentities assigns every assertion its stable identity and its own
// occurrence time.
//
// It runs before Save, and it refuses rather than guesses: an identity the
// model returned that this server never issued, an identity returned twice, an
// identity used for the wrong kind, or a re-projection that cannot be matched
// one-to-one all fail the projection. A projection that silently dropped a
// bad identity and treated the assertion as new would orphan whatever
// governance pointed at it, and an orphaned relation is invisible.
func (p *Projector) reconcileIdentities(ctx context.Context, existing *model.EpisodeMemory,
	document model.EpisodeDocument, evidence ProjectionEvidence) (model.EpisodeDocument, error) {
	var prior []model.Assertion
	if existing != nil {
		prior = model.AssertionsOf(*existing)
	}
	governed, err := p.governedAssertions(ctx, evidence.UserID, existing, prior)
	if err != nil {
		return model.EpisodeDocument{}, err
	}
	reconciled, err2 := model.ReconcileDocumentIdentities(prior, model.EpisodeMemory{
		ID: episodeIDFor(existing), UserID: evidence.UserID, Document: document,
	})
	if err2 != nil {
		return model.EpisodeDocument{}, fmt.Errorf("reconcile episode assertions: %w", err2)
	}
	reconciled = datedDocument(reconciled, messageTimes(evidence))

	// A governed assertion that no longer exists after this projection would
	// leave a relation pointing at nothing. The caller is told rather than the
	// relation being quietly dropped.
	surviving := model.AssertionsOf(model.EpisodeMemory{ID: episodeIDFor(existing), Document: reconciled})
	if orphans := model.OrphanedGovernance(governed, surviving); len(orphans) != 0 {
		p.reportOrphans(existing, orphans)
		if p.governance != nil {
			// Fail closed, and fail before Save: the alternative is a card that
			// no longer contains the assertion a relation points at, which is
			// not a recoverable state - the relation is still in the state
			// table, still says "this was replaced by that", and there is no
			// longer a "this" to show. Refusing leaves the previous Episode
			// version intact and the relation still resolvable, which is the
			// only outcome a later fix can start from.
			return model.EpisodeDocument{}, fmt.Errorf(
				"%w: %d governed assertion(s) would lose their evidence", ErrOrphanedGovernance, len(orphans))
		}
	}
	return reconciled, nil
}

// ErrOrphanedGovernance is a re-projection that would leave a governance
// relation pointing at an assertion the card no longer contains.
var ErrOrphanedGovernance = errors.New("re-projection would orphan governed assertions")

// governedAssertions is which of this card's prior assertions must survive.
//
// Without a governance index nothing is governed, which is what every mode
// before this one measured. With one, the answer is the index's - and it is
// intersected with what the card actually held, so a relation belonging to
// another card can never fail this one. A store failure is an error: an empty
// map read as "nothing is governed" would turn fail-closed into fail-open at
// precisely the moment the database is unwell.
func (p *Projector) governedAssertions(ctx context.Context, userID int64,
	existing *model.EpisodeMemory, prior []model.Assertion) ([]string, error) {
	if p.governance == nil || existing == nil {
		return nil, nil
	}
	indexed, err := p.governance.GovernedAssertions(ctx, userID, existing.ID)
	if err != nil {
		return nil, fmt.Errorf("read assertion governance: %w", err)
	}
	governed := []string{}
	for _, item := range prior {
		if _, ok := indexed[item.ID]; ok {
			governed = append(governed, item.ID)
		}
	}
	return governed, nil
}

// reportOrphans records assertions that lost their evidence in this projection.
// It logs identifiers and counts, never learner text.
func (p *Projector) reportOrphans(existing *model.EpisodeMemory, orphans []string) {
	if existing == nil {
		return
	}
	log.Printf("episode_projection orphaned_assertions episode=%s count=%d", existing.ID, len(orphans))
}

func episodeIDFor(existing *model.EpisodeMemory) string {
	if existing == nil {
		return ""
	}
	return existing.ID
}

// messageTimes indexes every message this projection saw, so an assertion can
// be dated from the evidence it cites rather than from the card.
func messageTimes(evidence ProjectionEvidence) map[uint]time.Time {
	out := make(map[uint]time.Time, len(evidence.Messages)+len(evidence.SummaryMessages))
	for _, group := range [][]model.Message{evidence.SummaryMessages, evidence.Messages} {
		for _, message := range group {
			out[message.ID] = message.CreatedAt.UTC()
		}
	}
	return out
}

// datedDocument stamps each assertion with the time of its latest evidence.
//
// The model never supplies a time. It is derived here because a decision is
// made once everything supporting it has been said, and because two decisions
// inside one session would otherwise share the card's timestamp and become
// unorderable against each other.
func datedDocument(document model.EpisodeDocument, times map[uint]time.Time) model.EpisodeDocument {
	stamp := func(entries []model.EpisodeEvidence) []model.EpisodeEvidence {
		out := make([]model.EpisodeEvidence, 0, len(entries))
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

func titleForEpisode(sessionTitle string, document model.EpisodeDocument) string {
	title := strings.TrimSpace(sessionTitle)
	if title == "" && len(document.Topics) > 0 {
		title = strings.TrimSpace(document.Topics[0].Label)
	}
	if title == "" {
		title = strings.TrimSpace(document.Synopsis)
	}
	return truncateRunes(title, model.EpisodeTitleMaxRunes)
}

func searchTextForEpisode(document model.EpisodeDocument) string {
	parts := []string{strings.TrimSpace(document.Synopsis)}
	for _, topic := range document.Topics {
		parts = append(parts, strings.TrimSpace(topic.Label), strings.TrimSpace(topic.Key))
	}
	for _, entries := range [][]model.EpisodeEvidence{document.Decisions, document.OpenLoops, document.UserCorrections} {
		for _, entry := range entries {
			parts = append(parts, strings.TrimSpace(entry.Text))
		}
	}
	return truncateRunes(strings.Join(parts, "\n"), model.EpisodeSearchTextMaxRunes)
}

// SearchTextForEpisode exposes the production search-text recipe so the
// M1/M2 paired harness banks and indexes exactly the text the projector
// would have stored, instead of writing a second composition rule.
func SearchTextForEpisode(document model.EpisodeDocument) string {
	return searchTextForEpisode(document)
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit]))
}

// verifySupport runs the second semantic layer over every assertion and returns
// the document with the unsupported ones removed.
//
// The rules are all one rule: an assertion survives only when something says it
// is entailed. not_entailed removes it, uncertain removes it, and a verifier
// that errors or cannot be reached removes it. A check that keeps the assertion
// whenever it cannot answer is not a check, and the failure it would let
// through is the one that matters - an unverifiable claim about what a learner
// said, stored as if they had said it.
//
// A nil verifier cannot normally occur - the constructor refuses one - so
// finding one here means a projector was assembled some other way. That fails
// the projection rather than passing everything through, because the version of
// this code that passed everything through is precisely what C1.1 is undoing.
func (p *Projector) verifySupport(ctx context.Context, document model.EpisodeDocument,
	evidence ProjectionEvidence) (model.EpisodeDocument, int, error) {
	if p.verifier == nil {
		return model.EpisodeDocument{}, 0, fmt.Errorf(
			"episode projector has no support verifier")
	}
	bodies := map[uint]string{}
	for _, group := range [][]model.Message{evidence.SummaryMessages, evidence.Messages} {
		for _, message := range group {
			if message.IsUser {
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
			// sibling assertion: a checker given the generator's whole context
			// can reproduce the generator's mistake for the generator's reasons.
			cited := make([]string, 0, len(entry.EvidenceMessageIDs))
			for _, id := range entry.EvidenceMessageIDs {
				if body, ok := bodies[id]; ok {
					cited = append(cited, body)
				}
			}
			verdict, err := p.verifier.Verify(ctx, SupportRequest{
				AssertionKind: group.Kind, AssertionText: entry.Text, UserMessages: cited})
			// Only an explicit "entailed" keeps the assertion. not_entailed,
			// uncertain, an unknown word, an empty answer and an error all trim
			// it, and they are one branch because they mean one thing here:
			// nothing said this sentence is what the learner said.
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

// saveAbstention records that this session was read and held nothing.
//
// It reuses the session's existing row when there is one, so a session that was
// memory and is being re-projected into nothing does not leave two rows, and a
// session that abstains twice does not either. The row keeps its watermark, so
// the next repeat at the same watermark short-circuits without calling a model.
func (p *Projector) saveAbstention(ctx context.Context, userID int64, sessionID string,
	targetMessageID uint, evidence ProjectionEvidence, existing *model.EpisodeMemory,
	record AbstainRecord) (*model.EpisodeMemory, error) {
	if !record.Reason.Valid() {
		return nil, fmt.Errorf("episode abstention needs a known reason, got %q", record.Reason)
	}
	episode := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: userID, SessionID: sessionID,
		Title: abstainTitle(evidence),
		// An abstained row carries no assertions, and its synopsis names what
		// happened rather than summarising a session nobody is remembering.
		Document: model.EpisodeDocument{
			Synopsis:      abstainSynopsis,
			Topics:        []model.EpisodeTopic{},
			SourceHandles: []string{abstainHandle(sessionID, targetMessageID)},
		},
		// Fixed text, not the learner's words. The row is an operational
		// record, and SearchText on an abstained row is never indexed: the
		// index write path refuses anything that is not active.
		SearchText:          abstainSynopsis,
		TopicKeys:           model.StringList{},
		SourceLastMessageID: targetMessageID,
		LastMessageAt:       evidence.TargetMessageAt,
		Status:              model.EpisodeStatusAbstained,
		IndexStatus:         model.EpisodeIndexIndexed,
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
	if err := p.repository.Save(ctx, episode, expectedVersion); err != nil {
		return nil, err
	}
	return episode, nil
}

// abstainTitle is what an operator sees in a listing. It is fixed text, never
// the learner's own words: an abstained row is an operational record, and the
// one thing it must not do is carry content out of a session nobody decided to
// remember.
func abstainTitle(ProjectionEvidence) string { return "（未保存：无长期内容）" }

// abstainSynopsis is the fixed body of an abstained row. It says what happened
// without quoting any part of the session, which is the point: nobody decided
// to remember this conversation, so none of it is carried forward.
const abstainSynopsis = "本次会话没有需要长期保存的内容。"

func abstainHandle(sessionID string, targetMessageID uint) string {
	return fmt.Sprintf("abstain:%s:%d", sessionID, targetMessageID)
}
