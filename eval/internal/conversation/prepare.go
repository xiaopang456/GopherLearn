// Package conversation implements offline history preparation without running an agent.
package conversation

import (
	"context"
	"fmt"
	"strings"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

type Turn struct {
	User        string    `json:"user"`
	Assistant   string    `json:"assistant"`
	UserAt      time.Time `json:"user_at"`
	AssistantAt time.Time `json:"assistant_at"`
}
type MessageWriter interface {
	Create(context.Context, *model.Message) (*model.Message, error)
}
type SummaryProvider interface {
	SessionSummary(context.Context, agent.RunContext) (agent.SummarySnapshot, error)
}

type PreparedTurn struct {
	UserID          uint
	AssistantID     uint
	Summary         agent.SummarySnapshot
	SummaryDegraded bool
	// CooldownWaitMilliseconds is how long the replay waited out the product's
	// failure cooldown before it wrote this turn.
	CooldownWaitMilliseconds int64
}

// Prepare appends each user message, runs the product summary policy, and then
// appends the supplied assistant message. It never creates agent runs or events.
// A message persistence failure stops immediately. Summary failures are recorded
// and preserve the returned snapshot, as the product's context builder does.
func Prepare(ctx context.Context, writer MessageWriter, summary SummaryProvider, owner int64, sessionID, username string, turns []Turn) ([]PreparedTurn, error) {
	return PrepareWithCooldown(ctx, writer, summary, owner, sessionID, username, turns, 0)
}

// PrepareWithCooldown prepares a conversation the way a real one happens: when a
// compaction fails, the product opens a failure cooldown and suppresses the next
// compactions for its length. A preparation that replays a whole history in a
// few seconds would spend that cooldown in one breath and never compact again,
// so the replay waits the cooldown out before it writes the next turn. A
// cooldown of zero keeps the old behaviour.
func PrepareWithCooldown(ctx context.Context, writer MessageWriter, summary SummaryProvider, owner int64, sessionID, username string, turns []Turn, cooldown time.Duration) ([]PreparedTurn, error) {
	if writer == nil || summary == nil || owner <= 0 || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(username) == "" || len(turns) == 0 {
		return nil, fmt.Errorf("invalid preparation parameters")
	}
	if cooldown < 0 {
		return nil, fmt.Errorf("the failure cooldown cannot be negative")
	}
	var previous time.Time
	for i, t := range turns {
		if strings.TrimSpace(t.User) == "" || strings.TrimSpace(t.Assistant) == "" {
			return nil, fmt.Errorf("turn %d has an empty message", i+1)
		}
		if t.UserAt.IsZero() || t.AssistantAt.IsZero() || !t.AssistantAt.After(t.UserAt) || (!previous.IsZero() && !t.UserAt.After(previous)) {
			return nil, fmt.Errorf("turn %d has invalid message timestamps", i+1)
		}
		previous = t.AssistantAt
	}
	results := make([]PreparedTurn, 0, len(turns))
	var failedAt time.Time
	for i, t := range turns {
		var waited time.Duration
		if !failedAt.IsZero() && cooldown > 0 {
			wait := time.Until(failedAt.Add(cooldown))
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return results, ctx.Err()
				case <-timer.C:
				}
				waited = wait
			}
		}
		user, err := writer.Create(ctx, &model.Message{SessionID: sessionID, UserName: username, Content: t.User, IsUser: true, CreatedAt: t.UserAt.UTC(), AnswerBasis: rag.AnswerBasisConversation, KnowledgeStatus: rag.KnowledgeStatusNotRequired, KnowledgeReason: rag.KnowledgeReasonNone})
		if err != nil {
			return results, fmt.Errorf("turn %d user: %w", i+1, err)
		}
		if user == nil || user.ID == 0 {
			return results, fmt.Errorf("turn %d user was not persisted", i+1)
		}
		s, err := summary.SessionSummary(ctx, agent.RunContext{UserID: owner, UserName: username, SessionID: sessionID, Question: t.User})
		degraded := err != nil
		if degraded {
			failedAt = time.Now()
		}
		if ctx.Err() != nil {
			return results, ctx.Err()
		}
		assistant, err := writer.Create(ctx, &model.Message{SessionID: sessionID, UserName: username, Content: t.Assistant, CreatedAt: t.AssistantAt.UTC()})
		if err != nil {
			return results, fmt.Errorf("turn %d assistant: %w", i+1, err)
		}
		if assistant == nil || assistant.ID <= user.ID {
			return results, fmt.Errorf("turn %d assistant id is invalid", i+1)
		}
		results = append(results, PreparedTurn{UserID: user.ID, AssistantID: assistant.ID, Summary: s, SummaryDegraded: degraded, CooldownWaitMilliseconds: waited.Milliseconds()})
	}
	return results, nil
}
