package agenttool

import (
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
)

const ReadHistoryMessageName = "read_history_message"

// maxHistoryMessageRunes bounds one returned message. A long pasted snippet
// should not be able to spend the answer's budget on a single quotation.
const maxHistoryMessageRunes = 600

// maxHistoryMessageIDsPerCall bounds one call. Two calls per run is what the
// tool budget allows, so five ids is what a learner gets asked about at once.
const maxHistoryMessageIDsPerCall = 5

// readHistoryMessageDescription is the contract the model reads.
//
// It is a constant rather than an inline argument because a freeze must be able
// to digest exactly these bytes: an offline gate that hashed a copy of this
// sentence would bind the number without binding the tool.
const readHistoryMessageDescription = "Read back the learner's own words for a message an episode cites. Use it when the learner asks what they actually said, or when an episode entry is too compressed to answer from. Only ids listed in an injected episode's message_ids can be read."

// ReadHistoryMessageContractDigest is read_history_message's name, description
// and parameter schema, digested together.
//
// The mode-isolated round widens what this tool may reach without changing what
// the model is shown, so a run seal has to be able to say that in numbers: the
// digest is identical for every mode, and the difference between two arms lives
// in the grant set and the per-tool limit, both of which the manifest records
// separately.
func ReadHistoryMessageContractDigest() (string, error) {
	built, err := toolutils.InferTool(ReadHistoryMessageName, readHistoryMessageDescription,
		func(context.Context, *ReadHistoryMessageInput) (*ReadHistoryMessageOutput, error) {
			return nil, nil
		})
	if err != nil {
		return "", fmt.Errorf("build the history read schema: %w", err)
	}
	info, err := built.Info(context.Background())
	if err != nil {
		return "", fmt.Errorf("read the history read schema: %w", err)
	}
	return toolContractDigest(ReadHistoryMessageName, readHistoryMessageDescription, info)
}

// EpisodeEvidenceFacts is what one stored Episode card says about itself, read
// back from the store at the moment a citation is about to be honoured.
//
// The grant the window handed the tool already carries an id, a version and a
// session, but every one of those came from the card that was *rendered* earlier
// in this turn. Nothing in the grant proves the card still exists, still has that
// version, is still active, or still cites the id. That is what this struct is
// for: it is the store's answer, taken after the model asked, and a grant that
// disagrees with it authorises nothing.
type EpisodeEvidenceFacts struct {
	EpisodeID   string
	UserID      int64
	SessionID   string
	Version     uint64
	Status      string
	IndexStatus string
	Watermark   uint
	// EvidenceMessageIDs is the closure the stored card cites right now. An id that
	// is not in it was not cited by this version, whatever the rendered block showed.
	EvidenceMessageIDs []uint
}

// EpisodeEvidenceReader loads those facts. The production implementation is the
// episode repository's own owner-scoped read, so a user id that does not match is
// a lookup miss rather than a permission check someone has to remember.
type EpisodeEvidenceReader interface {
	LoadEpisodeEvidence(ctx context.Context, userID int64, episodeID string) (EpisodeEvidenceFacts, error)
}

// EpisodeEvidenceReaderFunc adapts a function to the reader interface, the same way
// the other run-scoped providers in this service are wired.
type EpisodeEvidenceReaderFunc func(ctx context.Context, userID int64, episodeID string) (EpisodeEvidenceFacts, error)

func (f EpisodeEvidenceReaderFunc) LoadEpisodeEvidence(ctx context.Context, userID int64, episodeID string) (EpisodeEvidenceFacts, error) {
	return f(ctx, userID, episodeID)
}

// EpisodeGrantStatus is one refused citation and the reason it was refused. The
// reason travels because "not cited", "wrong version", "archived" and "past the
// watermark" are different findings for whoever is auditing a run, and folding them
// into a single refused list would throw that evidence away.
type EpisodeGrantStatus struct {
	MessageID int    `json:"message_id"`
	EpisodeID string `json:"episode_id"`
	Reason    string `json:"reason"`
}

// refusal reasons. Named rather than inlined because the evaluation gates compare
// against them, and a typo in a bare string would silently mislabel a refusal.
const (
	refusalEpisodeMissing   = "episode_not_found"
	refusalEpisodeOwner     = "episode_owner_mismatch"
	refusalEpisodeSession   = "episode_source_session_mismatch"
	refusalEpisodeVersion   = "episode_version_mismatch"
	refusalEpisodeInactive  = "episode_not_active"
	refusalEpisodeIndex     = "episode_index_not_verified"
	refusalEpisodeClosure   = "message_not_in_episode_closure"
	refusalEpisodeWatermark = "message_past_episode_watermark"
	refusalVerifierAbsent   = "episode_verifier_not_wired"
	refusalUnreadable       = "episode_evidence_unreadable"
)

// ErrEpisodeEvidenceMissing is what a reader returns when the store has no active
// card under this owner and id. It is a distinct error because "there is no such
// card" and "the store could not answer" must not be recorded as the same finding:
// the first is a fact about the learner's data, the second is a fact about this
// system, and a run ledger that cannot tell them apart explains neither.
var ErrEpisodeEvidenceMissing = errors.New("episode evidence not found")

// HistoryMessageReader is the message store's two bounded read shapes, both
// keyed on an owner the server trusts.
//
// ReadOwnedMessagesBySession is the grant-scoped form read_history_message
// needs. Message ids are sequential across every session an account owns, so a
// query keyed on owner plus id would serve a summary grant a row from an
// unrelated conversation - the same learner, but a session nobody showed the
// model. The map is therefore session -> ids, and the implementation must bind
// both in the database.
//
// ReadOwnedMessages is the older, owner-only form, and memory_search's bounded
// source quotes keep using it unchanged: those ids come from the episodes that
// same search just selected and are trimmed per card, so there is no per-id
// grant to carry. It stays on this interface rather than splitting a second one
// because every real implementation is the same repository, and the evaluation
// harnesses wire one value into both tools.
type HistoryMessageReader interface {
	ReadOwnedMessages(ctx context.Context, userID int64, messageIDs []uint) ([]model.Message, error)
	ReadOwnedMessagesBySession(ctx context.Context, userID int64, messageIDsBySession map[string][]uint) ([]model.Message, error)
}

// HistoryMessageRegistry exposes the messages an injected summary or episode
// cites.
//
// A summary and an episode are both compressions: "用户决定采用 Protobuf" is the
// projector's sentence, not the learner's. Each entry already carries the ids it
// was drawn from - the episode projector refuses to write an entry that cites
// nothing and the rolling summary's cleaner rejects an id that is not a stored
// user message of the session being summarised - so the exact words survive;
// nothing could reach them. Asked what they actually said, the assistant could
// only paraphrase the paraphrase.
//
// What makes this safe to offer is that the ids come from the server. Message
// ids are sequential integers, so a tool that read whatever id it was handed
// would let the model enumerate the learner's entire history by counting, and
// filtering by owner would not stop it: those rows are all theirs. The registry
// is built per run from the ids the injected cards cited, and refuses the rest.
type HistoryMessageRegistry struct {
	reader HistoryMessageReader
	// episodeEvidence re-establishes an episode card's facts at the moment one of its
	// citations is about to be honoured. nil means the deployment has not wired one,
	// and an episode grant is then refused rather than trusted: the card was rendered
	// earlier in this turn, and "the render still matches the store" is exactly the
	// claim a read of the learner's raw words has to be able to make.
	episodeEvidence EpisodeEvidenceReader
}

func NewHistoryMessageRegistry(reader HistoryMessageReader) (*HistoryMessageRegistry, error) {
	if reader == nil {
		return nil, fmt.Errorf("history message reader is required")
	}
	return &HistoryMessageRegistry{reader: reader}, nil
}

// UseEpisodeEvidenceReader wires the store-side verifier. It is a setter rather than
// a constructor argument because every existing caller - production wiring, the
// paired M1/M2 harnesses, the summary read-back evaluation - constructs this
// registry with the message reader alone, and an episode grant that nothing ever
// verified is the failure this closes.
func (r *HistoryMessageRegistry) UseEpisodeEvidenceReader(reader EpisodeEvidenceReader) {
	r.episodeEvidence = reader
}

type ReadHistoryMessageInput struct {
	MessageIDs []int `json:"message_ids" jsonschema:"required,description=Message ids taken verbatim from the message_ids field of an episode entry. At most five."`
}

type ReadHistoryMessageOutput struct {
	Messages []HistoryMessageView `json:"messages"`
	// Refused names ids that were asked for but are not citable in this run,
	// so the model learns the boundary instead of retrying.
	Refused []int `json:"refused,omitempty"`
	// RefusalReasons names why an episode citation was refused. An id the summary
	// never cited and an id whose card was archived after the render both belong in
	// Refused; only the second one is evidence about the store, so the reason is
	// recorded separately instead of being lost.
	RefusalReasons []EpisodeGrantStatus `json:"refusal_reasons,omitempty"`
}

type HistoryMessageView struct {
	MessageID int    `json:"message_id"`
	Role      string `json:"role"`
	SentAt    string `json:"sent_at"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated,omitempty"`
	// Reference is the citation label this run issued for the message it is attached to,
	// without brackets (e.g. "E3"), so the learner's answer can cite [E3] and the final
	// check resolves it. It is empty only when the run carried no evidence registry, in
	// which case nothing this tool returns is citable and the read itself is unchanged.
	Reference string `json:"reference,omitempty"`
}

func (r *HistoryMessageRegistry) Tools(ctx context.Context, run serviceagent.RunContext) ([]tool.BaseTool, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if run.UserID <= 0 || strings.TrimSpace(run.SessionID) == "" {
		return nil, fmt.Errorf("trusted history read identity is required")
	}
	// Every grant is re-checked against this run here, so the allow-list the
	// query obeys is the one the assembled window established - never a value
	// the request or the model supplied.
	grants := serviceagent.NewHistoryGrantSet(run, run.HistoryGrants)
	// Nothing was recalled, so there is nothing this run may read. Offering the
	// tool anyway would invite calls that can only be refused.
	if grants.Len() == 0 {
		return nil, nil
	}
	userID := run.UserID
	// One store read per episode card per run. Every citation on a card depends on
	// the same facts, and re-reading the row per id would turn two tool calls into a
	// query per citation.
	facts := &episodeFactsCache{reader: r.episodeEvidence, userID: userID,
		loaded: map[string]episodeFactsEntry{}}
	// A message this run reads back is evidence the learner actually said those words,
	// and the only thing that can make a citation to them legal. The registry comes from
	// the run, exactly like memory_search's does, so the reference the tool issues is the
	// same reference the final citation check resolves - and an id that was refused, never
	// granted, or not actually read back never reaches this registry at all.
	evidence := run.MemoryEvidence

	read, err := toolutils.InferTool(ReadHistoryMessageName, readHistoryMessageDescription,
		func(ctx context.Context, input *ReadHistoryMessageInput) (*ReadHistoryMessageOutput, error) {
			if input == nil || len(input.MessageIDs) == 0 {
				return nil, fmt.Errorf("at least one message id is required")
			}
			if len(input.MessageIDs) > maxHistoryMessageIDsPerCall {
				return nil, fmt.Errorf("at most five message ids may be read at once")
			}
			wanted := make([]uint, 0, len(input.MessageIDs))
			wantedSet := make(map[uint]struct{}, len(input.MessageIDs))
			// Only the grants that survived store verification authorise anything from
			// here on: the query is built from these, and so is the post-read coverage
			// check. Keeping the unverified list around would mean the verification
			// decided nothing.
			allowed := make(map[uint][]serviceagent.HistoryMessageGrant, len(input.MessageIDs))
			out := &ReadHistoryMessageOutput{}
			for _, id := range input.MessageIDs {
				if id <= 0 {
					continue
				}
				// An id nobody cited never reaches the database.
				held := grants.Grants(uint(id))
				if len(held) == 0 {
					out.Refused = append(out.Refused, id)
					continue
				}
				live := make([]serviceagent.HistoryMessageGrant, 0, len(held))
				for _, grant := range held {
					reason, ok := facts.confirm(ctx, grant)
					if ok {
						live = append(live, grant)
						continue
					}
					out.RefusalReasons = append(out.RefusalReasons, EpisodeGrantStatus{
						MessageID: id, EpisodeID: grant.EpisodeID, Reason: reason,
					})
				}
				if len(live) == 0 {
					out.Refused = append(out.Refused, id)
					continue
				}
				if _, duplicate := wantedSet[uint(id)]; duplicate {
					allowed[uint(id)] = appendSurviving(allowed[uint(id)], live)
					continue
				}
				wantedSet[uint(id)] = struct{}{}
				allowed[uint(id)] = live
				wanted = append(wanted, uint(id))
			}
			if len(wanted) == 0 {
				return out, nil
			}
			slices.Sort(wanted)
			// One entry per granting session, and one id per session however many
			// cards cited it: an id the summary and an episode both drew from the
			// same session is one row. The sessions differ only when the sources
			// differ, and the row's own session decides which provenance it
			// answers - the output still carries the message once.
			messageIDsBySession := make(map[string][]uint, len(wanted))
			for _, messageID := range wanted {
				for _, grant := range allowed[messageID] {
					if slices.Contains(messageIDsBySession[grant.SourceSessionID], messageID) {
						continue
					}
					messageIDsBySession[grant.SourceSessionID] = append(messageIDsBySession[grant.SourceSessionID], messageID)
				}
			}
			messages, err := r.reader.ReadOwnedMessagesBySession(ctx, userID, messageIDsBySession)
			if err != nil {
				return nil, err
			}
			emitted := make(map[uint]struct{}, len(wanted))
			for _, message := range messages {
				// Evidence ids are meant to point at learner statements. Fail
				// closed on anything else: an assistant row cannot prove what the
				// learner said, an unrequested id proves nothing about this run,
				// and a row from a session no grant named was read out of scope.
				if _, requested := wantedSet[message.ID]; !requested || !message.IsUser {
					continue
				}
				if _, duplicate := emitted[message.ID]; duplicate {
					continue
				}
				// The row's own session has to be one this id was verified against, and
				// the role has to be the learner's: a reader that ignored the session
				// scoping would otherwise be trusted to have filtered correctly.
				if !sessionCovered(allowed[message.ID], message.SessionID) {
					continue
				}
				content, truncated := boundRunes(message.Content, maxHistoryMessageRunes)
				if content == "" {
					continue
				}
				emitted[message.ID] = struct{}{}
				view := HistoryMessageView{
					MessageID: int(message.ID), Role: "user",
					SentAt:  message.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
					Content: content, Truncated: truncated,
				}
				// Only a message that cleared the grant set, the store re-check and this
				// read is registered, so the reference is issued for words the run actually
				// has - never for an id the model merely named.
				view.Reference = registerReadBackEvidence(evidence, userID, int64(message.ID),
					allowed[message.ID], message.CreatedAt, content)
				out.Messages = append(out.Messages, view)
			}
			return out, nil
		})
	if err != nil {
		return nil, fmt.Errorf("build history message tool: %w", err)
	}
	return []tool.BaseTool{read}, nil
}

func boundRunes(value string, max int) (string, bool) {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= max {
		return value, false
	}
	return string([]rune(value)[:max]), true
}

// registerReadBackEvidence issues one episodic reference for one message this run has
// actually read back, and returns the label so the tool can show it to the model.
//
// Both summary and episode read-backs are registered as Episodic because that is the one
// kind whose claims may stand behind "what happened in an earlier session"
// (MemoryEvidenceKind.SupportsHistory); the two provenances stay distinguishable through
// the entry's Source field, which is what the ledger's grant_provenance column reports.
func registerReadBackEvidence(registry *serviceagent.MemoryEvidenceRegistry, owner int64,
	messageID int64, grants []serviceagent.HistoryMessageGrant, occurredAt time.Time, content string) string {
	if registry == nil || len(grants) == 0 {
		return ""
	}
	var summary, episode bool
	var version uint64
	for _, grant := range grants {
		switch grant.SourceKind {
		case serviceagent.HistorySourceSummary:
			summary = true
			if grant.SummaryVersion > version {
				version = grant.SummaryVersion
			}
		case serviceagent.HistorySourceEpisode:
			episode = true
			if grant.EpisodeVersion > version {
				version = grant.EpisodeVersion
			}
		}
	}
	source := serviceagent.HistorySourceSummary
	switch {
	case summary && episode:
		source = "summary+episode"
	case episode:
		source = serviceagent.HistorySourceEpisode
	}
	subject, _ := boundRunes(content, 160)
	return registry.Register(serviceagent.MemoryEvidenceEntry{
		Kind:       serviceagent.MemoryEvidenceEpisodic,
		InternalID: fmt.Sprintf("history-message-%d", messageID),
		Version:    version, Owner: owner, Status: "active",
		Source: source, Subject: subject, OccurredAt: occurredAt.UTC(),
		MessageID: uint(messageID),
	})
}

// episodeFactsCache holds one run's store reads, keyed by episode id.
type episodeFactsCache struct {
	reader EpisodeEvidenceReader
	userID int64

	mu     sync.Mutex
	loaded map[string]episodeFactsEntry
}

type episodeFactsEntry struct {
	facts EpisodeEvidenceFacts
	err   error
}

// confirm reports whether one grant still describes a card the store agrees with,
// and names the reason when it does not. Summary grants pass through untouched:
// their authority is the session this run is in plus the watermark the summary was
// drawn under, both of which the grant set already re-checks, and the summary row is
// read by the same builder that made the grant in this turn.
func (c *episodeFactsCache) confirm(ctx context.Context, grant serviceagent.HistoryMessageGrant) (string, bool) {
	if grant.SourceKind != serviceagent.HistorySourceEpisode {
		return "", true
	}
	if c.reader == nil {
		// Fail closed. An unverified episode citation is exactly the case this
		// verification exists for, and "we could not check it" is not a permission.
		return refusalVerifierAbsent, false
	}
	entry := c.entry(ctx, grant.EpisodeID)
	if entry.err != nil {
		if errors.Is(entry.err, ErrEpisodeEvidenceMissing) {
			return refusalEpisodeMissing, false
		}
		return refusalUnreadable, false
	}
	facts := entry.facts
	if strings.TrimSpace(facts.EpisodeID) != strings.TrimSpace(grant.EpisodeID) {
		return refusalEpisodeMissing, false
	}
	if facts.UserID != c.userID {
		return refusalEpisodeOwner, false
	}
	if strings.TrimSpace(facts.SessionID) != strings.TrimSpace(grant.SourceSessionID) {
		return refusalEpisodeSession, false
	}
	if facts.Version == 0 || facts.Version != grant.EpisodeVersion {
		return refusalEpisodeVersion, false
	}
	if facts.Status != string(model.EpisodeStatusActive) {
		return refusalEpisodeInactive, false
	}
	if facts.IndexStatus != string(model.EpisodeIndexIndexed) {
		return refusalEpisodeIndex, false
	}
	if !slices.Contains(facts.EvidenceMessageIDs, grant.MessageID) {
		return refusalEpisodeClosure, false
	}
	if facts.Watermark > 0 && grant.MessageID > facts.Watermark {
		return refusalEpisodeWatermark, false
	}
	return "", true
}

func (c *episodeFactsCache) entry(ctx context.Context, episodeID string) episodeFactsEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, seen := c.loaded[episodeID]; seen {
		return cached
	}
	facts, err := c.reader.LoadEpisodeEvidence(ctx, c.userID, episodeID)
	loaded := episodeFactsEntry{facts: facts, err: err}
	c.loaded[episodeID] = loaded
	return loaded
}

// sessionCovered re-checks the returned row against the grants that survived
// verification, rather than against the window's original list.
func sessionCovered(grants []serviceagent.HistoryMessageGrant, sessionID string) bool {
	for _, grant := range grants {
		if grant.SourceSessionID == sessionID {
			return true
		}
	}
	return false
}

func appendSurviving(existing, live []serviceagent.HistoryMessageGrant) []serviceagent.HistoryMessageGrant {
	for _, grant := range live {
		if !slices.Contains(existing, grant) {
			existing = append(existing, grant)
		}
	}
	return existing
}
