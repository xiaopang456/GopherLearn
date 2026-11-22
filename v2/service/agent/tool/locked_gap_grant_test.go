package agenttool

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
)

// The words the locked block shows are readable back in the same turn; ids it
// does not show are not, even inside the locked conversation.

func lockedGapRun(grants ...serviceagent.HistoryMessageGrant) serviceagent.RunContext {
	run := historyRun(grants...)
	run.LockedSessionID = "p2c-13"
	return run
}

func newLockedHistoryTools(t *testing.T, reader HistoryMessageReader, run serviceagent.RunContext) []tool.BaseTool {
	t.Helper()
	registry, err := NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseAssistantContent(true)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func lockedGapGrant(id, segmentEnd uint) serviceagent.HistoryMessageGrant {
	return serviceagent.HistoryMessageGrant{
		MessageID: id, SourceKind: serviceagent.HistorySourceLockedSession,
		SourceSessionID: "p2c-13", SegmentEndMessageID: segmentEnd,
	}
}

func TestALockedGapWordShownInTheBlockIsReadableBack(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		1662: {ID: 1662, SessionID: "p2c-13", IsUser: false,
			Content: "慢查询清单和事务记录能看出哪些语句被包在同一个事务里跑得久。"},
	}}
	run := lockedGapRun(lockedGapGrant(1662, 1696))
	tools := newLockedHistoryTools(t, reader, run)
	out := invokeRead(t, tools, 1662)
	if len(out.Messages) != 1 || out.Messages[0].MessageID != 1662 {
		t.Fatalf("the shown gap word was not readable: %+v", out)
	}
}

func TestALockedIdTheBlockDoesNotShowIsRefused(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		1662: {ID: 1662, SessionID: "p2c-13", Content: "shown"},
		1700: {ID: 1700, SessionID: "p2c-13", Content: "not shown"},
	}}
	run := lockedGapRun(lockedGapGrant(1662, 1696))
	tools := newLockedHistoryTools(t, reader, run)
	out := invokeRead(t, tools, 1700)
	if len(out.Messages) != 0 || len(out.Refused) == 0 || out.Refused[0] != 1700 {
		t.Fatalf("an id the block never showed was readable: %+v", out)
	}
}

func TestALockedIdAboveItsGrantWatermarkIsRefused(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		1662: {ID: 1662, SessionID: "p2c-13", Content: "shown"},
	}}
	run := lockedGapRun(lockedGapGrant(1662, 1600))
	tools := newLockedHistoryTools(t, reader, run)
	if len(tools) == 0 {
		return // the grant was filtered: the id is not authorised at all
	}
	out := invokeRead(t, tools, 1662)
	if len(out.Messages) != 0 || len(out.Refused) == 0 || out.Refused[0] != 1662 {
		t.Fatalf("a grant above its watermark was accepted: %+v", out)
	}
}

func TestALockedGrantForAnotherConversationIsRefused(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		3141: {ID: 3141, SessionID: "p2c-99", Content: "outside"},
	}}
	grant := serviceagent.HistoryMessageGrant{
		MessageID: 3141, SourceKind: serviceagent.HistorySourceLockedSession,
		SourceSessionID: "p2c-99", SegmentEndMessageID: 3200,
	}
	run := lockedGapRun(grant)
	tools := newLockedHistoryTools(t, reader, run)
	if len(tools) == 0 {
		return
	}
	out := invokeRead(t, tools, 3141)
	if len(out.Messages) != 0 || len(out.Refused) == 0 || out.Refused[0] != 3141 {
		t.Fatalf("an id outside the lock was readable: %+v", out)
	}
}

// Before this fix the block showed the words but issued no grant: the same id
// could not be read back. A turn with no locked grant still has no read tool.
func TestWithoutTheLockedGrantTheShownIdIsRefused(t *testing.T) {
	reader := &fakeHistoryReader{rows: map[uint]model.Message{
		1662: {ID: 1662, SessionID: "p2c-13", Content: "shown"},
	}}
	run := lockedGapRun() // no grant
	tools := newLockedHistoryTools(t, reader, run)
	if len(tools) != 0 {
		t.Fatalf("the read tool was offered without a grant (%d tools)", len(tools))
	}
}
