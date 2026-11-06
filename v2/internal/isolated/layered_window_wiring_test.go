package isolated

import (
	"context"
	"encoding/json"
	"testing"

	"GopherAI/model"
	agentService "GopherAI/service/agent"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// The shipped configuration has to mean what the constants meant.
//
// The four numbers are restated in the config package because it cannot import
// service/agent. This is where the two copies are held together: a change to
// one that forgets the other lands here instead of silently resizing every
// deployment's window.
func TestTheConfiguredBlockBudgetsMatchTheAgentDefaults(t *testing.T) {
	agentConfig := testSettings(t).Config.AgentConfig
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"whiteboard", agentConfig.ContextMaxWhiteboardTokens, agentService.DefaultWhiteboardTokens},
		{"resident segments", agentConfig.ContextMaxResidentSegmentTokens, agentService.DefaultResidentSegmentTokens},
		{"retrieved segments", agentConfig.ContextMaxRetrievedSegmentTokens, agentService.DefaultRetrievedSegmentTokens},
		{"one segment", agentConfig.ContextMaxSegmentTokens, agentService.DefaultSegmentTokens},
	} {
		if c.got != c.want {
			t.Fatalf("the configured %s budget is %d and the agent default is %d", c.name, c.got, c.want)
		}
	}
}

// The mode that reads the new state back has to put the board and the segments
// in the window.
//
// This is the entry-level assertion the block budgets were missing. Turning the
// mode on used to be enough to make every unit test pass and still send an empty
// window, because the four block budgets were never handed to the context
// builder: the renderer saw zero and returned nothing. A scale, a mode and a
// table are not the same thing as a block that arrives.
func TestTheOnModeSendsTheBoardAndTheSegments(t *testing.T) {
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentMode = "on"
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-on-window")

	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	mustSeedLayeredState(t, db, row.UserID, session)

	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "我最初每天读多少页",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	var board, segments bool
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		if content := message.Content; len(content) > 0 {
			if len(content) >= len("Current state") && content[:len("Current state")] == "Current state" {
				board = true
			}
			if len(content) >= len("Summary segments") && content[:len("Summary segments")] == "Summary segments" {
				segments = true
			}
		}
	}
	if !board {
		t.Fatalf("the board did not reach the window (%d messages)", len(window.Messages))
	}
	if !segments {
		t.Fatalf("the segments did not reach the window (%d messages)", len(window.Messages))
	}
}

// The same window with the mode off carries neither block: the assertion above
// must be able to fail, or it is measuring the wrong thing.
func TestTheOffModeSendsNeitherBlock(t *testing.T) {
	s := testSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-off-window")

	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	mustSeedLayeredState(t, db, row.UserID, session)

	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "我最初每天读多少页",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		if len(message.Content) >= len("Current state") && message.Content[:len("Current state")] == "Current state" {
			t.Fatal("the board reached the window with the mode off")
		}
	}
}

// mustSeedLayeredState writes one board and one segment, which is what the
// background would have written by the time a session is long enough to ask
// about.
func mustSeedLayeredState(t *testing.T, db *gorm.DB, userID int64, sessionID string) {
	t.Helper()
	board, err := json.Marshal(model.SessionWhiteboardDocument{
		SchemaVersion: 1,
		Goal:          &model.SessionWhiteboardGoal{Text: "把每天的读量提到 30 页", SourceMessageIDs: []uint{1}},
		Plan: []model.SessionWhiteboardEntry{{
			Key: "daily-pages", Text: "每天读 30 页", Value: "30 页", SourceMessageIDs: []uint{2},
		}},
	})
	if err != nil {
		t.Fatalf("encode the board: %v", err)
	}
	if err := db.Create(&model.SessionWhiteboard{
		UserID: userID, SessionID: sessionID, Content: string(board), CoveredMessageID: 2,
	}).Error; err != nil {
		t.Fatalf("write the board: %v", err)
	}
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: 1, Topic: "每天的读量",
		Points: []model.SessionSummarySegmentPoint{{Text: "最初每天读 15 页", EvidenceMessageIDs: []uint{1}}},
	})
	if err != nil {
		t.Fatalf("encode the segment: %v", err)
	}
	if err := db.Create(&model.SessionSummarySegment{
		UserID: userID, SessionID: sessionID, Seq: 1, StartMessageID: 1, EndMessageID: 2,
		Content: string(content), IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 40,
	}).Error; err != nil {
		t.Fatalf("write the segment: %v", err)
	}
}
