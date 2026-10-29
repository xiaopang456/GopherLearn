package app

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// The recall path's job is to put exactly the live, confirmed, authorized rows in front of
// the model. Two of the four lifecycle states had only ever been checked at the store and
// service layers: a superseded answer and an archived one. Those are the states a real
// conversation produces after a learner changes their mind, so they are what the next
// Provider run will actually be sitting on - and a turn that cited them would cite something
// the user has already retired.

// citedIDs renders a citation set as sorted ids, so a mismatch reports what was attached.
func citedIDs(sources []domain.Source) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.SourceID)
	}
	sort.Strings(ids)
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestRetiredMemoriesNeverEnterAnAgentTurn is the lifecycle claim end to end: a global
// memory a newer confirmation superseded, and an episode the user archived, are neither shown
// to the model nor cited by the answer - while the rows that are still active in the same
// layers are, from the same query that matches all four.
func TestRetiredMemoriesNeverEnterAnAgentTurn(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	memories := service.NewMemoryService(fixture.repo)

	// Four rows, all containing the query token, all legitimately created and confirmed.
	retiredGlobal := fixture.file(t, globalMemory("用户的 fsync 偏好是 everysec", 1, fixture.agentSess, fixture.agentMsg), true)
	// Confirming a second statement about the same layer supersedes the first: two active
	// answers to one question is not a memory system, it is a contradiction.
	currentGlobal := fixture.file(t, globalMemory("用户的 fsync 偏好后来改成了 always", 1, fixture.agentSess, fixture.agentMsg), true)
	retiredEpisode := fixture.file(t, episodeMemory("这一度讲错的 fsync 例子", fixture.agentSess, fixture.agentMsg), true)
	if _, err := memories.ArchiveMemory(ctx, fixture.owner, retiredEpisode.MemoryID); err != nil {
		t.Fatalf("archive the episode: %v", err)
	}
	currentEpisode := fixture.file(t, episodeMemory("本轮确认过的 fsync 讲解方式", fixture.agentSess, fixture.agentMsg), true)

	// Positive controls on the rows themselves. Without these, the assertions below would
	// also pass if the retirement had simply never happened - or if the rows had never been
	// written at all.
	statuses, err := fixture.repo.ListMemoriesByStatus(ctx, fixture.owner, domain.MemoryStatusSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].MemoryID != retiredGlobal.MemoryID {
		t.Fatalf("superseded rows = %+v, want exactly the first global", statuses)
	}
	archived, err := fixture.repo.ListMemoriesByStatus(ctx, fixture.owner, domain.MemoryStatusArchived)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].MemoryID != retiredEpisode.MemoryID {
		t.Fatalf("archived rows = %+v, want exactly the retired episode", archived)
	}
	active, err := fixture.repo.ListMemoriesByStatus(ctx, fixture.owner, domain.MemoryStatusActive)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("active rows = %d, want the two survivors: %+v", len(active), active)
	}

	// The production authorizer: global plus this session's episodes, no topics. Both retired
	// rows sit in layers the turn IS authorized for, so a leak here would be a status bug and
	// not a scope mistake.
	recall := &countingRecall{inner: productionRecall(t, fixture)}
	chat := &toolChat{replies: []model.Completion{
		memoryCall("fsync 偏好"),
		stopReply("按你现在确认过的偏好回答。"),
	}}
	assembled, err := assembleWithMemories(t, recall, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize())
	pair, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "我的 fsync 偏好是什么？", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if recall.calls != 1 || chat.calls != 2 {
		t.Fatalf("recall=%d model=%d, want one read and two decision calls", recall.calls, chat.calls)
	}
	observed := toolResults(toolTranscript(t, chat, 2))
	for _, forbidden := range []string{retiredGlobal.Content, retiredEpisode.Content} {
		if strings.Contains(observed, forbidden) {
			t.Fatalf("a retired memory reached the model: %q\n%s", forbidden, observed)
		}
	}
	for _, want := range []string{currentGlobal.Content, currentEpisode.Content} {
		if !strings.Contains(observed, want) {
			t.Fatalf("the live memory %q never reached the model:\n%s", want, observed)
		}
	}
	cited := citedIDs(pair.Assistant.Sources)
	want := []string{"src-" + currentGlobal.MemoryID, "src-" + currentEpisode.MemoryID}
	sort.Strings(want)
	if strings.Join(cited, ",") != strings.Join(want, ",") {
		t.Fatalf("citations = %v, want exactly %v", cited, want)
	}
	for _, forbidden := range []string{"src-" + retiredGlobal.MemoryID, "src-" + retiredEpisode.MemoryID} {
		if containsID(cited, forbidden) {
			t.Fatalf("a retired memory was cited: %s", forbidden)
		}
	}
}

// TestEveryDoorWiredStillGivesChatAndKnowledgeNoTools is the keep-invariant under the new
// assembly: registering both memory doors for the agent must not put a single tool definition
// in front of the two chat strategies, and neither door may be reached by their turns.
func TestEveryDoorWiredStillGivesChatAndKnowledgeNoTools(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	// A real owned collection, so the grounded turn is a turn the service would actually run
	// rather than one that fails before reaching the model.
	store, ingest, ownedID, _ := agentFixture(t)
	writer := &countingProposer{inner: service.NewMemoryService(fixture.repo)}
	recall := &countingRecall{inner: productionRecall(t, fixture)}
	chat := &toolChat{}

	assembled, err := assembleDoorsOverStore(t, store, recall, writer, chat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(assembled.StartupLine, MemorySearchToolName) ||
		!strings.Contains(assembled.StartupLine, ProposeMemoryToolName) {
		t.Fatalf("the agent was assembled without one of the memory doors: %s", assembled.StartupLine)
	}

	// The two chat strategies, straight through the responder face the service uses.
	if _, err := assembled.Assistant.Respond(ctx, responder.Request{
		UserID: "user-1", SessionID: fixture.chatSess, Content: "随便聊聊 fsync",
		SessionMode: domain.SessionModeChat, AnswerStrategy: domain.AnswerStrategyDirectChat,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := assembled.Assistant.Respond(ctx, responder.Request{
		UserID: "user-1", SessionID: fixture.chatSess, Content: "AOF 的 fsync 可以选哪些？",
		SessionMode: domain.SessionModeChat, AnswerStrategy: domain.AnswerStrategyKnowledgeQA,
		KnowledgeEnabled: true, KnowledgeCollectionIDs: []string{ownedID},
	}); err != nil {
		t.Fatal(err)
	}
	if len(chat.seenTools) != 0 {
		t.Fatalf("a chat strategy was handed a tool offer %d time(s)", len(chat.seenTools))
	}
	for index, request := range chat.requests {
		if len(request.Tools) != 0 {
			t.Fatalf("chat call %d carried %d tool definitions", index+1, len(request.Tools))
		}
	}
	if recall.calls != 0 || writer.calls != 0 {
		t.Fatalf("chat turns reached the memory doors: recall=%d write=%d", recall.calls, writer.calls)
	}
	// The grounded turn really retrieved: an empty tool offer would also be what a turn that
	// never happened looks like.
	if len(chat.requests) != 2 {
		t.Fatalf("the two chat turns produced %d completions, want 2", len(chat.requests))
	}

	// Positive control in the same test and the same assembly: the agent strategy does get
	// the offer, so the assertions above cannot be passing because nothing was registered.
	agentChat := &toolChat{replies: []model.Completion{
		memoryCall("fsync 偏好"),
		stopReply("按偏好回答。"),
	}}
	agentAssembly, err := assembleDoorsOverStore(t, store, recall, writer, agentChat)
	if err != nil {
		t.Fatal(err)
	}
	agentMessages := NewMessageService(fixture.repo, agentAssembly.Assistant, ingest, service.ContextBudget{}.Normalize())
	if _, err := agentMessages.Send(ctx, fixture.owner, fixture.agentSess, "我的 fsync 偏好？", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(agentChat.seenTools) == 0 {
		t.Fatal("the agent turn was offered no tools either, which makes the chat checks meaningless")
	}
	if recall.calls < 1 {
		t.Fatalf("the agent turn never read memory: recall=%d", recall.calls)
	}
}

// assembleDoorsOverStore is assembleWithBothDoors over a caller's knowledge store, so a test
// can hand the grounded strategy a collection that exists.
func assembleDoorsOverStore(t *testing.T, store knowledge.Store, recall MemoryRecall,
	proposer MemoryProposalWriter, chat model.ChatModel,
) (Assembly, error) {
	t.Helper()
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		return Assembly{}, err
	}
	return Assemble(store, config, Options{
		NewModel:       func(openai.Config) (model.ChatModel, error) { return chat, nil },
		Memory:         recall,
		MemoryProposer: proposer,
	})
}

// TestAgentTurnIsOfferedTheSameThreeToolsEveryRound pins the tool surface of the current
// assembly at the application level: registration order, one entry per door, and a
// memory_search schema that stays exactly the two-key object it was built with - across
// every decision call of a turn that used both memory doors.
func TestAgentTurnIsOfferedTheSameThreeToolsEveryRound(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	fixture.file(t, globalMemory("用户的 fsync 偏好是 everysec", 1, fixture.agentSess, fixture.agentMsg), true)

	writer := &countingProposer{inner: service.NewMemoryService(fixture.repo)}
	recall := &countingRecall{inner: productionRecall(t, fixture)}
	chat := &toolChat{replies: []model.Completion{
		memoryCall("fsync 偏好"),
		proposalArguments(`{"scope":"global","content":"用户偏好先看代码再读文档"}`),
		stopReply("按你的偏好回答，并把它记下了。"),
	}}
	assembled, err := assembleWithBothDoors(t, recall, writer, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize())
	if _, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "讲讲 fsync", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if chat.calls != 3 {
		t.Fatalf("the turn made %d decision calls, want read + write + answer", chat.calls)
	}
	want := KnowledgeSearchToolName + "," + MemorySearchToolName + "," + ProposeMemoryToolName
	var memorySchema string
	for round, definitions := range chat.seenTools {
		if names := offeredToolNames(definitions); names != want {
			t.Fatalf("decision call %d was offered %q, want %q", round+1, names, want)
		}
		for _, definition := range definitions {
			if definition.Name != MemorySearchToolName {
				continue
			}
			if memorySchema == "" {
				memorySchema = string(definition.Parameters)
			} else if string(definition.Parameters) != memorySchema {
				t.Fatalf("memory_search was offered a different schema at call %d:\n%s\n%s",
					round+1, memorySchema, definition.Parameters)
			}
			var schema map[string]any
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				t.Fatalf("memory_search schema is not JSON: %v", err)
			}
			properties, _ := schema["properties"].(map[string]any)
			// The tool still takes only a query. No topic, no session, no user, no floor -
			// and no extra key sneaking in as the memory features grew.
			if len(properties) != 1 {
				t.Fatalf("memory_search advertises %d keys: %v", len(properties), properties)
			}
			if _, declared := properties["query"]; !declared {
				t.Fatalf("the only declared key must be query, got %v", properties)
			}
			if schema["additionalProperties"] != false {
				t.Fatalf("the schema must refuse fields it never declared: %v", schema["additionalProperties"])
			}
		}
	}
	// The surface was exercised, not merely advertised: one read and one write happened.
	if recall.calls != 1 || writer.calls != 1 {
		t.Fatalf("recall=%d write=%d, want the turn to have used each door once", recall.calls, writer.calls)
	}
	// And the description the model reads still says the scope is the server's decision.
	for _, definition := range chat.seenTools[0] {
		if definition.Name == MemorySearchToolName &&
			!strings.Contains(definition.Description, "检索范围由服务端决定") {
			t.Fatalf("the memory tool description no longer states who decides the scope: %s",
				definition.Description)
		}
	}
	// agent.MemoySearchName's spelling is pinned from the other side too: the name the
	// assembly registers is the constant the tool package exports.
	if agent.MemorySearchName != MemorySearchToolName || agent.ProposeMemoryName != ProposeMemoryToolName {
		t.Fatalf("the duplicated names drifted: %q %q", agent.MemorySearchName, agent.ProposeMemoryName)
	}
}
