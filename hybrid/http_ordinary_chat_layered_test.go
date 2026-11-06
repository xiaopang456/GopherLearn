package hybrid

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// An ordinary conversation is the turn this ceiling actually applies to.
//
// A legacy chat session has no library to switch on, so ResolveSessionPolicy
// answers knowledge_disabled for every one of its turns - the switch is not an
// option the learner has to remember. That makes "the window still carries the
// board and the segments" a requirement of the ordinary path rather than of an
// Agent turn that happened to turn the library off.
//
// The mode is the second half. Off is the shipped default and carries neither
// block; on is what the experiment runs, and the same request has to arrive
// with both. Asserting only the on case would pass on a window that ignores the
// mode, and asserting only the off case would pass on one that never renders
// them at all.
func TestAnOrdinaryConversationTurnSendsTheConversationBlocksUnderTheMode(t *testing.T) {
	for _, tc := range []struct {
		mode         string
		wantBoard    bool
		wantSegments bool
	}{
		{mode: "on", wantBoard: true, wantSegments: true},
		{mode: "off"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			settings := testSettings(t)
			settings.Config.AgentConfig.SummarySegmentMode = tc.mode
			app, db, ordinary, agent, token, _ := httpFixtureWithSettings(t, settings, nil)
			session := createSession(t, app, token, "ordinary-"+tc.mode, "chat")
			mustSeedOrdinaryLayeredState(t, db, 1, session)

			status, envelope, raw := postTurn(t, app, token, session, "")
			if status != 200 || envelope["error"] != nil {
				t.Fatalf("the ordinary turn failed: %d %s", status, raw)
			}
			// The premise, checked rather than assumed: this turn ran on the V2
			// path. If ordinary chat had gone back to the retained path, the
			// assertions below would be reading the wrong model entirely.
			if agent.CallCount() == 0 || len(ordinary.all()) != 0 {
				t.Fatalf("the ordinary turn did not run on the V2 path: v2 calls=%d retained calls=%d",
					agent.CallCount(), len(ordinary.all()))
			}
			// And it is a capability-free turn, which is why the blocks matter
			// here: a turn offered no tools has nothing but the conversation.
			if bound := agent.BoundToolCalls(); len(bound) != 0 {
				t.Fatalf("an ordinary conversation turn was offered tools: %v", bound)
			}

			var board, segments bool
			for _, request := range agent.Requests() {
				for _, message := range request {
					if message == nil {
						continue
					}
					if strings.HasPrefix(message.Content, "Current state (server state;") {
						board = true
					}
					if strings.HasPrefix(message.Content, "Summary segments (compressed record") {
						segments = true
					}
				}
			}
			if board != tc.wantBoard || segments != tc.wantSegments {
				t.Fatalf("with the mode %s the ordinary turn was sent board=%t segments=%t, want board=%t segments=%t",
					tc.mode, board, segments, tc.wantBoard, tc.wantSegments)
			}
		})
	}
}

// mustSeedOrdinaryLayeredState writes the board and the segment the background
// would have written before a session grew long enough to ask about.
func mustSeedOrdinaryLayeredState(t *testing.T, db *gorm.DB, userID int64, sessionID string) {
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

// The writer has to run on an ordinary conversation.
//
// This is the failure a capability gate would have caused, and the read side
// could never show it: an ordinary conversation resolves to knowledge_disabled
// on every turn, so an enqueue behind the gate would never run, and the window
// would simply always be empty - a reading that looks like "the layered summary
// did not help" rather than like a bug.
//
// So nothing is seeded here. The board and the segment are produced by the
// product from prose turns alone, by the same background worker a deployment
// runs, and only then does the next turn get asked what it was sent.
func TestAnOrdinaryConversationWritesItsBoardAndSegments(t *testing.T) {
	settings := testSettings(t)
	settings.Config.AgentConfig.SummarySegmentMode = "on"
	// The worker's clock, and the threshold: two unsummarized messages are
	// enough, which one turn already leaves behind.
	settings.Config.AgentConfig.SummarySegmentScanIntervalSeconds = 1
	settings.Config.AgentConfig.SummarySegmentTriggerMessages = 2
	settings.Config.AgentConfig.SummarySegmentTriggerTokens = 1000000
	settings.Config.AgentConfig.SummarySegmentMaxAttempts = 1
	app, db, _, agent, token, _ := httpFixtureWithSettings(t, settings, nil)

	// One fake answers both contracts. The reply is written to satisfy the
	// segment writer's own document - schema version, one point that cites a
	// message the batch contains, one board entry - and is a string as well,
	// so the turn that asks a question still gets an answer.
	const reply = `{"schema_version":1,` +
		`"segment":{"topic":"每天的读量","points":[{"text":"最初每天读 15 页","evidence_message_ids":[1]}]},` +
		`"whiteboard":{"schema_version":1,"goal":{"text":"把每天的读量提到 30 页","source_message_ids":[1]},` +
		`"plan":[{"key":"daily-pages","text":"每天读 30 页","value":"30 页","source_message_ids":[1]}]}}`
	agent.setRespond(func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage(reply, nil), nil
	})

	session := createSession(t, app, token, "ordinary-writes", "chat")
	turn := func(index int) {
		t.Helper()
		body := fmt.Sprintf(`{"content":"第 %d 轮：我们每天读多少页？","requestId":"ordinary-writes-%d"}`, index, index)
		status, envelope, raw := requestHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+session+"/messages", body, token)
		if status != 200 || envelope["error"] != nil {
			t.Fatalf("turn %d failed: %d %s", index, status, raw)
		}
	}
	for index := 1; index <= 3; index++ {
		turn(index)
	}

	// The writer is a background worker. The test waits for it instead of
	// assuming a clock.
	deadline := time.Now().Add(30 * time.Second)
	var segments, boards int64
	for {
		if err := db.Model(&model.SessionSummarySegment{}).Where("session_id = ?", session).Count(&segments).Error; err != nil {
			t.Fatalf("count segments: %v", err)
		}
		if err := db.Model(&model.SessionWhiteboard{}).Where("session_id = ?", session).Count(&boards).Error; err != nil {
			t.Fatalf("count boards: %v", err)
		}
		if segments > 0 && boards > 0 {
			break
		}
		if time.Now().After(deadline) {
			var jobs []model.SessionSummarySegmentJob
			_ = db.Where("session_id = ?", session).Find(&jobs).Error
			t.Fatalf("an ordinary conversation wrote %d segments and %d boards after 3 turns (%d jobs queued)",
				segments, boards, len(jobs))
		}
		time.Sleep(200 * time.Millisecond)
	}

	// And the turn after the write is sent them.
	turn(4)
	var board, segmentBlock bool
	for _, request := range agent.Requests() {
		for _, message := range request {
			if message == nil {
				continue
			}
			if strings.HasPrefix(message.Content, "Current state (server state;") {
				board = true
			}
			if strings.HasPrefix(message.Content, "Summary segments (compressed record") {
				segmentBlock = true
			}
		}
	}
	if !board || !segmentBlock {
		t.Fatalf("the turn after the write was sent board=%t segments=%t", board, segmentBlock)
	}
}
