package agent

import (
	"strings"
	"testing"
)

// The three STABILIZE-054 acceptance failures all had the same shape: a
// capability refusal, then a malformed argument, then a corrected call that was
// refused because the first two had already spent the budget. These tests pin
// each link of that chain.

func TestRefusalsAndCorrectedRetryAreDistinguished(t *testing.T) {
	ledger := newToolProgressLedger()
	const semantic = `{"kinds":["semantic","episodic"],"query":"learner understanding of etcd","top_k":6}`
	const episodicSix = `{"kinds":["episodic"],"query":"what happened in earlier sessions about etcd","top_k":6}`
	const episodicFive = `{"kinds":["episodic"],"query":"what happened in earlier sessions about etcd","top_k":5}`

	if decision, _ := ledger.Decide(memorySearchToolName, semantic); decision != ToolDecisionRun {
		t.Fatal("the first call must be allowed to run")
	}
	ledger.Observe(memorySearchToolName, semantic, ToolOutcome{Retrieved: false, Reason: "semantic_not_permitted"})

	// A different capability is a different request and must still be tried.
	if decision, _ := ledger.Decide(memorySearchToolName, episodicSix); decision != ToolDecisionRun {
		t.Fatal("an episodic search after a semantic refusal must be allowed")
	}
	ledger.Observe(memorySearchToolName, episodicSix, ToolOutcome{Retrieved: false, Reason: "top_k_invalid"})

	// The corrected call must RUN. Neither refusal reached storage, so nothing
	// is known about what an episodic search would return, and blocking it here
	// is exactly the behaviour that failed the acceptance.
	if decision, _ := ledger.Decide(memorySearchToolName, episodicFive); decision != ToolDecisionRun {
		t.Fatal("the corrected call must be allowed to run")
	}
	ledger.Observe(memorySearchToolName, episodicFive, ToolOutcome{Retrieved: true, NewEvidence: 0, Reason: "no_evidence"})

	// Once it has actually run and found nothing, repeating it is suppressed
	// without touching storage again. By this point the run has also spent its
	// allowance, and exhaustion is the stronger of the two verdicts; either way
	// the call does not execute.
	decision, _ := ledger.Decide(memorySearchToolName, episodicFive)
	if decision == ToolDecisionRun {
		t.Fatal("repeating an executed empty search must not reach storage again")
	}
	if outcome := ledger.lastOutcomeFor(memorySearchToolName, episodicFive); outcome.Reason != "no_evidence" {
		t.Fatalf("the executed outcome must still explain itself, got %q", outcome.Reason)
	}
	// And the whole sequence cost exactly three no-progress attempts: two
	// refusals that never reached storage, and one real search that found
	// nothing.
	if ledger.NoProgressCount() != 3 {
		t.Fatalf("no-progress count = %d, want 3", ledger.NoProgressCount())
	}
}

// Rotating a free-text rationale or reordering fields is not progress.
func TestCosmeticArgumentChangesAreTheSameRequest(t *testing.T) {
	base := `{"kinds":["episodic","semantic"],"query":"Past  sessions about RPC","reason":"first attempt","top_k":5}`
	variants := map[string]string{
		"reordered fields":   `{"top_k":5,"query":"Past  sessions about RPC","reason":"first attempt","kinds":["episodic","semantic"]}`,
		"rewritten reason":   `{"kinds":["episodic","semantic"],"query":"Past  sessions about RPC","reason":"let me try once more","top_k":5}`,
		"reordered kinds":    `{"kinds":["semantic","episodic"],"query":"Past  sessions about RPC","reason":"first attempt","top_k":5}`,
		"different top_k":    `{"kinds":["episodic","semantic"],"query":"Past  sessions about RPC","reason":"first attempt","top_k":3}`,
		"whitespace or case": `{"kinds":["episodic","semantic"],"query":"past sessions about rpc","reason":"first attempt","top_k":5}`,
	}
	want := semanticToolKey(memorySearchToolName, base)
	for name, variant := range variants {
		if got := semanticToolKey(memorySearchToolName, variant); got != want {
			t.Fatalf("%s produced a different semantic key", name)
		}
	}
	// A genuinely different question is a different request.
	different := `{"kinds":["episodic"],"query":"a completely unrelated subject","top_k":5}`
	if semanticToolKey(memorySearchToolName, different) == want {
		t.Fatal("a different query must not collapse onto the same key")
	}
}

// A call that actually returned evidence must not be blocked afterwards.
func TestProgressIsNeverMistakenForRepetition(t *testing.T) {
	ledger := newToolProgressLedger()
	const first = `{"kinds":["episodic"],"query":"earlier sessions about packet framing","top_k":5}`
	const second = `{"kinds":["episodic"],"query":"what we decided about buffer reuse","top_k":5}`

	ledger.Observe(memorySearchToolName, first, ToolOutcome{Retrieved: true, NewEvidence: 3})
	if decision, _ := ledger.Decide(memorySearchToolName, second); decision != ToolDecisionRun {
		t.Fatal("a new question after a productive search must run")
	}
	// Even repeating the productive call is allowed, because it added evidence
	// last time; the tool's own dedup decides whether it returns anything new.
	if decision, _ := ledger.Decide(memorySearchToolName, first); decision != ToolDecisionRun {
		t.Fatal("a previously productive call must not be suppressed")
	}
	if ledger.NoProgressCount() != 0 {
		t.Fatalf("a productive call must not count as no progress, got %d", ledger.NoProgressCount())
	}
}

// Rotating parameters must not let a model spend the whole run searching.
func TestNoProgressAllowanceBoundsParameterRotation(t *testing.T) {
	ledger := newToolProgressLedger()
	for index := 0; index < noProgressAllowance; index++ {
		arguments := `{"kinds":["episodic"],"query":"attempt ` + string(rune('a'+index)) + `","top_k":5}`
		if decision, _ := ledger.Decide(memorySearchToolName, arguments); decision != ToolDecisionRun {
			t.Fatalf("attempt %d should still be allowed", index)
		}
		ledger.Observe(memorySearchToolName, arguments, ToolOutcome{Retrieved: true, NewEvidence: 0, Reason: "no_evidence"})
	}
	// A brand-new wording, never tried before, is still refused once the run has
	// spent its allowance: otherwise rotation is unbounded.
	decision, _ := ledger.Decide(memorySearchToolName, `{"kinds":["episodic"],"query":"yet another rewording","top_k":5}`)
	if decision != ToolDecisionExhausted {
		t.Fatalf("decision = %v, want exhausted", decision)
	}
}

// The suppressed observation must tell the model to stop and answer, and must
// never leak a ticket, a topic key or a memory record.
func TestSuppressedObservationInstructsAndLeaksNothing(t *testing.T) {
	observation := noProgressObservation(ToolOutcome{Reason: "semantic_not_permitted"}, false)
	for _, want := range []string{`"status":"stop_searching"`, `"retryable":false`, "next_step"} {
		if !strings.Contains(observation, want) {
			t.Fatalf("observation missing %s: %s", want, observation)
		}
	}
	if !strings.Contains(observation, "用户在本轮确认后加载") {
		t.Fatalf("the semantic refusal must explain the confirmation rule: %s", observation)
	}
	exhausted := noProgressObservation(ToolOutcome{Reason: "no_evidence"}, true)
	if !strings.Contains(exhausted, "没有进展空间") {
		t.Fatalf("an exhausted run must say so: %s", exhausted)
	}
}

// The four enumerated results must stay distinguishable. Flattening them into
// "no records" tells the learner something untrue about their own history.
func TestTerminalReasonsAreNotFlattenedIntoNoRecords(t *testing.T) {
	seen := map[string]string{}
	for _, reason := range []string{"semantic_not_permitted", "already_in_context", "no_authoritative_topic", "no_evidence", "no_topic_consent"} {
		guidance, ok := TerminalToolGuidance(reason)
		if !ok || strings.TrimSpace(guidance) == "" {
			t.Fatalf("%s has no guidance", reason)
		}
		if previous, clash := seen[guidance]; clash {
			t.Fatalf("%s and %s share one message", reason, previous)
		}
		seen[guidance] = reason
	}
	if _, ok := TerminalToolGuidance("top_k_invalid"); ok {
		t.Fatal("a fixable schema error must stay retryable")
	}
}

func TestOnlyReadOnlyMemoryToolsUseTheLedger(t *testing.T) {
	for _, name := range []string{memorySearchToolName, topicMemoryReadToolName} {
		if !readOnlyMemoryTool(name) {
			t.Fatalf("%s must use the ledger", name)
		}
	}
	// Registering these under the ledger would change modes whose published
	// results were measured without it.
	for _, name := range []string{"knowledge_search", "read_history_message", "propose_memory", ""} {
		if readOnlyMemoryTool(name) {
			t.Fatalf("%s must not use the ledger", name)
		}
	}
}

// A refusal is free; a search that ran and found nothing is not.
func TestRefusalAndEmptyResultAreChargedDifferently(t *testing.T) {
	refusal := `{"status":"invalid_request","reason":"semantic_not_permitted","results":[]}`
	retrieved, reason := memorySearchRetrieved(refusal)
	if retrieved || reason != "semantic_not_permitted" {
		t.Fatalf("a refusal must not count as retrieval: retrieved=%v reason=%q", retrieved, reason)
	}
	empty := `{"status":"no_evidence","reason":"no_evidence","results":[]}`
	if retrieved, _ := memorySearchRetrieved(empty); !retrieved {
		t.Fatal("an executed search that found nothing did reach storage")
	}
	stop := `{"status":"stop_searching","reason":"no_evidence","results":[]}`
	if retrieved, _ := memorySearchRetrieved(stop); retrieved {
		t.Fatal("a suppressed call never reached storage")
	}
	// Anything unparseable is treated as a real call rather than a free one, so
	// an unknown shape can never become an unmetered retry loop.
	if retrieved, _ := memorySearchRetrieved("not json at all"); !retrieved {
		t.Fatal("an unrecognised result must be charged")
	}
}

// Two runs must not share a ledger: a replay or a concurrent run would
// otherwise inherit another run's exhausted allowance.
func TestLedgersAreNotSharedBetweenRuns(t *testing.T) {
	first := newToolProgressLedger()
	second := newToolProgressLedger()
	const arguments = `{"kinds":["episodic"],"query":"same question","top_k":5}`
	for i := 0; i < noProgressAllowance; i++ {
		first.Observe(memorySearchToolName, arguments, ToolOutcome{Retrieved: false, Reason: "top_k_invalid"})
	}
	if decision, _ := first.Decide(memorySearchToolName, arguments); decision == ToolDecisionRun {
		t.Fatal("the first run should have spent its allowance")
	}
	if decision, _ := second.Decide(memorySearchToolName, arguments); decision != ToolDecisionRun {
		t.Fatal("a second run must start with a clean ledger")
	}
	if second.NoProgressCount() != 0 {
		t.Fatalf("a fresh run starts at zero, got %d", second.NoProgressCount())
	}
}
