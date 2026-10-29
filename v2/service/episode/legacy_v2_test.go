package episode

import (
	"GopherAI/common/memoryrecall"
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"testing"
)

// The legacy path exists to be the pre-030 behaviour, so what it must be tested
// against is the pre-030 behaviour — including the parts v3 deliberately
// changed. Every assertion here is a v3 difference in reverse.
func TestLegacyRouterKeepsTheSingleMutuallyExclusiveIntent(t *testing.T) {
	cases := []struct {
		name, question string
		intent         QueryIntent
		reason         string
	}{
		// v3 routes this to latest_matching and keeps the topic; v2 does not.
		{"last session wins over topic", "上次关于缓冲区复用的决定是什么？", IntentLastSession, "last_session_marker"},
		// v3 recognises 那次 as a session pointer; v2 has no such marker.
		{"那次 is not a v2 marker", "那次聊天最后是什么结果？", IntentNone, "no_history_intent"},
		// v3 has an implicit first-person decision route; v2 has none.
		{"implicit decision is not a v2 route", "消息确认方式我是怎么定下来的？", IntentNone, "no_history_intent"},
		{"plain topic history", "以前讨论过 Redis 索引吗", IntentTopicHistory, "history_topic_marker"},
		{"decision history", "之前 RPC 粘包最后决定怎么做", IntentDecisionHistory, "history_decision_marker"},
		{"no history", "RPC 粘包怎么处理", IntentNone, "no_history_intent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := legacyRoute(RouteRequest{Question: tc.question})
			if route.Intent != tc.intent || route.Reason != tc.reason {
				t.Fatalf("intent=%s reason=%s want %s/%s", route.Intent, route.Reason, tc.intent, tc.reason)
			}
		})
	}
}

// v3 cuts the explicit topic at the sentence boundary. v2 took everything to
// the end and produced a key nothing carries — that is the defect v3 fixed, and
// legacy_v2 has to keep reproducing it.
func TestLegacyExplicitTopicKeepsTheWholeRemainder(t *testing.T) {
	key, ok := legacyExplicitTopicKey("关于 分块切片 我之前聊到哪一步了？")
	if !ok || key != "分块切片-我之前聊到哪一步了" {
		t.Fatalf("legacy explicit topic key=%q ok=%v", key, ok)
	}
	// The tail patterns v3 added must not be present here.
	trimmed, ok := legacyExplicitTopicKey("关于向量迁移定了什么？")
	if !ok || trimmed != "向量迁移定了" {
		t.Fatalf("legacy tail trimming changed: %q", trimmed)
	}
}

// An exact topic miss ends the read under v2. v3 falls back; legacy must not.
func TestLegacyExactTopicMissTerminatesTheRead(t *testing.T) {
	version := episodedao.CurrentEpisodeEmbeddingVersion
	search := &fakeEpisodeSearch{
		lexical: []episodedao.EpisodeSearchResult{{EpisodeID: "noise", EmbeddingVersion: version}},
		vector:  []episodedao.EpisodeSearchResult{{EpisodeID: "noise", EmbeddingVersion: version}},
	}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		withDecision(activeEpisode("noise", "s-a", 5, "无关")),
	}}
	retriever, err := NewLegacyRetriever(search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "之前关于 RPC 连接池的决定是什么？",
	})
	if err != nil || len(got.Episodes) != 0 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if got.Trace.Reason != "exact_topic_not_found" {
		t.Fatalf("legacy must stop at exact_topic_not_found, got %q", got.Trace.Reason)
	}
}

// The legacy path has no cycle and no resolved keys. Passing them must change
// nothing, so that a caller which starts filling those fields cannot silently
// move what legacy_v2 means.
func TestLegacyRetrieverIgnoresV3Inputs(t *testing.T) {
	search := &fakeEpisodeSearchByKey{byKey: map[string][]episodedao.EpisodeSearchResult{
		"packetbuffer": indexed("buffer"),
	}}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		withDecision(activeEpisode("buffer", "s-buffer", 43, "packetbuffer")),
	}}
	retriever, err := NewLegacyRetriever(search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	cycle := memoryrecall.New("上次在读缓冲上的取舍是什么？")
	got, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock,
		Question:          "上次在读缓冲上的取舍是什么？",
		ProposedTopicKeys: []string{"packetbuffer"}, Cycle: cycle,
	})
	if err != nil {
		t.Fatal(err)
	}
	// v3 would reach the episode through the resolved key. v2 routes this to
	// last_session and never consults the exact channel at all.
	if got.Trace.Intent != IntentLastSession {
		t.Fatalf("intent=%s", got.Trace.Intent)
	}
	if cycle.EmbeddingCalls() != 0 {
		t.Fatalf("legacy must not spend the request cycle: %d", cycle.EmbeddingCalls())
	}
	if len(cycle.EpisodeTopicKeys()) != 0 {
		t.Fatalf("legacy must not publish topic keys: %v", cycle.EpisodeTopicKeys())
	}
}

// The corroboration bound is a v3 change (D-156). Legacy admits a lexical hit
// corroborated anywhere in the vector ranking, and must keep doing so.
func TestLegacyFusionHasNoCorroborationBound(t *testing.T) {
	deep := make([]string, 0, 20)
	for i := 0; i < 19; i++ {
		deep = append(deep, "filler")
	}
	deep = append(deep, "target")
	search := &fakeEpisodeSearch{lexical: indexed("target"), vector: indexed(deep...)}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		withDecision(activeEpisode("target", "s-a", 5, "无关")),
	}}
	retriever, err := NewLegacyRetriever(search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	if err != nil {
		t.Fatal(err)
	}
	got, err := retriever.Recall(context.Background(), RecallRequest{
		UserID: 7, SessionID: "current", Now: recallClock, Question: "之前压测最后决定怎么做",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Episodes) != 1 || got.Episodes[0].ID != "target" {
		t.Fatalf("legacy fusion changed: %+v", got.Episodes)
	}
}
