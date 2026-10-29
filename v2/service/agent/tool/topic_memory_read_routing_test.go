package agenttool_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"
	agenttool "GopherAI/service/agent/tool"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// This file is the routing half of STABILIZE-040, and it is an end-to-end test
// on purpose. The question the experiment asks - "does the Agent reach for the
// narrow tool when the question is about the current topic, and leave it alone
// otherwise" - spans the mastery read, the window, the runner's tool wiring and
// two tools that have to agree about what has already been issued. A test of
// any one of them would assert an interface rather than the behaviour.

var routingClock = time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------

type routingLearning struct {
	session *model.LearningSession
	calls   atomic.Int32
}

func (r *routingLearning) GetActive(_ context.Context, userID int64,
	sessionID string) (*model.LearningSession, error) {
	r.calls.Add(1)
	if r.session == nil || r.session.UserID != userID || r.session.SessionID != sessionID {
		return nil, learningdao.ErrLearningSessionNotFound
	}
	return r.session, nil
}

func (r *routingLearning) GetQuestion(context.Context, int64, string, int) (*model.LearningQuestion, error) {
	return nil, learningdao.ErrLearningSessionNotFound
}

type emptyProgress struct{}

func (emptyProgress) ListTopicProgress(context.Context, int64, int) ([]model.TopicProgress, error) {
	return nil, nil
}

type emptyHistory struct{}

func (emptyHistory) ListRecentAfter(context.Context, string, uint, int) ([]model.Message, error) {
	return nil, nil
}

type discardSteps struct{}

func (discardSteps) AppendStep(context.Context, string, *model.AgentStep) error { return nil }

// routingChat replays a script of model turns and records which tools it was
// offered, so a test can assert the surface as well as the choice.
type routingChat struct {
	respond    func(call int, input []*schema.Message) *schema.Message
	calls      atomic.Int32
	toolNames  atomic.Value
	firstInput atomic.Value
}

func (c *routingChat) Generate(_ context.Context, input []*schema.Message,
	_ ...modelcomponent.Option) (*schema.Message, error) {
	call := int(c.calls.Add(1))
	if call == 1 {
		var assembled strings.Builder
		for _, message := range input {
			assembled.WriteString(message.Content)
			assembled.WriteString("\n")
		}
		c.firstInput.Store(assembled.String())
	}
	message := c.respond(call, input)
	if message.ResponseMeta == nil {
		message.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
		if len(message.ToolCalls) > 0 {
			message.ResponseMeta.FinishReason = "tool_calls"
		}
	}
	return message, nil
}

func (c *routingChat) Stream(ctx context.Context, input []*schema.Message,
	opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := c.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	writer.Send(message, nil)
	writer.Close()
	return reader, nil
}

func (c *routingChat) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	names := make([]string, 0, len(tools))
	for _, info := range tools {
		names = append(names, info.Name)
	}
	c.toolNames.Store(names)
	return c, nil
}

func (c *routingChat) offered() []string {
	value, _ := c.toolNames.Load().([]string)
	return value
}

func (c *routingChat) initialContext() string {
	value, _ := c.firstInput.Load().(string)
	return value
}

// fakeSearch is a memory_search stand-in with the real envelope shape. It
// registers episodic evidence through the run's own registry, which is what
// makes the shared issued-record dedup a real interaction rather than a mock.
type fakeSearchInput struct {
	Query string   `json:"query" jsonschema:"required"`
	Kinds []string `json:"kinds,omitempty"`
}

type fakeSearchCard struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Content string `json:"content"`
}

type fakeSearchOutput struct {
	Status  string           `json:"status"`
	Reason  string           `json:"reason"`
	Query   string           `json:"query"`
	Summary string           `json:"summary"`
	Results []fakeSearchCard `json:"results"`
}

type fakeSearchRegistry struct {
	cards      []fakeSearchCard
	ids        []string
	versions   []uint64
	executions atomic.Int32
	lastKinds  atomic.Value
}

func (r *fakeSearchRegistry) Tools(_ context.Context,
	run serviceagent.RunContext) ([]tool.BaseTool, error) {
	evidence := run.MemoryEvidence
	if evidence == nil {
		return nil, fmt.Errorf("the run carries no memory evidence registry")
	}
	built, err := toolutils.InferTool("memory_search", "search the learner's long-term memory",
		func(_ context.Context, input *fakeSearchInput) (*fakeSearchOutput, error) {
			r.executions.Add(1)
			r.lastKinds.Store(append([]string(nil), input.Kinds...))
			issued := make([]fakeSearchCard, 0, len(r.cards))
			refs := make([]serviceagent.MemoryEvidenceRef, 0, len(r.cards))
			suppressed := 0
			for index, card := range r.cards {
				kind := serviceagent.MemoryEvidenceTopic
				if card.Kind == "episodic" {
					kind = serviceagent.MemoryEvidenceEpisodic
				}
				id, version := r.ids[index], r.versions[index]
				// The same check the real tool performs under v3.
				if evidence.IssuedRecord(kind, id, version) {
					suppressed++
					continue
				}
				ref := evidence.Register(serviceagent.MemoryEvidenceEntry{
					Kind: kind, InternalID: id, Version: version, Owner: run.UserID,
					Subject: card.Subject, Status: string(model.LongTermMemoryActive),
				})
				card.Ref = ref
				issued = append(issued, card)
				refs = append(refs, serviceagent.MemoryEvidenceRef{
					Ref: ref, Kind: string(kind), InternalID: id, Version: version,
				})
			}
			status, reason := "ok", "none"
			if len(issued) == 0 {
				status, reason = "no_evidence", "no_candidate"
				if suppressed > 0 {
					reason = "already_in_context"
				}
			}
			evidence.Record(serviceagent.MemorySearchCall{
				Status: status, Reason: reason, Query: input.Query,
				Kinds: input.Kinds, Refs: refs, SuppressedHits: suppressed,
			})
			return &fakeSearchOutput{Status: status, Reason: reason, Query: input.Query,
				Summary: "memory", Results: issued}, nil
		})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{built}, nil
}

func (r *fakeSearchRegistry) kinds() []string {
	value, _ := r.lastKinds.Load().([]string)
	return value
}

// --- harness -------------------------------------------------------------

type routingSetup struct {
	chat     *routingChat
	search   *fakeSearchRegistry
	provider *countingTopicProvider
	runner   *serviceagent.Runner
}

// countingTopicProvider is the exact-key reader, counting database reads so a
// test can tell "the tool was called" from "the database was read".
type countingTopicProvider struct {
	records       []serviceagent.TopicMemoryRecord
	eligibleCount int
	reads         atomic.Int32
	err           error
}

func (p *countingTopicProvider) TopicMemoriesByKeys(_ context.Context, _ serviceagent.RunContext,
	_ []string) ([]serviceagent.TopicMemoryRecord, error) {
	p.reads.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	return p.records, nil
}

func (p *countingTopicProvider) TopicMemorySnapshotByKeys(_ context.Context, _ serviceagent.RunContext,
	_ []string) (serviceagent.AuthoritativeTopicMemorySnapshot, error) {
	p.reads.Add(1)
	if p.err != nil {
		return serviceagent.AuthoritativeTopicMemorySnapshot{}, p.err
	}
	eligible := p.eligibleCount
	if eligible == 0 {
		eligible = len(p.records)
	}
	return serviceagent.AuthoritativeTopicMemorySnapshot{
		Records: p.records, EligibleCount: eligible,
	}, nil
}

func routingRecord(id, topicKey, subject, content string, version uint64) serviceagent.TopicMemoryRecord {
	return serviceagent.TopicMemoryRecord{
		ID: id, Version: version, Owner: 1, Status: string(model.LongTermMemoryActive),
		Scope: string(model.MemoryScopeTopic), TopicKey: topicKey, Subject: subject, Content: content,
		Line: fmt.Sprintf("[topic; tags=%s] %s: %s", topicKey, subject, content),
	}
}

// newRoutingSetup builds a whole v3 turn over fakes: the real mastery provider
// so the authoritative-key rules are the production ones, the real context
// builder under agent_memory_tool_v3, the real topic_memory_read, a fake
// memory_search, and the real runner budget.
func newRoutingSetup(t *testing.T, session *model.LearningSession,
	records []serviceagent.TopicMemoryRecord,
	respond func(call int, input []*schema.Message) *schema.Message) *routingSetup {
	t.Helper()
	return newRoutingSetupInMode(t, memoryrecall.ModeAgentMemoryToolV3, session, records, respond)
}

func newRoutingSetupInMode(t *testing.T, mode memoryrecall.ReadMode, session *model.LearningSession,
	records []serviceagent.TopicMemoryRecord,
	respond func(call int, input []*schema.Message) *schema.Message) *routingSetup {
	t.Helper()
	learning := &routingLearning{session: session}
	mastery, err := serviceagent.NewMasteryProvider(emptyProgress{}, learning,
		serviceagent.DefaultMasteryConfig())
	if err != nil {
		t.Fatal(err)
	}
	mastery.UseClock(func() time.Time { return routingClock })

	config := serviceagent.DefaultContextConfig()
	config.MemoryReadMode = mode
	builder, err := serviceagent.NewContextBuilder(config, emptyHistory{}, nil, mastery, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(serviceagent.GlobalMemoryProviderFunc(
		func(context.Context, serviceagent.RunContext) ([]string, error) {
			return []string{"讲解偏好：先说结论"}, nil
		}))

	provider := &countingTopicProvider{records: records}
	search := &fakeSearchRegistry{
		cards: []fakeSearchCard{{Kind: "episodic", Subject: "上一次的结论", Content: "决定改用长度前缀"}},
		ids:   []string{"e-1"}, versions: []uint64{1730000100},
	}
	var registry serviceagent.ToolRegistry = search
	runnerConfig := serviceagent.AgentMemoryToolRunnerConfig()
	if mode.UsesTopicMemoryReadTool() {
		var topicRegistry serviceagent.ToolRegistry
		var topicErr error
		if mode.UsesTopicMemoryReadObservationV4() {
			topicRegistry, topicErr = agenttool.NewTopicMemoryReadV4Registry(provider,
				agenttool.DefaultTopicMemoryReadConfig())
		} else {
			topicRegistry, topicErr = agenttool.NewTopicMemoryReadRegistry(provider,
				agenttool.DefaultTopicMemoryReadConfig())
		}
		if topicErr != nil {
			t.Fatal(topicErr)
		}
		registry = serviceagent.CompositeToolRegistry{Registries: []serviceagent.ToolRegistry{
			search, topicRegistry,
		}}
		runnerConfig = serviceagent.AgentMemoryToolV3RunnerConfig()
	}
	if mode.UsesAuthoritativeTopicBootstrap() {
		builder.UseAuthoritativeTopicMemoryProvider(provider)
	}

	chat := &routingChat{respond: respond}
	runner, err := serviceagent.NewRunner(chat, registry, discardSteps{}, builder, runnerConfig)
	if err != nil {
		t.Fatal(err)
	}
	return &routingSetup{chat: chat, search: search, provider: provider, runner: runner}
}

func routingRun() serviceagent.RunContext {
	return serviceagent.RunContext{
		RunID: "run-1", UserID: 1, UserName: "learner", SessionID: "session-1",
		Question: "这个我还是没搞懂",
	}
}

func activeQuiz(topic string) *model.LearningSession {
	return &model.LearningSession{
		ID: "l-1", UserID: 1, SessionID: "session-1", AgentRunID: "r-1",
		Topic: topic, TopicKey: model.NormalizeLearningTopicKey(topic),
		Status: model.LearningSessionWaitingAnswer, CurrentQuestion: 1, TotalQuestions: 3,
		Version: 1, ExpiresAt: routingClock.Add(time.Hour),
	}
}

func topicReadToolCall(id int) *schema.Message {
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID: fmt.Sprintf("call-%d", id),
		Function: schema.FunctionCall{Name: agenttool.TopicMemoryReadName,
			Arguments: `{"reason":"asked about their understanding of the current topic"}`},
	}})
}

func searchToolCall(id int, query string, kinds ...string) *schema.Message {
	arguments, _ := json.Marshal(map[string]any{"query": query, "kinds": kinds})
	return schema.AssistantMessage("", []schema.ToolCall{{
		ID:       fmt.Sprintf("call-%d", id),
		Function: schema.FunctionCall{Name: "memory_search", Arguments: string(arguments)},
	}})
}

// issuedRefs reads the labels this turn actually handed the model back out of
// the conversation, so a scripted answer cites what exists rather than what the
// test author guessed. Reference numbers are one sequence across all four
// kinds, so the number a tool gets depends on how much the window issued first
// - which is exactly the kind of detail a test must not hard-code.
var refPattern = regexp.MustCompile(`"ref":"([GSEC][0-9]{1,3})"`)

func issuedRefs(input []*schema.Message, prefix string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, message := range input {
		for _, match := range refPattern.FindAllStringSubmatch(message.Content, -1) {
			if !strings.HasPrefix(match[1], prefix) {
				continue
			}
			if _, duplicate := seen[match[1]]; duplicate {
				continue
			}
			seen[match[1]] = struct{}{}
			out = append(out, match[1])
		}
	}
	return out
}

// cite renders the labels of the requested prefixes, or a plain sentence when
// the turn issued none of them.
func cite(input []*schema.Message, prefixes ...string) string {
	var out strings.Builder
	out.WriteString("这是回答。")
	found := false
	for _, prefix := range prefixes {
		for _, ref := range issuedRefs(input, prefix) {
			out.WriteString("[" + ref + "]")
			found = true
		}
	}
	if !found {
		return "长期记忆里没有可以支持这一点的记录。"
	}
	return out.String()
}

// --- the routing cases ---------------------------------------------------

// Case 1. Active quiz, question about the current topic: the narrow tool alone,
// the right reference, and not one embedding or vector query behind it.
func TestActiveQuizTopicQuestionUsesOnlyTheNarrowTool(t *testing.T) {
	records := []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000),
	}
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), records, func(call int, input []*schema.Message) *schema.Message {
		if call == 1 {
			return topicReadToolCall(1)
		}
		return schema.AssistantMessage(cite(input, "S"), nil)
	})
	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	// Both tools were on the table; only one was used.
	offered := setup.chat.offered()
	if len(offered) != 2 {
		t.Fatalf("tools offered = %v", offered)
	}
	if setup.search.executions.Load() != 0 {
		t.Fatalf("the general search ran %d times", setup.search.executions.Load())
	}
	if setup.provider.reads.Load() != 1 {
		t.Fatalf("the exact reader ran %d times", setup.provider.reads.Load())
	}
	if len(result.TopicMemoryReads) != 1 || len(result.TopicMemoryReads[0].Refs) != 1 {
		t.Fatalf("topic reads = %#v", result.TopicMemoryReads)
	}
	if len(result.MemoryEvidence) != 0 {
		t.Fatalf("a general search was recorded: %#v", result.MemoryEvidence)
	}
	if result.MemoryCitation.Failed() || len(result.MemoryCitation.Cited) != 1 {
		t.Fatalf("answer = %q citation = %#v", result.Answer, result.MemoryCitation)
	}
	entry := result.MemoryRefs[len(result.MemoryRefs)-1]
	if entry.InternalID != "m-1" || !strings.HasPrefix(entry.Source, "topic_memory_read:") {
		t.Fatalf("S1 = %#v", entry)
	}
}

// Case 2. Active quiz, question about an earlier session: the narrow tool is
// not called, and - the whole point of the mode - the initial context carried
// no topic memory to tempt it.
func TestActiveQuizEpisodeQuestionLeavesTheTopicToolAlone(t *testing.T) {
	records := []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000),
	}
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), records, func(call int, input []*schema.Message) *schema.Message {
		if call == 1 {
			return searchToolCall(1, "上一次会话里关于粘包的决定", "episodic")
		}
		return schema.AssistantMessage(cite(input, "E"), nil)
	})
	run := routingRun()
	run.Question = "上次我们最后定了什么？"
	result, err := setup.runner.Run(context.Background(), run, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if setup.provider.reads.Load() != 0 || len(result.TopicMemoryReads) != 0 {
		t.Fatalf("the topic tool ran: reads=%d trace=%#v",
			setup.provider.reads.Load(), result.TopicMemoryReads)
	}
	initial := setup.chat.initialContext()
	// The note itself, not the label: the system prompt legitimately names [S1]
	// and [S2] while explaining what the four kinds of reference mean.
	if strings.Contains(initial, "没有刻度的水管") {
		t.Fatalf("the initial context carried topic memory:\n%s", initial)
	}
	if result.ContextBudget.TopicUserMemory != 0 {
		t.Fatalf("the window spent %d tokens on topic memory", result.ContextBudget.TopicUserMemory)
	}
	if !strings.Contains(initial, "先说结论") {
		t.Fatalf("the initial context lost bounded global memory:\n%s", initial)
	}
	if result.MemoryCitation.Failed() || len(result.MemoryCitation.Cited) != 1 {
		t.Fatalf("answer = %q citation = %#v", result.Answer, result.MemoryCitation)
	}
}

// Case 3. Mixed: both tools, once each, and both kinds of evidence arrive under
// their own labels.
func TestMixedQuestionMayCallBothToolsOnce(t *testing.T) {
	records := []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000),
	}
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), records, func(call int, input []*schema.Message) *schema.Message {
		switch call {
		case 1:
			return topicReadToolCall(1)
		case 2:
			return searchToolCall(2, "上一次会话里关于粘包的决定", "episodic")
		}
		return schema.AssistantMessage(cite(input, "S", "E"), nil)
	})
	run := routingRun()
	run.Question = "我之前怎么理解粘包的，上次又定了什么？"
	result, err := setup.runner.Run(context.Background(), run, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.TopicMemoryReads) != 1 || len(result.MemoryEvidence) != 1 {
		t.Fatalf("topic reads = %d, searches = %d",
			len(result.TopicMemoryReads), len(result.MemoryEvidence))
	}
	if got := setup.search.kinds(); len(got) != 1 || got[0] != "episodic" {
		t.Fatalf("the general search was asked for %v", got)
	}
	if len(result.MemoryCitation.Cited) != 2 {
		t.Fatalf("answer = %q cited %v", result.Answer, result.MemoryCitation.Cited)
	}
	if result.MemoryCitation.Failed() {
		t.Fatalf("a correct two-kind citation failed the check: %#v", result.MemoryCitation)
	}
	if result.ToolCalls != 2 || result.ModelCalls != 3 {
		t.Fatalf("tools = %d, model = %d", result.ToolCalls, result.ModelCalls)
	}
}

// Case 4. No active quiz: the tool exists, says so honestly, reaches no
// database, and issues nothing. memory_search is still available and still
// works, which is what keeps a learner without a quiz on v1's behaviour.
func TestWithoutAnActiveQuizTheTopicToolProducesNoEvidence(t *testing.T) {
	setup := newRoutingSetup(t, nil, []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "不该被读到", 1730000000),
	}, func(call int, input []*schema.Message) *schema.Message {
		switch call {
		case 1:
			return topicReadToolCall(1)
		case 2:
			return searchToolCall(2, "这位学习者对粘包的理解", "semantic")
		}
		return schema.AssistantMessage("长期记忆里没有当前主题的记录。", nil)
	})
	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if setup.provider.reads.Load() != 0 {
		t.Fatalf("the exact reader ran without an authoritative key")
	}
	if len(result.TopicMemoryReads) != 1 ||
		result.TopicMemoryReads[0].Status != agenttool.TopicReadStatusNoAuthoritativeTopic {
		t.Fatalf("topic reads = %#v", result.TopicMemoryReads)
	}
	if len(result.TopicMemoryReads[0].Refs) != 0 {
		t.Fatalf("a topic reference was issued without a topic: %#v", result.TopicMemoryReads[0].Refs)
	}
	for _, entry := range result.MemoryRefs {
		if entry.Kind == serviceagent.MemoryEvidenceTopic {
			t.Fatalf("topic evidence was issued without an authoritative key: %#v", entry)
		}
	}
	if setup.search.executions.Load() != 1 {
		t.Fatalf("the general search ran %d times", setup.search.executions.Load())
	}
}

// Case 5. The lifecycle rules are the mastery provider's, and they are the same
// two gates STABILIZE-039 installed. A finished, expired or foreign-session quiz
// is a fine reason to keep pitching an explanation at that topic and never a
// reason to read the learner's private notes.
func TestNonAuthoritativeQuizzesNeverReachTheTopicTool(t *testing.T) {
	for name, session := range map[string]*model.LearningSession{
		"completed": func() *model.LearningSession {
			s := activeQuiz("RPC 粘包")
			s.Status = model.LearningSessionCompleted
			return s
		}(),
		"cancelled": func() *model.LearningSession {
			s := activeQuiz("RPC 粘包")
			s.Status = model.LearningSessionCancelled
			return s
		}(),
		"expired": func() *model.LearningSession {
			s := activeQuiz("RPC 粘包")
			s.ExpiresAt = routingClock.Add(-time.Minute)
			return s
		}(),
		"another chat session": func() *model.LearningSession {
			s := activeQuiz("RPC 粘包")
			s.SessionID = "session-9"
			return s
		}(),
		"another account": func() *model.LearningSession {
			s := activeQuiz("RPC 粘包")
			s.UserID = 9
			return s
		}(),
	} {
		setup := newRoutingSetup(t, session, []serviceagent.TopicMemoryRecord{
			routingRecord("m-1", "rpc-粘包", "粘包", "不该被读到", 1730000000),
		}, func(call int, input []*schema.Message) *schema.Message {
			if call == 1 {
				return topicReadToolCall(1)
			}
			return schema.AssistantMessage("没有当前主题的记录。", nil)
		})
		result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if setup.provider.reads.Load() != 0 {
			t.Fatalf("%s: a non-authoritative quiz reached the database", name)
		}
		if len(result.TopicMemoryReads) != 1 ||
			result.TopicMemoryReads[0].Status != agenttool.TopicReadStatusNoAuthoritativeTopic {
			t.Fatalf("%s: topic reads = %#v", name, result.TopicMemoryReads)
		}
		for _, entry := range result.MemoryRefs {
			if entry.Kind == serviceagent.MemoryEvidenceTopic {
				t.Fatalf("%s: topic evidence was issued: %#v", name, entry)
			}
		}
	}
}

// The two tools share one issued-record set, in both directions. Whichever ran
// first, the other must not hand the same row over under a second label.
func TestTheTwoMemoryToolsDeduplicateInBothDirections(t *testing.T) {
	shared := routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000)
	for name, order := range map[string][]string{
		"topic first":  {"topic", "search"},
		"search first": {"search", "topic"},
	} {
		setup := newRoutingSetup(t, activeQuiz("RPC 粘包"),
			[]serviceagent.TopicMemoryRecord{shared}, func(call int, input []*schema.Message) *schema.Message {
				if call <= len(order) {
					if order[call-1] == "topic" {
						return topicReadToolCall(call)
					}
					return searchToolCall(call, "这位学习者对粘包的理解", "semantic")
				}
				return schema.AssistantMessage(cite(input, "S"), nil)
			})
		// The general search returns the very same row the topic tool would.
		setup.search.cards = []fakeSearchCard{{Kind: "semantic", Subject: "粘包",
			Content: "把 TCP 当成没有刻度的水管"}}
		setup.search.ids = []string{"m-1"}
		setup.search.versions = []uint64{1730000000}

		result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		issued := 0
		for _, entry := range result.MemoryRefs {
			if entry.Kind == serviceagent.MemoryEvidenceTopic {
				issued++
				if entry.InternalID != "m-1" {
					t.Fatalf("%s: unexpected record %#v", name, entry)
				}
			}
		}
		if issued != 1 {
			t.Fatalf("%s: one record was issued %d times", name, issued)
		}
		// And the second tool said "nothing new", not "nothing recorded".
		suppressed := 0
		for _, call := range result.MemoryEvidence {
			suppressed += call.SuppressedHits
		}
		for _, read := range result.TopicMemoryReads {
			suppressed += read.SuppressedHits
		}
		if suppressed != 1 {
			t.Fatalf("%s: suppressed = %d", name, suppressed)
		}
	}
}

// Different version, different kind and different id are different records, and
// the dedup must not delete any of them.
func TestSharedDedupDoesNotRemoveDifferentRecords(t *testing.T) {
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "改过之后的理解", 1730000009),
	}, func(call int, input []*schema.Message) *schema.Message {
		switch call {
		case 1:
			return searchToolCall(1, "这位学习者对粘包的理解", "semantic")
		case 2:
			return topicReadToolCall(2)
		}
		return schema.AssistantMessage(cite(input, "S", "E"), nil)
	})
	// An older version of the same id, plus an episode that shares it.
	setup.search.cards = []fakeSearchCard{
		{Kind: "semantic", Subject: "粘包", Content: "旧版本的理解"},
		{Kind: "episodic", Subject: "上一次", Content: "决定改用长度前缀"},
	}
	setup.search.ids = []string{"m-1", "m-1"}
	setup.search.versions = []uint64{1730000000, 1730000000}

	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	topics, episodes := 0, 0
	for _, entry := range result.MemoryRefs {
		switch entry.Kind {
		case serviceagent.MemoryEvidenceTopic:
			topics++
		case serviceagent.MemoryEvidenceEpisodic:
			episodes++
		}
	}
	if topics != 2 || episodes != 1 {
		t.Fatalf("topics = %d, episodes = %d: the dedup removed a different record", topics, episodes)
	}
}

// The runner's per-capability limit is one, and it is enforced before the tool
// is entered rather than only inside it. A model that asks twice is stopped by
// the run budget, which is a different fuse from the tool's own: the tool
// refuses because it has nothing else to say, the runner because a turn only
// gets so many rounds.
func TestTheRunnerAllowsOneTopicReadPerTurn(t *testing.T) {
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000),
	}, func(call int, input []*schema.Message) *schema.Message {
		if call <= 2 {
			return topicReadToolCall(call)
		}
		return schema.AssistantMessage(cite(input, "S"), nil)
	})
	_, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err == nil {
		t.Fatal("a second topic read was allowed")
	}
	if !strings.Contains(err.Error(), agenttool.TopicMemoryReadName) {
		t.Fatalf("the second call was refused for the wrong reason: %v", err)
	}
	if setup.provider.reads.Load() != 1 {
		t.Fatalf("the exact reader ran %d times", setup.provider.reads.Load())
	}
	config := serviceagent.AgentMemoryToolV3RunnerConfig()
	if config.PerTool.TopicMemoryRead != 1 || config.MaxToolCalls != 6 ||
		config.MaxModelCalls != 7 || config.MaxGraphSteps != 16 ||
		config.Deadline != 45*time.Second {
		t.Fatalf("v3 budget = %#v", config)
	}
	if base := serviceagent.AgentMemoryToolRunnerConfig(); base.PerTool.TopicMemoryRead != 0 {
		t.Fatalf("v1 acquired a topic read budget: %#v", base)
	}
}

// A failing exact read loses the tool and nothing else: the turn continues, the
// general search is still available, and no fallback quietly answers the topic
// question from something else.
func TestAFailingExactReadDoesNotFailTheTurn(t *testing.T) {
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), nil, func(call int, input []*schema.Message) *schema.Message {
		switch call {
		case 1:
			return topicReadToolCall(1)
		case 2:
			return searchToolCall(2, "上一次会话里关于粘包的决定", "episodic")
		}
		return schema.AssistantMessage(cite(input, "E"), nil)
	})
	setup.provider.err = fmt.Errorf("mysql is unhappy")
	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatalf("a degraded topic read failed the turn: %v", err)
	}
	if len(result.TopicMemoryReads) != 1 || !result.TopicMemoryReads[0].Degraded {
		t.Fatalf("topic reads = %#v", result.TopicMemoryReads)
	}
	if result.MemoryCitation.Failed() || len(result.MemoryCitation.Cited) != 1 {
		t.Fatalf("answer = %q citation = %#v", result.Answer, result.MemoryCitation)
	}
}

// v1's turn does not change. Same window, same single tool, same absence of any
// topic read - which is what makes it the same baseline 033 through 039C were
// measured against.
func TestV1TurnIsUnchangedByTheNewTool(t *testing.T) {
	setup := newRoutingSetupInMode(t, memoryrecall.ModeAgentMemoryToolV1, activeQuiz("RPC 粘包"),
		[]serviceagent.TopicMemoryRecord{
			routingRecord("m-1", "rpc-粘包", "粘包", "不该被读到", 1730000000),
		}, func(call int, input []*schema.Message) *schema.Message {
			if call == 1 {
				return searchToolCall(1, "这位学习者对粘包的理解", "semantic")
			}
			return schema.AssistantMessage(cite(input, "E"), nil)
		})
	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if offered := setup.chat.offered(); len(offered) != 1 || offered[0] != "memory_search" {
		t.Fatalf("v1 was offered %v", offered)
	}
	if setup.provider.reads.Load() != 0 || len(result.TopicMemoryReads) != 0 {
		t.Fatalf("v1 read topic memory: %#v", result.TopicMemoryReads)
	}
	initial := setup.chat.initialContext()
	if strings.Contains(initial, "不该被读到") {
		t.Fatalf("v1's window carried topic memory:\n%s", initial)
	}
	if !strings.Contains(initial, "先说结论") {
		t.Fatalf("v1's window lost global memory:\n%s", initial)
	}
}

// v3's window never carries topic memory, and the builder refuses the turn
// rather than serving one that quietly behaved like v2. Every routing number
// the experiment reports depends on that holding on every case.
func TestV3WindowNeverCarriesTopicMemory(t *testing.T) {
	setup := newRoutingSetup(t, activeQuiz("RPC 粘包"), []serviceagent.TopicMemoryRecord{
		routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000),
	}, func(int, []*schema.Message) *schema.Message {
		return schema.AssistantMessage("长期记忆里没有相关记录。", nil)
	})
	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if result.ContextBudget.TopicUserMemory != 0 {
		t.Fatalf("the v3 window spent %d tokens on topic memory", result.ContextBudget.TopicUserMemory)
	}
	for _, entry := range result.MemoryRefs {
		if entry.Kind == serviceagent.MemoryEvidenceTopic {
			t.Fatalf("the v3 window issued a topic reference: %#v", entry)
		}
	}
	if setup.provider.reads.Load() != 0 {
		t.Fatal("the v3 window read topic memory on its own")
	}
}

// v4's new capability is not another route guessed before the read. The first
// observation names the server-held topic, after which the Agent may decide the
// user asked about a neighbour and use the general semantic search.
func TestV4MaySearchAfterObservingADifferentCurrentTopic(t *testing.T) {
	setup := newRoutingSetupInMode(t, memoryrecall.ModeAgentMemoryToolV4, activeQuiz("RPC 粘包"),
		[]serviceagent.TopicMemoryRecord{
			routingRecord("m-1", "rpc-粘包", "粘包", "把 TCP 当成没有刻度的水管", 1730000000),
		}, func(call int, input []*schema.Message) *schema.Message {
			switch call {
			case 1:
				return topicReadToolCall(1)
			case 2:
				return searchToolCall(2, "这位学习者对 Redis 过期删除的理解", "semantic")
			default:
				return schema.AssistantMessage(cite(input, "E"), nil)
			}
		})
	run := routingRun()
	run.Question = "我原来怎么理解 Redis 过期删除？"
	result, err := setup.runner.Run(context.Background(), run, serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.TopicMemoryReads) != 1 || len(result.MemoryEvidence) != 1 {
		t.Fatalf("topic reads = %d, searches = %d", len(result.TopicMemoryReads), len(result.MemoryEvidence))
	}
	if got := setup.search.kinds(); len(got) != 1 || got[0] != "semantic" {
		t.Fatalf("the follow-up search used %v", got)
	}
	if result.ToolCalls != 2 || result.ModelCalls != 3 {
		t.Fatalf("tools=%d model=%d", result.ToolCalls, result.ModelCalls)
	}
}

func TestV4MaySearchAfterObservingAnIncompleteTopicWindow(t *testing.T) {
	records := make([]serviceagent.TopicMemoryRecord, 0, 8)
	for i := 0; i < 8; i++ {
		records = append(records, routingRecord(fmt.Sprintf("m-%d", i), "rpc-粘包",
			fmt.Sprintf("记录%d", i), "窗口中的内容", uint64(1730000000+i)))
	}
	setup := newRoutingSetupInMode(t, memoryrecall.ModeAgentMemoryToolV4, activeQuiz("RPC 粘包"), records,
		func(call int, input []*schema.Message) *schema.Message {
			switch call {
			case 1:
				return topicReadToolCall(1)
			case 2:
				return searchToolCall(2, "这位学习者对粘包字节序的疏漏", "semantic")
			default:
				return schema.AssistantMessage(cite(input, "E"), nil)
			}
		})
	setup.provider.eligibleCount = 8
	result, err := setup.runner.Run(context.Background(), routingRun(), serviceagent.DiscardEvents)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.TopicMemoryReads) != 1 || result.TopicMemoryReads[0].EligibleCount != 8 ||
		result.TopicMemoryReads[0].ReturnedCount != 5 || result.TopicMemoryReads[0].OmittedCount != 3 ||
		result.TopicMemoryReads[0].Complete {
		t.Fatalf("topic read trace = %#v", result.TopicMemoryReads)
	}
	if setup.search.executions.Load() != 1 {
		t.Fatalf("general search executions = %d", setup.search.executions.Load())
	}
}
