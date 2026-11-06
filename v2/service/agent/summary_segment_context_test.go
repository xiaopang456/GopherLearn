package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The layered summary's read side.
//
// The tests here are the ones the plan names: the old value is marked and
// kept, the blocks travel in the order the plan puts them in, the raw
// conversation starts where the segments stop, and the fallback is recorded.

// fakeSegmentReader is the store the read side reads.
type fakeSegmentReader struct {
	board     *model.SessionWhiteboard
	segments  []model.SessionSummarySegment
	watermark uint
	boardErr  error
	// segmentErr fails the read of the segment list.
	segmentErr error
	// onSegmentRead runs before each read of the segment list, so a test can
	// make the store change under the reader.
	onSegmentRead func(read int)
	// segmentReads counts the reads of the segment list.
	segmentReads int
}

func (f *fakeSegmentReader) GetWhiteboard(_ context.Context, _ int64, _ string) (*model.SessionWhiteboard, error) {
	if f.boardErr != nil {
		return nil, f.boardErr
	}
	return f.board, nil
}

func (f *fakeSegmentReader) ListLatestSummarySegments(_ context.Context, _ int64, _ string, limit int) ([]model.SessionSummarySegment, error) {
	f.segmentReads++
	if f.onSegmentRead != nil {
		f.onSegmentRead(f.segmentReads)
	}
	if f.segmentErr != nil {
		return nil, f.segmentErr
	}
	if limit <= 0 || limit > len(f.segments) {
		limit = len(f.segments)
	}
	out := make([]model.SessionSummarySegment, 0, limit)
	for index := len(f.segments) - 1; index >= 0 && len(out) < limit; index-- {
		out = append(out, f.segments[index])
	}
	return out, nil
}

func (f *fakeSegmentReader) ListSummarySegments(_ context.Context, _ int64, _ string, _ int) ([]model.SessionSummarySegment, error) {
	return f.segments, nil
}

func (f *fakeSegmentReader) GetSummarySegment(_ context.Context, _ int64, _ string, seq int) (*model.SessionSummarySegment, error) {
	for index := range f.segments {
		if f.segments[index].Seq == seq {
			return &f.segments[index], nil
		}
	}
	return nil, model.ErrSummarySegmentNotFound
}

func (f *fakeSegmentReader) GetSummarySegmentWatermark(_ context.Context, _ int64, _ string) (uint, int, error) {
	return f.watermark, len(f.segments), nil
}

// testSegment builds one stored segment.
func testSegment(t *testing.T, seq int, start, end uint, topic string, points []model.SessionSummarySegmentPoint,
	changes []model.SessionSummarySegmentChange) model.SessionSummarySegment {
	t.Helper()
	content, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: topic, Points: points,
	})
	if err != nil {
		t.Fatalf("encode segment content: %v", err)
	}
	encoded, err := model.MarshalSessionSummarySegmentChanges(changes)
	if err != nil {
		t.Fatalf("encode segment changes: %v", err)
	}
	return model.SessionSummarySegment{
		UserID: 7, SessionID: "session", Seq: seq, StartMessageID: start, EndMessageID: end,
		StartAt: time.Unix(1790000000, 0).UTC(), EndAt: time.Unix(1790003600, 0).UTC(),
		Topic: topic, Content: content, WhiteboardChanges: encoded, TokenEstimate: 120,
	}
}

// testBoard builds one stored board.
func testBoard(t *testing.T, covered uint, entries []model.SessionWhiteboardEntry) *model.SessionWhiteboard {
	t.Helper()
	document := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1, Constraints: entries,
	}
	encoded, err := model.MarshalSessionWhiteboardDocument(document)
	if err != nil {
		t.Fatalf("encode board: %v", err)
	}
	return &model.SessionWhiteboard{
		UserID: 7, SessionID: "session", Content: encoded,
		CoveredMessageID: covered, TokenEstimate: 60, Version: 3,
	}
}

// layeredTestConfig is the config the layered summary's blocks need.
//
// The window is bigger than the one the other context tests use, because the
// board and the segments have fixed budgets of their own and the fixed
// budgets have to leave room for the raw conversation.
func layeredTestConfig() ContextConfig {
	config := testContextConfig()
	config.ModelWindowTokens = 16384
	config.InputTargetTokens = 8192
	config.MaxWhiteboardTokens = DefaultWhiteboardTokens
	config.MaxResidentSegmentTokens = DefaultResidentSegmentTokens
	config.MaxRetrievedSegmentTokens = DefaultRetrievedSegmentTokens
	config.MaxSegmentTokens = DefaultSegmentTokens
	return config
}

// buildLayeredWindow assembles a window with the layered summary attached.
func buildLayeredWindow(t *testing.T, reader *fakeSegmentReader, history *fakeHistoryReader,
	fallback SummarySegmentFallbackStarter, preselect *SummarySegmentPreselector) ContextWindow {
	t.Helper()
	builder, err := NewContextBuilder(layeredTestConfig(), history, staticTaskState(""), nil,
		staticMemories{}, EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build context builder: %v", err)
	}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, preselect, DefaultResidentSummarySegments)
	if err != nil {
		t.Fatalf("build layered provider: %v", err)
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		t.Fatalf("attach layered provider: %v", err)
	}
	if fallback != nil {
		builder.UseSummarySegmentFallback(fallback)
	}
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "我一开始定的学习时长是多少",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}
	return window
}

// The board travels in front of the raw conversation and the segments this
// question selected travel behind it.
//
// The order is the cache order: a block that changes on every turn sits
// behind the history, where the history can still be a cache hit.
func TestTheLayeredSummaryBlocksTravelAroundTheRawConversation(t *testing.T) {
	reader := &fakeSegmentReader{
		board: testBoard(t, 60, []model.SessionWhiteboardEntry{
			{Key: "duration", Text: "每次总时长 = 48 分钟", Value: "48 分钟", SourceMessageIDs: []uint{45}},
		}),
		watermark: 90,
		segments: []model.SessionSummarySegment{
			testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
				{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
			}, nil),
			testSegment(t, 2, 31, 60, "RPC 拆包", []model.SessionSummarySegmentPoint{
				{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
			}, nil),
			testSegment(t, 3, 61, 90, "学习时长调整", []model.SessionSummarySegmentPoint{
				{Text: "总时长改为 48 分钟", EvidenceMessageIDs: []uint{75}},
			}, []model.SessionSummarySegmentChange{{Key: "duration", OldValue: "30 分钟", NewValue: "48 分钟", NewMessageIDs: []uint{75}}}),
		},
	}
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 91, IsUser: true, Content: "刚才那个时长"},
	}}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	window := buildLayeredWindow(t, reader, history, nil, preselector)

	roles := make([]string, 0, len(window.Messages))
	for _, message := range window.Messages {
		roles = append(roles, string(message.Role))
	}
	if len(roles) < 4 {
		t.Fatalf("the window has %d messages: %v", len(roles), roles)
	}
	boardAt, residentAt, historyAt, retrievedAt := -1, -1, -1, -1
	for index, message := range window.Messages {
		switch {
		case strings.Contains(message.Content, "Current state (server state;"):
			boardAt = index
		case strings.Contains(message.Content, "Summary segments (compressed record"):
			if residentAt < 0 {
				residentAt = index
			} else {
				retrievedAt = index
			}
		case strings.Contains(message.Content, "刚才那个时长"):
			historyAt = index
		}
	}
	if boardAt < 0 || residentAt < 0 || retrievedAt < 0 || historyAt < 0 {
		t.Fatalf("a block is missing: board=%d resident=%d retrieved=%d question=%d", boardAt, residentAt, retrievedAt, historyAt)
	}
	if !(boardAt < residentAt && residentAt < historyAt && historyAt < retrievedAt) {
		t.Fatalf("the blocks are out of order: board=%d resident=%d question=%d retrieved=%d",
			boardAt, residentAt, historyAt, retrievedAt)
	}
	if window.Budget.Whiteboard == 0 || window.Budget.ResidentSegments == 0 {
		t.Fatalf("the board and the resident segments were not charged: %+v", window.Budget)
	}
}

// The board travels with every turn: it is a fixed cost, and it is what
// makes "what is true now" answerable without a retrieval.
func TestTheBoardTravelsWithEveryTurn(t *testing.T) {
	reader := &fakeSegmentReader{
		board:     testBoard(t, 60, []model.SessionWhiteboardEntry{{Key: "duration", Text: "48 分钟", Value: "48 分钟"}}),
		watermark: 60,
	}
	window := buildLayeredWindow(t, reader, &fakeHistoryReader{}, nil, nil)
	found := false
	for _, message := range window.Messages {
		if strings.Contains(message.Content, "48 分钟") {
			found = true
		}
	}
	if !found {
		t.Fatal("the board did not reach the window")
	}
}

// A value the board has since replaced is marked, and the old value stays.
//
// The marker is what stops the model from quoting a superseded value as the
// current one; keeping the old value is what lets "what did I set at the
// start" still be answered.
func TestASupersededValueIsMarkedAndTheOldValueStays(t *testing.T) {
	reader := &fakeSegmentReader{
		board: testBoard(t, 90, []model.SessionWhiteboardEntry{
			{Key: "duration", Text: "每次总时长 = 60 分钟", Value: "60 分钟", UpdatedAtMessageID: 90},
		}),
		watermark: 90,
		segments: []model.SessionSummarySegment{
			testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
				{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
			}, []model.SessionSummarySegmentChange{{Key: "duration", OldValue: "30 分钟", NewValue: "30 分钟", NewMessageIDs: []uint{12}}}),
			testSegment(t, 2, 31, 60, "学习时长调整", []model.SessionSummarySegmentPoint{
				{Text: "总时长改为 60 分钟", EvidenceMessageIDs: []uint{45}},
			}, []model.SessionSummarySegmentChange{{Key: "duration", OldValue: "30 分钟", NewValue: "60 分钟", NewMessageIDs: []uint{45}}}),
		},
	}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	window := buildLayeredWindow(t, reader, &fakeHistoryReader{}, nil, preselector)

	body := ""
	for _, message := range window.Messages {
		body += message.Content + "\n"
	}
	if !strings.Contains(body, "30 分钟") {
		t.Fatal("the old value was dropped from the window")
	}
	if !strings.Contains(body, "已改为 60 分钟") {
		t.Fatalf("the superseded value was not marked: %s", body)
	}
}

// The raw conversation starts where the segments stop.
//
// The segments are the compression in this mode, so the messages they cover
// must not be sent again as raw text.
func TestTheRawConversationStartsWhereTheSegmentsStop(t *testing.T) {
	// The segment that reaches the watermark is what carries the coverage,
	// so the store has to hold it: a watermark with no segment behind it is
	// a state the store cannot be in.
	reader := &fakeSegmentReader{
		segments:  []model.SessionSummarySegment{testSegment(t, 1, 0, 60, "话题", nil, nil)},
		watermark: 60,
	}
	history := &fakeHistoryReader{messages: []model.Message{{ID: 61, IsUser: true, Content: "还在的原文"}}}
	buildLayeredWindow(t, reader, history, nil, nil)
	if history.afterID != 60 {
		t.Fatalf("the raw conversation starts after %d, want 60", history.afterID)
	}
}

// A watermark the segments do not carry is not believed.
//
// The watermark says how far the summary reaches, and the raw messages below
// it are dropped from the window. A watermark without the segment that
// reaches it would drop a stretch of the conversation without putting
// anything in its place, so nothing is claimed as covered and the raw
// conversation is kept.
func TestAWatermarkTheSegmentsDoNotCarryIsNotBelieved(t *testing.T) {
	reader := &fakeSegmentReader{watermark: 60}
	history := &fakeHistoryReader{messages: []model.Message{{ID: 61, IsUser: true, Content: "还在的原文"}}}
	buildLayeredWindow(t, reader, history, nil, nil)
	if history.afterID != 0 {
		t.Fatalf("the raw conversation starts after %d, want nothing covered", history.afterID)
	}
}

// The fallback is recorded on the window.
//
// The evaluation reads it to count how often the background did not keep up,
// and a fallback that is not recorded is a fallback nobody can count.
func TestTheFallbackIsRecordedOnTheWindow(t *testing.T) {
	reader := &fakeSegmentReader{watermark: 0}
	history := &fakeHistoryReader{messages: []model.Message{{ID: 1, IsUser: true, Content: "一段尚未被压缩的原文"}}}
	window := buildLayeredWindow(t, reader, history, &stubFallback{record: SummarySegmentFallbackRecord{
		FellBack: true, Reason: "generated_before_answer", Succeeded: true,
	}}, nil)
	if !window.Budget.SummarySyncFallback {
		t.Fatal("the window does not say the turn fell back")
	}
	if window.Budget.SummarySyncFallbackReason != "generated_before_answer" {
		t.Fatalf("the fallback reason is %q", window.Budget.SummarySyncFallbackReason)
	}
}

// A decision that did not fall back is recorded too: an unrecorded decision
// is one nobody can count.
func TestAFallbackThatDidNotRunIsStillRecorded(t *testing.T) {
	reader := &fakeSegmentReader{watermark: 0}
	history := &fakeHistoryReader{messages: []model.Message{{ID: 1, IsUser: true, Content: "原文"}}}
	window := buildLayeredWindow(t, reader, history, &stubFallback{record: SummarySegmentFallbackRecord{
		Reason: "not_needed",
	}}, nil)
	if window.Budget.SummarySyncFallback {
		t.Fatal("the window claims a fallback that did not happen")
	}
	if window.Budget.SummarySyncFallbackReason != "not_needed" {
		t.Fatalf("the fallback reason is %q", window.Budget.SummarySyncFallbackReason)
	}
}

// stubFallback records the call and answers with a fixed decision.
type stubFallback struct {
	record SummarySegmentFallbackRecord
	calls  int
}

func (s *stubFallback) StartSummarySegmentFallback(_ context.Context, _ int64, _ string, _ uint) (SummarySegmentFallbackRecord, error) {
	s.calls++
	return s.record, nil
}

// The board and the segments authorise reading the learner's own words back,
// and the authorisation is bounded by what the segment covers.
func TestTheLayeredSummaryGrantsAreBoundedByTheSegment(t *testing.T) {
	segment := testSegment(t, 1, 1, 30, "话题", []model.SessionSummarySegmentPoint{
		{Text: "学习者定的是 30 分钟", EvidenceMessageIDs: []uint{12, 13}},
	}, nil)
	reader := &fakeSegmentReader{watermark: 30, segments: []model.SessionSummarySegment{segment}}
	window := buildLayeredWindow(t, reader, &fakeHistoryReader{}, nil, nil)
	if len(window.SummarySegmentHistoryGrants) == 0 {
		t.Fatal("the segment granted nothing")
	}
	run := RunContext{UserID: 7, SessionID: "session"}
	for _, grant := range window.SummarySegmentHistoryGrants {
		if !grant.UsableIn(run) {
			t.Fatalf("a grant the window made is not usable: %+v", grant)
		}
		if grant.MessageID > segment.EndMessageID {
			t.Fatalf("a grant above the segment's end was made: %+v", grant)
		}
	}
	// A grant above the segment's end is refused even when the builder made it.
	outside := HistoryMessageGrant{
		MessageID: 99, SourceKind: HistorySourceSummarySegment, SourceSessionID: "session",
		SegmentSeq: 1, SegmentEndMessageID: segment.EndMessageID,
	}
	if outside.UsableIn(run) {
		t.Fatal("a grant above the segment's end was accepted")
	}
	// A grant from another session is refused too.
	foreign := outside
	foreign.SourceSessionID = "other"
	foreign.MessageID = 12
	if foreign.UsableIn(run) {
		t.Fatal("a grant from another session was accepted")
	}
}

// The preselector picks the segment the question is about, and leaves the
// others alone.
func TestThePreselectorPicksTheSegmentTheQuestionIsAbout(t *testing.T) {
	segments := []model.SessionSummarySegment{
		testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
		}, nil),
		testSegment(t, 2, 31, 60, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	selected, err := preselector.Select(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包是怎么决定的"},
		segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 1 {
		t.Fatalf("the preselector chose %d segments, want 1", len(selected))
	}
	if selected[0].Seq != 2 {
		t.Fatalf("the preselector chose segment %d, want 2", selected[0].Seq)
	}
	none, err := preselector.Select(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "今天天气怎么样"},
		segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("the preselector chose %d segments for a question about nothing", len(none))
	}
}

// The best match wins, not the earliest one.
//
// The whole point of ranking the candidates is that the segment the question
// is about wins, whichever stretch of the conversation it sits in.
func TestThePreselectorRanksByTheMatchAndNotByPosition(t *testing.T) {
	segments := []model.SessionSummarySegment{
		testSegment(t, 1, 1, 30, "其他话题", []model.SessionSummarySegmentPoint{
			{Text: "提到了拆包一次", EvidenceMessageIDs: []uint{12}},
		}, nil),
		testSegment(t, 2, 31, 60, "拆包", []model.SessionSummarySegmentPoint{
			{Text: "拆包用长度字段，粘包和半包都要处理", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	selected, err := preselector.Select(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包和粘包是怎么处理的"},
		segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 2 {
		t.Fatalf("the preselector chose %d segments, want 2", len(selected))
	}
	if selected[0].Seq != 2 {
		t.Fatalf("the preselector put segment %d first, want 2", selected[0].Seq)
	}
}

// The resident segments are not preselected a second time: they travel with
// every turn anyway.
func TestThePreselectorLeavesTheResidentSegmentsAlone(t *testing.T) {
	segments := []model.SessionSummarySegment{
		testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
		}, nil),
	}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	selected, err := preselector.Select(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "学习时长"},
		segments, segments)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 0 {
		t.Fatalf("the preselector chose a resident segment again: %d", len(selected))
	}
}

// The preselector spends its budget on what the window sends, not on the body
// the segment is stored in.
//
// The stored body is JSON: braces, quotes, field names and evidence ids. On a
// real segment it runs to roughly three times the block the model reads, so a
// budget spent on the body rejected segments that fitted - and the segment
// carrying the answer was among them.
func TestThePreselectorSpendsItsBudgetOnWhatTheWindowSends(t *testing.T) {
	points := make([]model.SessionSummarySegmentPoint, 0, 12)
	for index := 0; index < 12; index++ {
		points = append(points, model.SessionSummarySegmentPoint{
			Text: "梧苑每次读十五页，后来改成三十页", EvidenceMessageIDs: []uint{21, 22, 23},
		})
	}
	segment := testSegment(t, 1, 1, 30, "梧苑读书计划的页数", points, nil)
	stored := rag.EstimateTokens(segment.Content)
	if stored <= DefaultPreselectedSegmentTokens {
		t.Fatalf("the stored body costs %d tokens; the fixture does not exceed the budget", stored)
	}
	if rendered := RenderedSegmentTokens(segment); rendered > DefaultSegmentTokens+segmentSeparatorTokens {
		t.Fatalf("the block costs %d tokens, over the budget of %d", rendered, DefaultSegmentTokens+segmentSeparatorTokens)
	}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	selected, err := preselector.Select(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "梧苑我每次读多少页？"},
		[]model.SessionSummarySegment{segment}, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 1 || selected[0].Seq != 1 {
		t.Fatalf("the segment the block fits was not selected: %+v", selected)
	}
}
