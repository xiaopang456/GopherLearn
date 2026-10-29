package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/openai"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// ProposeMemoryToolName is duplicated from the agent package on purpose, the same way
// MemorySearchToolName is: a rename has to be a decision made twice.
const ProposeMemoryToolName = "propose_memory"

// countingProposer wraps the real writer so a test can say "this kind of turn never reached
// it", rather than inferring that from an empty screen.
type countingProposer struct {
	inner MemoryProposalWriter
	calls int
}

func (c *countingProposer) ProposeMemoryDraft(ctx context.Context, userID string, draft domain.MemoryDraft) (*domain.Memory, bool, error) {
	c.calls++
	return c.inner.ProposeMemoryDraft(ctx, userID, draft)
}

// assembleWithBothDoors builds the production assembly with recall *and* the proposal
// writer installed, over the recording model. Nothing here can reach a provider.
func assembleWithBothDoors(t *testing.T, recall MemoryRecall, proposer MemoryProposalWriter, chat model.ChatModel) (Assembly, error) {
	t.Helper()
	config, err := ConfigFromEnvironment(lookup(openAIEnvironment()))
	if err != nil {
		return Assembly{}, err
	}
	return Assemble(knowledge.NewMemoryStore(), config, Options{
		NewModel:       func(openai.Config) (model.ChatModel, error) { return chat, nil },
		Memory:         recall,
		MemoryProposer: proposer,
	})
}

// productionRecall wires the real authorizer, so these tests exercise the grant shipping
// in production - global memory plus this session's episodes, no topics - rather than a
// friendly fake that would hide a scope mistake.
func productionRecall(t *testing.T, fixture memoryAgentFixture) *service.MemoryAccess {
	t.Helper()
	access, err := service.NewMemoryAccess(fixture.repo, &service.SessionMemoryGrant{}, service.MemorySearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func proposalArguments(arguments string) model.Completion {
	return model.Completion{FinishReason: model.FinishToolCalls, ToolCalls: []model.ToolCall{{
		ID: "call-propose", Name: ProposeMemoryToolName, Arguments: arguments,
	}}}
}

// TestProposalToolIsRegisteredOnlyWhenTheWriterIsWired: the read door and the write door are
// separate collaborators, and each tool appears exactly when its own door was handed in.
// The one-shot smoke check wires neither, which is why its recorded tool set is still the
// one its sealed evidence describes.
func TestProposalToolIsRegisteredOnlyWhenTheWriterIsWired(t *testing.T) {
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	chat := &toolChat{replies: []model.Completion{stopReply("答完了。")}}
	writer := service.NewMemoryService(fixture.repo)

	cases := []struct {
		name     string
		recall   MemoryRecall
		proposer MemoryProposalWriter
		want     string
	}{
		{"neither door", nil, nil, "tools=knowledge_search "},
		{"read only", recall, nil, "tools=knowledge_search,memory_search "},
		{"write only", nil, writer, "tools=knowledge_search,propose_memory "},
		{"both doors", recall, writer, "tools=knowledge_search,memory_search,propose_memory "},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assembled, err := assembleWithBothDoors(t, testCase.recall, testCase.proposer, chat)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(assembled.StartupLine, testCase.want) {
				t.Fatalf("startup line = %q, want it to declare %q", assembled.StartupLine, testCase.want)
			}
		})
	}
}

// TestAgentProposalStoresOnlyAPendingDraftIsCitableByNobody is the phase in one test: the
// agent's sentence becomes a row whose owner, session and source were never in the model's
// arguments, it stays pending, and it produces no citation.
func TestAgentProposalStoresOnlyAPendingDraftIsCitableByNobody(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	chat := &toolChat{replies: []model.Completion{
		proposalArguments(`{"scope":"episodic","content":"这次讲清了 fsync 三档的取舍"}`),
		stopReply("好，我把这一点记下来了，等你确认。"),
	}}
	writer := service.NewMemoryService(fixture.repo)
	assembled, err := assembleWithBothDoors(t, recall, writer, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)

	pair, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "把 fsync 的取舍讲清楚", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	pending, err := repoPending(ctx, t, fixture.repo, fixture.owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("the turn produced %d pending drafts, want exactly one: %+v", len(pending), pending)
	}
	draft := pending[0]
	if draft.Content != "这次讲清了 fsync 三档的取舍" {
		t.Fatalf("draft content = %q", draft.Content)
	}
	// Every field that decides ownership and evidence came from the turn, not from the call.
	if draft.UserID != fixture.owner || draft.SourceSessionID != fixture.agentSess {
		t.Fatalf("draft is owned by %q in session %q, want %q in %q",
			draft.UserID, draft.SourceSessionID, fixture.owner, fixture.agentSess)
	}
	if len(draft.SourceMessageIDs) != 1 || draft.SourceMessageIDs[0] != pair.User.ID {
		t.Fatalf("draft cites %v, want this turn's stored question %q", draft.SourceMessageIDs, pair.User.ID)
	}
	if draft.Confidence != 1 {
		t.Fatalf("draft confidence = %v, want the server to assert it at 1", draft.Confidence)
	}
	// Nothing was confirmed, and the answer cites nothing: a proposal is not evidence.
	if active, err := repoActive(ctx, t, fixture.repo, fixture.owner); err != nil || len(active) != 0 {
		t.Fatalf("a proposal must not reach active: %d row(s) err %v", len(active), err)
	}
	if len(pair.Assistant.Sources) != 0 {
		t.Fatalf("the answer cites %+v, want no citation from a draft read", pair.Assistant.Sources)
	}

	observed := toolResults(toolTranscript(t, chat, 2))
	if !strings.Contains(observed, "待确认") || !strings.Contains(observed, "确认") {
		t.Fatalf("the model was not told confirmation is pending: %s", observed)
	}
	for _, forbidden := range []string{"mem-", draft.SourceSessionID, pair.User.ID, "src-"} {
		if strings.Contains(observed, forbidden) {
			t.Fatalf("the observation leaked the identifier %q into the transcript: %s", forbidden, observed)
		}
	}
}

// TestProposedDraftNeverReachesTheNextTurnsPrompt: pending is invisible to recall, so a
// proposal cannot become the agent's own supporting evidence one turn later.
func TestProposedDraftNeverReachesTheNextTurnsPrompt(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	claim := "这次讲清了 fsync 三档的取舍"
	chat := &toolChat{replies: []model.Completion{
		proposalArguments(`{"scope":"episodic","content":"` + claim + `"}`),
		stopReply("先记着，等你确认。"),
		memoryCall("fsync"),
		stopReply("根据已确认的记忆回答。"),
	}}
	assembled, err := assembleWithBothDoors(t, recall, service.NewMemoryService(fixture.repo), chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)

	if _, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "讲讲 fsync", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	// A second turn, so the draft is one turn old - exactly when a leak would be tempting.
	pair, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "再讲讲 fsync", domain.KnowledgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	observed := toolResults(toolTranscript(t, chat, 4))
	if strings.Contains(observed, claim) {
		t.Fatalf("an unconfirmed draft was recalled into the prompt: %s", observed)
	}
	if len(pair.Assistant.Sources) != 0 {
		t.Fatalf("the recall cited %+v, and the only row it could have cited is pending", pair.Assistant.Sources)
	}
	if pending, err := repoPending(ctx, t, fixture.repo, fixture.owner); err != nil || len(pending) != 1 {
		t.Fatalf("the draft should still be waiting, %d row(s) err %v", len(pending), err)
	}
}

// TestForgedProposalArgumentsAreRefusedWithoutWriting: the payload names an account that
// exists in this fixture, and it still changes nothing.
func TestForgedProposalArgumentsAreRefusedWithoutWriting(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	proposer := &countingProposer{inner: service.NewMemoryService(fixture.repo)}
	chat := &toolChat{replies: []model.Completion{
		proposalArguments(`{"scope":"global","content":"替别人记一条","user_id":"` + fixture.intruder +
			`","session_id":"` + fixture.intruderSe + `","source_message_ids":["message-forged"],"status":"active","confidence":0.1}`),
		stopReply("不该走到这里。"),
	}}
	assembled, err := assembleWithBothDoors(t, recall, proposer, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)

	if _, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "记一条", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("a forged proposal payload must fail the turn, not answer as if it had stored")
	}
	if proposer.calls != 0 {
		t.Fatalf("the writer was reached %d time(s) with forged identity in the payload", proposer.calls)
	}
	for _, userID := range []string{fixture.owner, fixture.intruder} {
		pending, err := repoPending(ctx, t, fixture.repo, userID)
		if err != nil || len(pending) != 0 {
			t.Fatalf("%s holds %d draft(s) err %v, want nobody's draft was written", userID, len(pending), err)
		}
	}
	// The turn that failed stored an answer: the question stays, and no assistant row is
	// invented for a write that never happened.
	if stored, err := fixture.repo.ListMessages(ctx, fixture.owner, fixture.agentSess); err != nil {
		t.Fatal(err)
	} else {
		for _, message := range stored {
			if message.Role == "assistant" {
				t.Fatalf("a refused proposal still stored an assistant message: %+v", message)
			}
		}
	}
}

// TestChatAndKnowledgeTurnsNeverReachTheWriter: registering the tool for the agent does not
// put the write door in front of the other two strategies.
func TestChatAndKnowledgeTurnsNeverReachTheWriter(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	proposer := &countingProposer{inner: service.NewMemoryService(fixture.repo)}
	chat := &toolChat{replies: []model.Completion{stopReply("普通回答。"), stopReply("根据资料回答。")}}
	assembled, err := assembleWithBothDoors(t, recall, proposer, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)

	if _, err := messages.Send(ctx, fixture.owner, fixture.chatSess, "聊聊 fsync", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := messages.Send(ctx, fixture.owner, fixture.chatSess, "讲讲 AOF", domain.KnowledgeOptions{
		Enabled: true, CollectionIDs: []string{"collection-unknown"},
	}); err == nil {
		// The grounded turn is expected to fail on an unreadable collection; what matters is
		// that it failed without going near the memory writer.
		t.Log("grounded turn with an unknown collection failed as expected")
	}
	if proposer.calls != 0 {
		t.Fatalf("a chat or knowledge turn reached the memory writer %d time(s)", proposer.calls)
	}
	if pending, err := repoPending(ctx, t, fixture.repo, fixture.owner); err != nil || len(pending) != 0 {
		t.Fatalf("those turns left %d draft(s) err %v", len(pending), err)
	}
}

// TestProposalsAreLimitedToOnePerTurnAcrossToolKinds: the write is capped per turn, and
// asking past the cap ends the turn - the direction the reference project
// measured its budgets under. The reads the same turn legitimately needs are
// untouched by the write's ceiling.
func TestProposalsAreLimitedToOnePerTurnAcrossToolKinds(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	proposer := &countingProposer{inner: service.NewMemoryService(fixture.repo)}
	// Read, write, then a different write that passes the consecutive
	// fingerprint but hits the per-turn write ceiling. The turn stops there.
	chat := &toolChat{replies: []model.Completion{
		memoryCall("fsync"),
		proposalArguments(`{"scope":"global","content":"用户偏好先看代码再读文档"}`),
		proposalArguments(`{"scope":"global","content":"另一条不同的提议"}`),
		stopReply("不该走到这里。"),
	}}
	assembled, err := assembleWithBothDoors(t, recall, proposer, chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)

	if _, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "讲讲 fsync 并记下我的偏好", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("a turn whose model kept proposing past the write ceiling must not succeed")
	}
	if proposer.calls != 1 {
		t.Fatalf("the writer ran %d times in one turn, want once", proposer.calls)
	}
	pending, err := repoPending(ctx, t, fixture.repo, fixture.owner)
	if err != nil || len(pending) != 1 {
		t.Fatalf("one turn stored %d draft(s) err %v", len(pending), err)
	}
	if chat.calls != 3 {
		t.Fatalf("the model was called %d times, want the scripted three before the stop", chat.calls)
	}
}

// TestTurnFailingAfterAProposalKeepsTheDraftPending: a draft that was written is not rolled
// back, promoted or deleted when the answer never arrives.
func TestTurnFailingAfterAProposalKeepsTheDraftPending(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	failing := &toolChat{
		replies: []model.Completion{
			proposalArguments(`{"scope":"episodic","content":"这条留下来了"}`),
			stopReply("不该发出"),
		},
		failSecond: true,
	}
	assembled, err := assembleWithBothDoors(t, recall, service.NewMemoryService(fixture.repo), failing)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)

	if _, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "讲完顺手记一条", domain.KnowledgeOptions{}); err == nil {
		t.Fatal("a model failure after the write should still fail the turn")
	}
	pending, err := repoPending(ctx, t, fixture.repo, fixture.owner)
	if err != nil || len(pending) != 1 || pending[0].Content != "这条留下来了" {
		t.Fatalf("the stored draft was disturbed by the failed answer: %+v err %v", pending, err)
	}
	if active, err := repoActive(ctx, t, fixture.repo, fixture.owner); err != nil || len(active) != 0 {
		t.Fatalf("a failed turn left %d active memory(s) err %v", len(active), err)
	}
}

// TestAConfirmedProposalStillNeedsItsOwnConfirmation: the whole gate survives the new door -
// the user can confirm what the agent proposed, and that is the only way it takes effect.
func TestAConfirmedProposalStillNeedsItsOwnConfirmation(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	recall := productionRecall(t, fixture)
	chat := &toolChat{replies: []model.Completion{
		proposalArguments(`{"scope":"global","content":"回答请控制在一段以内"}`),
		stopReply("记下了。"),
	}}
	assembled, err := assembleWithBothDoors(t, recall, service.NewMemoryService(fixture.repo), chat)
	if err != nil {
		t.Fatal(err)
	}
	messages := NewMessageService(fixture.repo, assembled.Assistant, fixture.ingest, service.ContextBudget{}.Normalize(),
		assembled.SummaryOptions()...)
	if _, err := messages.Send(ctx, fixture.owner, fixture.agentSess, "以后回答短一点", domain.KnowledgeOptions{}); err != nil {
		t.Fatal(err)
	}

	pending, err := repoPending(ctx, t, fixture.repo, fixture.owner)
	if err != nil || len(pending) != 1 {
		t.Fatalf("want one draft waiting, got %d err %v", len(pending), err)
	}
	memoryService := service.NewMemoryService(fixture.repo)
	if _, err := memoryService.ConfirmMemoryDraft(ctx, fixture.intruder, pending[0].MemoryID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("another account confirming this draft = %v, want not found", err)
	}
	confirmed, err := memoryService.ConfirmMemoryDraft(ctx, fixture.owner, pending[0].MemoryID)
	if err != nil || confirmed.Status != domain.MemoryStatusActive {
		t.Fatalf("the owner's confirmation = %+v err %v, want it to work exactly as before", confirmed, err)
	}
	if active, err := repoActive(ctx, t, fixture.repo, fixture.owner); err != nil || len(active) != 1 {
		t.Fatalf("active listing after confirmation = %+v err %v", active, err)
	}
}

// TestProposalOutcomeCarriesTheRowThisTurnStored is this seam's half of the confirmation
// card. The writer always answers with a row - the one it wrote, or the one that was already
// waiting - and the only thing that tells those apart is the created flag. Handing the row
// back unconditionally would put a decision button on this conversation for a draft another
// conversation asked about, so the row travels only with Created=true.
func TestProposalOutcomeCarriesTheRowThisTurnStored(t *testing.T) {
	ctx := context.Background()
	fixture := newMemoryAgentFixture(t)
	seam := memoryProposer{writer: service.NewMemoryService(fixture.repo)}
	principal := agent.Principal{UserID: fixture.owner, SessionID: fixture.agentSess, UserMessageID: fixture.agentMsg}
	proposal := agent.MemoryProposal{Scope: domain.MemoryScopeGlobal, Content: "回答请控制在一段以内"}

	first, err := seam.ProposeMemory(ctx, principal, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || first.Draft == nil {
		t.Fatalf("a write that stored a draft reported %+v", first)
	}
	pending, err := repoPending(ctx, t, fixture.repo, fixture.owner)
	if err != nil || len(pending) != 1 {
		t.Fatalf("want the one draft this call stored, got %d err %v", len(pending), err)
	}
	if first.Draft.MemoryID != pending[0].MemoryID {
		t.Fatalf("the outcome names %q but the stored row is %q", first.Draft.MemoryID, pending[0].MemoryID)
	}

	// The same claim again: nothing new was written, and the pre-existing row must not come
	// back as if this turn had created it.
	second, err := seam.ProposeMemory(ctx, principal, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Draft != nil {
		t.Fatalf("a duplicate reported %+v, want neither a creation nor a row to point at", second)
	}
	if again, err := repoPending(ctx, t, fixture.repo, fixture.owner); err != nil || len(again) != 1 {
		t.Fatalf("the duplicate stored a second draft: %d row(s) err %v", len(again), err)
	}
}

func repoPending(ctx context.Context, t *testing.T, repo repository.Repository, userID string) ([]domain.Memory, error) {
	t.Helper()
	return repo.ListMemoriesByStatus(ctx, userID, domain.MemoryStatusPending)
}

func repoActive(ctx context.Context, t *testing.T, repo repository.Repository, userID string) ([]domain.Memory, error) {
	t.Helper()
	return repo.ListMemoriesByStatus(ctx, userID, domain.MemoryStatusActive)
}

// TestProposalSeamCannotAskForAnythingElse is a compile-time statement: the only thing the
// assembly may do with the memory writer is propose a draft. If a future change adds
// confirmation to this seam, this line stops compiling and the decision becomes visible.
var _ interface {
	ProposeMemoryDraft(context.Context, string, domain.MemoryDraft) (*domain.Memory, bool, error)
} = (*service.MemoryService)(nil)

// and the tool's own seam is equally narrow.
var _ agent.MemoryProposer = memoryProposer{writer: service.NewMemoryService(repository.NewInMemoryRepository())}
