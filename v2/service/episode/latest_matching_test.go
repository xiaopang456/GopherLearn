package episode

import (
	"GopherAI/common/memoryrecall"
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

var recallClock = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

func at(daysAgo int) time.Time { return recallClock.AddDate(0, 0, -daysAgo) }

func indexed(ids ...string) []episodedao.EpisodeSearchResult {
	out := make([]episodedao.EpisodeSearchResult, 0, len(ids))
	for _, id := range ids {
		out = append(out, episodedao.EpisodeSearchResult{EpisodeID: id, EmbeddingVersion: episodedao.CurrentEpisodeEmbeddingVersion})
	}
	return out
}

func activeEpisode(id, session string, daysAgo int, topics ...string) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: id, UserID: 7, SessionID: session, LastMessageAt: at(daysAgo),
		TopicKeys: model.StringList(topics), Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
}

func withDecision(episode model.EpisodeMemory) model.EpisodeMemory {
	episode.Document.Decisions = []model.EpisodeEvidence{{Text: "decision"}}
	return episode
}

func withCorrection(episode model.EpisodeMemory) model.EpisodeMemory {
	episode.Document.UserCorrections = []model.EpisodeEvidence{{Text: "correction"}}
	return episode
}

func withOpenLoop(episode model.EpisodeMemory) model.EpisodeMemory {
	episode.Document.OpenLoops = []model.EpisodeEvidence{{Text: "open loop"}}
	return episode
}

// The defect this whole task exists for: "上次" plus a topic used to return the
// newest session regardless of what it was about. Recency now ranks the
// relevant candidates instead of replacing them.
func TestLatestMatchingIgnoresANewerUnrelatedEpisode(t *testing.T) {
	search := &fakeEpisodeSearch{exact: indexed("chunk-old")}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		withDecision(activeEpisode("chunk-old", "s-chunk", 30, "分块切片")),
		withDecision(activeEpisode("metric-new", "s-metric", 2, "熔断限流")),
	}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock,
		Question: "上次关于 分块切片 的决定是什么？",
	})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "chunk-old" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if got.Trace.TemporalMode != TemporalLatestMatching {
		t.Fatalf("temporal=%s", got.Trace.TemporalMode)
	}
}

// Within the relevant set, recency does decide.
func TestLatestMatchingPicksTheNewestRelevantEpisode(t *testing.T) {
	search := &fakeEpisodeSearch{exact: indexed("chunk-old", "chunk-new")}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		activeEpisode("chunk-old", "s-chunk", 30, "分块切片"),
		activeEpisode("chunk-new", "s-overlap", 14, "分块切片"),
	}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock,
		Question: "上次关于 分块切片 聊到哪一步了？",
	})
	if err != nil || len(got.Episodes) == 0 || got.Episodes[0].ID != "chunk-new" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

// A TopicKey the resolver confirmed is the third way into the exact channel. It
// is what reaches an episode whose text never contains the learner's wording.
func TestResolvedTopicKeyReachesAnEpisodeLexicalSearchCannotFind(t *testing.T) {
	search := &fakeEpisodeSearchByKey{byKey: map[string][]episodedao.EpisodeSearchResult{"packetbuffer": indexed("buffer")}}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{withDecision(activeEpisode("buffer", "s-buffer", 43, "packetbuffer"))}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())

	without, _ := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock,
		Question: "上次在读缓冲上的取舍是什么？",
	})
	if len(without.Episodes) != 0 {
		t.Fatalf("lexical wording alone must not reach it: %+v", without.Episodes)
	}
	with, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock,
		Question: "上次在读缓冲上的取舍是什么？", ProposedTopicKeys: []string{"packetbuffer"},
	})
	if err != nil || len(with.Episodes) != 1 || with.Episodes[0].ID != "buffer" {
		t.Fatalf("resolved key result=%+v err=%v", with, err)
	}
}

func TestEvidenceModeFiltersOnTheStructuredProjection(t *testing.T) {
	rows := []model.EpisodeMemory{
		withDecision(activeEpisode("decided", "s-a", 10, "主题")),
		withCorrection(activeEpisode("corrected", "s-b", 9, "主题")),
		withOpenLoop(activeEpisode("open", "s-c", 8, "主题")),
	}
	cases := []struct {
		question string
		want     string
	}{
		{"上次关于 主题 的决定是什么？", "decided"},
		{"上次关于 主题 我纠正了什么？", "corrected"},
		{"关于 主题 还欠着什么？", "open"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			search := &fakeEpisodeSearch{exact: indexed("decided", "corrected", "open")}
			loader := &fakeEpisodeLoader{rows: rows}
			retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
			got, err := retriever.Recall(context.Background(), RecallRequest{
				UserID: 7, SessionID: "current", Now: recallClock, Question: tc.question,
			})
			if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != tc.want {
				t.Fatalf("result=%+v err=%v", got, err)
			}
		})
	}
}

// latest_any keeps the pre-v3 semantics, and the evidence filter still applies:
// the newest session need not be the newest one that recorded a correction.
func TestLatestAnyAppliesTheEvidenceFilterAfterReload(t *testing.T) {
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		activeEpisode("newest", "s-a", 2, "熔断限流"),
		withCorrection(activeEpisode("corrected", "s-b", 20, "go并发")),
	}}
	retriever, _ := NewRetriever(NewQueryRouter(), &fakeEpisodeSearch{}, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	plain, _ := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "上次聊了什么？",
	})
	if len(plain.Episodes) != 1 || plain.Episodes[0].ID != "newest" {
		t.Fatalf("plain latest_any=%+v", plain.Episodes)
	}
	corrected, _ := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "上次我纠正了什么？",
	})
	if len(corrected.Episodes) != 1 || corrected.Episodes[0].ID != "corrected" {
		t.Fatalf("correction latest_any=%+v", corrected.Episodes)
	}
}

// A date question whose leftover words are not a subject must not be emptied by
// a topic filter it never expressed; an explicitly named topic must be a filter.
func TestTimeRangeTopicIsAPreferenceUnlessItWasNamed(t *testing.T) {
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{activeEpisode("only", "s-a", 1, "redis过期")}}
	retriever, _ := NewRetriever(NewQueryRouter(), &fakeEpisodeSearch{}, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	inferred, _ := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "2026-08-31 那次排查的处理办法是什么？",
	})
	if len(inferred.Episodes) != 1 || inferred.Episodes[0].ID != "only" {
		t.Fatalf("inferred topic must not empty a date query: %+v", inferred.Episodes)
	}
	named, _ := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "2026-08-31 关于 向量检索 定了什么？",
	})
	if len(named.Episodes) != 0 {
		t.Fatalf("named topic must filter the range: %+v", named.Episodes)
	}
}

// Degradation contract: no channel, no history. Never "the nearest thing in the
// index" and never "the newest session instead".
func TestDegradedProvidersPreferNoMemoryOverArbitraryHistory(t *testing.T) {
	rows := []model.EpisodeMemory{withDecision(activeEpisode("newest", "s-a", 1, "无关"))}

	t.Run("redis down", func(t *testing.T) {
		retriever, _ := NewRetriever(NewQueryRouter(), &fakeEpisodeSearch{err: errors.New("down")}, &fakeEpisodeLoader{rows: rows}, fakeQueryEmbedder{}, DefaultRetrieverConfig())
		got, err := retriever.Recall(context.Background(), RecallRequest{
			UserID: 7, SessionID: "current", Now: recallClock, Question: "上次关于 分块切片 的决定是什么？",
		})
		if err != nil || len(got.Episodes) != 0 || got.Trace.Reason != "redis_unavailable" {
			t.Fatalf("result=%+v err=%v", got, err)
		}
	})

	t.Run("embedding down", func(t *testing.T) {
		search := &fakeEpisodeSearch{lexical: indexed("newest"), vector: indexed("newest")}
		retriever, _ := NewRetriever(NewQueryRouter(), search, &fakeEpisodeLoader{rows: rows}, fakeQueryEmbedder{err: errors.New("down")}, DefaultRetrieverConfig())
		got, err := retriever.Recall(context.Background(), RecallRequest{
			UserID: 7, SessionID: "current", Now: recallClock, Question: "之前关于 分块切片 的决定是什么？",
		})
		if err != nil || len(got.Episodes) != 0 {
			t.Fatalf("a lexical-only candidate must not be admitted without its second opinion: %+v err=%v", got, err)
		}
		if !containsTerm(got.Trace.ReasonCodes, "embedding_unavailable") {
			t.Fatalf("reason codes=%v", got.Trace.ReasonCodes)
		}
	})

	t.Run("mysql rejects every candidate", func(t *testing.T) {
		search := &fakeEpisodeSearch{lexical: indexed("foreign"), vector: indexed("foreign")}
		loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
			{ID: "foreign", UserID: 8, SessionID: "s-a", Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed},
		}}
		retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
		got, err := retriever.Recall(context.Background(), RecallRequest{
			UserID: 7, SessionID: "current", Now: recallClock, Question: "之前关于 分块切片 聊了什么？",
		})
		if err != nil || len(got.Episodes) != 0 {
			t.Fatalf("result=%+v err=%v", got, err)
		}
	})
}

// A vector channel that returns most of the corpus is not an opinion, so it
// cannot corroborate a lexical hit from the far end of its own ranking.
func TestCorroborationRequiresAHighVectorRankNotMerePresence(t *testing.T) {
	deep := make([]string, 0, 20)
	for i := 0; i < 19; i++ {
		deep = append(deep, "filler")
	}
	deep = append(deep, "target")
	search := &fakeEpisodeSearch{lexical: indexed("target"), vector: indexed(deep...)}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{withDecision(activeEpisode("target", "s-a", 5, "无关"))}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	got, _ := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "之前关于 压测 的决定是什么？",
	})
	if len(got.Episodes) != 0 {
		t.Fatalf("deep vector presence must not count as agreement: %+v", got.Episodes)
	}
}

// The episode retriever publishes the TopicKeys of what survived the reload, so
// the topic memory read that follows treats them as authoritative.
func TestRecallPublishesSurvivingTopicKeysToTheCycle(t *testing.T) {
	search := &fakeEpisodeSearch{exact: indexed("kept")}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{activeEpisode("kept", "s-a", 5, "gorm软删除")}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	cycle := memoryrecall.New("关于 gorm软删除 我们讨论过什么？")
	if _, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock,
		Question: "关于 gorm软删除 我们讨论过什么？", Cycle: cycle,
	}); err != nil {
		t.Fatal(err)
	}
	keys := cycle.EpisodeTopicKeys()
	if len(keys) != 1 || keys[0] != "gorm软删除" {
		t.Fatalf("published keys=%v", keys)
	}
}

// fakeEpisodeSearchByKey answers the exact channel per topic key, so a test can
// tell "found through the resolved key" from "found through anything else".
type fakeEpisodeSearchByKey struct {
	byKey map[string][]episodedao.EpisodeSearchResult
}

func (f *fakeEpisodeSearchByKey) SearchByVector(context.Context, int64, string, []float32, int) ([]episodedao.EpisodeSearchResult, error) {
	return nil, nil
}
func (f *fakeEpisodeSearchByKey) SearchByText(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error) {
	return nil, nil
}
func (f *fakeEpisodeSearchByKey) SearchExactTopics(_ context.Context, _ int64, _ string, keys []string, _ int) ([]episodedao.EpisodeSearchResult, error) {
	out := []episodedao.EpisodeSearchResult{}
	for _, key := range keys {
		out = append(out, f.byKey[key]...)
	}
	return out, nil
}
