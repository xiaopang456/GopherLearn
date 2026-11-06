package isolated

// The read-back authorization, measured at the tool.
//
// Everything else in this repository that checks authorization checks the
// grant set: given these grants, is this id readable. That is the decision
// the tool consults, not the tool itself. A test that stops there cannot say
// whether asking for an id nobody showed gets refused, because nothing was
// asked.
//
// So the two tools are built through their own registries and run for real,
// the way the model runs them: one call with a batch of ids, then whatever
// the tool answers. The store behind them is a stand-in, because what is
// under test is the boundary, not the query.

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"

	"GopherAI/model"
	agentService "GopherAI/service/agent"
	agentTool "GopherAI/service/agent/tool"
)

// authorityReader is a message store that belongs to one user.
//
// It answers only for its owner, which is what a store scoped to the caller
// does: an id belonging to somebody else is a miss, not a permission error,
// and the tool must treat it as unreadable rather than as an accident.
type authorityReader struct {
	owner    int64
	messages map[uint]model.Message
}

func (r authorityReader) ReadOwnedMessages(_ context.Context, userID int64, ids []uint) ([]model.Message, error) {
	if userID != r.owner {
		return nil, nil
	}
	out := make([]model.Message, 0, len(ids))
	for _, id := range ids {
		if message, ok := r.messages[id]; ok {
			out = append(out, message)
		}
	}
	return out, nil
}

func (r authorityReader) ReadOwnedMessagesBySession(_ context.Context, userID int64,
	idsBySession map[string][]uint) ([]model.Message, error) {
	if userID != r.owner {
		return nil, nil
	}
	var out []model.Message
	for _, ids := range idsBySession {
		for _, id := range ids {
			if message, ok := r.messages[id]; ok {
				out = append(out, message)
			}
		}
	}
	return out, nil
}

// authorityBrowser is a segment store that belongs to one session.
//
// It answers only for segments it owns. That is what an owner-scoped query
// does: a segment of another session is a miss rather than a row the caller
// has to remember to filter.
type authorityBrowser struct {
	owner    int64
	session  string
	segments map[int]model.SessionSummarySegment
}

func (b authorityBrowser) owned(segment model.SessionSummarySegment, userID int64, sessionID string) bool {
	return segment.UserID == userID && segment.SessionID == sessionID && userID == b.owner && sessionID == b.session
}

func (b authorityBrowser) GetSummarySegment(_ context.Context, userID int64, sessionID string, seq int) (*model.SessionSummarySegment, error) {
	segment, ok := b.segments[seq]
	if !ok || !b.owned(segment, userID, sessionID) {
		return nil, model.ErrSummarySegmentNotFound
	}
	return &segment, nil
}

func (b authorityBrowser) ListSummarySegments(_ context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	out := make([]model.SessionSummarySegment, 0, len(b.segments))
	for seq := 1; seq <= len(b.segments); seq++ {
		segment, ok := b.segments[seq]
		if ok && b.owned(segment, userID, sessionID) {
			out = append(out, segment)
		}
	}
	return out, nil
}

func (b authorityBrowser) GetWhiteboard(_ context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	return nil, model.ErrWhiteboardNotFound
}

// segmentVerifier is the store-side check the read-back tool makes before it
// honours a segment citation.
//
// It is not optional in a deployment: without one the tool refuses every
// segment grant, because "the segment still says what the grant claims" is
// the fact a read of the learner's words has to be able to re-establish.
type segmentVerifier struct {
	browser authorityBrowser
}

func (v segmentVerifier) LoadSummarySegmentEvidence(_ context.Context, userID int64,
	sessionID string, seq int) (agentTool.SummarySegmentEvidenceFacts, error) {
	segment, err := v.browser.GetSummarySegment(context.Background(), userID, sessionID, seq)
	if err != nil {
		return agentTool.SummarySegmentEvidenceFacts{}, err
	}
	return agentTool.SummarySegmentEvidenceFacts{
		Seq: segment.Seq, UserID: segment.UserID, SessionID: segment.SessionID,
		StartMessageID: segment.StartMessageID, EndMessageID: segment.EndMessageID,
	}, nil
}

// segmentGrant is the grant shape the layered read side draws: a segment's
// stretch of turns, cited by the points that reached the window.
func segmentGrant(messageID uint, session string, seq int, start, end uint) agentService.HistoryMessageGrant {
	return agentService.HistoryMessageGrant{
		MessageID: messageID, SourceKind: agentService.HistorySourceSummarySegment,
		SourceSessionID: session, SegmentSeq: seq,
		SegmentStartMessageID: start, SegmentEndMessageID: end,
	}
}

// historyTool builds the read-back tool for one run.
//
// The segment verifier is wired because a deployment wires it: the tool
// refuses every segment citation without one, and a test that left it out
// would be measuring the "not wired" branch instead of the boundary.
func historyTool(t *testing.T, run agentService.RunContext, reader authorityReader,
	browser authorityBrowser) tool.InvokableTool {
	t.Helper()
	built, err := historyRegistry(t, reader, browser).Tools(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	readBack := readBackTool(t, built)
	if readBack == nil {
		t.Fatal("the history registry offered no read-back tool")
	}
	return readBack
}

func historyRegistry(t *testing.T, reader authorityReader, browser authorityBrowser) *agentTool.HistoryMessageRegistry {
	t.Helper()
	registry, err := agentTool.NewHistoryMessageRegistry(reader)
	if err != nil {
		t.Fatal(err)
	}
	registry.UseSummarySegmentEvidenceReader(segmentVerifier{browser: browser})
	return registry
}

// readBackTool returns the read-back tool from a registry's output, or nil
// when the registry offered none.
//
// A registry offers the tool only when the run holds a grant to use it with,
// so "no tool" is the product saying this turn has nothing to read back. That
// is a different refusal from a tool that answers and refuses an id, and a
// test has to be able to tell them apart.
func readBackTool(t *testing.T, built []tool.BaseTool) tool.InvokableTool {
	t.Helper()
	for _, item := range built {
		invokable, ok := item.(tool.InvokableTool)
		if !ok {
			continue
		}
		info, infoErr := invokable.Info(context.Background())
		if infoErr != nil {
			t.Fatal(infoErr)
		}
		if info != nil && info.Name == "read_history_message" {
			return invokable
		}
	}
	return nil
}

func TestTheReadBackToolRefusesIdsTheWindowNeverShowed(t *testing.T) {
	ctx := context.Background()
	reader := authorityReader{owner: 7, messages: map[uint]model.Message{
		11: {ID: 11, SessionID: "s1", UserName: "learner", IsUser: true, Content: "我最初定的是每周读 15 页"},
		12: {ID: 12, SessionID: "s1", UserName: "learner", IsUser: true, Content: "后来改成每次读 30 页"},
		13: {ID: 13, SessionID: "s1", UserName: "learner", IsUser: true, Content: "这句话从来没进过窗口"},
	}}
	run := agentService.RunContext{UserID: 7, SessionID: "s1", Question: "我最初每次读多少页"}
	run.GrantSet = agentService.NewHistoryGrantSet(run, []agentService.HistoryMessageGrant{
		segmentGrant(11, "s1", 1, 1, 12),
		segmentGrant(12, "s1", 1, 1, 12),
	})
	readBack := historyTool(t, run, reader, authorityBrowser{owner: 7, session: "s1",
		segments: map[int]model.SessionSummarySegment{
			1: {UserID: 7, SessionID: "s1", Seq: 1, StartMessageID: 1, EndMessageID: 12},
		}})

	raw, err := readBack.InvokableRun(ctx, `{"message_ids":[11,13]}`)
	if err != nil {
		t.Fatalf("the tool failed instead of refusing the id: %v", err)
	}
	if !strings.Contains(raw, "每周读 15 页") {
		t.Fatalf("the grant the window showed was not readable: %s", raw)
	}
	if strings.Contains(raw, "从来没进过窗口") {
		t.Fatalf("the tool returned a message the window never showed: %s", raw)
	}
	if !strings.Contains(raw, "13") {
		t.Fatalf("the refusal was silent, so the model would retry the id: %s", raw)
	}
}

func TestTheReadBackToolRefusesAGrantFromAnotherSessionOrUser(t *testing.T) {
	ctx := context.Background()
	reader := authorityReader{owner: 7, messages: map[uint]model.Message{
		21: {ID: 21, SessionID: "s1", UserName: "learner", IsUser: true, Content: "这个会话里的话"},
	}}
	// Two refusals that look the same from the answer and happen in different
	// places. The grant of another session is dropped before the store is
	// reached; the grant for another user's row survives the grant set because
	// nothing in it says whose row it is, and the store is what refuses. A test
	// that only checked "not readable" would not know which of the two is
	// holding, and the second one is the boundary a future change is most
	// likely to move.
	for _, c := range []struct {
		name           string
		grant          agentService.HistoryMessageGrant
		owner          int64
		wantGranted    bool
		wantGrantLevel bool
	}{
		{
			name:  "the grant was drawn for another session",
			grant: segmentGrant(21, "s2", 1, 1, 30),
			owner: 7, wantGranted: false, wantGrantLevel: true,
		},
		{
			name:  "the message belongs to another user",
			grant: segmentGrant(21, "s1", 1, 1, 30),
			owner: 8, wantGranted: true, wantGrantLevel: false,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := agentService.RunContext{UserID: 7, SessionID: "s1", Question: "这个会话说了什么"}
			run.GrantSet = agentService.NewHistoryGrantSet(run, []agentService.HistoryMessageGrant{c.grant})
			if got := run.GrantSet.Covers(21, "s1"); got != c.wantGranted {
				t.Fatalf("the grant set covers the id = %t, want %t", got, c.wantGranted)
			}
			// The reader answers for its own owner. A run whose grants point
			// at a store that does not belong to it has to come back empty.
			browser := authorityBrowser{owner: 7, session: "s1", segments: map[int]model.SessionSummarySegment{
				1: {UserID: 7, SessionID: "s1", Seq: 1, StartMessageID: 1, EndMessageID: 30},
			}}
			built, err := historyRegistry(t, authorityReader{owner: c.owner, messages: reader.messages},
				browser).Tools(ctx, run)
			if err != nil {
				t.Fatal(err)
			}
			readBack := readBackTool(t, built)
			if !c.wantGranted {
				// Nothing to read back, so the product does not offer the tool
				// at all. That is a stronger refusal than an empty answer: the
				// model cannot ask for an id it was never given a way to ask
				// about.
				if readBack != nil {
					t.Fatal("a run whose only grant belongs to another session was still offered the read-back tool")
				}
				return
			}
			if readBack == nil {
				t.Fatal("the read-back tool was not offered for a grant this run holds")
			}
			raw, err := readBack.InvokableRun(ctx, `{"message_ids":[21]}`)
			if err != nil {
				t.Fatalf("the tool failed instead of refusing: %v", err)
			}
			if strings.Contains(raw, "这个会话里的话") {
				t.Fatalf("a message outside this run was readable: %s", raw)
			}
		})
	}
}

func TestTheBrowseToolOpensOnlyThisSessionsSegments(t *testing.T) {
	ctx := context.Background()
	browser := authorityBrowser{owner: 7, session: "s1", segments: map[int]model.SessionSummarySegment{
		1: {UserID: 7, SessionID: "s1", Seq: 1, StartMessageID: 1, EndMessageID: 12,
			Topic: "阅读计划", Content: `{"schema_version":1,"topic":"阅读计划","points":[{"text":"每次读 30 页","evidence_message_ids":[11]}]}`},
		2: {UserID: 8, SessionID: "s2", Seq: 2, StartMessageID: 13, EndMessageID: 30,
			Topic: "别人的会话", Content: `{"schema_version":1,"topic":"别人的会话","points":[{"text":"不该被看到","evidence_message_ids":[21]}]}`},
	}}
	registry, err := agentTool.NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatal(err)
	}
	run := agentService.RunContext{UserID: 7, SessionID: "s1", Question: "我的阅读计划是什么"}
	built, err := registry.Tools(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	var browse tool.InvokableTool
	for _, item := range built {
		if invokable, ok := item.(tool.InvokableTool); ok {
			browse = invokable
		}
	}
	if browse == nil {
		t.Fatal("the browse registry offered no tool")
	}

	raw, err := browse.InvokableRun(ctx, `{"query":"阅读计划","segments":[1]}`)
	if err != nil {
		t.Fatalf("browse failed on this session's own segment: %v", err)
	}
	if !strings.Contains(raw, "每次读 30 页") {
		t.Fatalf("the segment this session owns was not readable: %s", raw)
	}

	other, err := browse.InvokableRun(ctx, `{"query":"别人的会话","segments":[2]}`)
	if err != nil {
		t.Fatalf("browse failed instead of refusing another session's segment: %v", err)
	}
	if strings.Contains(other, "不该被看到") {
		t.Fatalf("another session's segment was readable: %s", other)
	}
}
