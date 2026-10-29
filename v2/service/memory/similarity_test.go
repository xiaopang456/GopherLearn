package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"errors"
	"math"
	"testing"
)

type stubIndex struct {
	semantic []memorydao.MemorySearchResult
	lexical  []memorydao.MemorySearchResult
	textErr  error
}

func (s *stubIndex) SearchByVector(context.Context, int64, []float32, int) ([]memorydao.MemorySearchResult, error) {
	return s.semantic, nil
}

func (s *stubIndex) SearchByText(context.Context, int64, []string, int) ([]memorydao.MemorySearchResult, error) {
	return s.lexical, s.textErr
}

type stubEmbedder struct{ err error }

func (s stubEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	if s.err != nil {
		return nil, s.err
	}
	return make([]float32, 4), nil
}

type stubExpander struct {
	value  string
	err    error
	called int
}

func (s *stubExpander) Expand(context.Context, string) (string, error) {
	s.called++
	return s.value, s.err
}

type stubLoader struct{ rows []model.LongTermMemory }

func (s stubLoader) LoadByIDs(_ context.Context, userID int64, ids []string) ([]model.LongTermMemory, error) {
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	out := make([]model.LongTermMemory, 0, len(ids))
	for _, row := range s.rows {
		if wanted[row.ID] && row.UserID == userID {
			out = append(out, row)
		}
	}
	return out, nil
}

// hits builds a candidate set from cosine similarities. RediSearch reports
// distance, so the fixture converts back the way the DAO does.
func hits(similarities ...float64) []memorydao.MemorySearchResult {
	out := make([]memorydao.MemorySearchResult, len(similarities))
	for i, similarity := range similarities {
		out[i] = memorydao.MemorySearchResult{
			MemoryID: string(rune('a' + i)), Distance: 1 - similarity, Rank: i,
		}
	}
	return out
}

func topicNote(id string, userID int64) model.LongTermMemory {
	return model.LongTermMemory{
		ID: id, UserID: userID, Scope: model.MemoryScopeTopic,
		Subject: id, Content: id + " content", Confidence: 0.9,
		Status: model.LongTermMemoryActive,
	}
}

func standoutConfig(floor float64, rescue bool) SimilarityConfig {
	config := DefaultSimilarityConfig()
	config.Rule, config.MinStandout, config.LexicalRescue = AbstainByStandout, floor, rescue
	return config
}

// The point of the standout rule is that it reads the candidate set, not an
// absolute score. These two cases share a top similarity of 0.62 and differ
// only in what surrounds it, and the rule must separate them where the absolute
// floor of 0.535 admits both.
func TestStandoutRuleSeparatesSetsTheAbsoluteFloorCannot(t *testing.T) {
	loader := stubLoader{rows: []model.LongTermMemory{topicNote("a", 7)}}
	cases := []struct {
		name     string
		semantic []memorydao.MemorySearchResult
		abstain  bool
	}{
		{"best match stands out", hits(0.62, 0.40, 0.38, 0.37, 0.35), false},
		{"whole set is equally similar", hits(0.62, 0.61, 0.60, 0.60, 0.59), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			index := &stubIndex{semantic: c.semantic}
			recall, err := NewSimilarityRecall(index, stubEmbedder{}, nil, loader, standoutConfig(1.2, false))
			if err != nil {
				t.Fatal(err)
			}
			got, err := recall.Recall(context.Background(), 7, "问题")
			if err != nil {
				t.Fatal(err)
			}
			if c.abstain && len(got) != 0 {
				t.Fatalf("expected abstention, got %d memories", len(got))
			}
			if !c.abstain && len(got) == 0 {
				t.Fatal("expected a result, got abstention")
			}
			// Whichever way it went, the absolute rule would have admitted both:
			// 0.62 clears 0.535 in each case. That is the regression this test
			// exists to prevent.
			if absolute := c.semantic[0].Similarity(); absolute < DefaultSimilarityConfig().MinSimilarity {
				t.Fatalf("fixture no longer isolates the rule: top similarity %.3f is below the absolute floor", absolute)
			}
		})
	}
}

// The blind run lost five answerable queries because the gate read the semantic
// channel while the answer was in the lexical one.
func TestLexicalRescueKeepsResultsBothChannelsAgreeOn(t *testing.T) {
	loader := stubLoader{rows: []model.LongTermMemory{topicNote("a", 7)}}
	flat := hits(0.62, 0.61, 0.60, 0.60, 0.59)
	agreeing := []memorydao.MemorySearchResult{{MemoryID: "a", Rank: 0}}

	index := &stubIndex{semantic: flat, lexical: agreeing}
	rescued, err := NewSimilarityRecall(index, stubEmbedder{}, nil, loader, standoutConfig(1.2, true))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rescued.Recall(context.Background(), 7, "问题")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("both channels named the same memory first, so it must not be discarded")
	}

	// Disagreement must not rescue: otherwise the gate never fires.
	index.lexical = []memorydao.MemorySearchResult{{MemoryID: "z", Rank: 0}}
	strict, _ := NewSimilarityRecall(index, stubEmbedder{}, nil, loader, standoutConfig(1.2, true))
	if got, err := strict.Recall(context.Background(), 7, "问题"); err != nil || len(got) != 0 {
		t.Fatalf("lexical disagreement must not rescue: %d memories, err=%v", len(got), err)
	}
}

func TestStandoutRatioResistsTheDriftThatBrokeTheAbsoluteFloor(t *testing.T) {
	// Scaling the whole set leaves the ratio exactly where it was, up to float
	// rounding. This is the easy half of the property.
	low := standoutRatio(hits(0.40, 0.20, 0.20, 0.20, 0.20))
	high := standoutRatio(hits(0.80, 0.40, 0.40, 0.40, 0.40))
	if math.Abs(low-high) > 1e-9 {
		t.Fatalf("scaling the set must not move the ratio: %.6f vs %.6f", low, high)
	}

	// The drift that actually broke the absolute floor is a shift, not a
	// scaling: every similarity rose by about 0.07 as the corpus grew from 100
	// memories to 539. Under a shift the ratio is not invariant either — it is
	// merely far less sensitive, which is exactly what the corpus measurement
	// found (4.2% against 11.5%). Asserting the weaker, true property keeps this
	// test honest about what the design does and does not buy.
	base := hits(0.56, 0.40, 0.38, 0.37, 0.35)
	shifted := hits(0.63, 0.47, 0.45, 0.44, 0.42)
	rawDrift := (shifted[0].Similarity() - base[0].Similarity()) / base[0].Similarity()
	ratioDrift := math.Abs(standoutRatio(shifted)-standoutRatio(base)) / standoutRatio(base)
	if ratioDrift >= rawDrift {
		t.Fatalf("the ratio must absorb an upward shift better than the raw score: ratio %.1f%%, raw %.1f%%",
			ratioDrift*100, rawDrift*100)
	}

	if empty := standoutRatio(nil); empty != 0 {
		t.Fatalf("an empty candidate set has no standout, got %.4f", empty)
	}
}

// An uncalibrated floor is worse than a known-imperfect one, so the shipped
// default must not silently switch rules.
func TestDefaultConfigStillShipsTheAbsoluteRule(t *testing.T) {
	config := DefaultSimilarityConfig()
	if config.Rule != AbstainByAbsoluteCosine || config.MinSimilarity != 0.535 {
		t.Fatalf("default changed before calibration: %+v", config)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	standout := config
	standout.Rule = AbstainByStandout
	if err := standout.Validate(); err == nil {
		t.Fatal("the standout rule must not validate with an unset floor")
	}
	if err := (SimilarityConfig{CandidateDepth: 20, MaxResults: 5, Rule: "guess"}).Validate(); err == nil {
		t.Fatal("an unknown rule must be rejected rather than defaulted")
	}
}

func TestExpansionFailureFallsBackToTheRawQuestion(t *testing.T) {
	loader := stubLoader{rows: []model.LongTermMemory{topicNote("a", 7)}}
	index := &stubIndex{semantic: hits(0.62, 0.40, 0.38, 0.37, 0.35)}
	expander := &stubExpander{err: errors.New("provider timeout")}
	recall, err := NewSimilarityRecall(index, stubEmbedder{}, expander, loader, standoutConfig(1.2, false))
	if err != nil {
		t.Fatal(err)
	}
	got, err := recall.Recall(context.Background(), 7, "问题")
	if err != nil || len(got) == 0 {
		t.Fatalf("a failed expansion must not cost the user their recall: %d memories, err=%v", len(got), err)
	}
	if expander.called != 1 || recall.Degradation().ExpansionFailures != 1 {
		t.Fatalf("the degradation must be counted, not swallowed: %+v", recall.Degradation())
	}
}

func TestIndexFailureAsksTheCallerToFallBack(t *testing.T) {
	loader := stubLoader{}
	recall, err := NewSimilarityRecall(&stubIndex{}, stubEmbedder{err: errors.New("redis down")}, nil, loader, standoutConfig(1.2, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recall.Recall(context.Background(), 7, "问题"); !errors.Is(err, errIndexUnavailable) {
		t.Fatalf("the retriever needs a distinguishable signal to fall back on, got %v", err)
	}
}

// Redis already filters on user, status and type. This is the second gate, and
// it is the one that holds if the index is ever stale or wrongly built.
func TestRecallRejectsRowsTheIndexShouldNotHaveReturned(t *testing.T) {
	foreign := topicNote("a", 8)
	profile := topicNote("b", 7)
	profile.Scope = model.MemoryScopeGlobal
	lowConfidence := topicNote("c", 7)
	lowConfidence.Confidence = 0.1
	loader := stubLoader{rows: []model.LongTermMemory{foreign, profile, lowConfidence, topicNote("d", 7)}}
	index := &stubIndex{semantic: hits(0.62, 0.40, 0.38, 0.37)}
	recall, err := NewSimilarityRecall(index, stubEmbedder{}, nil, loader, standoutConfig(1.2, false))
	if err != nil {
		t.Fatal(err)
	}
	got, err := recall.Recall(context.Background(), 7, "问题")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "d" {
		t.Fatalf("only the owner's active topic note above the confidence floor may survive: %+v", got)
	}
}

// AGENT-021 demotes retrieval to a shortlist. What it must get right is which
// topics are worth showing, and a topic is judged by its single best note.
func TestCandidateTopicsRankByBestNoteNotByCount(t *testing.T) {
	// Rank 0 belongs to a topic with one note; ranks 1-3 to a topic with three.
	// Summing would put the crowded topic first, which is the aggregation
	// mistake that sank the topic-centroid method.
	semantic := []memorydao.MemorySearchResult{
		{MemoryID: "sharp", Distance: 0.30, Rank: 0},
		{MemoryID: "bulk1", Distance: 0.45, Rank: 1},
		{MemoryID: "bulk2", Distance: 0.46, Rank: 2},
		{MemoryID: "bulk3", Distance: 0.47, Rank: 3},
	}
	rows := []model.LongTermMemory{
		note("sharp", 7, "rpc-粘包"), note("bulk1", 7, "redis-ttl"),
		note("bulk2", 7, "redis-ttl"), note("bulk3", 7, "redis-ttl"),
	}
	recall, err := NewSimilarityRecall(&stubIndex{semantic: semantic}, stubEmbedder{}, nil,
		stubLoader{rows: rows}, standoutConfig(1.2, false))
	if err != nil {
		t.Fatal(err)
	}
	topics, err := recall.CandidateTopics(context.Background(), 7, "问题", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 2 || topics[0] != "rpc-粘包" {
		t.Fatalf("the topic holding the best single note must lead: %v", topics)
	}
}

// The shortlist must not abstain. Removing a topic here is not the same act as
// answering with nothing — the model still sees the rest of the catalogue.
func TestCandidateTopicsDoNotApplyTheAbstentionGate(t *testing.T) {
	flat := hits(0.62, 0.61, 0.60, 0.60, 0.59)
	rows := make([]model.LongTermMemory, 0, len(flat))
	for i, hit := range flat {
		rows = append(rows, note(hit.MemoryID, 7, "topic-"+string(rune('a'+i))))
	}
	recall, err := NewSimilarityRecall(&stubIndex{semantic: flat}, stubEmbedder{}, nil,
		stubLoader{rows: rows}, standoutConfig(1.2, false))
	if err != nil {
		t.Fatal(err)
	}
	// The same candidate set makes Recall abstain.
	if got, _ := recall.Recall(context.Background(), 7, "问题"); len(got) != 0 {
		t.Fatal("fixture no longer triggers the gate, so this test proves nothing")
	}
	topics, err := recall.CandidateTopics(context.Background(), 7, "问题", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 3 {
		t.Fatalf("a shortlist must still be offered: %v", topics)
	}
}

func TestCandidateTopicsSkipForeignAndUntopicedRows(t *testing.T) {
	semantic := hits(0.62, 0.55, 0.50)
	rows := []model.LongTermMemory{
		note("a", 8, "foreign-topic"), note("b", 7, ""), note("c", 7, "rpc-粘包"),
	}
	recall, err := NewSimilarityRecall(&stubIndex{semantic: semantic}, stubEmbedder{}, nil,
		stubLoader{rows: rows}, standoutConfig(1.2, false))
	if err != nil {
		t.Fatal(err)
	}
	topics, err := recall.CandidateTopics(context.Background(), 7, "问题", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || topics[0] != "rpc-粘包" {
		t.Fatalf("another user's topic and an unfiled note must not appear: %v", topics)
	}
	if _, err := recall.CandidateTopics(context.Background(), 7, "问题", 0); err == nil {
		t.Error("a non-positive limit must be rejected")
	}
}

func note(id string, userID int64, topicKey string) model.LongTermMemory {
	memory := topicNote(id, userID)
	memory.TopicKey = topicKey
	return memory
}
