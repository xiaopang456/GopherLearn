package responder

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

type recordingRetriever struct {
	queries []retrieval.Query
	results []retrieval.Result
	err     error
}

func (r *recordingRetriever) Search(_ context.Context, query retrieval.Query) ([]retrieval.Result, error) {
	r.queries = append(r.queries, query)
	if r.err != nil {
		return nil, r.err
	}
	return r.results, nil
}

type stubTitles struct{ titles map[string]string }

func (s stubTitles) DocumentTitles(context.Context, string, []string) (map[string]string, error) {
	return s.titles, nil
}

type failingGenerator struct{ err error }

func (g failingGenerator) Generate(context.Context, Generation) (string, error) {
	return "", g.err
}

// staticGenerator records the generation it was asked to answer so a test can
// check what actually reached the generator, not just what came back.
type staticGenerator struct {
	content string
	calls   int
	got     Generation
}

func (g *staticGenerator) Generate(_ context.Context, generation Generation) (string, error) {
	g.calls++
	g.got = generation
	return g.content, nil
}

func groundedRequest() Request {
	return Request{
		UserID:                 "user-1",
		SessionID:              "session-1",
		Content:                "AOF 怎么配",
		SessionMode:            domain.SessionModeChat,
		AnswerStrategy:         domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled:       true,
		KnowledgeCollectionIDs: []string{"collection-1"},
	}
}

func TestStrategyResponderRoutesByResolvedStrategy(t *testing.T) {
	direct, err := NewEchoStrategy(domain.AnswerStrategyDirectChat)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewEchoStrategy(domain.AnswerStrategyAgent)
	if err != nil {
		t.Fatal(err)
	}
	grounded := NewKnowledgeStrategy(&recordingRetriever{}, stubTitles{}, &staticGenerator{content: "answer"})
	responder, err := NewStrategyResponder(direct, agent, grounded)
	if err != nil {
		t.Fatal(err)
	}

	answer, err := responder.Respond(context.Background(), Request{
		Content:        "question",
		AnswerStrategy: domain.AnswerStrategyDirectChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.Content != "question" || len(answer.Sources) != 0 {
		t.Fatalf("direct answer = %#v", answer)
	}
	if _, err := responder.Respond(context.Background(), Request{Content: "x", AnswerStrategy: domain.AnswerStrategy("nope")}); !errors.Is(err, ErrStrategyNotConfigured) {
		t.Fatalf("unknown strategy error = %v", err)
	}
	// A missing implementation must not be papered over with an ungrounded
	// answer that still looks like a retrieved one.
	only := mustResponder(t, direct)
	if _, err := only.Respond(context.Background(), groundedRequest()); !errors.Is(err, ErrStrategyNotConfigured) {
		t.Fatalf("missing knowledge strategy error = %v", err)
	}
}

func TestStrategyResponderRejectsBadWiring(t *testing.T) {
	direct, err := NewEchoStrategy(domain.AnswerStrategyDirectChat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStrategyResponder(direct, direct); err == nil {
		t.Fatal("duplicate strategies were accepted")
	}
	if _, err := NewStrategyResponder(nil); err == nil {
		t.Fatal("a nil strategy was accepted")
	}
	unknown, err := NewEchoStrategy(domain.AnswerStrategy("made-up"))
	if err == nil && unknown != nil {
		t.Fatal("an echo strategy accepted an unknown answer strategy")
	}
}

func mustResponder(t *testing.T, strategies ...Strategy) Responder {
	t.Helper()
	responder, err := NewStrategyResponder(strategies...)
	if err != nil {
		t.Fatal(err)
	}
	return responder
}

// TestKnowledgeStrategyAsksOnlyForTheAuthorizedScope pins the query the
// retriever receives, which is the only place the per-turn collection list is
// turned into a search.
func TestKnowledgeStrategyAsksOnlyForTheAuthorizedScope(t *testing.T) {
	retriever := &recordingRetriever{results: []retrieval.Result{{
		Chunk: domain.DocumentChunk{ID: "chunk-1", DocumentID: "doc-1", CollectionID: "collection-1", Order: 1, Section: "Redis", Text: "AOF 以追加写记录命令。"},
	}}}
	strategy := NewKnowledgeStrategy(retriever, stubTitles{titles: map[string]string{"doc-1": "redis.md"}}, ExcerptGenerator{})
	answer, err := strategy.Respond(context.Background(), groundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(retriever.queries) != 1 {
		t.Fatalf("retriever calls = %d, want 1", len(retriever.queries))
	}
	query := retriever.queries[0]
	if query.UserID != "user-1" || query.Text != "AOF 怎么配" || len(query.CollectionIDs) != 1 || query.CollectionIDs[0] != "collection-1" {
		t.Fatalf("retriever received %#v", query)
	}
	if len(answer.Sources) != 1 {
		t.Fatalf("sources = %#v", answer.Sources)
	}
	source := answer.Sources[0]
	if source.DocumentID != "doc-1" || source.ChunkID != "chunk-1" || source.CollectionID != "collection-1" || source.Title != "redis.md" {
		t.Fatalf("citation provenance is wrong: %#v", source)
	}
	if !strings.Contains(source.Snippet, "AOF") {
		t.Fatalf("snippet = %q", source.Snippet)
	}
	// Scores stay inside the server: the citation structure has no score field.
	if strings.Contains(strings.Join([]string{source.SourceID, source.ChunkID}, " "), "score") {
		t.Fatal("a score leaked into the citation identifiers")
	}
	if !strings.Contains(answer.Content, "redis.md") || !strings.Contains(answer.Content, "未接入生成模型") {
		t.Fatalf("extractive answer does not label itself: %q", answer.Content)
	}
}

func TestKnowledgeStrategySyncAndStreamAgree(t *testing.T) {
	build := func() *KnowledgeStrategy {
		return NewKnowledgeStrategy(&recordingRetriever{results: []retrieval.Result{{
			Chunk: domain.DocumentChunk{ID: "chunk-1", DocumentID: "doc-1", CollectionID: "collection-1", Order: 1, Text: "内容"},
		}}}, stubTitles{titles: map[string]string{"doc-1": "redis.md"}}, ExcerptGenerator{})
	}
	sync, err := build().Respond(context.Background(), groundedRequest())
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	streamed, err := build().Stream(context.Background(), groundedRequest(), func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if sync.Content != streamed.Content {
		t.Fatalf("sync and stream answered differently:\n%q\n%q", sync.Content, streamed.Content)
	}
	if strings.Join(deltas, "") != streamed.Content {
		t.Fatal("streamed deltas do not reassemble into the answer")
	}
	if len(streamed.Sources) != len(sync.Sources) || streamed.Sources[0].ChunkID != sync.Sources[0].ChunkID {
		t.Fatalf("stream lost the citations: %#v", streamed.Sources)
	}
}

// TestKnowledgeStrategyReportsNothingWhenNothingWasFound is the anti-fabrication
// rule: an empty retrieval must produce an empty citation set.
func TestKnowledgeStrategyReportsNothingWhenNothingWasFound(t *testing.T) {
	cases := []struct {
		name      string
		retriever *recordingRetriever
	}{
		{"no candidates in scope", &recordingRetriever{err: retrieval.ErrNoCandidates}},
		{"empty result set", &recordingRetriever{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			answer, err := NewKnowledgeStrategy(testCase.retriever, stubTitles{}, ExcerptGenerator{}).Respond(context.Background(), groundedRequest())
			if err != nil {
				t.Fatal(err)
			}
			if len(answer.Sources) != 0 {
				t.Fatalf("fabricated %d citations: %#v", len(answer.Sources), answer.Sources)
			}
			if !strings.Contains(answer.Content, "没有检索到") {
				t.Fatalf("answer should say nothing was found: %q", answer.Content)
			}
		})
	}
}

func TestKnowledgeStrategyPropagatesFailures(t *testing.T) {
	boom := errors.New("index unavailable")
	strategy := NewKnowledgeStrategy(&recordingRetriever{err: boom}, stubTitles{}, ExcerptGenerator{})
	if _, err := strategy.Respond(context.Background(), groundedRequest()); !errors.Is(err, boom) {
		t.Fatalf("retriever failure error = %v", err)
	}

	generatorErr := errors.New("generator unavailable")
	strategy = NewKnowledgeStrategy(&recordingRetriever{results: []retrieval.Result{{
		Chunk: domain.DocumentChunk{ID: "chunk-1", DocumentID: "doc-1", Text: "内容"},
	}}}, stubTitles{}, failingGenerator{err: generatorErr})
	if _, err := strategy.Respond(context.Background(), groundedRequest()); !errors.Is(err, generatorErr) {
		t.Fatalf("generator failure error = %v", err)
	}

	// Retrieving without an authorized scope is a server bug, not a bad request.
	if _, err := NewKnowledgeStrategy(&recordingRetriever{}, stubTitles{}, ExcerptGenerator{}).Respond(context.Background(), Request{
		AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
	}); err == nil {
		t.Fatal("a grounded strategy ran with no collection scope")
	}
	if _, err := (&KnowledgeStrategy{}).Respond(context.Background(), groundedRequest()); !errors.Is(err, ErrStrategyNotConfigured) {
		t.Fatalf("unconfigured strategy error = %v", err)
	}
}

func TestEchoStrategyAnswersWithoutGrounding(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []domain.AnswerStrategy{domain.AnswerStrategyDirectChat, domain.AnswerStrategyAgent} {
		strategy, err := NewEchoStrategy(kind)
		if err != nil {
			t.Fatal(err)
		}
		if strategy.Strategy() != kind {
			t.Fatalf("strategy reports %q, want %q", strategy.Strategy(), kind)
		}
		answer, err := strategy.Respond(ctx, Request{Content: "问题", AnswerStrategy: kind, KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{"collection-1"}})
		if err != nil {
			t.Fatal(err)
		}
		// Even when a caller asks for grounding, the echo strategy cannot
		// retrieve: it must return the plain answer and no citations.
		if answer.Content != "问题" || len(answer.Sources) != 0 {
			t.Fatalf("%s answer = %#v", kind, answer)
		}
	}
}
