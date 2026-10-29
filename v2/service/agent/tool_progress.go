package agent

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

// The progress ledger exists because STABILIZE-054's acceptance found three
// runs failing on tool_call_limit after three memory_search calls, and the
// records show why. Every one of them went:
//
//	1. kinds=[semantic,...]  -> invalid_request / semantic_not_permitted
//	2. kinds=[episodic] top_k=6 -> invalid_request / top_k_invalid
//	3. kinds=[episodic] top_k=5 -> refused, per-tool budget exhausted
//
// The model actually corrected itself. What defeated it is that two protocol
// refusals - neither of which touched a DAO, an index or a provider, and
// neither of which returned a single card - had already consumed the whole
// per-tool retrieval budget. memory_search's own code says a malformed call
// "never spends one of the run's two searches"; the runner charged it anyway,
// because reserveTool increments before the tool body ever runs. Two layers
// disagreed about what a call costs, and the model paid for the difference.
//
// So this is not "add repeated-call detection". ErrRepeatedToolCall already
// exists, and it would not have fired here either: it compares only against the
// immediately previous fingerprint, and calls 2 and 3 differ by one digit of
// top_k. The fix is to make the cost of a call match what the call actually
// did.

// ToolOutcome is what one executed tool call achieved.
type ToolOutcome struct {
	// Retrieved reports whether the call reached storage at all. A schema
	// rejection did not, and must not be charged as a retrieval.
	Retrieved bool
	// NewEvidence is how many evidence references the call added to the run.
	NewEvidence int
	// Reason is the tool's own enumerated reason, used to decide whether this
	// kind of request is worth retrying at all.
	Reason string
}

// ToolDecision is what the ledger says about a call that is about to be made.
type ToolDecision int

const (
	// ToolDecisionRun means execute normally and charge the budget.
	ToolDecisionRun ToolDecision = iota
	// ToolDecisionStop means do not execute: an equivalent call already ran and
	// produced nothing new, so repeating it cannot reach storage again. The
	// caller returns a terminal observation instead of erroring.
	ToolDecisionStop
	// ToolDecisionExhausted means the run has spent its allowance for calls
	// that make no progress. Every further read-only memory call is answered
	// with a terminal observation.
	ToolDecisionExhausted
)

// noProgressAllowance bounds how many attempts that achieved nothing one run
// may make before the ledger answers only with "stop searching".
//
// Three, because of exactly the sequence the acceptance produced: a refused
// capability, a malformed argument, and then the corrected call. Two would
// block the correction - the very thing that was already being punished - and a
// larger number would just be budget for rotating words.
const noProgressAllowance = 3

// toolProgressLedger tracks, for one run, what each read-only tool call
// achieved, so a call that cannot achieve anything new is neither executed nor
// charged.
//
// It is per-run by construction: it is created with the runState and never
// shared, so a concurrent run and an idempotent replay cannot inherit another
// run's history.
type toolProgressLedger struct {
	mu sync.Mutex
	// outcomes maps a semantic key to what the last equivalent call achieved.
	outcomes map[string]ToolOutcome
	// noProgress counts attempts that reached no storage or added no evidence.
	noProgress int
	// lastRefusal is the most recent non-retrieving outcome, kept so an
	// exhausted run can still explain itself specifically.
	lastRefusal ToolOutcome
}

func newToolProgressLedger() *toolProgressLedger {
	return &toolProgressLedger{outcomes: map[string]ToolOutcome{}}
}

// Decide reports what should happen to a call before it runs.
func (l *toolProgressLedger) Decide(name, arguments string) (ToolDecision, ToolOutcome) {
	if l == nil {
		return ToolDecisionRun, ToolOutcome{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.noProgress >= noProgressAllowance {
		return ToolDecisionExhausted, ToolOutcome{}
	}
	previous, seen := l.outcomes[semanticToolKey(name, arguments)]
	if seen && previous.NewEvidence == 0 {
		// The same request, semantically, already reached storage and came back
		// with nothing. It must not reach a DAO, an index or a provider again.
		return ToolDecisionStop, previous
	}
	return ToolDecisionRun, ToolOutcome{}
}

// lastOutcomeFor is used to give an exhausted run the most specific guidance
// available rather than a generic one.
func (l *toolProgressLedger) lastOutcomeFor(name, arguments string) ToolOutcome {
	if l == nil {
		return ToolOutcome{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if outcome, ok := l.outcomes[semanticToolKey(name, arguments)]; ok {
		return outcome
	}
	return l.lastRefusal
}

// Observe records what a call achieved and reports whether it made progress.
func (l *toolProgressLedger) Observe(name, arguments string, outcome ToolOutcome) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if outcome.Retrieved {
		// Only an executed call tells us anything about storage. Recording a
		// refusal here would mark the request as fruitless without ever having
		// asked, and would block the corrected retry - which is precisely how
		// the acceptance runs died.
		l.outcomes[semanticToolKey(name, arguments)] = outcome
	} else {
		l.lastRefusal = outcome
	}
	progressed := outcome.Retrieved && outcome.NewEvidence > 0
	if !progressed {
		l.noProgress++
	}
	return progressed
}

// NoProgressCount is diagnostics for tests and reports.
func (l *toolProgressLedger) NoProgressCount() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.noProgress
}

// semanticToolKey is the identity two calls share when they ask the same
// question of the same tool.
//
// It is not the raw JSON and not canonicalJSON either. Field order and
// whitespace are already normalised by canonicalisation, but that is not
// enough: a model can rotate a free-text rationale or nudge a result count and
// produce a different fingerprint while asking for exactly the same thing.
// Fields that cannot change what storage returns are therefore dropped, and the
// remaining values are normalised.
func semanticToolKey(name, arguments string) string {
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		return strings.ToLower(strings.TrimSpace(name)) + "\x00" + strings.Join(strings.Fields(arguments), " ")
	}
	for _, field := range nonSemanticToolFields {
		delete(decoded, field)
	}
	parts := make([]string, 0, len(decoded))
	for key, value := range decoded {
		parts = append(parts, strings.ToLower(strings.TrimSpace(key))+"="+normaliseToolValue(value))
	}
	sort.Strings(parts)
	return strings.ToLower(strings.TrimSpace(name)) + "\x00" + strings.Join(parts, "\x1f")
}

// nonSemanticToolFields cannot change which records storage returns.
//
// top_k is here because it selects how many of the same ranked results come
// back, not which records exist: a search that found nothing at top_k=6 finds
// nothing at top_k=5, and treating those as different requests is precisely how
// the acceptance runs burned their budget. reason and note are free text the
// model writes for itself.
var nonSemanticToolFields = []string{"reason", "note", "notes", "rationale", "explanation", "top_k", "topk"}

func normaliseToolValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.ToLower(strings.Join(strings.Fields(typed), " "))
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			items = append(items, normaliseToolValue(item))
		}
		// Sorted: asking for [semantic,episodic] and [episodic,semantic] is one
		// request, and a model must not get a second budgeted attempt by
		// swapping the order.
		sort.Strings(items)
		return strings.Join(items, ",")
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+":"+normaliseToolValue(typed[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// terminalToolReasons are enumerated results that cannot be improved by asking
// again with different words.
//
// They are distinguished from one another on purpose. "you are not allowed to
// search this" and "there is nothing recorded" are different facts about the
// learner, and an answer that confuses them tells them something untrue.
var terminalToolReasons = map[string]string{
	"semantic_not_permitted": "本轮不允许通过 memory_search 读取主题语义记忆：主题记忆只能由用户在本轮确认后加载。" +
		"不要再用 kinds=[\"semantic\"] 重试；改用通用知识、知识库证据或已经在上下文里的内容作答。",
	"already_in_context": "这些记录已经在你的上下文里了，重复检索不会得到新内容。直接使用已有的引用作答。",
	"no_authoritative_topic": "服务端当前没有可用的权威主题，本轮没有可读取的主题记忆。" +
		"不要改写检索词重试；基于通用知识作答，并说明本轮未使用学习记录。",
	"no_evidence": "这次检索确实没有找到相关记录。不要换个说法再搜一次；据实说明没有找到相关历史，然后正常作答。",
	"no_topic_consent": "用户本轮没有授权任何学习主题，因此没有 LearningState 或主题记忆可读。" +
		"不要尝试用 memory_search 绕过；正常作答并说明本轮未获得学习记录授权，用户可以重新开启并选择主题。",
}

// readOnlyMemoryTool reports whether a tool only reads long-term memory.
//
// Only these participate in the progress ledger. propose_memory writes, and
// knowledge_search and read_history_message have their own budgets and their
// own meaning of progress; charging them by this rule would change modes whose
// results were measured without it.
func readOnlyMemoryTool(name string) bool {
	return CapabilityOf(name).ReadsLongTermMemory()
}

// memorySearchRetrieved reports whether a tool result represents a call that
// actually reached storage, and the enumerated reason it carried.
//
// A refusal is recognised by its own status rather than by an empty result set:
// "I searched and found nothing" and "I refused to search" are different facts,
// and only the second one is free.
func memorySearchRetrieved(result string) (bool, string) {
	var payload struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		return true, ""
	}
	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case "invalid_request", "not_permitted", "unauthorized", "stop_searching":
		return false, payload.Reason
	default:
		return true, payload.Reason
	}
}

// TerminalToolGuidance returns the instruction for an enumerated reason, and
// whether that reason is terminal at all.
func TerminalToolGuidance(reason string) (string, bool) {
	guidance, ok := terminalToolReasons[strings.ToLower(strings.TrimSpace(reason))]
	return guidance, ok
}

// noProgressObservation is what a suppressed call returns to the model.
//
// It is a normal tool result rather than an error, because the run must
// continue: the model has to be told that searching again is pointless and that
// answering now is the expected next step.
func noProgressObservation(previous ToolOutcome, exhausted bool) string {
	guidance, ok := TerminalToolGuidance(previous.Reason)
	if !ok {
		guidance = "同样的检索已经做过，没有返回新的证据。不要再用同义改写重试。"
	}
	if exhausted {
		guidance = "本轮记忆检索已经没有进展空间。" + guidance
	}
	payload := map[string]any{
		"status":    "stop_searching",
		"reason":    strings.TrimSpace(previous.Reason),
		"retryable": false,
		"results":   []any{},
		"summary":   guidance,
		"next_step": "立即基于现有信息作答；如果问题需要学习记录而本轮没有授权，请说明这一点，不要编造。",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return `{"status":"stop_searching","retryable":false,"summary":"停止检索并直接作答。"}`
	}
	return string(encoded)
}
