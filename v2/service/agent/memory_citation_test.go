package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

// registryWith builds a registry holding one entry of each kind, in the order
// the letters imply, so a test can name G1, S2, E3 and C4 by construction.
func registryWith(t *testing.T) *MemoryEvidenceRegistry {
	t.Helper()
	registry := NewMemoryEvidenceRegistry()
	registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal, Subject: "讲解偏好", Owner: 1})
	registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceTopic, InternalID: "m-1", Subject: "消息边界", Owner: 1})
	registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-1", Version: 3, Subject: "分片扩容", Owner: 1})
	registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceCurrent, MessageID: 42, Subject: "刚才说的阈值", Owner: 1})
	return registry
}

// A reference the run actually issued resolves, and resolves as the kind it is.
func TestLegalReferencesResolve(t *testing.T) {
	registry := registryWith(t)
	for ref, kind := range map[string]MemoryEvidenceKind{
		"G1": MemoryEvidenceGlobal, "S2": MemoryEvidenceTopic,
		"E3": MemoryEvidenceEpisodic, "C4": MemoryEvidenceCurrent,
	} {
		entry, err := registry.Lookup(ref)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if entry.Kind != kind {
			t.Fatalf("%s resolved to %s", ref, entry.Kind)
		}
	}
	outcome := CheckMemoryCitations(registry, "你上次决定用双写迁移 [E3]，而你一向要求先说结论 [G1]。")
	if outcome.Failed() || len(outcome.Cited) != 2 {
		t.Fatalf("a legal answer failed the check: %#v", outcome)
	}
}

// A number this run never issued is not a reference, however plausible it looks.
func TestInventedReferencesAreRefused(t *testing.T) {
	registry := registryWith(t)
	for _, ref := range []string{"M99", "E9", "S7", "G12", "C0"} {
		if _, err := registry.Lookup(ref); err == nil {
			t.Fatalf("%s resolved", ref)
		}
	}
	outcome := CheckMemoryCitations(registry, "你上次决定用双写迁移 [E9]。")
	if len(outcome.Invalid) != 1 || outcome.Invalid[0] != "E9" || !outcome.Failed() {
		t.Fatalf("outcome = %#v", outcome)
	}
}

// References belong to one run. Carrying one over from an earlier conversation
// is the same failure as inventing it, and must not resolve because the numbers
// happen to line up.
func TestReferencesFromAnotherRunDoNotResolve(t *testing.T) {
	first := registryWith(t)
	second := NewMemoryEvidenceRegistry()
	second.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal, Subject: "讲解偏好", Owner: 1})
	if _, err := second.Lookup("E3"); err == nil {
		t.Fatal("a reference from the previous run resolved in the next one")
	}
	entry, err := first.Lookup("E3")
	if err != nil || entry.InternalID != "e-1" {
		t.Fatalf("the first run lost its own reference: %v %#v", err, entry)
	}
}

// A preference cannot support a past decision, and the failure has its own name
// so a report can tell it apart from an invented number.
func TestCitingAPreferenceAsHistoryIsAKindMismatch(t *testing.T) {
	registry := registryWith(t)
	outcome := CheckMemoryCitations(registry, "你上次讨论的结论是先做可观测性 [E1]。")
	if len(outcome.KindMismatch) != 1 || outcome.KindMismatch[0] != "E1" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(outcome.Invalid) != 0 {
		t.Fatalf("a kind mismatch was reported as an unknown reference: %#v", outcome)
	}
	if !outcome.Failed() {
		t.Fatal("a preference cited as history was allowed through")
	}
}

// A run whose searches all came back empty holds no long-term evidence, so an
// answer that cites some is asserting a history it was never shown.
func TestCitingMemoryAfterAnEmptySearchFails(t *testing.T) {
	registry := NewMemoryEvidenceRegistry()
	registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal, Subject: "学习目标", Owner: 1})
	registry.Record(MemorySearchCall{Status: "no_evidence"})
	outcome := CheckMemoryCitations(registry, "你上次的结论是先做可观测性 [E5]。")
	if !outcome.Failed() || len(outcome.Invalid) != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	// Answering from the profile alone is legal as far as references go; that
	// it is the wrong answer is a judgement, not a lookup.
	clean := CheckMemoryCitations(registry, "记忆里没有相关记录。你一贯的目标是补服务治理 [G1]。")
	if clean.Failed() {
		t.Fatalf("a profile-only answer was refused: %#v", clean)
	}
}

// A run holding evidence whose answer cites none of it is recorded and not
// gated: a search that returned only neighbouring subjects should end in
// "nothing recorded", and that answer legitimately cites nothing.
func TestMissingCitationIsRecordedNotGated(t *testing.T) {
	registry := NewMemoryEvidenceRegistry()
	ref := registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-1", Owner: 1})
	registry.Record(MemorySearchCall{Status: "ok", Refs: []MemoryEvidenceRef{{Ref: ref}}})
	outcome := CheckMemoryCitations(registry, "记忆里没有能回答这件事的记录。")
	if !outcome.MissingCitation {
		t.Fatal("a missing citation was not recorded")
	}
	if outcome.Failed() {
		t.Fatal("a missing citation was gated")
	}
}

// Document citations and memory references share no vocabulary, so neither can
// be parsed as the other.
func TestDocumentAndMemoryCitationsDoNotMix(t *testing.T) {
	registry := registryWith(t)
	outcome := CheckMemoryCitations(registry, "TCP 是字节流 [1][2]，而你把它理解成水管 [S2]。")
	if outcome.Failed() {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(outcome.Cited) != 1 || outcome.Cited[0] != "S2" {
		t.Fatalf("document citations were parsed as memory: %#v", outcome.Cited)
	}
	if finalCitationPattern.FindString("[S2]") != "" {
		t.Fatal("a memory reference was parsed as a document citation")
	}
}

// Two searches in one run continue the numbering, so a label never means two
// different records.
func TestTwoSearchesProduceStableNonConflictingReferences(t *testing.T) {
	registry := NewMemoryEvidenceRegistry()
	first := registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceTopic, InternalID: "m-1"})
	second := registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-1"})
	third := registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-2"})
	if first != "S1" || second != "E2" || third != "E3" {
		t.Fatalf("references = %s %s %s", first, second, third)
	}
	seen := map[string]string{}
	for _, entry := range registry.Entries() {
		if existing, duplicate := seen[entry.Ref]; duplicate {
			t.Fatalf("reference %s stood for both %s and %s", entry.Ref, existing, entry.InternalID)
		}
		seen[entry.Ref] = entry.InternalID
	}
}

// --- runner-level: repair, fallback, budget ---

// mustCitingModel answers with the given texts in order, so a test can drive a
// first draft, a repair and whatever follows.
func mustCitingModel(answers ...string) *scriptedChatModel {
	index := 0
	return &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		if index >= len(answers) {
			return schema.AssistantMessage(answers[len(answers)-1], nil), nil
		}
		answer := answers[index]
		index++
		return schema.AssistantMessage(answer, nil), nil
	}}
}

func runWithEvidence(t *testing.T, model *scriptedChatModel, seed func(*MemoryEvidenceRegistry),
	config RunnerConfig) (RunnerResult, error) {
	t.Helper()
	registry := &seedingRegistry{seed: seed}
	runner := mustRunner(t, model, registry, newMemoryStepRepository(), config)
	return runner.Run(context.Background(), testRunContext(), newEventCollector())
}

// seedingRegistry stands in for the tool: it fills the run's registry the way a
// completed memory_search would, without a search.
type seedingRegistry struct {
	seed  func(*MemoryEvidenceRegistry)
	mu    sync.Mutex
	calls atomic.Int32
}

func (r *seedingRegistry) Tools(_ context.Context, run RunContext) ([]tool.BaseTool, error) {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if run.MemoryEvidence == nil {
		return nil, fmt.Errorf("no registry")
	}
	if r.seed != nil {
		r.seed(run.MemoryEvidence)
	}
	return nil, nil
}

// One repair, given only the legal references, and the repaired answer is what
// the learner receives.
func TestOneCitationRepairIsGrantedAndCanSucceed(t *testing.T) {
	model := mustCitingModel(
		"你上次决定用双写迁移 [E9]。",
		"你上次决定用双写迁移 [E1]。",
	)
	result, err := runWithEvidence(t, model, func(registry *MemoryEvidenceRegistry) {
		registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-1", Subject: "分片扩容"})
		registry.Record(MemorySearchCall{Status: "ok", Refs: []MemoryEvidenceRef{{Ref: "E1"}}})
	}, AgentMemoryToolRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "你上次决定用双写迁移 [E1]。" {
		t.Fatalf("answer = %q", result.Answer)
	}
	if !result.MemoryCitation.RepairAttempted || !result.MemoryCitation.RepairSucceeded {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
	if result.MemoryCitation.SafeFallback {
		t.Fatal("a successful repair still reported a fallback")
	}
	if result.ModelCalls != 2 {
		t.Fatalf("the repair cost %d model calls", result.ModelCalls)
	}
}

// A model that invents twice gets the deterministic sentence, never its own
// draft.
func TestRepeatedInventionEndsInTheSafeFallback(t *testing.T) {
	model := mustCitingModel(
		"你上次决定用双写迁移 [E9]。",
		"你上次决定用双写迁移 [E8]，我很确定。",
	)
	result, err := runWithEvidence(t, model, func(registry *MemoryEvidenceRegistry) {
		registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-1"})
		registry.Record(MemorySearchCall{Status: "ok", Refs: []MemoryEvidenceRef{{Ref: "E1"}}})
	}, AgentMemoryToolRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != MemoryCitationFallbackAnswer {
		t.Fatalf("answer = %q", result.Answer)
	}
	if strings.Contains(result.Answer, "E8") || strings.Contains(result.Answer, "双写迁移") {
		t.Fatal("the failing draft reached the learner")
	}
	if !result.MemoryCitation.SafeFallback || result.MemoryCitation.RepairSucceeded {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
}

// The repair is a model call like any other. With no generation left in the
// budget it is refused and the run falls back rather than exceeding the ceiling.
func TestRepairIsRefusedWhenTheModelBudgetIsSpent(t *testing.T) {
	model := mustCitingModel("你上次决定用双写迁移 [E9]。")
	config := AgentMemoryToolRunnerConfig()
	config.MaxModelCalls = 1
	result, err := runWithEvidence(t, model, func(registry *MemoryEvidenceRegistry) {
		registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "e-1"})
		registry.Record(MemorySearchCall{Status: "ok", Refs: []MemoryEvidenceRef{{Ref: "E1"}}})
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != MemoryCitationFallbackAnswer {
		t.Fatalf("answer = %q", result.Answer)
	}
	if result.ModelCalls != 1 {
		t.Fatalf("the repair was spent past the ceiling: %d model calls", result.ModelCalls)
	}
	if !result.MemoryCitation.RepairAttempted || !result.MemoryCitation.SafeFallback {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
}

// An answer with no memory claims at all is left alone; the contract governs
// claims about the learner, not ordinary technical explanation.
func TestAnswersWithoutMemoryClaimsPassUntouched(t *testing.T) {
	model := mustCitingModel("TCP 是字节流，没有消息边界，需要自己分帧。")
	result, err := runWithEvidence(t, model, nil, AgentMemoryToolRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "TCP 是字节流，没有消息边界，需要自己分帧。" {
		t.Fatalf("answer = %q", result.Answer)
	}
	if result.MemoryCitation.Failed() || result.MemoryCitation.RepairAttempted {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
	if result.ModelCalls != 1 {
		t.Fatalf("model calls = %d", result.ModelCalls)
	}
}

// Two runs in flight issue their own references over their own evidence.
func TestConcurrentRunsKeepSeparateRegistries(t *testing.T) {
	var wait sync.WaitGroup
	answers := make([]RunnerResult, 4)
	for index := 0; index < 4; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			model := mustCitingModel(fmt.Sprintf("你上次的决定是第 %d 号 [E1]。", slot))
			result, err := runWithEvidence(t, model, func(registry *MemoryEvidenceRegistry) {
				registry.Register(MemoryEvidenceEntry{
					Kind: MemoryEvidenceEpisodic, InternalID: fmt.Sprintf("e-%d", slot),
				})
				registry.Record(MemorySearchCall{Status: "ok", Refs: []MemoryEvidenceRef{{Ref: "E1"}}})
			}, AgentMemoryToolRunnerConfig())
			if err != nil {
				t.Error(err)
				return
			}
			answers[slot] = result
		}(index)
	}
	wait.Wait()
	for slot, result := range answers {
		if result.MemoryCitation.Failed() {
			t.Fatalf("run %d = %#v", slot, result.MemoryCitation)
		}
		if len(result.MemoryRefs) != 1 || result.MemoryRefs[0].InternalID != fmt.Sprintf("e-%d", slot) {
			t.Fatalf("run %d saw another run's evidence: %#v", slot, result.MemoryRefs)
		}
	}
}

// The repair prompt carries this run's legal references and their kinds, and
// nothing else: no database id, no other run's labels.
func TestRepairPromptOffersOnlyThisRunsReferences(t *testing.T) {
	registry := registryWith(t)
	state := &runState{memoryTrace: registry, input: RunContext{Question: "上次我们定了什么？"}}
	messages := state.memoryRepairMessages("上次我们定了什么？", "你上次决定用双写迁移 [E9]。",
		MemoryCitationOutcome{Invalid: []string{"E9"}})
	content := messages[1].Content
	for _, want := range []string{"[G1]", "[S2]", "[E3]", "[C4]", "E9"} {
		if !strings.Contains(content, want) {
			t.Fatalf("the repair prompt omits %s", want)
		}
	}
	for _, forbidden := range []string{"m-1", "e-1"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("the repair prompt leaked the internal id %q", forbidden)
		}
	}
	if !strings.Contains(messages[0].Content, "tool") {
		t.Fatal("the repair prompt does not forbid tools")
	}
}

// The evidence blocks the window carries are labelled, and the labels resolve.
func TestInjectedEvidenceIsLabelledAndResolvable(t *testing.T) {
	provider := &recordingMemoryProvider{globalMemory: []string{"先说结论再展开", "不要用 Java 举例"}}
	builder, err := NewContextBuilder(memoryToolContextConfig(), stubHistory{rows: []model.Message{
		{ID: 11, IsUser: true, Content: "我们就把慢查询阈值按接口预算倒推。"},
		{ID: 12, IsUser: false, Content: "记下了。"},
	}}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	builder.UseGlobalMemoryProvider(provider)
	evidence := NewMemoryEvidenceRegistry()
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 1, SessionID: "s-1", Question: "刚才定的阈值口径是什么？", MemoryEvidence: evidence,
	})
	if err != nil {
		t.Fatal(err)
	}
	assembled := renderWindow(window)
	if !strings.Contains(assembled, "[G1] 先说结论再展开") || !strings.Contains(assembled, "[G2] 不要用 Java 举例") {
		t.Fatalf("global memory is unlabelled: %s", assembled)
	}
	if !strings.Contains(assembled, "[C3] 我们就把慢查询阈值按接口预算倒推。") {
		t.Fatalf("the current conversation has no citable handle: %s", assembled)
	}
	if strings.Contains(assembled, "[C4] 记下了。") {
		t.Fatal("an assistant message was offered as evidence about the learner")
	}
	entry, err := evidence.Lookup("C3")
	if err != nil || entry.MessageID != 11 {
		t.Fatalf("C3 = %#v (%v)", entry, err)
	}
	if window.Budget.CurrentEvidence <= 0 {
		t.Fatalf("the current evidence block was not budgeted: %#v", window.Budget)
	}
	if _, err := evidence.Lookup("G1"); err != nil {
		t.Fatalf("G1 does not resolve: %v", err)
	}
	if rag.EstimateTokens(assembled) == 0 {
		t.Fatal("empty window")
	}
}

// BUG-AGENT-152, and the honest boundary of what a server can check.
//
// v1.1 answered "what did we conclude last time" out of the learner's stated
// goals. Under the citation contract that answer can take two shapes. If it
// cites the preference as a past decision - [E1] where entry 1 is global - the
// kind mismatch catches it deterministically. If it cites the preference as a
// preference - [G1] - every reference is legal and the server has no way to
// know the sentence around it is a claim about an earlier session.
//
// This test pins both halves, including the one that does not hold, because a
// contract whose limits are undocumented gets read as a guarantee. The second
// shape is caught by the anonymous judge in development and by the pilot's own
// regression rule, which knows the question is about a past session because the
// case says so.
func TestHistoryFromAPreferenceIsCaughtOnlyWhenTheKindIsWrong(t *testing.T) {
	registry := NewMemoryEvidenceRegistry()
	registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal, Subject: "学习目标"})
	registry.Record(MemorySearchCall{Status: "no_evidence"})

	mismatched := CheckMemoryCitations(registry, "咱们最近那回收在的结论是先做可观测性 [E1]。")
	if len(mismatched.KindMismatch) != 1 || !mismatched.Failed() {
		t.Fatalf("a preference cited as a past decision was allowed: %#v", mismatched)
	}

	honestLabel := CheckMemoryCitations(registry, "咱们最近那回收在的结论是先做可观测性 [G1]。")
	if honestLabel.Failed() {
		t.Fatalf("the reference itself is legal; the check must not pretend otherwise: %#v", honestLabel)
	}
	if len(honestLabel.Cited) != 1 || honestLabel.Cited[0] != "G1" {
		t.Fatalf("citation outcome = %#v", honestLabel)
	}
}

// The repair does reach the learner when the kind was wrong.
func TestKindMismatchIsRepairedOrFallsBack(t *testing.T) {
	model := mustCitingModel(
		"咱们最近那回收在的结论是：先把可观测性做扎实 [E1]。",
		"记忆中没有找到那次会话的结论记录。",
	)
	result, err := runWithEvidence(t, model, func(registry *MemoryEvidenceRegistry) {
		registry.Register(MemoryEvidenceEntry{Kind: MemoryEvidenceGlobal, Subject: "学习目标"})
		registry.Record(MemorySearchCall{Status: "no_evidence"})
	}, AgentMemoryToolRunnerConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Answer, "可观测性做扎实") {
		t.Fatalf("a history invented out of the profile reached the learner: %q", result.Answer)
	}
	if !result.MemoryCitation.RepairAttempted || !result.MemoryCitation.RepairSucceeded {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
}

// Two serial searches plus an answer is three generations. It is the structural
// minimum of the path the design allows, and v1.1 failed a correct case for it.
func TestTwoSearchesAndAnAnswerCostThreeModelCalls(t *testing.T) {
	var executions atomic.Int32
	fake := &scriptedChatModel{respond: func(_ context.Context, call int) (*schema.Message, error) {
		if call <= 2 {
			return memorySearchCall(call, fmt.Sprintf("第 %d 次不同的检索", call)), nil
		}
		return schema.AssistantMessage("你上次决定用双写迁移。[E1]", nil), nil
	}}
	registry := memoryToolRegistry(t, &executions, "ok",
		[]fakeMemoryCard{{Kind: "episodic", Subject: "分片扩容", Content: "决定用双写迁移"}})
	config := AgentMemoryToolRunnerConfig()
	runner := mustRunner(t, fake, registry, newMemoryStepRepository(), config)
	result, err := runner.Run(context.Background(), testRunContext(), newEventCollector())
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 2 || result.ModelCalls != 3 {
		t.Fatalf("two searches and an answer cost %d tools and %d model calls", result.ToolCalls, result.ModelCalls)
	}
	if result.ToolCalls > config.MaxToolCalls || result.ModelCalls > config.MaxModelCalls {
		t.Fatalf("the run exceeded its own ceiling: %#v", result)
	}
	if result.MemoryCitation.Failed() {
		t.Fatalf("citation outcome = %#v", result.MemoryCitation)
	}
}

// STABILIZE-062 v1.4, BUG-AGENT-215.
//
// The bug the failure matrix found by accident. Two searches of earlier
// sessions in one turn usually return some of the same episodes, and the second
// issue of a slot already held by that same episode was refused - so the memory
// tool returned an error, the tool boundary typed it as an infrastructure
// fault, and one ordinary second question voided a twenty-four question round.
func TestReissuingASlotToTheSameMemoryIsNotACollision(t *testing.T) {
	registry := &MemoryEvidenceRegistry{}
	entry := MemoryEvidenceEntry{Kind: MemoryEvidenceEpisodic, InternalID: "lg_1", Version: 3}
	if err := registry.RegisterAs("[E1.s]", entry); err != nil {
		t.Fatalf("first issue: %v", err)
	}
	if err := registry.RegisterAs("[E1.s]", entry); err != nil {
		t.Fatalf("a second search returning the same episode must reissue the "+
			"same slot silently, got %v", err)
	}
	if err := registry.RegisterAs("[E1.s]", MemoryEvidenceEntry{
		Kind: MemoryEvidenceEpisodic, InternalID: "lg_2", Version: 3}); err == nil {
		t.Fatalf("a slot pointing at a different memory is a real collision and " +
			"must still be refused: a citation would resolve to something the " +
			"learner was never shown")
	}
}
