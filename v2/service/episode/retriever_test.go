package episode

import (
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"errors"
	"testing"
	"time"
)

type fakeEpisodeSearch struct {
	exact, lexical, vector []episodedao.EpisodeSearchResult
	err                    error
}

func (f *fakeEpisodeSearch) SearchByVector(context.Context, int64, string, []float32, int) ([]episodedao.EpisodeSearchResult, error) {
	return f.vector, f.err
}
func (f *fakeEpisodeSearch) SearchByText(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error) {
	return f.lexical, f.err
}
func (f *fakeEpisodeSearch) SearchExactTopics(context.Context, int64, string, []string, int) ([]episodedao.EpisodeSearchResult, error) {
	return f.exact, f.err
}

type fakeEpisodeLoader struct {
	rows    []model.EpisodeMemory
	timeErr error
}

func (f *fakeEpisodeLoader) LoadCandidates(_ context.Context, _ int64, current string, ids []string) ([]model.EpisodeMemory, error) {
	by := map[string]model.EpisodeMemory{}
	for _, row := range f.rows {
		if row.UserID == 7 && row.SessionID != current && row.Status == model.EpisodeStatusActive {
			by[row.ID] = row
		}
	}
	out := []model.EpisodeMemory{}
	for _, id := range ids {
		if row, ok := by[id]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}
func (f *fakeEpisodeLoader) ListByTime(context.Context, int64, string, time.Time, time.Time, int) ([]model.EpisodeMemory, error) {
	return f.rows, f.timeErr
}

type fakeQueryEmbedder struct{ err error }

func (f fakeQueryEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	return []float32{1}, f.err
}

func TestRetrieverVectorOnlyAbstainsAndDualChannelPasses(t *testing.T) {
	version := episodedao.CurrentEpisodeEmbeddingVersion
	search := &fakeEpisodeSearch{vector: []episodedao.EpisodeSearchResult{{EpisodeID: "vector-only", EmbeddingVersion: version}, {EpisodeID: "both", EmbeddingVersion: version}}, lexical: []episodedao.EpisodeSearchResult{{EpisodeID: "both", EmbeddingVersion: version}}}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{{ID: "both", UserID: 7, SessionID: "old", Document: model.EpisodeDocument{Decisions: []model.EpisodeEvidence{{Text: "decision"}}}, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "之前这个方案最后决定怎么做"})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "both" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestRetrieverExactWinsAndMySQLDropsStaleRows(t *testing.T) {
	version := episodedao.CurrentEpisodeEmbeddingVersion
	search := &fakeEpisodeSearch{exact: []episodedao.EpisodeSearchResult{{EpisodeID: "foreign", EmbeddingVersion: version}, {EpisodeID: "exact", EmbeddingVersion: version}, {EpisodeID: "old-vector", EmbeddingVersion: version - 1}}}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{{ID: "foreign", UserID: 8, SessionID: "old", Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}, {ID: "exact", UserID: 7, SessionID: "old", Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, nil, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "以前讨论过 exact 吗"})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "exact" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestRetrieverExplicitTopicPrefersExactAndFallsBackConstrained(t *testing.T) {
	version := episodedao.CurrentEpisodeEmbeddingVersion
	search := &fakeEpisodeSearch{
		exact:   []episodedao.EpisodeSearchResult{{EpisodeID: "rpc-packet", EmbeddingVersion: version}, {EpisodeID: "recall-echo", EmbeddingVersion: version}},
		lexical: []episodedao.EpisodeSearchResult{{EpisodeID: "http-noise", EmbeddingVersion: version}},
		vector:  []episodedao.EpisodeSearchResult{{EpisodeID: "http-noise", EmbeddingVersion: version}},
	}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{
		{ID: "rpc-packet", UserID: 7, SessionID: "a", SearchText: "RPC 粘包使用长度字段拆包", Document: model.EpisodeDocument{Decisions: []model.EpisodeEvidence{{Text: "decision"}}}, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed},
		{ID: "recall-echo", UserID: 7, SessionID: "echo", SearchText: "用户询问之前关于 RPC 粘包的决定", Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed},
		{ID: "http-noise", UserID: 7, SessionID: "b", SearchText: "HTTP 消息边界使用 Content-Length", Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed},
	}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, fakeQueryEmbedder{}, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "之前关于 RPC 粘包的决定是什么？"})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "rpc-packet" {
		t.Fatalf("exact result=%+v err=%v", got, err)
	}
	// STABILIZE-030: an exact miss no longer ends the read. It records why and
	// continues into the constrained fallback, where a candidate still has to
	// be corroborated by two channels and still has to carry the evidence the
	// question asked for. Here http-noise reaches both channels but recorded no
	// decision, so nothing is injected — abstaining for the right reason.
	search.exact = nil
	got, err = retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "之前关于 RPC 连接池的决定是什么？"})
	if err != nil || len(got.Episodes) != 0 || !got.Trace.Abstained {
		t.Fatalf("missing explicit topic result=%+v err=%v", got, err)
	}
	if !containsReason(got.Trace.ReasonCodes, "exact_topic_miss_fallback") {
		t.Fatalf("exact miss must record the fallback it took: %v", got.Trace.ReasonCodes)
	}
	if len(got.Trace.Candidates) != 1 || got.Trace.Candidates[0].EpisodeID != "http-noise" {
		t.Fatalf("fallback candidates=%+v", got.Trace.Candidates)
	}

	// The same fallback admits the candidate once it carries a decision, which
	// is what makes the miss a fallback rather than a second dead end.
	loader.rows[2].Document.Decisions = []model.EpisodeEvidence{{Text: "decision"}}
	got, err = retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "之前关于 RPC 连接池的决定是什么？"})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "http-noise" {
		t.Fatalf("constrained fallback result=%+v err=%v", got, err)
	}
}

func containsReason(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}

func TestRetrieverExactIdentifierAllowsLexicalOnlyCandidate(t *testing.T) {
	version := episodedao.CurrentEpisodeEmbeddingVersion
	search := &fakeEpisodeSearch{lexical: []episodedao.EpisodeSearchResult{{EpisodeID: "packet", EmbeddingVersion: version}}}
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{{ID: "packet", UserID: 7, SessionID: "old", Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}}}
	retriever, _ := NewRetriever(NewQueryRouter(), search, loader, nil, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "之前 PacketBuffer 是怎么处理的"})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "packet" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestRetrieverRedisDownKeepsMySQLTimeFallback(t *testing.T) {
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{{ID: "last"}}}
	retriever, _ := NewRetriever(NewQueryRouter(), &fakeEpisodeSearch{err: errors.New("down")}, loader, nil, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "上次聊了什么"})
	if err != nil || len(got.Episodes) != 1 {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestRetrieverNarrowsTimeRangeByExplicitTopic(t *testing.T) {
	loader := &fakeEpisodeLoader{rows: []model.EpisodeMemory{{ID: "rpc", TopicKeys: model.StringList{"rpc-粘包"}}, {ID: "redis", TopicKeys: model.StringList{"redis"}}}}
	retriever, _ := NewRetriever(NewQueryRouter(), &fakeEpisodeSearch{}, loader, nil, DefaultRetrieverConfig())
	got, err := retriever.Recall(context.Background(), RecallRequest{UserID: 7, SessionID: "current", Question: "上周关于 RPC 粘包聊了什么", Now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.Local)})
	if err != nil || len(got.Episodes) != 1 || got.Episodes[0].ID != "rpc" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}
