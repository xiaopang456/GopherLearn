package isolated

import (
	"context"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
)

// The shadow mode is a write-side mode: it writes the segments and the board
// while the context still comes from the old path.
//
// It only does anything if the entry actually starts the write path and asks
// it to enqueue after an answer. An assembly that is built and never asked is
// a worker that waits for jobs nobody records, and the mode that is supposed
// to be observed observes nothing.
func TestTheShadowModeEnqueuesASegmentAfterAnAnswer(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentMode = "shadow"
	// One turn is enough to want a segment here. The threshold exists to keep
	// a short conversation out of the queue, and this test is about the wiring,
	// not about the threshold.
	s.Config.AgentConfig.SummarySegmentTriggerMessages = 1
	s.Config.AgentConfig.SummarySegmentTriggerTokens = 1
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-shadow")
	runHTTP(t, app, token, session, "run-shadow", "你好")

	var jobs []model.SessionSummarySegmentJob
	if err := db.Where("session_id = ?", session).Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("the shadow run recorded %d segment jobs, want 1", len(jobs))
	}
	if jobs[0].Status == model.SessionSummarySegmentJobCompleted {
		t.Fatal("the segment job was already finished, so nothing was written")
	}
	if jobs[0].StartMessageID == 0 {
		t.Fatal("the segment job has no start")
	}
}

// The write path is off by default, and an entry that is off has no worker
// and no enqueue.
func TestTheOffModeRecordsNoSegmentJob(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentTriggerMessages = 1
	s.Config.AgentConfig.SummarySegmentTriggerTokens = 1
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-off")
	runHTTP(t, app, token, session, "run-off", "你好")

	var jobs []model.SessionSummarySegmentJob
	if err := db.Where("session_id = ?", session).Find(&jobs).Error; err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Fatalf("the off mode recorded %d segment jobs", len(jobs))
	}
}
