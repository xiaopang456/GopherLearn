package episode

import (
	"context"
	"fmt"
	"log"
)

type SessionHistoryReader interface {
	HasSessionHistory(context.Context, int64, string) (bool, error)
}

// ShadowObserver executes recall and emits metadata only. The recalled Episode
// body is intentionally discarded here and cannot reach ContextBuilder.
type ShadowObserver struct {
	history   SessionHistoryReader
	retriever Recaller
	logf      func(string, ...any)
}

// Recaller is what the shadow observer and the context builder need from an
// Episode read path. Both the legacy_v2 retriever and the v3 one satisfy it, so
// the mode switch happens once at wiring time rather than at every call site.
type Recaller interface {
	Recall(context.Context, RecallRequest) (RecallResult, error)
}

func NewShadowObserver(history SessionHistoryReader, retriever Recaller) (*ShadowObserver, error) {
	if history == nil || retriever == nil {
		return nil, fmt.Errorf("episode shadow dependencies required")
	}
	return &ShadowObserver{history: history, retriever: retriever, logf: log.Printf}, nil
}
func (s *ShadowObserver) Observe(ctx context.Context, userID int64, sessionID, question string) error {
	hasHistory, err := s.history.HasSessionHistory(ctx, userID, sessionID)
	if err != nil {
		return err
	}
	result, err := s.retriever.Recall(ctx, RecallRequest{UserID: userID, SessionID: sessionID, Question: question, CurrentSessionHasHistory: hasHistory})
	if err != nil {
		return err
	}
	s.logf("episode_shadow intent=%s abstained=%t reason=%s candidates=%v elapsed_ms=%d estimated_tokens=%d", result.Trace.Intent, result.Trace.Abstained, result.Trace.Reason, result.Trace.Candidates, result.Trace.ElapsedMS, result.Trace.EstimatedTokens)
	return nil
}
