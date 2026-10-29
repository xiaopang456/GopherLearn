package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
	"github.com/xiaopang456/GopherLearn/internal/security"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// fixtureMarkdown is the seeded knowledge content. It is written in Chinese
// because the tokenizer's CJK path and the product's real content both need to
// be exercised by the contract, not just by unit tests.
const fixtureMarkdown = "# Redis 持久化\n\n" +
	"Redis 提供 RDB 快照与 AOF 追加日志两种持久化方式，取舍点在于恢复速度与写入 durability。\n\n" +
	"## AOF\n\n" +
	"AOF 以追加写记录每条写命令，fsync 策略可以选 always、everysec 或 no。\n\n" +
	"## RDB\n\n" +
	"RDB 在 fork 出的子进程里生成内存快照，代价与实例内存大小相关。\n\n" +
	"# 分布式锁\n\n" +
	"SETNX 配合过期时间是常见做法，Redlock 的安全性存在争议。\n"

// apiFixture is the seeded application state behind the HTTP tests.
type apiFixture struct {
	server    *Server
	knowledge *knowledge.MemoryStore
	service   *service.KnowledgeService
	// retriever is the same pipeline the knowledge strategy searches through, so a test can ask
	// "what did the server actually retrieve" instead of building a second pipeline and hoping
	// it agrees. A citation assertion that re-created the retriever would be comparing two
	// constructions rather than one truth.
	retriever  *retrieval.Pipeline
	store      repository.Repository
	memory     *service.MemoryService
	tokens     *security.TokenManager
	owner      string
	other      string
	session    string
	collection string
	document   string
	// foreignCollection and foreignDocument belong to the second user, so a
	// request can name a collection or a document the caller must not read.
	foreignCollection string
	foreignDocument   string
	// memoryDraft, memoryActive and foreignDraft exist only on a fixture built by
	// seedMemory: one row the owner has not decided on, one they confirmed, and one that
	// belongs to the other account. memoryMessage and memoryForeignSession are the
	// provenance those rows cite, so a case can name a message that really exists - and a
	// session that belongs to somebody else.
	memoryDraft          string
	memoryActive         string
	foreignDraft         string
	memoryMessage        string
	memoryForeignSession string
	// learningSession is a learning-agent conversation, and learningQuiz is an open quiz
	// on it with one question already graded. Only a fixture built for the quiz workflow
	// fills them; every other case leaves them empty and never names them.
	learningSession     string
	learningBusySession string
	learningQuiz        string
	// agentSession is a learning-agent conversation with nothing open on it, and the only
	// place a topic-memory row can store a decision that the mode accepts.
	agentSession string
}

// newFixture builds the server with the real knowledge stack installed: a memory
// knowledge store holding one owned collection and one Markdown document, a
// retrieval pipeline over the lexical and deterministic-vector scorers, and the
// knowledge_qa strategy on top of it. direct_chat and agent keep using the
// supplied double so request capture still works unchanged.
func newFixture(t *testing.T, assistant responder.Responder) *apiFixture {
	t.Helper()
	return newFixtureWithChat(t, assistant, nil)
}

// newFixtureWithChat builds the same seeded application, but routes direct_chat
// and knowledge_qa through the supplied chat model when one is given. A nil model
// keeps the echo and extractive wiring, which is what the HTTP golden measures.
func newFixtureWithChat(t *testing.T, assistant responder.Responder, chat model.ChatModel) *apiFixture {
	t.Helper()
	ctx := context.Background()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateUser(ctx, &domain.User{Email: "other@example.com", DisplayName: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := security.NewTokenManagerWithLifetime("test-secret", "test-issuer", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	knowledgeStore := knowledge.NewMemoryStore()
	knowledgeService := service.NewKnowledgeService(knowledgeStore)
	collection, err := knowledgeService.CreateCollection(ctx, user.ID, "运行手册")
	if err != nil {
		t.Fatal(err)
	}
	document, err := knowledgeService.AddDocument(ctx, user.ID, collection.ID, service.DocumentInput{
		Title:   "redis-persistence.md",
		Format:  "markdown",
		Content: fixtureMarkdown,
	})
	if err != nil {
		t.Fatal(err)
	}

	foreign, err := knowledgeService.CreateCollection(ctx, other.ID, "别人的手册")
	if err != nil {
		t.Fatal(err)
	}
	foreignDoc, err := knowledgeService.AddDocument(ctx, other.ID, foreign.ID, service.DocumentInput{
		Title:   "private-notes.md",
		Format:  "markdown",
		Content: "# 别人的部署手册\n\n这段内容不属于当前用户，任何读取都应报 not_found。\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	pipeline := retrieval.NewPipeline(
		knowledge.NewChunkSource(knowledgeStore),
		[]retrieval.Scorer{
			retrieval.NewLexicalScorer(retrieval.BM25Options{}),
			retrieval.NewVectorScorer(retrieval.NewDeterministicEmbedder(0), retrieval.VectorOptions{}),
		},
		retrieval.Options{},
	)
	var generator responder.Generator = responder.ExcerptGenerator{}
	directStrategy := responder.Strategy(captureStrategy{kind: domain.AnswerStrategyDirectChat, body: assistant})
	if chat != nil {
		generator, err = responder.NewModelGenerator(chat, responder.KnowledgeSystemPrompt)
		if err != nil {
			t.Fatal(err)
		}
		directStrategy, err = responder.NewModelStrategy(domain.AnswerStrategyDirectChat, chat, responder.DirectSystemPrompt)
		if err != nil {
			t.Fatal(err)
		}
	}
	assistantForTurns, err := responder.NewStrategyResponder(
		directStrategy,
		responder.NewKnowledgeStrategy(pipeline, knowledgeService, generator),
		captureStrategy{kind: domain.AnswerStrategyAgent, body: assistant},
	)
	if err != nil {
		t.Fatal(err)
	}

	memoryService := service.NewMemoryService(store)
	server := NewServer(
		service.NewAuthService(store, tokens, time.Now),
		service.NewSessionService(store),
		service.NewMessageService(store, assistantForTurns, service.WithCollectionAccess(knowledgeService)),
		tokens,
		WithKnowledge(knowledgeService),
		WithMemory(memoryService),
	)
	return &apiFixture{
		server:            server,
		knowledge:         knowledgeStore,
		retriever:         pipeline,
		service:           knowledgeService,
		store:             store,
		memory:            memoryService,
		tokens:            tokens,
		owner:             user.ID,
		other:             other.ID,
		session:           session.ID,
		collection:        collection.ID,
		document:          document.ID,
		foreignCollection: foreign.ID,
		foreignDocument:   foreignDoc.ID,
	}
}

// seedMemory puts three rows in front of the confirmation surface: a pending draft the
// owner has not decided on, an active memory the owner confirmed, and a pending draft that
// belongs to the other account. It runs only for cases that ask for it, because it consumes
// identifiers from the repository's shared sequence and would otherwise shift the literal
// user id that the authentication rows in the golden file compare exactly.
func seedMemory(t *testing.T, fixture *apiFixture) *apiFixture {
	t.Helper()
	ctx := context.Background()
	message, err := fixture.store.AppendMessage(ctx, fixture.owner, fixture.session, &domain.Message{
		Role: "user", Content: "我确认这条事实要长期记住。",
	})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := fixture.memory.CreateMemoryDraft(ctx, fixture.owner, domain.MemoryDraft{
		Scope: domain.MemoryScopeGlobal, Content: "用户用 Go 写后端服务", Confidence: 1,
		SourceSessionID: fixture.session, SourceMessageIDs: []string{message.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := fixture.memory.CreateMemoryDraft(ctx, fixture.owner, domain.MemoryDraft{
		Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "AOF 的 fsync 策略选 everysec", Confidence: 1,
		SourceSessionID: fixture.session, SourceMessageIDs: []string{message.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.memory.ConfirmMemoryDraft(ctx, fixture.owner, active.MemoryID); err != nil {
		t.Fatal(err)
	}

	otherSession, err := fixture.store.CreateSession(ctx, &domain.Session{UserID: fixture.other, Title: "Other", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := fixture.store.AppendMessage(ctx, fixture.other, otherSession.ID, &domain.Message{Role: "user", Content: "与我无关"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := fixture.memory.CreateMemoryDraft(ctx, fixture.other, domain.MemoryDraft{
		Scope: domain.MemoryScopeEpisodic, Content: "别人的一次经历", Confidence: 1,
		SourceSessionID: otherSession.ID, SourceMessageIDs: []string{otherMessage.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.memoryDraft = draft.MemoryID
	fixture.memoryActive = active.MemoryID
	fixture.foreignDraft = foreign.MemoryID
	fixture.memoryMessage = message.ID
	fixture.memoryForeignSession = otherSession.ID
	return fixture
}

// newMemoryFixture is the plain fixture plus the seeded memory state, for the contract
// cases that exercise the confirmation surface.
// newAgentContractFixture is the plain fixture plus one learning-agent conversation with nothing
// open on it. That is the state a learner is in after starting a learning conversation and before
// asking for a quiz, and it is the state the topic-memory switch has to be able to record - a
// decision stored, and no subject for it to unlock yet.
func newAgentContractFixture(t *testing.T) *apiFixture {
	t.Helper()
	fixture := newFixture(t, apiResponder{})
	session, err := fixture.store.CreateSession(t.Context(), &domain.Session{
		UserID: fixture.owner, Title: "无测验的学习会话", Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	fixture.agentSession = session.ID
	return fixture
}

func newMemoryFixture(t *testing.T) *apiFixture {
	t.Helper()
	return seedMemory(t, newFixture(t, apiResponder{}))
}

// captureStrategy adapts any Responder to one answer strategy so the existing
// doubles keep observing resolved turns.
type captureStrategy struct {
	kind domain.AnswerStrategy
	body responder.Responder
}

func (s captureStrategy) Strategy() domain.AnswerStrategy { return s.kind }

func (s captureStrategy) Respond(ctx context.Context, request responder.Request) (responder.Answer, error) {
	if s.body == nil {
		return responder.Answer{}, nil
	}
	return s.body.Respond(ctx, request)
}

func (s captureStrategy) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	if s.body == nil {
		return responder.Answer{}, nil
	}
	return s.body.Stream(ctx, request, emit)
}

// The step faces are forwarded only if the wrapped responder actually has them. A
// decorator that cannot answer the question is reported as unable, which is why forgetting
// to forward shows up as a refused request rather than as a stream that quietly stops
// reporting progress - the failure mode an optional interface always risks.
func (s captureStrategy) SupportsSteps(request responder.Request) bool {
	capable, ok := s.body.(interface{ SupportsSteps(responder.Request) bool })
	return ok && capable.SupportsSteps(request)
}

func (s captureStrategy) StreamSteps(ctx context.Context, request responder.Request, emit func(string) error,
	onStep responder.StepSink,
) (responder.Answer, error) {
	streamer, ok := s.body.(responder.StepStreamer)
	if !ok {
		return s.Stream(ctx, request, emit)
	}
	return streamer.StreamSteps(ctx, request, emit, onStep)
}
