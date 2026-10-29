package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// EpisodeSystem is the assembled episodic-memory subsystem: the publisher the
// conversation service triggers, the worker that projects, the indexer that
// maintains the derived view, the retriever that recalls, and the per-turn
// evidence ledger that makes read-back and citation share one boundary.
//
// It exists as one struct because the pieces share one invariant - the store is
// the only authority, and everything else is a derived view or a derived
// claim - and a caller that assembled the pieces separately could not be
// prevented from mixing views from different stores.

// EpisodeSystemConfig is everything the system needs to run.
type EpisodeSystemConfig struct {
	Worker EpisodeWorkerConfig
	// QuietWindow is the projection debounce.
	QuietWindow time.Duration
	// MaxMessages bounds one projection's evidence read.
	MaxMessages int
	// MaxInputTokens and MaxOutputTokens bound the generator's estimates.
	MaxInputTokens  int
	MaxOutputTokens int
	// RecallMode governs the context-supply recall (ContextRecall). Its zero
	// value is the shipped default, shadow_only: an unconfigured deployment
	// observes without changing what its window carries.
	RecallMode EpisodeRecallMode
	// AdmissionMode selects the fusion admission rule for recall. Its zero
	// value is the shipped rule; the v3 bound is opt-in only.
	AdmissionMode EpisodeAdmissionMode
}

// EpisodeSystem is the wired subsystem.
type EpisodeSystem struct {
	repo      repository.Repository
	projector *Projector
	publisher *EpisodePublisher
	worker    *EpisodeWorker
	indexer   *EpisodeIndexer
	retriever *EpisodeRetriever
	config    EpisodeSystemConfig
	ledger    episodeGrantLedger
}

// NewEpisodeSystem assembles the subsystem from a repository, a tool-calling
// chat model, an embedder and the derived view. Both model seams are
// constructor-required: a projector that cannot verify, or a verifier without a
// model, is a startup failure rather than a boundary that silently does not
// exist.
func NewEpisodeSystem(repo repository.Repository, chat model.ToolChatModel, embedder retrieval.Embedder, view EpisodeVectorIndex, config EpisodeSystemConfig) (*EpisodeSystem, error) {
	if repo == nil || chat == nil || embedder == nil || view == nil {
		return nil, fmt.Errorf("episode system dependencies are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	loader, err := NewRepositoryEvidenceLoader(repo, config.MaxMessages)
	if err != nil {
		return nil, err
	}
	generator, err := NewModelGenerator(chat, config.MaxInputTokens, config.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	verifier, err := NewModelSupportVerifier(chat)
	if err != nil {
		return nil, err
	}
	projector, err := NewProjector(loader, generator, repo, verifier)
	if err != nil {
		return nil, err
	}
	publisher, err := NewEpisodePublisher(repo, config.QuietWindow)
	if err != nil {
		return nil, err
	}
	reconciler, err := NewEpisodeReconciler(repo, config.QuietWindow, 200)
	if err != nil {
		return nil, err
	}
	worker, err := NewEpisodeWorker(repo, projector, reconciler, config.Worker)
	if err != nil {
		return nil, err
	}
	identity := retrieval.VectorIdentity(embedder, EpisodeContentVersion)
	indexer, err := NewEpisodeIndexer(view, repositoryEpisodeIndexSource{repo: repo}, embedder, identity)
	if err != nil {
		return nil, err
	}
	retriever, err := NewEpisodeRetriever(repo, view, embedder, identity)
	if err != nil {
		return nil, err
	}
	// The derived-view writer is part of the system, not an optional attachment:
	// without it every card would stay visibly unindexed and recall would answer
	// nothing, which is a deployment fault, not an empty index.
	worker.UseIndexer(indexer)
	// The context-supply mode and the fusion rule are deployment decisions, not
	// defaults to drift into: an unconfigured system observes without injecting
	// and fuses with the shipped rule.
	if config.RecallMode == "" {
		config.RecallMode = DefaultEpisodeRecallMode()
	}
	if config.AdmissionMode == "" {
		config.AdmissionMode = EpisodeAdmissionLegacy
	}
	retriever.UseAdmissionMode(config.AdmissionMode)
	return &EpisodeSystem{
		repo: repo, projector: projector, publisher: publisher, worker: worker,
		indexer: indexer, retriever: retriever, config: config,
	}, nil
}

// EpisodeContentVersion names how episode search text is produced today. It is
// part of the vector identity, so a change to the recipe invalidates every
// stored vector instead of letting the view mix two incompatible readings. It
// is exported because the reindex tool must compute the same identity the
// server computes, or its rebuilt view would be invisible to recall.
const EpisodeContentVersion = "episode-searchtext-v1"

const (
	episodeDefaultCardRunes        = 600
	episodeDefaultObservationRunes = 3000
)

func (c EpisodeSystemConfig) validate() error {
	if c.QuietWindow < 0 || c.MaxMessages < 32 || c.MaxInputTokens < 1024 || c.MaxOutputTokens < 256 {
		return fmt.Errorf("invalid episode system config")
	}
	return nil
}

// Publisher returns the conversation-completion trigger. It only writes
// durable job rows and never blocks the conversation path on generation.
func (s *EpisodeSystem) Publisher() *EpisodePublisher { return s.publisher }

// Worker returns the background executor for Start/Stop.
func (s *EpisodeSystem) Worker() *EpisodeWorker { return s.worker }

// Indexer returns the derived-view writer, for warm starts and reindexing.
func (s *EpisodeSystem) Indexer() *EpisodeIndexer { return s.indexer }

// Projector exposes the projection pass for a caller that must drive one
// projection directly (the watermark short-circuit checks do this).
func (s *EpisodeSystem) Projector() *Projector { return s.projector }

// WarmStart rebuilds the derived view from the authority. A process-local view
// calls this at startup; a Redis-backed view keeps its content across restarts
// and uses Reindex instead when it falls behind.
func (s *EpisodeSystem) WarmStart(ctx context.Context) (int, error) {
	return s.indexer.WarmStart(ctx)
}

// ReindexUser rebuilds one user's derived view.
func (s *EpisodeSystem) ReindexUser(ctx context.Context, userID string) (int, error) {
	return s.indexer.Reindex(ctx, userID)
}

// DropSession removes the session's cards from the derived retrieval view. The
// store rows - the card and its projection job - are already gone: they are
// deleted inside the session-delete transaction, so a card whose source session
// is gone cannot outlive the delete by even one crashed step. This hook is
// therefore best-effort view cleanup only: the reload re-checks the authority
// on every read, so a stale key costs recall, never correctness. episodeIDs is
// what the caller captured while the session was still readable.
func (s *EpisodeSystem) DropSession(ctx context.Context, userID, sessionID string, episodeIDs []string) {
	if len(episodeIDs) == 0 {
		return
	}
	viewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.indexerIndex().Delete(viewCtx, userID, episodeIDs); err != nil {
		// MySQL is authoritative and every reload re-checks it; the stale key
		// is harmless and a later reindex clears it.
		logPrintf("episode view delete deferred: session=%s episodes=%d error_type=%T", sessionID, len(episodeIDs), err)
	}
}

func (s *EpisodeSystem) indexerIndex() EpisodeVectorIndex { return s.indexer.index }

// --- Per-turn recall and the evidence ledger --------------------------------

// EpisodeCard is one recalled episode as the tool may show it: the card's own
// words plus the message ids its assertions cite. It carries no owner, no
// version and no other session's identity - the ids the model may read back are
// the only handles it is given, and each one is registered server-side.
type EpisodeCard struct {
	// ID and Version are server-side identity used to build stable citations;
	// adapters must not expose them as model-controlled handles.
	ID          string
	Version     uint64
	Title       string
	Synopsis    string
	Topics      []string
	Decisions   []EpisodeCardEntry
	OpenLoops   []EpisodeCardEntry
	Corrections []EpisodeCardEntry
	Preferences []EpisodeCardEntry
	Experiences []EpisodeCardEntry
	OccurredAt  time.Time
}

// EpisodeCardEntry is one assertion line and the message ids behind it.
type EpisodeCardEntry struct {
	Text       string
	MessageIDs []string
	OccurredAt time.Time
}

// The two provenances a read-back grant can carry. An episode grant comes from
// a card that was recalled and displayed this turn; a summary grant comes from
// the structured session summary that reached this turn's window. The kinds
// share one ledger and one read tool because both compressions cite the same
// kind of thing - the learner's own stored words - and the read happens once
// regardless of how many compressions cited it. They stay distinguishable
// because "why may this id be read" has a different answer per source, and a
// citation that says the wrong thing about its own provenance is worse than no
// citation.
const (
	grantKindEpisode = "episode"
	grantKindSummary = "summary"
)

// episodeGrant is one read-back permission: "the card whose id and version are
// recorded cited this message, no later than this watermark" for an episode
// grant, or "the summary at this version, covering up to this watermark, cited
// this message" for a summary grant. A grant without the version would keep
// authorising a source that has since moved on.
type episodeGrant struct {
	Kind            string
	MessageID       string
	EpisodeID       string
	EpisodeVersion  uint64
	SummaryVersion  int64
	SourceSessionID string
	Watermark       string
}

// turnLedger is one turn's read-back allow-list.
type turnLedger struct {
	turnID string
	grants map[string][]episodeGrant
}

// episodeGrantLedger is the shared evidence ledger of one session's current
// turn. Sessions are serialised by the conversation service's lock, so one
// entry per (user, session) is exact: a new turn replaces the old ledger, and a
// read-back that arrives with a different turn id is refused rather than
// served from a stale list.
type episodeGrantLedger struct {
	mu      sync.Mutex
	current map[string]*turnLedger
}

func (l *episodeGrantLedger) key(userID, sessionID string) string {
	return userID + "\x00" + sessionID
}

// beginTurn starts a fresh ledger for the named turn. Only content registered
// after this point is citable in the turn.
func (l *episodeGrantLedger) beginTurn(userID, sessionID, turnID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.current == nil {
		l.current = make(map[string]*turnLedger)
	}
	key := l.key(userID, sessionID)
	if existing, ok := l.current[key]; ok && existing.turnID == turnID {
		return
	}
	l.current[key] = &turnLedger{turnID: turnID, grants: make(map[string][]episodeGrant)}
}

// registerDisplayed records read-back grants for exactly the ids passed in -
// the intersection the caller computed between what rendering showed and what
// the stored card cites. Each grant is derived from the stored card's own
// facts (id, version, session, watermark); the card is read, never rewritten,
// and no id outside the passed list can enter the ledger through it.
func (l *episodeGrantLedger) registerDisplayed(userID, sessionID, turnID string, episode *domain.Episode, messageIDs []string) {
	if episode == nil || episode.Status != domain.EpisodeStatusActive {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ledger, ok := l.current[l.key(userID, sessionID)]
	if !ok || ledger.turnID != turnID {
		// The turn moved on; its ledger is gone and nothing may register into it.
		return
	}
	for _, messageID := range messageIDs {
		grant := episodeGrant{
			Kind:            grantKindEpisode,
			MessageID:       messageID,
			EpisodeID:       episode.ID,
			EpisodeVersion:  episode.Version,
			SourceSessionID: episode.SessionID,
			Watermark:       episode.SourceLastMessageID,
		}
		grants := ledger.grants[messageID]
		duplicate := false
		for _, existing := range grants {
			if existing == grant {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ledger.grants[messageID] = append(grants, grant)
		}
	}
}

// registerSummaryDisplayed records read-back grants derived from the structured
// summary that reached this turn's window. The ids arrive already filtered:
// only ones the rendered block actually showed, each within the summary's
// watermark, computed by the caller from the summary row this turn read.
//
// A summary grant's authority is the session being answered plus the watermark
// the summary was drawn under - there is no stored card to re-verify at grant
// time, because the summary row is read by the same turn that renders it. The
// read-back path still re-checks both facts before serving a row.
func (l *episodeGrantLedger) registerSummaryDisplayed(userID, sessionID, turnID string,
	version int64, watermark string, messageIDs []string,
) {
	if version <= 0 || strings.TrimSpace(watermark) == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ledger, ok := l.current[l.key(userID, sessionID)]
	if !ok || ledger.turnID != turnID {
		// The turn moved on; nothing may register into it.
		return
	}
	for _, messageID := range messageIDs {
		messageID = strings.TrimSpace(messageID)
		if messageID == "" || domain.CompareMessageIDs(messageID, watermark) > 0 {
			// An id above the watermark is a citation the summary never had.
			continue
		}
		grant := episodeGrant{
			Kind:            grantKindSummary,
			MessageID:       messageID,
			SummaryVersion:  version,
			SourceSessionID: sessionID,
			Watermark:       watermark,
		}
		grants := ledger.grants[messageID]
		duplicate := false
		for _, existing := range grants {
			if existing == grant {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ledger.grants[messageID] = append(grants, grant)
		}
	}
}

// grants returns the live grants for one message id, or nil when the turn has
// moved on or nobody registered the id.
func (l *episodeGrantLedger) grants(userID, sessionID, turnID, messageID string) []episodeGrant {
	l.mu.Lock()
	defer l.mu.Unlock()
	ledger, ok := l.current[l.key(userID, sessionID)]
	if !ok || ledger.turnID != turnID {
		return nil
	}
	return append([]episodeGrant(nil), ledger.grants[messageID]...)
}

// carried reports the distinct episodes any registered grant names, with the
// version each was granted under. An episode cited through two ids appears
// once, at its one version.
func (l *episodeGrantLedger) carried(userID, sessionID, turnID string) []EpisodeCarried {
	l.mu.Lock()
	defer l.mu.Unlock()
	ledger, ok := l.current[l.key(userID, sessionID)]
	if !ok || ledger.turnID != turnID {
		return nil
	}
	seen := make(map[string]uint64)
	var order []string
	for _, grants := range ledger.grants {
		for _, grant := range grants {
			if grant.Kind != grantKindEpisode || grant.EpisodeID == "" {
				continue
			}
			if _, exists := seen[grant.EpisodeID]; !exists {
				seen[grant.EpisodeID] = grant.EpisodeVersion
				order = append(order, grant.EpisodeID)
			}
		}
	}
	out := make([]EpisodeCarried, 0, len(order))
	for _, id := range order {
		out = append(out, EpisodeCarried{EpisodeID: id, Version: seen[id]})
	}
	return out
}

// RegisterSummaryEvidence is the summary side of the same handoff the episode
// tool uses: the caller renders the structured summary into this turn's window,
// computes which cited ids actually reached the model (SummaryReadGrants does
// the whole computation from the stored row), and registers them here so the
// read-back tool serves them under the turn boundary. The turnID is this
// turn's stored question id, the same key the episode ledger uses.
//
// An id whose registration arrives after the turn moved on is dropped, the
// same way a late episode grant is: the ledger is per turn and a new turn
// cannot inherit its authorizations.
func (s *EpisodeSystem) RegisterSummaryEvidence(userID, sessionID, turnID string,
	version int64, watermark string, messageIDs []string,
) {
	s.ledger.registerSummaryDisplayed(userID, sessionID, turnID, version, watermark, messageIDs)
}

// EpisodeCarried is one episode this turn was shown, at the exact version that
// was shown. It is the record a caller persists on the assistant message so a
// later projection can tell recalled text from lived conversation - the echo
// boundary: a card quoted into the transcript must not become source-less
// history the next projection summarizes as if the learner had said it.
type EpisodeCarried struct {
	EpisodeID string
	Version   uint64
}

// CarriedEpisodes reports the distinct episodes whose cards this turn actually
// displayed, with their versions, from the turn's grant ledger. It reports what
// was shown, not what was cited: an answer that used a recalled card without
// naming it still carried that card's text into the transcript.
func (s *EpisodeSystem) CarriedEpisodes(userID, sessionID, turnID string) []EpisodeCarried {
	return s.ledger.carried(userID, sessionID, turnID)
}

// RecallForTurn is the tool-facing read: it recalls cross-session episodes for
// the caller's own user, registers what it actually returned into this turn's
// ledger, and projects the cards down to what the model may see.
//
// The scope decision is entirely server-side: the caller cannot name a user, a
// session, a topic filter outside its question, or a card id. Current-session
// episodes are excluded on purpose - this session's content is already in the
// conversation window, and duplicating it would double-cite one turn.
func (s *EpisodeSystem) RecallForTurn(ctx context.Context, userID, sessionID, turnID, query string) ([]EpisodeCard, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(turnID) == "" {
		return nil, fmt.Errorf("episode recall identity required")
	}
	// The history probe is fail-closed, matching the source project: a store
	// that cannot answer is not evidence that the session has no history. The
	// flag decides whether the "继续上次" resumption branch may run, so
	// guessing "no" on a store error would silently change what the recall is
	// allowed to look at - the read fails instead, and the tool reports a
	// lookup failure rather than an honest-looking empty result.
	messages, err := s.repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("episode history probe failed: %w", err)
	}
	hasHistory := len(messages) > 0
	result, err := s.retriever.Recall(ctx, EpisodeRecallRequest{
		UserID: userID, SessionID: sessionID, Question: query,
		Now:                      time.Now(),
		CurrentSessionHasHistory: hasHistory,
	})
	if err != nil {
		return nil, err
	}
	// The ledger is rebuilt only when the turn id changes. Repeated searches in
	// the same turn accumulate grants, so a later read may use a card found by
	// the earlier search. A new turn replaces the previous ledger and cannot
	// inherit its authorizations.
	s.ledger.beginTurn(userID, sessionID, turnID)
	cards := make([]EpisodeCard, 0, len(result.Episodes))
	for index := range result.Episodes {
		episode := result.Episodes[index]
		cards = append(cards, projectEpisodeCard(&episode))
	}
	return cards, nil
}

func projectEpisodeCard(episode *domain.Episode) EpisodeCard {
	card := EpisodeCard{
		ID:         episode.ID,
		Version:    episode.Version,
		Title:      episode.Title,
		Synopsis:   episode.Document.Synopsis,
		Topics:     make([]string, 0, len(episode.Document.Topics)),
		OccurredAt: episode.LastMessageAt.UTC(),
	}
	for _, topic := range episode.Document.Topics {
		card.Topics = append(card.Topics, topic.Label)
	}
	projectEntries := func(entries []domain.EpisodeEvidence) []EpisodeCardEntry {
		out := make([]EpisodeCardEntry, 0, len(entries))
		for _, entry := range entries {
			out = append(out, EpisodeCardEntry{
				Text: entry.Text, MessageIDs: append([]string(nil), entry.EvidenceMessageIDs...),
				OccurredAt: entry.OccurredAt.UTC(),
			})
		}
		return out
	}
	card.Decisions = projectEntries(episode.Document.Decisions)
	card.OpenLoops = projectEntries(episode.Document.OpenLoops)
	card.Corrections = projectEntries(episode.Document.UserCorrections)
	card.Preferences = projectEntries(episode.Document.Preferences)
	card.Experiences = projectEntries(episode.Document.DurableExperiences)
	return card
}

// EpisodeDisplayedAuthorization is the exact subset of one recalled card that
// reached the model after tool budgeting and rendering.
type EpisodeDisplayedAuthorization struct {
	EpisodeID      string
	EpisodeVersion uint64
	MessageIDs     []string
}

// AuthorizeDisplayed is the explicit handoff from tool rendering to the
// evidence ledger. It grants exactly the intersection of what rendering showed
// and what the store cites right now, one id at a time: an id whose neighbours
// in the same assertion were clipped away is granted alone, and an id the
// stored card no longer cites is not granted at all. The stored card is only
// ever read - filtering never rewrites the persisted document.
//
// The store reads happen before any grant is registered, so a read failure
// leaves the ledger untouched and the error reaches the tool: an observation
// whose read-back rights are unknown must fail loudly, not serve silently.
func (s *EpisodeSystem) AuthorizeDisplayed(ctx context.Context, userID, sessionID, turnID string, displayed []EpisodeDisplayedAuthorization) error {
	type pendingGrant struct {
		episode *domain.Episode
		ids     []string
	}
	pending := make([]pendingGrant, 0, len(displayed))
	for _, item := range displayed {
		if len(item.MessageIDs) == 0 {
			continue
		}
		episode, err := s.repo.GetEpisodeByID(ctx, userID, item.EpisodeID)
		if err != nil {
			if errors.Is(err, repository.ErrEpisodeNotFound) {
				// The card moved on between recall and rendering; there is
				// nothing to grant, and the read tool would refuse anyway.
				continue
			}
			return fmt.Errorf("episode authorization cannot verify card %s: %w", item.EpisodeID, err)
		}
		if episode.Version != item.EpisodeVersion || episode.Status != domain.EpisodeStatusActive ||
			episode.IndexStatus != domain.EpisodeIndexIndexed {
			continue
		}
		cited := make(map[string]struct{})
		for _, group := range episode.Document.AssertionGroups() {
			for _, entry := range *group.Entries {
				for _, messageID := range entry.EvidenceMessageIDs {
					cited[messageID] = struct{}{}
				}
			}
		}
		ids := make([]string, 0, len(item.MessageIDs))
		for _, messageID := range item.MessageIDs {
			if _, ok := cited[messageID]; ok {
				ids = append(ids, messageID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		pending = append(pending, pendingGrant{episode: episode, ids: ids})
	}
	for _, grant := range pending {
		s.ledger.registerDisplayed(userID, sessionID, turnID, grant.episode, grant.ids)
	}
	return nil
}

type EpisodeHistoryMessage struct {
	MessageID string
	Role      string
	SentAt    time.Time
	Content   string
	Truncated bool
	// EpisodeID names the card whose grant authorised this read, so the answer
	// can cite the card it actually used.
	EpisodeID      string
	EpisodeVersion uint64
}

// EpisodeEvidenceFacts is what one stored card says about itself, read back
// from the store at the moment a citation is about to be honoured. The grant
// the ledger handed the reader already carries an id, a version and a session,
// but every one of those came from a card that was *recalled* earlier in this
// turn. Nothing in the grant proves the card still exists, still has that
// version, is still active, or still cites the id. That is what this struct is
// for: it is the store's answer, taken after the model asked, and a grant that
// disagrees with it authorises nothing.
type EpisodeEvidenceFacts struct {
	EpisodeID   string
	UserID      string
	SessionID   string
	Version     uint64
	Status      domain.EpisodeStatus
	IndexStatus domain.EpisodeIndexStatus
	Watermark   string
	// EvidenceMessageIDs is the closure the stored card cites right now. An id
	// that is not in it was not cited by this version, whatever the recalled
	// card showed.
	EvidenceMessageIDs []string
}

// LoadEpisodeEvidence reads those facts for one owner-scoped card.
func (s *EpisodeSystem) LoadEpisodeEvidence(ctx context.Context, userID, episodeID string) (EpisodeEvidenceFacts, error) {
	episode, err := s.repo.GetEpisodeByID(ctx, userID, episodeID)
	if err != nil {
		return EpisodeEvidenceFacts{}, err
	}
	facts := EpisodeEvidenceFacts{
		EpisodeID:   episode.ID,
		UserID:      episode.UserID,
		SessionID:   episode.SessionID,
		Version:     episode.Version,
		Status:      episode.Status,
		IndexStatus: episode.IndexStatus,
		Watermark:   episode.SourceLastMessageID,
	}
	for _, group := range episode.Document.AssertionGroups() {
		for _, entry := range *group.Entries {
			facts.EvidenceMessageIDs = append(facts.EvidenceMessageIDs, entry.EvidenceMessageIDs...)
		}
	}
	return facts, nil
}

// ReadHistoryForTurn reads back the learner's own words for message ids an
// episode card cited this turn.
//
// Every gate is checked in order and each one has its own way of being wrong:
// the id must be in this turn's ledger (only what this run was actually shown
// may be read - message ids are enumerable, and a tool that read whatever id it
// was handed would let the model walk the learner's history by counting), the
// granting card must still exist with the granted version, still be active,
// still indexed, must still cite the id, and the id must not be past the
// card's watermark. Only then is the message read - from the granted session,
// as the learner's own words - and returned.
func (s *EpisodeSystem) ReadHistoryForTurn(ctx context.Context, userID, sessionID, turnID string, messageIDs []string, maxRunes int) ([]EpisodeHistoryMessage, []EpisodeRefusal, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(turnID) == "" {
		return nil, nil, fmt.Errorf("episode history read identity required")
	}
	if maxRunes <= 0 || maxRunes > 1200 {
		maxRunes = episodeHistoryMessageRunes
	}
	var out []EpisodeHistoryMessage
	var refusals []EpisodeRefusal
	seen := make(map[string]struct{}, len(messageIDs))
	for _, messageID := range messageIDs {
		messageID = strings.TrimSpace(messageID)
		if messageID == "" {
			continue
		}
		if _, duplicate := seen[messageID]; duplicate {
			continue
		}
		seen[messageID] = struct{}{}
		grants := s.ledger.grants(userID, sessionID, turnID, messageID)
		if len(grants) == 0 {
			refusals = append(refusals, EpisodeRefusal{MessageID: messageID, Reason: "message_not_cited_this_turn"})
			continue
		}
		// One store read per granting source, not per id: every citation on a
		// source depends on the same facts.
		var live []episodeGrant
		factsByEpisode := make(map[string]*EpisodeEvidenceFacts, len(grants))
		for _, grant := range grants {
			if grant.Kind == grantKindSummary {
				// A summary grant's facts were established when it was
				// registered - the summary row this turn read - and the read
				// re-checks the two claims that matter here: that the grant
				// belongs to the session being answered, and that the id sits
				// within the watermark the summary was drawn under. Both fail
				// closed; a grant assembled against another session, or above
				// its watermark, authorises nothing.
				if strings.TrimSpace(grant.SourceSessionID) != strings.TrimSpace(sessionID) {
					refusals = append(refusals, EpisodeRefusal{MessageID: messageID, Reason: "summary_source_session_mismatch"})
					continue
				}
				if grant.SummaryVersion <= 0 || strings.TrimSpace(grant.Watermark) == "" ||
					domain.CompareMessageIDs(grant.MessageID, grant.Watermark) > 0 {
					refusals = append(refusals, EpisodeRefusal{MessageID: messageID, Reason: "message_past_summary_watermark"})
					continue
				}
				live = append(live, grant)
				continue
			}
			facts, ok := factsByEpisode[grant.EpisodeID]
			if !ok {
				loaded, err := s.LoadEpisodeEvidence(ctx, userID, grant.EpisodeID)
				if err != nil {
					if errors.Is(err, repository.ErrEpisodeNotFound) {
						refusals = append(refusals, EpisodeRefusal{MessageID: messageID, EpisodeID: grant.EpisodeID, Reason: "episode_not_found"})
					} else {
						refusals = append(refusals, EpisodeRefusal{MessageID: messageID, EpisodeID: grant.EpisodeID, Reason: "episode_evidence_unreadable"})
					}
					continue
				}
				facts = &loaded
				factsByEpisode[grant.EpisodeID] = facts
			}
			if reason := confirmEpisodeGrant(grant, facts); reason != "" {
				refusals = append(refusals, EpisodeRefusal{MessageID: messageID, EpisodeID: grant.EpisodeID, Reason: reason})
				continue
			}
			live = append(live, grant)
		}
		if len(live) == 0 {
			continue
		}
		// The read is keyed by the granted session, not by the message id
		// alone: a store that ignored the session would be trusted to have
		// filtered correctly, which is exactly the trust this check refuses.
		messages, err := s.repo.ListMessages(ctx, userID, live[0].SourceSessionID)
		if err != nil {
			refusals = append(refusals, EpisodeRefusal{MessageID: messageID, EpisodeID: live[0].EpisodeID, Reason: "episode_source_session_unreadable"})
			continue
		}
		for _, message := range messages {
			if message.ID != messageID {
				continue
			}
			// Only a learner message can prove what the learner said. Fail
			// closed on anything else.
			if !isUserMessage(message) {
				refusals = append(refusals, EpisodeRefusal{MessageID: messageID, EpisodeID: live[0].EpisodeID, Reason: "message_not_user_authored"})
				continue
			}
			content := strings.TrimSpace(message.Content)
			truncated := false
			if runes := []rune(content); len(runes) > maxRunes {
				content, truncated = string(runes[:maxRunes]), true
			}
			if content == "" {
				continue
			}
			out = append(out, EpisodeHistoryMessage{
				MessageID: message.ID, Role: message.Role, SentAt: message.CreatedAt.UTC(),
				Content: content, Truncated: truncated,
				EpisodeID: live[0].EpisodeID, EpisodeVersion: live[0].EpisodeVersion,
			})
			break
		}
		if len(out) == 0 || out[len(out)-1].MessageID != messageID {
			refusals = append(refusals, EpisodeRefusal{MessageID: messageID, EpisodeID: live[0].EpisodeID, Reason: "message_not_found"})
		}
	}
	return out, refusals, nil
}

// confirmEpisodeGrant re-establishes a card's facts at citation time and names
// the reason when the grant no longer describes the store. An empty reason
// means the read may proceed.
func confirmEpisodeGrant(grant episodeGrant, facts *EpisodeEvidenceFacts) string {
	if facts == nil || strings.TrimSpace(facts.EpisodeID) != strings.TrimSpace(grant.EpisodeID) {
		return "episode_not_found"
	}
	if facts.Version == 0 || facts.Version != grant.EpisodeVersion {
		// A re-projection moved the card on: the version this turn saw is no
		// longer the current one, so its words are no longer citable evidence.
		return "episode_version_mismatch"
	}
	if facts.Status != domain.EpisodeStatusActive {
		return "episode_not_active"
	}
	if facts.IndexStatus != domain.EpisodeIndexIndexed {
		return "episode_view_not_verified"
	}
	cited := false
	for _, id := range facts.EvidenceMessageIDs {
		if id == grant.MessageID {
			cited = true
			break
		}
	}
	if !cited {
		return "message_not_in_episode_closure"
	}
	if facts.Watermark != "" && domain.CompareMessageIDs(grant.MessageID, facts.Watermark) > 0 {
		return "message_past_episode_watermark"
	}
	return ""
}

// EpisodeRefusal is one refused read-back and the reason it was refused. The
// reason travels because "not cited", "wrong version" and "archived" are
// different findings, and folding them into one list would throw that evidence
// away.
type EpisodeRefusal struct {
	MessageID string `json:"message_id"`
	EpisodeID string `json:"episode_id,omitempty"`
	Reason    string `json:"reason"`
}

// episodeHistoryMessageRunes bounds one returned message: a long pasted
// snippet should not be able to spend the answer's budget on a single
// quotation.
const episodeHistoryMessageRunes = 600

// --- Context-supply recall: the three-state mode ----------------------------
//
// The source project ran its episodic context behind a three-state switch -
// disabled, shadow_only, inject_enabled - because "only observe, never inject"
// is the only way to evaluate the supply on live sessions without clearing the
// store. The tool path (episode_search) is the model's own choice and exists in
// every mode; this supply is what a context builder consumes, and the mode
// decides whether it may carry anything into the window.

// EpisodeContextSupply is one context-supply recall's outcome.
//
// Inject is the only field that licenses window injection. In shadow mode the
// cards are returned so the caller can observe and record what injection would
// have served, and Inject is false: a caller that injects them anyway bypasses
// the mode, which is a wiring fault the mode cannot defend against.
type EpisodeContextSupply struct {
	Mode   EpisodeRecallMode
	Inject bool
	// Cards is empty in every case the recall did not run or found nothing.
	// Each card is projected down to what the model may see, exactly as the
	// tool path projects them.
	Cards []EpisodeCard
	// Trace says what the recall did, for the shadow observer's log.
	Trace EpisodeRecallTrace
	// AuthorizeDisplayed is the same post-rendering handoff the search tool
	// uses. After the context builder decides what its window actually shows,
	// it calls this with the displayed slice; only then do read-back grants
	// exist for this turn. It is nil when the recall did not run.
	AuthorizeDisplayed func(displayed []EpisodeDisplayedAuthorization) error
}

// ContextRecall recalls cross-session episodes for the context builder, under
// the configured recall mode.
//
//   - disabled: nothing runs and the zero supply (with the mode named) is
//     returned; the caller injects nothing and pays for nothing.
//   - shadow_only: the full recall runs exactly as injection would, the trace
//     is logged for observation, and Inject stays false.
//   - inject_enabled: the recall runs and Inject is true.
//
// The history probe is fail-closed in every mode that recalls: a store that
// cannot answer is an error, never a quiet "no history". The turnID is this
// turn's stored question id - the same key the read-back ledger uses - so
// grants authorized through the supply land in the same per-turn boundary the
// tool path registers into.
func (s *EpisodeSystem) ContextRecall(ctx context.Context, userID, sessionID, turnID, question string) (EpisodeContextSupply, error) {
	return s.contextRecall(ctx, userID, sessionID, turnID, question, false, true)
}

// ContextRecallWithHistory is the production pre-turn seam. The caller computes
// history before inserting the current user message, preserving the v2
// "continue last time" branch semantics.
func (s *EpisodeSystem) ContextRecallWithHistory(ctx context.Context, userID, sessionID, turnID, question string, currentSessionHasHistory bool) (EpisodeContextSupply, error) {
	return s.contextRecall(ctx, userID, sessionID, turnID, question, currentSessionHasHistory, false)
}

func (s *EpisodeSystem) contextRecall(ctx context.Context, userID, sessionID, turnID, question string, currentSessionHasHistory, probeHistory bool) (EpisodeContextSupply, error) {
	supply := EpisodeContextSupply{Mode: s.config.RecallMode}
	if s.config.RecallMode == EpisodeRecallDisabled {
		return supply, nil
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(turnID) == "" {
		return supply, fmt.Errorf("episode context recall identity required")
	}
	if probeHistory {
		// Fail-closed probe: storage failure is never treated as an empty history.
		messages, err := s.repo.ListMessages(ctx, userID, sessionID)
		if err != nil {
			return supply, fmt.Errorf("episode history probe failed: %w", err)
		}
		currentSessionHasHistory = len(messages) > 0
	}
	result, err := s.retriever.Recall(ctx, EpisodeRecallRequest{
		UserID: userID, SessionID: sessionID, Question: question,
		Now:                      time.Now(),
		CurrentSessionHasHistory: currentSessionHasHistory,
	})
	if err != nil {
		return supply, err
	}
	supply.Trace = result.Trace
	// The ledger is rebuilt only when the turn id changes; repeated supplies in
	// the same turn accumulate, exactly like the tool path.
	s.ledger.beginTurn(userID, sessionID, turnID)
	for index := range result.Episodes {
		supply.Cards = append(supply.Cards, projectEpisodeCard(&result.Episodes[index]))
	}
	supply.AuthorizeDisplayed = func(displayed []EpisodeDisplayedAuthorization) error {
		return s.AuthorizeDisplayed(ctx, userID, sessionID, turnID, displayed)
	}
	if s.config.RecallMode == EpisodeRecallInject {
		supply.Inject = true
	} else {
		// The shadow observation: what would have been injected, recorded where
		// an operator can compare it against the window the turn actually sent.
		logPrintf("[episode-shadow] recall observed: session=%s need=%v abstained=%v reason=%s cards=%d",
			sessionID, result.Trace.NeedEpisode, result.Trace.Abstained, result.Trace.Reason, len(supply.Cards))
	}
	return supply, nil
}
