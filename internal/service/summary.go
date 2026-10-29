package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// SummaryOutcome reports what one turn's summary step actually did. It exists because
// "the summary is unchanged" covers three different events - below threshold,
// disabled, and lost a race - and a test or an operator needs to tell them apart
// without inferring them from the conversation length.
type SummaryOutcome string

const (
	// SummaryUnchanged: the stored summary was used as it is.
	SummaryUnchanged SummaryOutcome = "unchanged"
	// SummaryRefreshed: this turn generated and committed a new summary.
	SummaryRefreshed SummaryOutcome = "refreshed"
	// SummarySkipped: a refresh was due by count but the planner refused, for the
	// reason carried in SummaryDetail.
	SummarySkipped SummaryOutcome = "skipped"
	// SummaryAdopted: this turn generated a summary, found the stored row had moved
	// on, and adopted the newer one. Its own generation was discarded, and no second
	// model call was made.
	SummaryAdopted SummaryOutcome = "conflict_adopted"
	// SummaryDisabled: no summarizer is installed, so the recent window is the whole
	// context and no model is ever contacted for summarizing.
	SummaryDisabled SummaryOutcome = "disabled"
	// SummaryDegraded: a due refresh failed and the turn went on with the last good
	// summary instead of ending. SummaryDetail names the failure class. This is the
	// source project's product semantics for the agent path, where the summary is an
	// optimization over history that is still on disk.
	SummaryDegraded SummaryOutcome = "degraded"
)

// Closed-set reasons a due refresh can fail. They travel in SummaryDetail on a
// degraded turn so a reader can tell the three apart without unpacking errors:
// a refusing model, a stage that could not afford the attempt, and a store that
// could not commit what the model wrote are different findings for whoever is
// auditing a session.
const (
	// SummaryFailureModel: the summarizing model call failed, timed out, or
	// returned nothing usable. This failure opens the session's cooldown.
	SummaryFailureModel = "refresh_failed_model"
	// SummaryFailureStage: the refresh was not attempted - the session is inside
	// its failure cooldown, or the turn had too little time left to be worth the
	// call. Nothing was tried, so nothing new was learned about the model.
	SummaryFailureStage = "refresh_stopped_by_stage"
	// SummaryFailureStorage: the model wrote a summary but the store refused the
	// commit. The stored row is untouched and the old summary is still exact.
	SummaryFailureStorage = "refresh_failed_storage"
)

// SummaryDetail carries the planner's skip reason, or "" when there is nothing to
// explain. It is a closed set of strings from the summary package, never an error text.
type SummaryDetail = string

// ErrSummaryBoundaryMissing reports a stored summary whose covered boundary names no
// message in this session. Its coverage is then undecidable: the two remaining options
// both rewrite history - re-summarize messages the text already contains, or drop
// everything the window cannot hold - so the turn is refused instead. It wraps
// ErrInternal because an API caller sees the ordinary internal failure; the wrapped
// reason is what a log or a test can still match with errors.Is.
var ErrSummaryBoundaryMissing = fmt.Errorf("%w: summary_boundary_missing", ErrInternal)

// storedTurn is one turn's stored starting point, read once under the session lock:
// the conversation as it was before this question, and the summary covering it.
type storedTurn struct {
	messages  []domain.Message
	stored    *domain.SessionSummary
	userAudit *domain.Message
}

// openTurn reads the stored conversation and the sealed summary, then appends this
// turn's question.
//
// The read happens before the append on purpose: the question being answered is not
// part of the history, so it reaches the model exactly once, as the current question
// rather than as a past turn plus the question. It also means the summary can never
// swallow a question this turn has not answered yet.
func (s *MessageService) openTurn(ctx context.Context, userID, sessionID, content string) (storedTurn, error) {
	if s.repo == nil {
		return storedTurn{}, ErrInternal
	}
	messages, err := s.repo.ListMessages(ctx, userID, sessionID)
	if err != nil {
		return storedTurn{}, mapRepositoryError(err)
	}
	stored, err := s.repo.GetSessionSummary(ctx, userID, sessionID)
	switch {
	case err == nil:
	case errors.Is(err, repository.ErrNotFound):
		// No summary yet, which is the normal state of a young session.
		stored = nil
	default:
		return storedTurn{}, mapRepositoryError(err)
	}
	userMessage, err := s.repo.AppendMessage(ctx, userID, sessionID, &domain.Message{
		Role:    roleUser,
		Content: content,
	})
	if err != nil {
		return storedTurn{}, mapRepositoryError(err)
	}
	return storedTurn{messages: messages, stored: stored, userAudit: userMessage}, nil
}

// conversationFor refreshes the summary if the policy says it is due, then assembles
// what the turn may be answered from: the sealed summary, and the complete exchanges
// after the summary's boundary trimmed to the conversation budget.
//
// Both the refresh and the window are cut at the same boundary, which is what keeps a
// message from arriving twice - summarized once and verbatim again - in one prompt. A
// stored boundary this session cannot locate is therefore not a detail to work around:
// the turn ends with ErrSummaryBoundaryMissing rather than being answered from a summary
// and a tail whose relationship to each other is unknown, and the same is true of a
// summary adopted from a race.
func (s *MessageService) conversationFor(ctx context.Context, userID, sessionID string, turn storedTurn,
	strategy domain.AnswerStrategy,
) (string, []responder.Turn, SummaryOutcome, SummaryDetail, error) {
	if s.frozenOrdinarySummary && strategy == domain.AnswerStrategyDirectChat {
		return s.ordinaryConversationFor(ctx, userID, sessionID, turn)
	}
	boundary := ""
	if turn.stored != nil {
		boundary = turn.stored.CoveredMessageID
	}
	content := ""
	if turn.stored != nil {
		content = turn.stored.Content
	}
	version := int64(0)
	if turn.stored != nil {
		version = turn.stored.Version
	}
	outcome := SummaryUnchanged
	detail := ""

	if s.summarizer != nil {
		// Only the learning agent runs the migrated summarizing rules. A chat or
		// knowledge turn is planned and bounded exactly as it was before this package
		// existed, because "adopt the agent's context budget" does not license
		// "every strategy now triggers, times out and cools down differently".
		controls := summary.LegacyControls()
		if strategy == domain.AnswerStrategyAgent {
			controls = summary.MigratedControls()
		}
		plan := summary.Build(turn.messages, boundary, content, s.summaryPolicy, controls)
		detail = plan.Skipped
		switch plan.Skipped {
		case "":
			// The refresh is its own model call for its own reason, so it carries its own
			// label: a summary's tokens must not be read as part of the answer's cost.
			refreshed, failure, err := s.refreshSummary(modelcall.WithPurpose(ctx, modelcall.PurposeSummary),
				userID, sessionID, plan, content, version, controls)
			if err != nil {
				if strategy == domain.AnswerStrategyAgent && summaryFailureMayDegrade(failure) {
					// The source project's semantics, restored: a summary is an
					// optimization over history that is still on disk, so a failed
					// refresh leaves the turn on the last good summary plus the raw
					// messages after its watermark rather than ending the turn. The
					// failure class is closed-set and reported; it is not swallowed
					// into one undifferentiated "degraded" - a refusing model, a
					// stage that never ran, and a store that refused the commit are
					// three different findings, and only the first two say anything
					// about summarizing itself.
					//
					// The stage/caller context separation above is untouched: only
					// the model call ran under the stage deadline, and cancellation
					// attribution still comes from the caller's context (a caller
					// who left gets ctx.Err() with no failure class, which ends the
					// turn the way a canceled turn always has).
					logPrintf("[agent-summary] refresh degraded: session=%s class=%s", sessionID, failure)
					detail = failure
					outcome = SummaryDegraded
					break
				}
				return "", nil, SummaryUnchanged, detail, err
			}
			if refreshed != nil {
				content = refreshed.Content
				boundary = refreshed.CoveredMessageID
				outcome = SummaryRefreshed
			} else {
				// The commit lost a race and the newer stored summary was adopted
				// instead: nothing this turn wrote, and nothing was regenerated.
				stored, err := s.repo.GetSessionSummary(ctx, userID, sessionID)
				switch {
				case err == nil:
					content, boundary = stored.Content, stored.CoveredMessageID
					outcome = SummaryAdopted
				case errors.Is(err, repository.ErrNotFound):
					// The row vanished between the read and the commit - a deleted
					// session. The turn continues on the recent window alone.
					content, boundary, outcome = "", "", SummaryAdopted
				default:
					return "", nil, outcome, detail, mapRepositoryError(err)
				}
				detail = ""
			}
		default:
			// Below threshold, nothing beyond the kept window, or a boundary this
			// session cannot locate. The last one is not a normal skip: windowAfter
			// below refuses to cut a window at a place it cannot find, so a turn is
			// never answered from a summary plus a tail whose relationship is unknown.
			outcome = SummarySkipped
		}
	} else {
		outcome = SummaryDisabled
	}

	// A stored boundary this session cannot locate ends the turn either way, and the
	// same rule for both paths: a summary adopted from a race has to be locatable
	// before its boundary is allowed to cut the window.
	switch strategy {
	case domain.AnswerStrategyAgent:
		// The candidate set, not a trimmed window: the agent's own budget decides what
		// fits once it can see its system prompt, its tool offers and its summary
		// together. Trimming by runes here and by tokens there would leave two window
		// rules arguing over one transcript.
		history, err := agentWindow(turn.messages, boundary, s.context)
		if err != nil {
			return "", nil, outcome, detail, err
		}
		return content, history, outcome, detail, nil
	default:
		history, err := windowAfter(turn.messages, boundary, s.context)
		if err != nil {
			return "", nil, outcome, detail, err
		}
		return content, history, outcome, detail, nil
	}
}

// agentWindow is the boundary check shared with the rune window, over the candidate set
// the agent's budget will choose from. The boundary rule is the same one and for the same
// reason: an unlocatable coverage means the summary and the tail may overlap, and neither
// "send it twice" nor "send nothing" is an honest fallback.
func agentWindow(messages []domain.Message, boundary string, budget ContextBudget) ([]responder.Turn, error) {
	source := afterMessage(messages, boundary)
	if source == nil {
		return nil, ErrSummaryBoundaryMissing
	}
	return agentCandidates(source, budget), nil
}

func summaryFailureMayDegrade(class string) bool {
	return class == SummaryFailureModel || class == SummaryFailureStage
}

// refreshSummary asks the model for one incremental summary and commits it under the
// version this turn read. It returns (nil, nil) when the commit lost a race, which the
// caller reports as an adoption rather than a failure.
//
// The second return value classifies a failure for the degradation policy: it is one of
// the closed-set SummaryFailure* classes when the turn may safely go on with the last
// good summary, and empty when it may not - a caller that has already canceled is ending
// the turn anyway, and an error with no class must propagate (that is how storage reads
// and other turn-fatal conditions stay distinct from model degradation).
//
// The caller's context is kept apart from the stage context on purpose, and for two
// different reasons that a single context would destroy:
//
//   - Scope. The stage budget bounds the model call, which is the slow, external,
//     skippable part. The commit is a local write that must not inherit a deadline meant
//     for a provider, so it runs on the caller's context.
//   - Attribution. "The summarizing stage ran out of its share" and "the learner hung up"
//     are opposite findings. The first is evidence about the model and opens a cooldown;
//     the second is not, and must not silence the session because the user left. Only the
//     caller's own context can tell them apart, so the stage context is never used to
//     answer that question.
func (s *MessageService) refreshSummary(ctx context.Context, userID, sessionID string, plan summary.Plan,
	previous string, expectVersion int64, controls summary.Controls,
) (*domain.SessionSummary, string, error) {
	caller := ctx
	key := summaryGuardKey(userID, sessionID)
	if controls.FailureCooldown && s.summaryGuard.coolingDown(key) {
		// Not attempted, so not re-recorded: extending a cooldown with each turn that
		// meets it would turn one model failure into an ever-longer silence, and the
		// session would never get to find out that the model came back.
		return nil, SummaryFailureStage, ErrSummaryCoolingDown
	}
	// callCtx is what the summarizing model sees. It is the caller's context plus, for a
	// migrated turn, the stage budget - and it is dropped as soon as that call returns.
	callCtx := caller
	if controls.StageDeadline {
		budget, ok := stageBudget(caller, s.summaryPolicy)
		if !ok {
			return nil, SummaryFailureStage, ErrSummaryStageSkipped
		}
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(caller, budget)
		defer cancel()
	}
	result, err := s.summarizer.Summarize(callCtx, summary.Request{
		Previous: previous, Turns: plan.Compress, IDs: plan.CompressIDs,
		Sources: plan.CompressSources, Controls: controls,
	})
	if err != nil {
		if callerErr := caller.Err(); callerErr != nil {
			return nil, "", callerErr
		}
		// The cause is noted before the mapping below: mapSummaryError decides what an API caller may see, and that is
		// not a fact about which provider failure occurred.
		noteFailure(modelcall.CurrentTask(caller), err, stageSummary)
		if controls.FailureCooldown {
			// A refresh that was attempted and refused - by the model, by the stage
			// budget, or by an empty answer - is evidence about summarizing, and it is
			// the stage budget's job to stop this turn spending more of the learner's
			// time on it. A caller who already walked away is not such evidence, and
			// was returned above.
			s.summaryGuard.recordFailure(key)
		}
		if controls.StageDeadline && callCtx.Err() != nil {
			// The stage deadline is the model call's own failure: it opened a
			// cooldown above, and the sentinel names the clock rather than the
			// provider for whoever reads the log.
			return nil, SummaryFailureModel, ErrSummaryStageTimedOut
		}
		return nil, SummaryFailureModel, mapSummaryError(err)
	}
	if strings.TrimSpace(result.Content) == "" {
		if callerErr := caller.Err(); callerErr != nil {
			return nil, "", callerErr
		}
		// The same attribution rule as a refused call: an empty summary over a caller
		// that is still waiting is the model's doing, but a blank answer returned to a
		// learner who already left says nothing about summarizing.
		if controls.FailureCooldown {
			s.summaryGuard.recordFailure(key)
		}
		return nil, SummaryFailureModel, ErrInternal
	}
	committed, err := s.repo.CommitSessionSummary(caller, &domain.SessionSummary{
		SessionID:        sessionID,
		UserID:           userID,
		Content:          result.Content,
		CoveredMessageID: plan.CoveredMessageID,
	}, expectVersion)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// A lost CAS is not a failure: the newer stored summary is adopted
			// instead, and nothing was regenerated. The other writer succeeded, so
			// this session's cooldown ends here rather than blocking a healthy one.
			s.summaryGuard.clear(key)
			return nil, "", nil
		}
		if callerErr := caller.Err(); callerErr != nil {
			return nil, "", callerErr
		}
		noteFailure(modelcall.CurrentTask(caller), err, stageSummary)
		// Storage failures are turn-fatal: they are not model failures, do not
		// open the model cooldown, and must not let the answer model run over
		// an uncommitted summary boundary.
		return nil, "", mapRepositoryError(err)
	}
	s.summaryGuard.clear(key)
	return committed, "", nil
}

// mapSummaryError keeps a summarizer failure inside the service's existing error
// vocabulary: an API caller sees the same internal error it sees for a failed model
// call, while cancellation stays distinguishable and the cause stays available to
// errors.Is for tests and logs.
func mapSummaryError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return ErrInternal
	}
}

// windowAfter is recentTurns over the suffix the summary has not covered. The boundary
// is a message id, not a clock: two messages can share a timestamp, and a summary that
// covered the wrong half of that pair would either repeat a turn or lose one. A boundary
// that names no message in this session is an error rather than a fallback, because the
// only fallbacks are "send it twice" and "send nothing".
func windowAfter(messages []domain.Message, boundary string, budget ContextBudget) ([]responder.Turn, error) {
	source := afterMessage(messages, boundary)
	if source == nil {
		return nil, ErrSummaryBoundaryMissing
	}
	return recentTurns(source, budget), nil
}

// afterMessage returns the messages following the given id. A nil answer means "no
// usable boundary", which is the case the caller treats as an unknown boundary rather
// than as an empty conversation.
func afterMessage(messages []domain.Message, boundary string) []domain.Message {
	boundary = strings.TrimSpace(boundary)
	if boundary == "" {
		return messages
	}
	for index := range messages {
		if messages[index].ID == boundary {
			return messages[index+1:]
		}
	}
	return nil
}

// --- Summary evidence read-back ---------------------------------------------
//
// A structured summary is a compression that cites the messages it was folded
// from. Reading those back needs the same care as an episode read-back, with
// one difference that decides the design: a summary only ever authorizes its
// own evidence ids, never every id below its watermark. The watermark says
// "these are folded in", not "these are citable" - most of what it covers was
// dropped precisely because it was not worth recording, and opening the whole
// prefix would hand the model the learner's entire session by id enumeration.

// SummaryEvidenceGrant is one read-back authorization derived from the summary
// that actually reached this turn's window.
type SummaryEvidenceGrant struct {
	MessageID string
	// SummaryVersion is the summary row's version the ids were drawn from. It
	// is what makes "this summary, up to this message" a claim that can be
	// checked later; a snapshot without one is not a frozen authority.
	SummaryVersion int64
	// Watermark is the summary's own covered-message boundary. A grant above it
	// is not a citation the summary ever had.
	Watermark string
}

// SummaryReadGrants turns the evidence ids of the summary that actually reached
// this turn's window into read-back authorizations.
//
// rendered is the block this turn sends, not the stored row: a long summary is
// cut to the window budget, and a cut document still decodes from the store
// while its tail is absent from the prompt, so visibility is decided per id
// against the text the model will actually see. Granting an id the budget
// trimmed away would mean the model can read back a citation it was never
// shown.
//
// Every early return is a different way a summary can be present without
// licensing a read: no stored row, no version, a watermark that names nothing,
// a legacy plain-prose record whose items carry no ids, or a document that
// cites nothing. Each yields nothing rather than an error, because the summary
// is an optimization and a turn that cannot read it back must still be
// answered.
//
// The caller registers the returned grants into the turn's evidence ledger
// (EpisodeSystem.RegisterSummaryEvidence) with the stored question id as the
// turn id, so the read-back tool serves them under the same boundary the
// episode grants already use.
func SummaryReadGrants(stored *domain.SessionSummary, rendered, sessionID string) []SummaryEvidenceGrant {
	if stored == nil || strings.TrimSpace(rendered) == "" || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if stored.Version <= 0 || strings.TrimSpace(stored.CoveredMessageID) == "" {
		return nil
	}
	document, structured := summary.ParseStored(stored.Content)
	if !structured {
		// A legacy plain-prose summary has no ids to grant. It keeps working as
		// context exactly as it did; nothing about it authorizes a read.
		return nil
	}
	cited := document.EvidenceMessageIDs()
	if len(cited) == 0 {
		return nil
	}
	ids := summary.VisibleEvidenceIDs(rendered, cited)
	grants := make([]SummaryEvidenceGrant, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || domain.CompareMessageIDs(id, stored.CoveredMessageID) > 0 {
			// An id above the watermark is a citation the projector invented
			// rather than one the summary drew from stored history.
			continue
		}
		grants = append(grants, SummaryEvidenceGrant{
			MessageID: id, SummaryVersion: stored.Version, Watermark: stored.CoveredMessageID,
		})
	}
	return grants
}
