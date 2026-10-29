package memory

import (
	"GopherAI/model"
	"context"
	"errors"
	"strings"
	"testing"
)

type enqueueRecorder struct{ jobs []model.MemoryExtractionJob }

func (e *enqueueRecorder) Enqueue(_ context.Context, job *model.MemoryExtractionJob) (bool, error) {
	e.jobs = append(e.jobs, *job)
	return true, nil
}

func TestPublisherOnlyEnqueuesHighValueAgentEvents(t *testing.T) {
	recorder := &enqueueRecorder{}
	publisher, _ := NewPublisher(recorder)
	if err := publisher.PublishAgentRun(context.Background(), 1, "s", "ordinary", "你好", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishAgentRun(context.Background(), 1, "s", "remember", "请记住以后先讲调用链", 0, false); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishAgentRun(context.Background(), 1, "s", "tool", "修复部署问题", 1, false); err != nil {
		t.Fatal(err)
	}
	// A run that proposed a draft is the user's to decide on, so it must not
	// also be extracted from.
	if err := publisher.PublishAgentRun(context.Background(), 1, "s", "drafted", "请记住我用 Go", 1, true); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishSummaryCompacted(context.Background(), 1, "s", 3, `{"current_goal":"finish","user_corrections":["不是 JSON，是 Protobuf"]}`); err != nil {
		t.Fatal(err)
	}
	if len(recorder.jobs) != 3 || recorder.jobs[0].EventType != model.MemoryEventExplicitRemember || recorder.jobs[1].EventType != model.MemoryEventAgentRun || recorder.jobs[2].EventType != model.MemoryEventSummaryCompacted || recorder.jobs[2].SourceID != "s:v3" || recorder.jobs[2].EventPayload == "" {
		t.Fatalf("jobs=%#v", recorder.jobs)
	}
	if strings.Contains(recorder.jobs[2].EventPayload, "current_goal") {
		t.Fatalf("session-scoped field reached queued payload: %s", recorder.jobs[2].EventPayload)
	}
	if err := publisher.PublishSummaryCompacted(context.Background(), 1, "s", 4, `{"current_goal":"temporary","open_loops":["later"]}`); err != nil {
		t.Fatal(err)
	}
	if len(recorder.jobs) != 3 {
		t.Fatalf("session-only summary enqueued a job: %#v", recorder.jobs)
	}
	if err := publisher.PublishSummaryCompacted(context.Background(), 1, "s", 5, `{"current_goal":`); !errors.Is(err, ErrInvalidSummaryEvidence) {
		t.Fatalf("malformed summary error=%v", err)
	}
	if len(recorder.jobs) != 3 {
		t.Fatalf("malformed summary enqueued a job: %#v", recorder.jobs)
	}
}
