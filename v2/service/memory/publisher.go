package memory

import (
	"GopherAI/model"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type JobEnqueuer interface {
	Enqueue(context.Context, *model.MemoryExtractionJob) (bool, error)
}

type Publisher struct {
	repository JobEnqueuer
	now        func() time.Time
}

func NewPublisher(repository JobEnqueuer) (*Publisher, error) {
	if repository == nil {
		return nil, fmt.Errorf("memory job enqueuer is required")
	}
	return &Publisher{repository: repository, now: time.Now}, nil
}

func (p *Publisher) PublishAgentRun(ctx context.Context, userID int64, sessionID, runID, question string, toolCalls int, draftProposed bool) error {
	// A run that produced a draft has already handed this turn to the user, and
	// extracting from it as well writes the same statement down twice.
	//
	// Observed: one "请记住：我理解 TCP 粘包的本质是……" produced a confirmed draft
	// and, from the same run, an explicit_remember extraction. The two texts said
	// the same thing in different languages, so neither the deduplication step
	// nor the deleted-content check recognised the pair, and both were stored.
	//
	// The worse half is cancellation. Declining a draft only marks it cancelled;
	// it leaves no record that the content was refused. Extraction running in
	// parallel would then store what the user just declined to store, which is
	// the one outcome a confirmation step exists to prevent.
	//
	// The cost is that a second, incidental fact mentioned in the same turn is
	// not extracted. That is the right way round: the draft is visible and
	// reversible, while a silent duplicate is neither.
	if draftProposed {
		return nil
	}
	eventType := model.MemoryEventAgentRun
	if explicitRemember(question) {
		eventType = model.MemoryEventExplicitRemember
	} else if toolCalls == 0 {
		return nil
	}
	_, err := p.repository.Enqueue(ctx, &model.MemoryExtractionJob{
		ID: uuid.NewString(), UserID: userID, EventType: eventType, SourceID: runID,
		SourceSessionID: sessionID, SourceRunID: runID, Status: model.MemoryJobPending, AvailableAt: p.now(),
	})
	return err
}

func (p *Publisher) PublishLearningCompleted(ctx context.Context, userID int64, sessionID, learningSessionID string) error {
	_, err := p.repository.Enqueue(ctx, &model.MemoryExtractionJob{
		ID: uuid.NewString(), UserID: userID, EventType: model.MemoryEventLearningComplete, SourceID: learningSessionID,
		SourceSessionID: sessionID, Status: model.MemoryJobPending, AvailableAt: p.now(),
	})
	return err
}

func (p *Publisher) PublishSummaryCompacted(ctx context.Context, userID int64, sessionID string, version uint64, content string) error {
	durable, err := durableSummaryEvidence(content)
	if errors.Is(err, ErrNoDurableMemory) {
		return nil
	}
	if err != nil {
		return err
	}
	sourceID := fmt.Sprintf("%s:v%d", sessionID, version)
	_, err = p.repository.Enqueue(ctx, &model.MemoryExtractionJob{
		ID: uuid.NewString(), UserID: userID, EventType: model.MemoryEventSummaryCompacted, SourceID: sourceID,
		SourceSessionID: sessionID, EventPayload: durable, Status: model.MemoryJobPending, AvailableAt: p.now(),
	})
	return err
}

func explicitRemember(question string) bool {
	value := strings.ToLower(strings.TrimSpace(question))
	for _, marker := range []string{"请记住", "帮我记住", "以后请", "以后都", "不要再", "记一下", "纠正一下"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
