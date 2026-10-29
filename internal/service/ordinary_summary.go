package service

import (
	"context"
	"errors"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/modelcall"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/summary"
)

// ordinaryConversationFor refreshes the summary if the policy says it is due, then assembles
// what the turn may be answered from: the sealed summary, and the complete exchanges
// after the summary's boundary trimmed to the conversation budget.
//
// Both the refresh and the window are cut at the same boundary, which is what keeps a
// message from arriving twice - summarized once and verbatim again - in one prompt. A
// stored boundary this session cannot locate is therefore not a detail to work around:
// the turn ends with ErrSummaryBoundaryMissing rather than being answered from a summary
// and a tail whose relationship to each other is unknown, and the same is true of a
// summary adopted from a race.
func (s *MessageService) ordinaryConversationFor(ctx context.Context, userID, sessionID string, turn storedTurn) (string, []responder.Turn, SummaryOutcome, SummaryDetail, error) {
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
		plan := summary.OrdinaryBuild(turn.messages, boundary, s.summaryPolicy)
		detail = plan.Skipped
		switch plan.Skipped {
		case "":
			// The refresh is its own model call for its own reason, so it carries its own
			// label: a summary's tokens must not be read as part of the answer's cost.
			refreshed, err := s.ordinaryRefreshSummary(modelcall.WithPurpose(ctx, modelcall.PurposeSummary), userID, sessionID, plan, content, version)
			if err != nil {
				// A refresh that was due and failed ends the turn. Carrying on with
				// the old summary would let the conversation silently outgrow what
				// the summary covers, and the turn would be answered out of a
				// context the deployment can no longer describe.
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

	history, err := windowAfter(turn.messages, boundary, s.context)
	if err != nil {
		// Also covers the summary adopted from a race: a row this turn did not plan has
		// to be locatable before its boundary is allowed to cut the window.
		return "", nil, outcome, detail, err
	}
	return content, history, outcome, detail, nil
}

// ordinaryRefreshSummary asks the model for one incremental summary and commits it under the
// version this turn read. It returns (nil, nil) when the commit lost a race, which the
// caller reports as an adoption rather than a failure.
func (s *MessageService) ordinaryRefreshSummary(ctx context.Context, userID, sessionID string, plan summary.Plan, previous string, expectVersion int64) (*domain.SessionSummary, error) {
	result, err := s.summarizer.Summarize(ctx, summary.Request{Previous: previous, Turns: plan.Compress})
	if err != nil {
		// The cause is noted before the mapping below, from the task carried on the
		// context: mapSummaryError decides what an API caller may see, and that is
		// not a fact about which provider failure occurred.
		noteFailure(modelcall.CurrentTask(ctx), err, stageSummary)
		// A cancelled caller is reported as the cancellation - whose event this is gets
		// decided from the context, not from the error text - and anything else keeps
		// its own identity for errors.Is; the map only decides what an API caller sees.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, mapSummaryError(err)
	}
	if strings.TrimSpace(result.Content) == "" {
		return nil, ErrInternal
	}
	committed, err := s.repo.CommitSessionSummary(ctx, &domain.SessionSummary{
		SessionID:        sessionID,
		UserID:           userID,
		Content:          result.Content,
		CoveredMessageID: plan.CoveredMessageID,
	}, expectVersion)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// A lost CAS is not a failure: the newer stored summary is adopted
			// instead, and nothing was regenerated.
			return nil, nil
		}
		noteFailure(modelcall.CurrentTask(ctx), err, stageSummary)
		return nil, mapRepositoryError(err)
	}
	return committed, nil
}
