package app

import (
	"context"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// MemoryProposalWriter is the slice of the memory write side this assembly needs. It is
// spelled out as an interface so the wiring stays testable; the production value is
// *service.MemoryService.
//
// One method, and it is the only one there is to call: this seam cannot confirm, reject,
// archive, list or read. An agent that had any of those could complete the loop the
// confirmation gate exists to break - deciding for itself what the user believes.
type MemoryProposalWriter interface {
	ProposeMemoryDraft(ctx context.Context, userID string, draft domain.MemoryDraft) (*domain.Memory, bool, error)
}

// memoryProposer supplies everything a proposal needs that a model may not choose: whose
// memory it is, which conversation it came from, which stored message it rests on, and how
// certain the claim is.
//
// Confidence is deliberately not set here. The service owns it - see
// service.MemoryService.ProposeMemoryDraft - and one owner is the point: two places that
// each pin it can drift, and a drifted value would store a memory the recall floor then
// silently never shows. Status is not a parameter at any point on this path either:
// domain.MemoryDraft has no status field, and the only state this can write is pending.
type memoryProposer struct {
	writer MemoryProposalWriter
}

func newMemoryProposer(writer MemoryProposalWriter) agent.MemoryProposer {
	return memoryProposer{writer: writer}
}

// ProposeMemory implements agent.MemoryProposer. The principal arrives from the turn, which
// is why the source is a single message id rather than a list the caller assembled: the
// question this turn stored, and nothing else.
func (p memoryProposer) ProposeMemory(
	ctx context.Context, principal agent.Principal, proposal agent.MemoryProposal,
) (agent.MemoryProposalOutcome, error) {
	if p.writer == nil {
		return agent.MemoryProposalOutcome{}, service.ErrNoMemoryStore
	}
	stored, created, err := p.writer.ProposeMemoryDraft(ctx, principal.UserID, domain.MemoryDraft{
		UserID:           principal.UserID,
		Scope:            proposal.Scope,
		TopicKey:         proposal.TopicKey,
		Content:          proposal.Content,
		SourceSessionID:  principal.SessionID,
		SourceMessageIDs: []string{principal.UserMessageID},
	})
	if err != nil {
		return agent.MemoryProposalOutcome{}, err
	}
	// The row is handed back only when this call wrote it. On a duplicate the writer
	// returns the pre-existing record, and passing that id along would put a decision
	// button on this turn for a draft another conversation is waiting on.
	outcome := agent.MemoryProposalOutcome{Created: created}
	if created {
		outcome.Draft = stored
	}
	return outcome, nil
}
