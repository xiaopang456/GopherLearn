package isolated

import (
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// stubProberRegistry is a registry that can run the server-side
// pre-retrieval and records what it was asked for.
type stubProberRegistry struct {
	agentService.StaticToolRegistry
	probes    atomic.Int32
	question  string
	threshold float64
}

func (r *stubProberRegistry) ProbeRelevance(_ context.Context, _ agentService.RunContext, question string, threshold float64) (agentService.RelevanceOutcome, error) {
	r.probes.Add(1)
	r.question = question
	r.threshold = threshold
	return agentService.RelevanceOutcome{
		Status: agentService.RelevanceHit, Threshold: threshold, TopScore: 0.93, RerankApplied: true,
	}, nil
}

// TestRunnerRegistryKeepsThePreRetrieval is the regression test for the
// feature that never ran: the runner asks the registry for a relevance
// prober, and a wrapper that forwards only the tool table answers "no", so
// every turn falls back to the forced document search without a word of
// complaint.
func TestRunnerRegistryKeepsThePreRetrieval(t *testing.T) {
	inner := &stubProberRegistry{}
	registry := registryForRunner(inner)
	prober, ok := registry.(agentService.RelevanceProber)
	if !ok {
		t.Fatal("the runner registry dropped the pre-retrieval")
	}
	question := "TCP 为什么要三次握手？"
	outcome, err := prober.ProbeRelevance(context.Background(), agentService.RunContext{}, question, 0.8)
	if err != nil {
		t.Fatal(err)
	}
	if inner.probes.Load() != 1 {
		t.Fatalf("the wrapper did not forward the pre-retrieval: %d probes", inner.probes.Load())
	}
	if inner.question != question || inner.threshold != 0.8 {
		t.Fatalf("the wrapper changed the question or the threshold: %q %v", inner.question, inner.threshold)
	}
	if outcome.Status != agentService.RelevanceHit {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// TestRunnerRegistryKeepsADeploymentWithoutAProber keeps the other half of
// the contract: a deployment that cannot probe must not claim that it can.
func TestRunnerRegistryKeepsADeploymentWithoutAProber(t *testing.T) {
	registry := registryForRunner(agentService.StaticToolRegistry{})
	if _, ok := registry.(agentService.RelevanceProber); ok {
		t.Fatal("a registry without a pre-retrieval claimed one")
	}
}

// fakeReranker serves the local rerank protocol a deployment uses: the
// health check, the model identity, and the scores themselves.
type fakeReranker struct {
	server *httptest.Server
	calls  atomic.Int32
	score  float64
}

func newFakeReranker(t *testing.T, score float64) *fakeReranker {
	t.Helper()
	fake := &fakeReranker{score: score}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"model_id": rag.RerankModel, "model_sha": rag.RerankRevision,
		})
	})
	mux.HandleFunc("/rerank", func(w http.ResponseWriter, r *http.Request) {
		fake.calls.Add(1)
		var payload struct {
			Texts []string `json:"texts"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		scores := make([]rag.RerankScore, 0, len(payload.Texts))
		for index := range payload.Texts {
			value := 0.05
			if index == 0 {
				value = fake.score
			}
			scores = append(scores, rag.RerankScore{Index: index, Score: value})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scores)
	})
	fake.server = httptest.NewServer(mux)
	t.Cleanup(fake.server.Close)
	return fake
}

// TestProductionAssemblyRunsThePreRetrieval runs the whole entry the way a
// deployment runs it - the real wiring, the real runner, the real DAO - and
// checks that the pre-retrieval really happens.
//
// The check is on the product's own record of it: a knowledge_search step
// whose summary is the server's pre-retrieval sentence. That is the same
// evidence an operator reads, so a wiring that loses the pre-retrieval again
// cannot pass this test.
func TestProductionAssemblyRunsThePreRetrieval(t *testing.T) {
	reranker := newFakeReranker(t, 0.93)
	s := testSettings(t)
	// The rerank credential is the one setting that never lives in a
	// configuration file, so the test sets it the way a deployment does.
	const rerankToken = "synthetic-rerank-token-of-at-least-32-bytes"
	t.Setenv("RAG_RERANK_AUTH_TOKEN", rerankToken)
	s.Config.RagModelConfig.RagRerankEnabled = true
	s.Config.RagModelConfig.RagRerankProvider = config.RerankProviderLocal
	s.Config.RagModelConfig.RagRerankEndpoint = reranker.server.URL
	s.Config.RagModelConfig.RagRerankToken = rerankToken
	s.Config.RagModelConfig.RagRerankTimeoutMs = 5000
	app, db, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("三次握手是为了确认双方的收发能力 [1]。", nil), nil
	})
	chat.chunks.rows = []rag.RedisChunkSearchResult{
		{
			RedisKey: "offline-vector", ChunkID: "chunk-1", DocID: "offline-doc", DocVersion: 1,
			ChunkIndex: 0, ChunkType: "prose", SourceFile: "TCP.md", TitlePath: "TCP",
			StartLine: 1, EndLine: 2, RawContent: "三次握手确认双方的收发能力。",
			EmbeddingContent: "三次握手确认双方的收发能力。", Distance: 0.01, LexicalScore: 1,
		},
		{
			RedisKey: "offline-vector", ChunkID: "chunk-2", DocID: "offline-doc", DocVersion: 1,
			ChunkIndex: 1, ChunkType: "prose", SourceFile: "TCP.md", TitlePath: "TCP",
			StartLine: 3, EndLine: 4, RawContent: "四次挥手关闭连接。",
			EmbeddingContent: "四次挥手关闭连接。", Distance: 0.02, LexicalScore: 0.5,
		},
	}
	// The retrieval only looks at documents the metadata store reports as
	// active, so the learner needs one before there is anything to find.
	doc := model.KnowledgeDocument{
		ID: "offline-doc", UserID: 1, OriginalName: "TCP.md", StoragePath: filepath.Join(t.TempDir(), "TCP.md"),
		FileType: "md", ContentHash: "offline-hash", FileSize: 32, Version: 1, ActiveVersion: 1,
		IndexStatus: model.KnowledgeStatusReady,
	}
	doc.PrepareForCreate()
	if err := db.Create(&doc).Error; err != nil {
		t.Fatal(err)
	}
	session := createSession(t, app, token, "create-pre-retrieval")
	runHTTP(t, app, token, session, "run-pre-retrieval", "TCP 为什么要三次握手？")

	var steps []model.AgentStep
	if err := db.Find(&steps).Error; err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if strings.HasPrefix(step.ResultSummary, "服务端预检索：") {
			if !strings.Contains(step.ResultSummary, "0.930") {
				t.Fatalf("the pre-retrieval reported another score: %q", step.ResultSummary)
			}
			return
		}
	}
	t.Fatalf("the pre-retrieval did not run under the production assembly: %s", stepSummaries(steps))
}

func stepSummaries(steps []model.AgentStep) string {
	var out []string
	for _, step := range steps {
		out = append(out, fmt.Sprintf("%s/%s", step.ToolName, step.ResultSummary))
	}
	return strings.Join(out, " | ")
}
