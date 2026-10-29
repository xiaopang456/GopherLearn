package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"

	"github.com/cloudwego/eino/components/tool"
	toolutils "github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
)

// STABILIZE-061, the behavioural half.
//
// STABILIZE-060's three failures were all the same event: a request to save
// something reached the Episode search first. These tests pin what the explicit
// surface does about that, and - just as importantly - what it must not do to
// the read direction, which was 19/20 and is not being repaired.

// recordingMemoryTools is the pair of tools under their explicit names. It
// records the order they were called in, because the order is the claim.
type recordingMemoryTools struct {
	mu       sync.Mutex
	sequence []string
	// searchResult is what the Episode search hands back. It is deliberately a
	// sentence a model could plausibly copy into a draft.
	searchResult string
}

const plantedEpisodeText = "上次你说 select 的 default 分支会让循环空转"

func (r *recordingMemoryTools) note(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence = append(r.sequence, name)
}

func (r *recordingMemoryTools) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sequence...)
}

func (r *recordingMemoryTools) called(name string) int {
	count := 0
	for _, entry := range r.calls() {
		if entry == name {
			count++
		}
	}
	return count
}

type draftArgs struct {
	Scope   string `json:"scope" jsonschema:"required"`
	Subject string `json:"subject" jsonschema:"required"`
	Content string `json:"content" jsonschema:"required"`
}

type draftReply struct {
	DraftID  string `json:"draft_id"`
	Scope    string `json:"scope"`
	Subject  string `json:"subject"`
	Content  string `json:"content"`
	Requires bool   `json:"requires_user_confirmation"`
}

type pastSessionArgs struct {
	Query string `json:"query" jsonschema:"required"`
}

type pastSessionReply struct {
	Status  string   `json:"status"`
	Summary string   `json:"summary"`
	Results []string `json:"results"`
}

// build returns the two tools under the explicit surface's names. The draft
// tool echoes whatever content it was given, so a test can ask what the model
// actually put in the draft rather than what the description asked it to.
func (r *recordingMemoryTools) build(t *testing.T) []tool.BaseTool {
	t.Helper()
	save, err := toolutils.InferTool(SaveMemoryDraftToolName, saveDescriptionStub,
		func(_ context.Context, input *draftArgs) (*draftReply, error) {
			r.note(SaveMemoryDraftToolName)
			return &draftReply{DraftID: "draft-1", Scope: input.Scope, Subject: input.Subject,
				Content: input.Content, Requires: true}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	search, err := toolutils.InferTool(SearchPastSessionsToolName, searchDescriptionStub,
		func(context.Context, *pastSessionArgs) (*pastSessionReply, error) {
			r.note(SearchPastSessionsToolName)
			return &pastSessionReply{Status: "ok", Summary: "1 card",
				Results: []string{r.searchResult}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return []tool.BaseTool{save, search}
}

const (
	saveDescriptionStub   = "record what the user just asked you to remember, as a draft"
	searchDescriptionStub = "read-only lookup of earlier sessions"
)

// explicitTurn drives one turn in the STABILIZE-061 envelope.
func explicitTurn(t *testing.T, question string, tools []tool.BaseTool,
	script func(int) *schema.Message) (*scriptedChatModel, []model.AgentStep, []Event) {
	t.Helper()
	steps := newMemoryStepRepository()
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		return script(call), nil
	}}
	runner, err := NewRunner(fake, StaticToolRegistry{Registered: tools}, steps,
		MinimalContextBuilder{}, AgentMemoryExplicitActionsV1RunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	input := testRunContext()
	input.Question = question
	input.KnowledgePolicy = rag.KnowledgePreferred
	input.MemoryEvidence = NewMemoryEvidenceRegistry()
	var mu sync.Mutex
	events := []Event{}
	sink := EventSinkFunc(func(_ context.Context, event Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		return nil
	})
	if _, err := runner.Run(context.Background(), input, sink); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	steps.mu.Lock()
	defer steps.mu.Unlock()
	return fake, append([]model.AgentStep(nil), steps.steps...), events
}

func toolCall(id, name, arguments string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{
		ID: id, Function: schema.FunctionCall{Name: name, Arguments: arguments},
	}}}
}

const saveArguments = `{"scope":"global","subject":"学习偏好","content":"以后先讲原理再看代码"}`

// 13. A pure save request. The draft tool is bound exclusively as the first
// tool, the Episode search never runs, and the confirmation card is issued.
//
// The binding is the assertion rather than the model's choice, because a fake
// model choosing correctly proves nothing about a real one. What this test can
// prove is that the server offers the real model no opportunity to get the
// first step wrong.
func TestPureSaveRequestBindsTheDraftToolFirstAndNeverSearches(t *testing.T) {
	tools := &recordingMemoryTools{searchResult: plantedEpisodeText}
	fake, _, events := explicitTurn(t, "记住我以后学新东西都想先讲原理再看代码。", tools.build(t),
		func(call int) *schema.Message {
			if call == 1 {
				return toolCall("d1", SaveMemoryDraftToolName, saveArguments)
			}
			return schema.AssistantMessage("好的，已经准备好一条待你确认的记忆。", nil)
		})

	if fake.ForcedToolChoiceCount() == 0 || !fake.HasExclusiveToolBinding(SaveMemoryDraftToolName) {
		t.Fatalf("first tool bindings = %v, want an exclusive %s",
			fake.BoundToolCalls(), SaveMemoryDraftToolName)
	}
	if got := tools.called(SearchPastSessionsToolName); got != 0 {
		t.Fatalf("%s ran %d times on a pure save request", SearchPastSessionsToolName, got)
	}
	if got := tools.called(SaveMemoryDraftToolName); got != 1 {
		t.Fatalf("%s ran %d times, want 1", SaveMemoryDraftToolName, got)
	}
	assertWaitingConfirmation(t, events)
}

// 14. The read direction is untouched. With no draft tool registered - which is
// what explicitMemoryIntent produces for a recall question - nothing is forced
// towards memory and the search is available.
//
// This is the half STABILIZE-060 passed at 19/20, and the one this round most
// easily breaks: a rule that forced a draft whenever the mode was on would turn
// every "你还记得" into a write attempt.
func TestRecallQuestionForcesNothingWhenNoDraftToolIsRegistered(t *testing.T) {
	tools := &recordingMemoryTools{searchResult: plantedEpisodeText}
	registered := tools.build(t)
	// Only the search tool: the draft tool's own permission gate declined.
	readOnly := registered[1:]
	fake, _, _ := explicitTurn(t, "你还记得我上次卡在 channel 的哪个地方吗？", readOnly,
		func(call int) *schema.Message {
			if call == 1 {
				return toolCall("s1", SearchPastSessionsToolName, `{"query":"channel"}`)
			}
			return schema.AssistantMessage("上次你卡在 select 的 default 分支。", nil)
		})

	if fake.HasExclusiveToolBinding(SaveMemoryDraftToolName) {
		t.Fatal("a recall question was forced towards the draft tool")
	}
	if got := tools.called(SearchPastSessionsToolName); got != 1 {
		t.Fatalf("%s ran %d times on a recall question, want 1", SearchPastSessionsToolName, got)
	}
	if tools.called(SaveMemoryDraftToolName) != 0 {
		t.Fatal("a recall question created a draft")
	}
}

// 15. A mixed request: the draft is forced first, and the search is still
// available afterwards. Both are recorded, separately, in the run's own steps.
func TestMixedRequestDraftsBeforeItSearchesAndRecordsBoth(t *testing.T) {
	tools := &recordingMemoryTools{searchResult: plantedEpisodeText}
	_, steps, events := explicitTurn(t,
		"记住我以后想先讲原理再看代码，另外我上次卡在 channel 的哪个地方？", tools.build(t),
		func(call int) *schema.Message {
			switch call {
			case 1:
				return toolCall("d1", SaveMemoryDraftToolName, saveArguments)
			case 2:
				return toolCall("s1", SearchPastSessionsToolName, `{"query":"channel"}`)
			}
			return schema.AssistantMessage("已记下待确认的偏好；上次你卡在 select 的 default 分支。", nil)
		})

	sequence := tools.calls()
	if len(sequence) != 2 || sequence[0] != SaveMemoryDraftToolName ||
		sequence[1] != SearchPastSessionsToolName {
		t.Fatalf("call order = %v, want the draft then the search", sequence)
	}
	assertWaitingConfirmation(t, events)
	// The two calls are separate steps with their own names, so a report can
	// count the directions apart rather than reading one blended number.
	names := []string{}
	for _, step := range steps {
		if capability := CapabilityOf(step.ToolName); capability == CapabilityMemoryDraft ||
			capability == CapabilityEpisodeSearch {
			names = append(names, step.ToolName)
		}
	}
	if len(names) != 2 || names[0] != SaveMemoryDraftToolName ||
		names[1] != SearchPastSessionsToolName {
		t.Fatalf("recorded steps = %v, want both directions in order", names)
	}
}

// 16. Draft provenance. Whatever the search returned, the draft the user is
// asked to confirm holds only what the model sent to the draft tool - and the
// server records the draft from the tool's own result, never from the search
// observation sitting beside it in the same turn.
//
// The structural half of this is that no code path carries a search result into
// a DraftRequest. That is checked where the draft is built. What this test adds
// is the run-level statement: with a planted, quotable sentence in the search
// result and the draft called after it, the confirmation card the user sees
// does not contain that sentence.
func TestSearchObservationsNeverReachTheConfirmationCard(t *testing.T) {
	tools := &recordingMemoryTools{searchResult: plantedEpisodeText}
	_, _, events := explicitTurn(t,
		"我上次卡在 channel 的哪个地方？顺便记住我以后想先讲原理再看代码。", tools.build(t),
		func(call int) *schema.Message {
			switch call {
			case 1:
				return toolCall("d1", SaveMemoryDraftToolName, saveArguments)
			case 2:
				return toolCall("s1", SearchPastSessionsToolName, `{"query":"channel"}`)
			}
			return schema.AssistantMessage("好的。", nil)
		})

	card := assertWaitingConfirmation(t, events)
	blob, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), plantedEpisodeText) {
		t.Fatalf("the confirmation card carries retrieved text: %s", blob)
	}
	if card.Content != "以后先讲原理再看代码" {
		t.Fatalf("draft content = %q, want exactly what the model sent the draft tool", card.Content)
	}
}

// 17. The step summary of a renamed search still reports shape rather than
// content. The branch that does this used to switch on the tool's name; a
// renamed search falling through it would put personal memory text into the
// persisted step, the event stream and the citation parser at once.
func TestRenamedSearchStillSummarisesShapeNotContent(t *testing.T) {
	tools := &recordingMemoryTools{searchResult: plantedEpisodeText}
	registered := tools.build(t)
	_, steps, events := explicitTurn(t, "你还记得我上次卡在 channel 的哪个地方吗？", registered[1:],
		func(call int) *schema.Message {
			if call == 1 {
				return toolCall("s1", SearchPastSessionsToolName, `{"query":"channel"}`)
			}
			return schema.AssistantMessage("上次你卡在 select 的 default 分支。", nil)
		})

	found := false
	for _, step := range steps {
		if step.ToolName != SearchPastSessionsToolName {
			continue
		}
		found = true
		if strings.Contains(step.ResultSummary, plantedEpisodeText) {
			t.Fatalf("the persisted step carries memory text: %q", step.ResultSummary)
		}
	}
	if !found {
		t.Fatal("no step recorded the renamed search")
	}
	for _, event := range events {
		if event.ToolName == SearchPastSessionsToolName &&
			strings.Contains(event.Content, plantedEpisodeText) {
			t.Fatalf("an event carries memory text: %q", event.Content)
		}
	}
}

func assertWaitingConfirmation(t *testing.T, events []Event) *MemoryDraftCard {
	t.Helper()
	for _, event := range events {
		if event.Type == EventWaitingConfirmation && event.MemoryDraft != nil {
			return event.MemoryDraft
		}
	}
	t.Fatal("no waiting_confirmation event carried a draft card")
	return nil
}
