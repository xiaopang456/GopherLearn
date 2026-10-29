package app

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// topicTurn is one end-to-end agent turn over the memory read side: the fixture's data, the
// assembly the deployment chose, the recorded model, and what the turn produced.
type topicTurn struct {
	observation string
	sources     []string
	titles      []string
	schema      string
	tools       string
}

// seedQuizOn files one open quiz on a conversation straight through the store. Going through
// LearningService.Start would need a model and retrieved evidence, and a grant test that
// depends on either is a grant test that is really testing question generation.
func seedQuizOn(t *testing.T, repo repository.Repository, userID, sessionID, topic string, expiresAt time.Time) {
	t.Helper()
	if _, err := repo.CreateLearningSession(t.Context(), &domain.LearningSession{
		UserID: userID, SessionID: sessionID, Topic: topic,
		TopicKey:        domain.NormalizeLearningTopicKey(topic),
		Status:          domain.LearningSessionCreated,
		CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions:  domain.LearningQuestionCount,
		Version:         1,
		ExpiresAt:       expiresAt,
	}); err != nil {
		t.Fatalf("open quiz on %s: %v", sessionID, err)
	}
}

// learnerSaysYes records the conversation's consent through the service, which is also the only
// way a chat conversation can be refused: a test that wrote the row straight into the store could
// not tell "the switch is on" from "the switch could never have been turned off".
func learnerSaysYes(t *testing.T, fixture memoryAgentFixture, sessionID string) {
	t.Helper()
	committed, err := service.NewSessionService(fixture.repo).
		CommitTopicMemory(t.Context(), fixture.owner, sessionID, true)
	if err != nil {
		t.Fatalf("commit topic memory on %s: %v", sessionID, err)
	}
	if !committed.Enabled {
		t.Fatalf("the consent was committed as off: %+v", committed)
	}
}

// runMemoryTurn answers one learning-agent turn in which the model asks memory_search, and
// reports what the model was shown, what the answer cited, and what tool schema was offered.
func runMemoryTurn(t *testing.T, fixture memoryAgentFixture, recall MemoryRecall, sessionID, question, memoryQuery string) topicTurn {
	t.Helper()
	ctx := context.Background()
	chat := &toolChat{replies: []model.Completion{
		memoryCall(memoryQuery),
		stopReply("按读到的记忆回答。"),
	}}
	assembled, err := assembleWithMemories(t, recall, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize())
	pair, err := messages.Send(ctx, fixture.owner, sessionID, question, domain.KnowledgeOptions{})
	if err != nil {
		t.Fatalf("turn on %s: %v", sessionID, err)
	}
	turn := topicTurn{
		observation: toolResults(toolTranscript(t, chat, 2)),
		tools:       offeredToolNames(chat.seenTools[0]),
	}
	for _, source := range pair.Assistant.Sources {
		turn.sources = append(turn.sources, source.SourceID)
		turn.titles = append(turn.titles, source.Title)
	}
	for _, definition := range chat.seenTools[0] {
		if definition.Name == MemorySearchToolName {
			turn.schema = string(definition.Parameters)
		}
	}
	return turn
}

func sortedCopy(values []string) string {
	copied := append([]string(nil), values...)
	sort.Strings(copied)
	return strings.Join(copied, ",")
}

// seedTopicRows files the six rows one opt-in test needs: two subjects, the two layers that
// were always readable, an episode of a different conversation, and a row in another account.
func seedTopicRows(t *testing.T, fixture memoryAgentFixture, agentSession, agentMessage string) map[string]string {
	t.Helper()
	return map[string]string{
		"redis":  fixture.file(t, topicMemory("redis", "redis 主题：AOF 的 fsync 选 everysec", 1, agentSession, agentMessage), true).MemoryID,
		"mysql":  fixture.file(t, topicMemory("mysql", "mysql 主题：binlog 的 fsync 已确认", 1, agentSession, agentMessage), true).MemoryID,
		"global": fixture.file(t, globalMemory("全局：解释 fsync 时用中文", 1, agentSession, agentMessage), true).MemoryID,
		"here":   fixture.file(t, episodeMemory("本轮决定先掌握 fsync", agentSession, agentMessage), true).MemoryID,
		"there":  fixture.file(t, episodeMemory("另一会话里的 fsync 决定", fixture.chatSess, fixture.chatMsg), true).MemoryID,
	}
}

// TestTopicOptInAssemblyIsTheProductionGrantWhenSwitchedOff is the compatibility proof at
// the door a deployment actually walks through. The zero MemoryAccessConfig must produce the
// same observation, the same citations and the same tool schema as the authorizer value
// production passes today, on data that would otherwise show a difference: an open quiz on a
// learning-agent conversation with a confirmed topic memory under it.
func TestTopicOptInAssemblyIsTheProductionGrantWhenSwitchedOff(t *testing.T) {
	fixture := newMemoryAgentFixture(t)
	seedTopicRows(t, fixture, fixture.agentSess, fixture.agentMsg)
	seedQuizOn(t, fixture.repo, fixture.owner, fixture.agentSess, "redis", time.Now().Add(time.Hour))
	// Everything a topic read needs except the deployment's decision. A switched-off assembly
	// must still read exactly what the authorizer production passes today reads, and that claim
	// is only worth testing on data where the two could have differed.
	learnerSaysYes(t, fixture, fixture.agentSess)

	off, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// The wiring cmd/gopherlearn/main.go performs today, spelled out rather than assumed.
	today, err := service.NewMemoryAccess(fixture.repo, &service.SessionMemoryGrant{}, service.MemorySearchOptions{})
	if err != nil {
		t.Fatal(err)
	}

	first := runMemoryTurn(t, fixture, off, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	second := runMemoryTurn(t, fixture, today, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")

	if first.observation != second.observation {
		t.Fatalf("the assembly's off path differs from the production authorizer\noff:  %s\ntoday: %s",
			first.observation, second.observation)
	}
	if sortedCopy(first.sources) != sortedCopy(second.sources) {
		t.Fatalf("citations differ: %v vs %v", first.sources, second.sources)
	}
	if first.schema != second.schema || first.tools != second.tools {
		t.Fatalf("the tool surface moved: %q/%q vs %q/%q", first.tools, first.schema, second.tools, second.schema)
	}
	if strings.Contains(first.observation, "redis 主题") {
		t.Fatalf("topic memory reached the model with the switch off: %s", first.observation)
	}
}

// TestTopicOptInReachesTheModelThroughTheServerOnly runs the whole production path - the real
// authorizer, the real store, the real tool - with the opt-in installed. The only new thing
// the model can see is the subject its own conversation has an open quiz on.
func TestTopicOptInReachesTheModelThroughTheServerOnly(t *testing.T) {
	fixture := newMemoryAgentFixture(t)
	rows := seedTopicRows(t, fixture, fixture.agentSess, fixture.agentMsg)
	fixture.fileFor(t, fixture.intruder, fixture.intruderSe,
		mustAppend(context.Background(), t, fixture.repo, fixture.intruder, fixture.intruderSe, "别人的话"),
		topicMemory("redis", "别人的 redis fsync，绝不可见", 1, "", ""))
	seedQuizOn(t, fixture.repo, fixture.owner, fixture.agentSess, "redis", time.Now().Add(time.Hour))
	learnerSaysYes(t, fixture, fixture.agentSess)

	access, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	turn := runMemoryTurn(t, fixture, access, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")

	for _, required := range []string{"redis 主题：AOF 的 fsync 选 everysec", "全局：解释 fsync 时用中文",
		"本轮决定先掌握 fsync"} {
		if !strings.Contains(turn.observation, required) {
			t.Fatalf("%q never reached the model: %s", required, turn.observation)
		}
	}
	for _, forbidden := range []string{"mysql 主题", "另一会话里的 fsync 决定", "别人的 redis fsync", "绝不可见"} {
		if strings.Contains(turn.observation, forbidden) {
			t.Fatalf("an unauthorized memory reached the model: %s", forbidden)
		}
	}
	// Every citation is a memory that was shown, and every memory shown is cited: the topic
	// layer joins the same rule the other two layers were already held to.
	wantCitations := sortedCopy([]string{"src-" + rows["redis"], "src-" + rows["global"], "src-" + rows["here"]})
	if got := sortedCopy(turn.sources); got != wantCitations {
		t.Fatalf("citations = %v, want %s", turn.sources, wantCitations)
	}
	if got, want := sortedCopy(turn.titles), sortedCopy([]string{"主题记忆：redis", "全局记忆", "经历记录"}); got != want {
		t.Fatalf("citation titles = %v, want the three server-written labels", turn.titles)
	}
	// What a model must never be able to forge its way back into: the row and conversation
	// identifiers behind the text.
	for _, forbidden := range []string{rows["redis"], rows["mysql"], fixture.agentSess, fixture.agentMsg} {
		if strings.Contains(turn.observation, forbidden) {
			t.Fatalf("the observation leaked the internal identifier %q", forbidden)
		}
	}
}

// TestTopicOptInGrantsNothingWithoutAnOpenQuiz says out loud that the learner's switch is not the
// whole grant. Installing the authorizer and honouring a yes must not turn a learning-agent
// conversation into a topic read while no quiz is open, because there is no subject for the yes to
// apply to - and the scope note must not claim one.
func TestTopicOptInGrantsNothingWithoutAnOpenQuiz(t *testing.T) {
	fixture := newMemoryAgentFixture(t)
	seedTopicRows(t, fixture, fixture.agentSess, fixture.agentMsg)
	learnerSaysYes(t, fixture, fixture.agentSess)

	access, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	turn := runMemoryTurn(t, fixture, access, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	if strings.Contains(turn.observation, "redis 主题") || strings.Contains(turn.observation, "mysql 主题") {
		t.Fatalf("a conversation with no quiz read topic memory: %s", turn.observation)
	}
	if strings.Contains(turn.observation, "已授权主题") {
		t.Fatalf("the scope note claimed a subject that was never authorized: %s", turn.observation)
	}
	for _, required := range []string{"全局：解释 fsync 时用中文", "本轮决定先掌握 fsync"} {
		if !strings.Contains(turn.observation, required) {
			t.Fatalf("the base grant stopped covering %q: %s", required, turn.observation)
		}
	}
}

// TestTopicOptInNeedsTheLearnersOwnYes is the same claim from the other side: with the deployment
// installed and a live quiz open, an unanswered switch reads no topic memory - and the model is
// told, in the observation, what was searched.
func TestTopicOptInNeedsTheLearnersOwnYes(t *testing.T) {
	fixture := newMemoryAgentFixture(t)
	seedTopicRows(t, fixture, fixture.agentSess, fixture.agentMsg)
	seedQuizOn(t, fixture.repo, fixture.owner, fixture.agentSess, "redis", time.Now().Add(time.Hour))

	access, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	before := runMemoryTurn(t, fixture, access, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	if strings.Contains(before.observation, "redis 主题") {
		t.Fatalf("a conversation the learner never switched on read topic memory: %s", before.observation)
	}
	if strings.Contains(before.observation, "已授权主题") {
		t.Fatalf("the scope note named a subject nobody authorized: %s", before.observation)
	}

	// The positive control, and the whole product requirement in three lines: the same assembly,
	// the same quiz and the same rows, and the only thing that changes is the learner's answer.
	learnerSaysYes(t, fixture, fixture.agentSess)
	after := runMemoryTurn(t, fixture, access, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	if !strings.Contains(after.observation, "redis 主题：AOF 的 fsync 选 everysec") {
		t.Fatalf("switching the consent on changed nothing: %s", after.observation)
	}
	if !strings.Contains(after.observation, "已授权主题的记忆（redis）") {
		t.Fatalf("the observation must say which subject it read: %s", after.observation)
	}
	if sortedCopy(after.sources) == sortedCopy(before.sources) {
		t.Fatalf("the citations did not move with the newly readable memory: %v then %v",
			before.sources, after.sources)
	}

	// Taking it back takes the read away in the same process, with no cache to expire: this is
	// the direction a learner is most likely to test by trying it.
	off, err := service.NewSessionService(fixture.repo).
		CommitTopicMemory(t.Context(), fixture.owner, fixture.agentSess, false)
	if err != nil {
		t.Fatal(err)
	}
	if off.Enabled {
		t.Fatal("the switch did not commit as off")
	}
	revoked := runMemoryTurn(t, fixture, access, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	if strings.Contains(revoked.observation, "redis 主题") {
		t.Fatalf("topic memory was still read after the learner said no: %s", revoked.observation)
	}
	if revoked.observation != before.observation {
		t.Fatalf("a revoked consent left a different trace than never having given one\nwas:  %s\nnow: %s",
			before.observation, revoked.observation)
	}
}

// TestChatConversationCannotBeSwitchedOnToReadTopics covers both halves of the rule. The write
// path refuses a chat conversation; and when a row reaches storage by some other route, the recall
// path refuses it too. The second half is the one that matters: a boundary that holds only while
// every caller refused is not a boundary.
func TestChatConversationCannotBeSwitchedOnToReadTopics(t *testing.T) {
	ctx := t.Context()
	fixture := newMemoryAgentFixture(t)
	seedTopicRows(t, fixture, fixture.agentSess, fixture.agentMsg)

	if _, err := service.NewSessionService(fixture.repo).
		CommitTopicMemory(ctx, fixture.owner, fixture.chatSess, true); !errors.Is(err, service.ErrTopicMemoryNeedsLearningAgent) {
		t.Fatalf("a chat conversation was switched on: %v", err)
	}
	// Plant the row the service refused, and open a quiz on the same chat conversation, so the
	// only thing left to stop the read is the recall side checking the mode for itself.
	if _, err := fixture.repo.CommitTopicMemoryPreference(ctx, fixture.owner, fixture.chatSess, true); err != nil {
		t.Fatal(err)
	}
	seedQuizOn(t, fixture.repo, fixture.owner, fixture.chatSess, "redis", time.Now().Add(time.Hour))

	access, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	hits, note := recallThrough(t, access, fixture.owner, fixture.chatSess, "fsync")
	for _, hit := range hits {
		if hit.Memory.Scope == domain.MemoryScopeTopic {
			t.Fatalf("a chat conversation read topic memory through a row the service refused: %q", hit.Content)
		}
	}
	if strings.Contains(note, "已授权主题") {
		t.Fatalf("the scope note named a subject a chat conversation cannot authorize: %q", note)
	}

	// Control: the same rows are readable from a learning conversation that has its own quiz and
	// its own yes, so the refusal above was the mode and not an empty table.
	seedQuizOn(t, fixture.repo, fixture.owner, fixture.agentSess, "redis", time.Now().Add(time.Hour))
	learnerSaysYes(t, fixture, fixture.agentSess)
	control, _ := recallThrough(t, access, fixture.owner, fixture.agentSess, "fsync")
	if !strings.Contains(contentsOf(control), "redis 主题：AOF 的 fsync 选 everysec") {
		t.Fatalf("the control read nothing, so the refusal above proves nothing: %s", contentsOf(control))
	}

	// A conversation that is not the caller's is refused in the same shape, with the same note:
	// the mode rule and the ownership rule must not be distinguishable from here.
	for _, sessionID := range []string{fixture.intruderSe, "session-that-never-was"} {
		hits, note := recallThrough(t, access, fixture.owner, sessionID, "fsync")
		for _, hit := range hits {
			if hit.Memory.Scope == domain.MemoryScopeTopic {
				t.Fatalf("session %s reached topic memory: %q", sessionID, hit.Content)
			}
		}
		if strings.Contains(note, "已授权主题") {
			t.Fatalf("session %s was described as topic-authorized: %q", sessionID, note)
		}
	}
}

// recallThrough is the service-level read the chat cases above need: the agent loop only runs for
// a learning-agent conversation, so a chat turn has no tool call to make and the read has to be
// asked directly.
func recallThrough(t *testing.T, access *service.MemoryAccess, userID, sessionID, query string) ([]service.MemoryHit, string) {
	t.Helper()
	hits, note, err := access.Recall(t.Context(), service.MemoryQuery{UserID: userID, SessionID: sessionID, Query: query})
	if err != nil {
		t.Fatalf("recall for %s in %s: %v", userID, sessionID, err)
	}
	return hits, note
}

// contentsOf joins the text a read returned, for assertions about what reached the model.
func contentsOf(hits []service.MemoryHit) string {
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, hit.Content)
	}
	return strings.Join(parts, "\n")
}

// TestChatTurnStillReadsNoMemoryWithTheOptInInstalled: the memory tool only exists inside the
// agent loop, and a chat conversation - ordinary Q&A or knowledge-grounded Q&A - resolves to
// the other strategies. Opting in must not add a memory read to those turns.
func TestChatTurnStillReadsNoMemoryWithTheOptInInstalled(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	seedTopicRows(t, fixture, fixture.agentSess, fixture.agentMsg)
	seedQuizOn(t, fixture.repo, fixture.owner, fixture.chatSess, "redis", time.Now().Add(time.Hour))

	access, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	recall := &countingRecall{inner: access}
	chat := &toolChat{replies: []model.Completion{stopReply("不该调用工具。")}}
	assembled, err := assembleWithMemories(t, recall, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize())
	pair, err := messages.Send(ctx, fixture.owner, fixture.chatSess, "Redis 的 fsync 该选哪个？", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if recall.calls != 0 {
		t.Fatalf("a chat turn read memory %d times", recall.calls)
	}
	if len(pair.Assistant.Sources) != 0 {
		t.Fatalf("a chat turn cited a memory: %+v", pair.Assistant.Sources)
	}
}

// TestMemorySearchSchemaIsUnchangedByTheOptIn pins the public parameter face. A model is
// offered exactly one key on one tool, and the topic opt-in must not be a reason to grow it:
// a schema that could name a subject would be a schema that could be steered into one.
func TestMemorySearchSchemaIsUnchangedByTheOptIn(t *testing.T) {
	fixture := newMemoryAgentFixture(t)
	off, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{})
	if err != nil {
		t.Fatal(err)
	}
	on, err := NewMemoryAccess(fixture.repo, MemoryAccessConfig{TopicMemory: true})
	if err != nil {
		t.Fatal(err)
	}
	// Neither turn may reach a topic row here: no quiz is open on this conversation, so the
	// two turns read the same rows and their schemas and tool lists must be equal.
	offTurn := runMemoryTurn(t, fixture, off, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	onTurn := runMemoryTurn(t, fixture, on, fixture.agentSess, "Redis 的 fsync 该选哪个？", "fsync")
	if offTurn.schema != onTurn.schema {
		t.Fatalf("the memory_search schema changed with the opt-in\noff: %s\non:  %s", offTurn.schema, onTurn.schema)
	}
	if offTurn.tools != onTurn.tools {
		t.Fatalf("the offered tools changed: %q vs %q", offTurn.tools, onTurn.tools)
	}

	var schema struct {
		Type                 string          `json:"type"`
		AdditionalProperties bool            `json:"additionalProperties"`
		Required             []string        `json:"required"`
		Properties           json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(onTurn.schema), &schema); err != nil {
		t.Fatalf("the offered schema is not this tool's schema: %v (%s)", err, onTurn.schema)
	}
	if schema.AdditionalProperties || len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("the schema stopped refusing the unknown: %s", onTurn.schema)
	}
	var properties map[string]json.RawMessage
	if err := json.Unmarshal(schema.Properties, &properties); err != nil {
		t.Fatal(err)
	}
	if len(properties) != 1 {
		t.Fatalf("memory_search grew a second parameter: %s", onTurn.schema)
	}
	for _, forbidden := range []string{"topic", "scope", "user", "session", "all_users", "memory_id"} {
		if strings.Contains(onTurn.schema, forbidden) {
			t.Fatalf("the schema offers %q to a model: %s", forbidden, onTurn.schema)
		}
	}
}
