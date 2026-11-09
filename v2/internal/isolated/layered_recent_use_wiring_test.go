package isolated

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"GopherAI/model"
	agentService "GopherAI/service/agent"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// The segments the learner keeps reaching for have to come back.
//
// The record is what brings them back: the browse tool writes it when the
// model opens a segment, the preselector writes it when a question selects
// one, and the reader reads it. An entry that wires the two writers and not
// the reader looks complete and loses the segment exactly when the learner
// wants it - the newest segments have moved past it by then.
//
// The wiring lives in the entry, so the test goes through the entry: the
// model opens a segment with the browse tool, and the next turn is asked
// what it was sent.

// The newest segments are never pushed out by the record.
//
// The record adds the segments the learner reached for to the resident set,
// and the resident set is a budget. The question the record has to answer is
// whether it takes the room the newest segments need.
//
// It does not: the reader only ever adds to the set, and the block that
// renders it walks the segments from the newest backwards, so the newest ones
// are the ones that are kept when the budget runs out. The recently used
// segments take what is left.
//
// The budget here is deliberately tight: three of the four segments fit, so
// the fourth - the one the record brought back - competes for the room and
// loses it. A test with room to spare would pass on a reader that replaced
// the set instead of adding to it.
func TestTheRecentUseRecordDoesNotPushTheNewestSegmentsOut(t *testing.T) {
	s := recentUseSettings(t)
	s.Config.AgentConfig.SummarySegmentTriggerMessages = 100
	s.Config.AgentConfig.SummarySegmentTriggerTokens = 1000000
	app, db, chat, token := newFixture(t, s, func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return toolCall("browse_summary_segments", `{"query":"读量","segments":[1]}`), nil
		}
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-recent-use-oldest")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	segments := mustSeedLargeSegmentSet(t, db, row.UserID, session)

	// The turn that opens the segment, and the turn after it: the second one
	// is the one the record is for.
	runHTTP(t, app, token, session, "run-recent-use-browse", "你好")
	runHTTP(t, app, token, session, "run-recent-use-again", "你好")

	blocks := segmentBlocks(chat)
	if len(blocks) == 0 {
		t.Fatalf("no segment block reached the window: %s", requestText(chat))
	}
	last := blocks[len(blocks)-1]

	// The premise: the budget really is tight. Three segments fit and the
	// fourth does not, so the competition the test is about actually
	// happens.
	fits := 0
	used := agentService.SummarySegmentBlockHeaderTokens()
	for _, segment := range segments {
		cost := agentService.RenderedSegmentTokens(segment)
		if used+cost > s.Config.AgentConfig.ContextMaxResidentSegmentTokens {
			break
		}
		used += cost
		fits++
	}
	if fits < 2 || fits >= len(segments) {
		t.Fatalf("the budget holds %d of %d segments; the test needs it to bind", fits, len(segments))
	}

	// The newest segments are still there: the record adds to the set, it
	// does not replace it.
	for _, seq := range []int{4, 3, 2} {
		if !strings.Contains(last, segmentMarker(seq)) {
			t.Fatalf("the newest segment %d was pushed out of the resident block: %s", seq, last)
		}
	}
	// And the record's segment is the one that was cut, not one of the
	// newest ones.
	if strings.Contains(last, segmentMarker(1)) {
		t.Fatalf("the resident block holds a segment the budget had no room for: %s", last)
	}
}

// A segment the learner opened is in the resident set on the next turn.
//
// The segment the learner opened is older than the ones that travel with
// every turn, so the record is the only thing that can bring it back. Without
// the reader wired to the record the window would carry the newest segments
// and nothing else, and the learner would have to ask for the same stretch
// again.
//
// The budget here has room for all of them: what the test is about is which
// segments are in the set, not what the budget cuts.
func TestTheBrowsedSegmentIsResidentOnTheNextTurn(t *testing.T) {
	s := recentUseSettings(t)
	app, db, chat, token := newFixture(t, s, func(_ context.Context, call int) (*schema.Message, error) {
		if call == 1 {
			return toolCall("browse_summary_segments", `{"query":"读量","segments":[1]}`), nil
		}
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-recent-use-next")
	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	mustSeedSegmentSet(t, db, row.UserID, session)

	// The turn that opens the segment: the window it was sent must not
	// carry the segment yet, or the assertion below would pass on a window
	// that ignores the record.
	runHTTP(t, app, token, session, "run-recent-use-browse", "你好")
	before := segmentBlocks(chat)
	if len(before) == 0 {
		t.Fatalf("no segment block reached the window: %s", requestText(chat))
	}
	if strings.Contains(before[len(before)-1], segmentMarker(1)) {
		t.Fatalf("the opened segment was in the window before it was opened: %s", before[len(before)-1])
	}

	// The next turn.
	runHTTP(t, app, token, session, "run-recent-use-next", "你好")
	after := segmentBlocks(chat)
	if len(after) == 0 {
		t.Fatalf("no segment block reached the window: %s", requestText(chat))
	}
	last := after[len(after)-1]
	if !strings.Contains(last, segmentMarker(1)) {
		t.Fatalf("the opened segment is not in the resident block on the next turn: %q", last)
	}
}

// recentUseSettings is the configuration the two tests above run with.
//
// The background writer must not add a segment of its own while the tests
// watch a set of four, so the trigger is a hundred turns and the tests run
// two.
func recentUseSettings(t *testing.T) *Settings {
	t.Helper()
	s := testSettings(t)
	s.Config.AgentConfig.SummarySegmentMode = "on"
	s.Config.AgentConfig.SummarySegmentTriggerMessages = 100
	s.Config.AgentConfig.SummarySegmentTriggerTokens = 1000000
	return s
}

// mustSeedSegmentSet writes four small segments: the newest three travel
// with every turn and the oldest one is the stretch a learner goes back to.
//
// Small ones, so the block has room for all of them.
func mustSeedSegmentSet(t *testing.T, db *gorm.DB, userID int64, sessionID string) {
	t.Helper()
	topics := []string{"每天的读量", "周末的计划", "上个月的进度", "最开始的目标"}
	for index, topic := range topics {
		writeSegment(t, db, userID, sessionID, index+1, topic, 1)
	}
}

// mustSeedLargeSegmentSet writes four segments that each fill the
// per-segment budget, so the block that renders them holds three of the four
// and the fourth has to be cut.
//
// The budget the tests measure against is the one the configuration carries,
// so the segments have to be as large as the configuration allows: a small
// segment would fit whatever the budget is and the test would prove
// nothing.
func mustSeedLargeSegmentSet(t *testing.T, db *gorm.DB, userID int64, sessionID string) []model.SessionSummarySegment {
	t.Helper()
	topics := []string{"每天的读量", "周末的计划", "上个月的进度", "最开始的目标"}
	segments := make([]model.SessionSummarySegment, 0, len(topics))
	for index, topic := range topics {
		segments = append(segments, writeSegment(t, db, userID, sessionID, index+1, topic, 12))
	}
	return segments
}

// writeSegment writes one segment with the given number of points.
func writeSegment(t *testing.T, db *gorm.DB, userID int64, sessionID string, seq int,
	topic string, points int) model.SessionSummarySegment {
	t.Helper()
	entries := make([]model.SessionSummarySegmentPoint, 0, points)
	for index := 0; index < points; index++ {
		entries = append(entries, model.SessionSummarySegmentPoint{
			Text:               fmt.Sprintf("%s：第 %d 条记录，每天的读量与上个月的进度都对得上", topic, index+1),
			EvidenceMessageIDs: []uint{uint(seq*10 + index)},
		})
	}
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: 1, Topic: topic, Points: entries,
	})
	if err != nil {
		t.Fatalf("encode segment %d: %v", seq, err)
	}
	segment := model.SessionSummarySegment{
		UserID: userID, SessionID: sessionID, Seq: seq,
		StartMessageID: uint(seq*10 - 9), EndMessageID: uint(seq * 10),
		Content: string(content), IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 300,
	}
	if err := db.Create(&segment).Error; err != nil {
		t.Fatalf("write segment %d: %v", seq, err)
	}
	return segment
}

// segmentMarker is how a segment says which one it is in the window.
func segmentMarker(seq int) string {
	return fmt.Sprintf("[第 %d 段", seq)
}

// segmentBlocks returns the segment blocks the model was sent, oldest first.
//
// Both blocks carry the same header, so they are told apart by their order:
// the resident one is rendered before the retrieved one.
func segmentBlocks(chat *captureModel) []string {
	requests := chat.all()
	if len(requests) == 0 {
		return nil
	}
	var blocks []string
	for _, message := range requests[len(requests)-1].Messages {
		if message == nil {
			continue
		}
		if strings.HasPrefix(message.Content, "Summary segments (compressed record") {
			blocks = append(blocks, message.Content)
		}
	}
	return blocks
}
