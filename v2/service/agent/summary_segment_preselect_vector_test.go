package agent

import (
	"context"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/model"
)

// The vector channel of the preselection.
//
// A segment is a piece of this session's own conversation, and a wrong one is
// merely useless. That is why the threshold is looser here than it is for the
// episode cards: a lexical match is enough on its own, and a vector hit has
// to be both near the top and above a score floor.

// fakeVectorSearcher is the index the vector channel reads.
type fakeVectorSearcher struct {
	hits    []SummarySegmentVectorHit
	calls   int
	lastTop int
}

func (f *fakeVectorSearcher) SearchByVector(_ context.Context, _ int64, _ string, _ []float32,
	limit int) ([]SummarySegmentVectorHit, error) {
	f.calls++
	f.lastTop = limit
	return f.hits, nil
}

// fakeRecentUse records what the turn reached for.
type fakeRecentUse struct {
	recorded []int
}

func (f *fakeRecentUse) RecordRecentUse(_ context.Context, _ int64, _ string, seq int) error {
	f.recorded = append(f.recorded, seq)
	return nil
}

func (f *fakeRecentUse) ListRecentUse(context.Context, int64, string, int) ([]int, error) {
	return f.recorded, nil
}

// constantEmbedder returns one fixed vector.
func constantEmbedder(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0, 0}, nil
}

// vectorRun is a run with the recall cycle the vector channel needs.
func vectorRun(question string) RunContext {
	return RunContext{UserID: 7, SessionID: "session", Question: question,
		RecallCycle: memoryrecall.New(question)}
}

// A segment the words miss is still found, when the meaning is close.
//
// This is the case the vector channel exists for: the learner asks about a
// thing in words the segment never used, and the segment is the only place
// the answer is.
func TestTheVectorChannelFindsASegmentTheWordsMiss(t *testing.T) {
	searcher := &fakeVectorSearcher{hits: []SummarySegmentVectorHit{{Seq: 2, Score: 0.91}}}
	preselector, err := NewSummarySegmentPreselector(constantEmbedder, searcher)
	if err != nil {
		t.Fatal(err)
	}
	segments := []model.SessionSummarySegment{
		testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
		}, nil),
		testSegment(t, 2, 31, 60, "网络协议", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}
	selected, err := preselector.Select(context.Background(), vectorRun("半包怎么处理"),
		segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 1 || selected[0].Seq != 2 {
		t.Fatalf("the preselector chose %v, want the segment the vector found", selected)
	}
	if searcher.calls != 1 {
		t.Fatalf("the vector channel ran %d times", searcher.calls)
	}
}

// A vector hit that is too far down the list or too weak is not a match.
//
// The threshold is the plan's: the top three, and a score of at least 0.6.
func TestAWeakVectorHitIsNotSelected(t *testing.T) {
	searcher := &fakeVectorSearcher{hits: []SummarySegmentVectorHit{{Seq: 2, Score: 0.4}}}
	preselector, err := NewSummarySegmentPreselector(constantEmbedder, searcher)
	if err != nil {
		t.Fatal(err)
	}
	segments := []model.SessionSummarySegment{
		testSegment(t, 2, 31, 60, "网络协议", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}
	selected, err := preselector.Select(context.Background(), vectorRun("半包怎么处理"),
		segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 0 {
		t.Fatalf("the preselector chose %v, want nothing", selected)
	}
}

// The turn records what it reached for.
//
// The record is what keeps the segment the learner keeps asking about in
// front of them after the newest segments have moved past it.
func TestThePreselectionRecordsWhatItReachedFor(t *testing.T) {
	searcher := &fakeVectorSearcher{hits: []SummarySegmentVectorHit{{Seq: 2, Score: 0.91}}}
	recent := &fakeRecentUse{}
	preselector, err := NewSummarySegmentPreselector(constantEmbedder, searcher)
	if err != nil {
		t.Fatal(err)
	}
	preselector.UseRecentUse(recent)
	segments := []model.SessionSummarySegment{
		testSegment(t, 2, 31, 60, "网络协议", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}
	if _, err := preselector.Select(context.Background(), vectorRun("半包怎么处理"), segments, nil); err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(recent.recorded) != 1 || recent.recorded[0] != 2 {
		t.Fatalf("the recent use record holds %v", recent.recorded)
	}
}

// A preselector without a vector channel runs on the words alone.
//
// It costs no embedding, and a turn that asks nothing about an old segment
// pays nothing for the channel that would have looked.
func TestAPreselectorWithoutTheVectorChannelCostsNoEmbedding(t *testing.T) {
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	segments := []model.SessionSummarySegment{
		testSegment(t, 2, 31, 60, "网络协议", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}
	selected, err := preselector.Select(context.Background(), vectorRun("拆包"),
		segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if len(selected) != 1 || selected[0].Seq != 2 {
		t.Fatalf("the preselector chose %v, want the segment the words match", selected)
	}
}

// countingEmbedder counts the query vectors a turn buys.
type countingEmbedder struct{ calls int }

func (e *countingEmbedder) embed(context.Context, string) ([]float32, error) {
	e.calls++
	return []float32{1, 0, 0, 0}, nil
}

// A switched-off turn preselects on the words alone.
//
// The promise about a capability-free turn is about its answer, and a query
// vector is the first half of a retrieval. The channel is wired here on
// purpose: with it absent the zero would prove nothing, and the control below
// is what turns the zero into a ceiling rather than an unplugged seam.
//
// The keyword channel still runs, so the block is smaller rather than empty -
// and the background writer is untouched, because a segment that is never
// embedded can never be found by a later turn.
func TestASwitchedOffTurnPreselectsOnTheWordsAlone(t *testing.T) {
	embedder := &countingEmbedder{}
	searcher := &fakeVectorSearcher{hits: []SummarySegmentVectorHit{{Seq: 1, Score: 0.99}}}
	preselector, err := NewSummarySegmentPreselector(embedder.embed, searcher)
	if err != nil {
		t.Fatal(err)
	}
	segments := []model.SessionSummarySegment{
		testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
			{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
		}, nil),
		testSegment(t, 2, 31, 60, "网络协议", []model.SessionSummarySegmentPoint{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
		}, nil),
	}

	off := vectorRun("拆包")
	off.KnowledgePolicy = rag.KnowledgeDisabled
	selected, err := preselector.Select(context.Background(), off, segments, nil)
	if err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if embedder.calls != 0 {
		t.Fatalf("a switched-off turn bought %d query vectors", embedder.calls)
	}
	if searcher.calls != 0 {
		t.Fatalf("a switched-off turn searched the index %d times", searcher.calls)
	}
	if len(selected) != 1 || selected[0].Seq != 2 {
		t.Fatalf("a switched-off turn selected %v, want the segment the words match", selected)
	}

	// The control: the same wiring, one switch away, does buy the vector.
	on := vectorRun("拆包")
	on.KnowledgePolicy = rag.KnowledgePreferred
	if _, err := preselector.Select(context.Background(), on, segments, nil); err != nil {
		t.Fatalf("preselect: %v", err)
	}
	if embedder.calls != 1 || searcher.calls != 1 {
		t.Fatalf("the control bought %d vectors and %d searches, want one of each",
			embedder.calls, searcher.calls)
	}
}
