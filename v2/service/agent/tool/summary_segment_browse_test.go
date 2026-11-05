package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	"github.com/cloudwego/eino/components/tool"
)

// fakeSegmentBrowser is the store the browse tool reads.
type fakeSegmentBrowser struct {
	segments []model.SessionSummarySegment
	board    *model.SessionWhiteboard
}

func (f *fakeSegmentBrowser) GetSummarySegment(_ context.Context, _ int64, _ string, seq int) (*model.SessionSummarySegment, error) {
	for index := range f.segments {
		if f.segments[index].Seq == seq {
			return &f.segments[index], nil
		}
	}
	return nil, model.ErrSummarySegmentNotFound
}

func (f *fakeSegmentBrowser) ListSummarySegments(_ context.Context, _ int64, _ string, _ int) ([]model.SessionSummarySegment, error) {
	return f.segments, nil
}

func (f *fakeSegmentBrowser) GetWhiteboard(_ context.Context, _ int64, _ string) (*model.SessionWhiteboard, error) {
	if f.board == nil {
		return nil, model.ErrWhiteboardNotFound
	}
	return f.board, nil
}

// browseSegment builds one stored segment.
func browseSegment(t *testing.T, seq int, start, end uint, topic string,
	points []model.SessionSummarySegmentPoint) model.SessionSummarySegment {
	t.Helper()
	content, err := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: topic, Points: points,
	})
	if err != nil {
		t.Fatalf("encode segment content: %v", err)
	}
	return model.SessionSummarySegment{
		UserID: 7, SessionID: "session", Seq: seq, StartMessageID: start, EndMessageID: end,
		Topic: topic, Content: content, TokenEstimate: 120,
	}
}

// browseRun is a run in a session that owns the segments.
func browseRun() serviceagent.RunContext {
	return serviceagent.RunContext{UserID: 7, SessionID: "session", Question: "拆包"}
}

// callBrowse runs the browse tool once and returns what it reported.
func callBrowse(t *testing.T, browse tool.BaseTool, query string) (*BrowseSummarySegmentsOutput, error) {
	t.Helper()
	invokable, ok := browse.(tool.InvokableTool)
	if !ok {
		t.Fatal("the browse tool is not invokable")
	}
	raw, err := invokable.InvokableRun(context.Background(), `{"query":"`+query+`"}`)
	if err != nil {
		return nil, err
	}
	out := &BrowseSummarySegmentsOutput{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return nil, err
	}
	return out, nil
}

// The browse tool finds the segment the question is about and shows it.
func TestBrowseSummarySegmentsFindsTheSegmentTheQuestionIsAbout(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
		}),
		browseSegment(t, 2, 31, 60, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	tools, err := registry.Tools(context.Background(), browseRun())
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("the registry offers %d tools, want 1", len(tools))
	}
	info, err := tools[0].Info(context.Background())
	if err != nil {
		t.Fatalf("tool info: %v", err)
	}
	if info.Name != BrowseSummarySegmentsName {
		t.Fatalf("the tool is named %q", info.Name)
	}
}

// The browse tool grants the ids it showed, so the read-back tool can read
// them.
//
// A segment the model was shown but whose ids the read-back tool refuses
// would be a segment the model can see and not use.
func TestBrowseSummarySegmentsGrantsTheIdsItShowed(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{12}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	run := browseRun()
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	// The tool is exercised through its own call, because the grant set is
	// only extended when the tool actually returns a segment.
	if _, err := callBrowse(t, tools[0], "拆包"); err != nil {
		t.Fatalf("invoke browse: %v", err)
	}
	held := run.GrantSet.Grants(12)
	if len(held) == 0 {
		t.Fatal("the id the browse tool showed is not authorised")
	}
	for _, grant := range held {
		if !grant.UsableIn(run) {
			t.Fatalf("the grant the browse tool made is not usable: %+v", grant)
		}
		if grant.SourceKind != serviceagent.HistorySourceSummarySegment {
			t.Fatalf("the grant has the wrong kind: %+v", grant)
		}
	}
}

// A question about nothing finds nothing, and says so.
func TestBrowseSummarySegmentsReportsNoCandidate(t *testing.T) {
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "RPC 拆包", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	tools, err := registry.Tools(context.Background(), browseRun())
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	out, err := callBrowse(t, tools[0], "今天天气怎么样")
	if err != nil {
		t.Fatalf("invoke browse: %v", err)
	}
	if out.Status != browseStatusNoEvidence {
		t.Fatalf("the status is %q, want %q", out.Status, browseStatusNoEvidence)
	}
	if len(out.Segments) != 0 {
		t.Fatalf("the tool returned %d segments for a question about nothing", len(out.Segments))
	}
}

// The tool's name and description are the contract the model reads, and the
// name is the one the capability mapping knows.
func TestBrowseSummarySegmentsIsAKnownCapability(t *testing.T) {
	if got := serviceagent.CapabilityOf(BrowseSummarySegmentsName); got != serviceagent.CapabilityBrowseSummarySegments {
		t.Fatalf("the tool maps to %q", got)
	}
	name, err := serviceagent.ToolNameFor(serviceagent.CapabilityBrowseSummarySegments, serviceagent.SurfaceLegacy)
	if err != nil {
		t.Fatalf("tool name: %v", err)
	}
	if name != BrowseSummarySegmentsName {
		t.Fatalf("the capability names the tool %q", name)
	}
	if !strings.Contains(browseSummarySegmentsDescription, "segment") {
		t.Fatal("the description does not say what the tool reaches")
	}
}

// One segment is trimmed to its own budget, whatever the rest of the call
// spent.
//
// The per-segment bound is what keeps one long stretch of conversation from
// taking the whole observation. Every part of the view counts against it: the
// topic and the recorded names are text too, and a long topic on its own can
// push a segment over the limit.
func TestBrowseSummarySegmentsTrimsOneSegmentToItsOwnBudget(t *testing.T) {
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	long := strings.Repeat("拆包边界处理", 70)
	browsed, ok := registry.render(browseSegment(t, 1, 1, 30, "拆包边界", []model.SessionSummarySegmentPoint{
		{Text: long},
	}), model.SessionWhiteboardDocument{})
	if !ok {
		t.Fatal("the segment did not render")
	}
	// The recorded names and the board notes are text too, and a long one of
	// them can push a segment over the bound on its own.
	browsed.view.NamesAndNumbers = []string{long}
	browsed.view.WhiteboardStatus = []string{long}
	trimmed := trimBrowseSegment(browsed, maxBrowseSummaryTokens)
	if cost := estimateBrowseTokens(trimmed); cost > maxBrowseSummaryTokens {
		t.Fatalf("the trimmed segment costs %d tokens, the bound is %d", cost, maxBrowseSummaryTokens)
	}
	if len(trimmed.view.Points) != 0 {
		t.Fatalf("the segment still carries %d points", len(trimmed.view.Points))
	}
	if len(trimmed.view.NamesAndNumbers) != 0 || len(trimmed.view.WhiteboardStatus) != 0 {
		t.Fatalf("the trimmed segment still carries %d names and %d notes",
			len(trimmed.view.NamesAndNumbers), len(trimmed.view.WhiteboardStatus))
	}
}

// The budget is the segment's own, not what is left of the call's.
//
// A segment that arrives fifth is trimmed by the same rule as the one that
// arrives first: a budget that subtracted the earlier segments would empty
// the later ones, and the model would see nothing of the stretch it asked
// for.
func TestBrowseSummarySegmentsGivesEverySegmentTheSameBudget(t *testing.T) {
	registry, err := NewBrowseSummarySegmentsRegistry(&fakeSegmentBrowser{})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	text := strings.Repeat("拆包", 90)
	first, ok := registry.render(browseSegment(t, 1, 1, 30, "拆包", []model.SessionSummarySegmentPoint{
		{Text: text}, {Text: text},
	}), model.SessionWhiteboardDocument{})
	if !ok {
		t.Fatal("the first segment did not render")
	}
	second, ok := registry.render(browseSegment(t, 2, 31, 60, "粘包", []model.SessionSummarySegmentPoint{
		{Text: text}, {Text: text},
	}), model.SessionWhiteboardDocument{})
	if !ok {
		t.Fatal("the second segment did not render")
	}
	trimmedFirst := trimBrowseSegment(first, maxBrowseSummaryTokens)
	trimmedSecond := trimBrowseSegment(second, maxBrowseSummaryTokens)
	if len(trimmedFirst.view.Points) != len(trimmedSecond.view.Points) {
		t.Fatalf("the first segment kept %d points and the second %d", len(trimmedFirst.view.Points),
			len(trimmedSecond.view.Points))
	}
	if cost := estimateBrowseTokens(trimmedSecond); cost > maxBrowseSummaryTokens {
		t.Fatalf("the trimmed segment costs %d tokens, the bound is %d", cost, maxBrowseSummaryTokens)
	}
}

// The browse tool grants the ids of the points it showed, and only those.
//
// A point the budget cut is a point the model never saw, and an id it never
// saw is not one it may ask to read back.
func TestBrowseSummarySegmentsDoesNotGrantTheIdsOfCutPoints(t *testing.T) {
	// Three points of about a hundred tokens each: two fit inside the
	// per-segment budget and the third does not.
	long := strings.Repeat("拆包边界处理", 18)
	browser := &fakeSegmentBrowser{segments: []model.SessionSummarySegment{
		browseSegment(t, 1, 1, 30, "拆包", []model.SessionSummarySegmentPoint{
			{Text: long, EvidenceMessageIDs: []uint{12}},
			{Text: long, EvidenceMessageIDs: []uint{13}},
			{Text: long, EvidenceMessageIDs: []uint{14}},
		}),
	}}
	registry, err := NewBrowseSummarySegmentsRegistry(browser)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	run := browseRun()
	run.GrantSet = serviceagent.NewHistoryGrantSet(run, nil)
	tools, err := registry.Tools(context.Background(), run)
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	out, err := callBrowse(t, tools[0], "拆包")
	if err != nil {
		t.Fatalf("invoke browse: %v", err)
	}
	if len(out.Segments) != 1 {
		t.Fatalf("the tool returned %d segments, want 1", len(out.Segments))
	}
	if !out.Truncated {
		t.Fatal("the segment was cut but the tool did not say so")
	}
	if len(out.Segments[0].Points) >= 3 {
		t.Fatalf("the segment still carries %d points, the budget did not cut it", len(out.Segments[0].Points))
	}
	if len(run.GrantSet.Grants(14)) != 0 {
		t.Fatal("the id of a cut point was authorised")
	}
	if len(run.GrantSet.Grants(12)) == 0 {
		t.Fatal("the id of a point that was shown was not authorised")
	}
}
