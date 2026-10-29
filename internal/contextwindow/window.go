package contextwindow

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

var (
	// ErrRequiredExceedsBudget reports a turn whose mandatory parts - the system
	// prompt, the learner's question and the tool offers - do not fit inside the
	// input target once the evidence allowance is held back. It is a refusal rather
	// than a trim: the three things that do not fit are the three that cannot be
	// dropped, and dropping the question to keep the rest would answer something the
	// learner never asked.
	ErrRequiredExceedsBudget = errors.New("contextwindow: the required parts of this turn exceed the input target")
	// ErrAssembledExceedsTarget is the final check over the messages as they will
	// actually be sent. It should not be reachable, and a turn that reaches it is
	// refused instead of being sent on an estimate that has disagreed with itself.
	ErrAssembledExceedsTarget = errors.New("contextwindow: the assembled request exceeds the input target")
	// ErrWindowExceeded reports an estimated input that, added to the output the same
	// request would allow itself, does not fit the declared window. It is the check
	// the input target alone cannot make, because a target says nothing about the
	// reply.
	ErrWindowExceeded = errors.New("contextwindow: the request does not fit the declared window")
)

// Parts are the pieces a learning-agent turn is made of, before any of them has been
// trimmed. The caller hands over what it has; this package decides what fits.
type Parts struct {
	// SystemPrompt is the strategy's own instruction text, including any server
	// commentary appended to it. It is the only instruction in the request.
	SystemPrompt string
	// TaskState is the server's own structured statement of what the learner is
	// mid-way through - the active quiz's topic, status, progress and, when one
	// is open, the question now being asked. It arrives as a system message of
	// its own, placed after the policy and before every optional block, because
	// it is required rather than injected: like the question, it cannot be
	// trimmed, and a turn whose task state did not fit is refused rather than
	// sent half-said. Empty means there is no active task.
	TaskState string
	// Mastery is the independently budgeted learner-progress block. It is
	// optional context, never part of SystemPrompt; the policy remains required
	// and stable while this block may be trimmed or omitted.
	Mastery string
	// GlobalMemory, TopicMemory and EpisodeMemory name the v2 layer seams even
	// when a provider is absent. Empty means no provider result, not that a
	// layer was folded into another block.
	GlobalMemory  string
	TopicMemory   string
	EpisodeMemory string
	// Summary is the session's sealed rolling summary, unbounded.
	Summary string
	// History holds complete exchanges after the summary's boundary, oldest first.
	// A caller passes every candidate it has: choosing among them by budget is this
	// package's job, so exactly one window rule is applied to a turn.
	History []responder.Turn
	// Question is what the learner asked now. It appears once, as the last turn.
	Question string
	// Tools are the offers sent with every call of this turn.
	Tools []model.ToolDefinition
	// DocumentKnowledgeReachable says whether the turn's evidence tools can
	// emit anything at all. It is the one half of a two-sided rule the caller
	// owns: when it is false the evidence allowance is not held back - the
	// required parts and the history win back those tokens - and the caller
	// must, on the same predicate, not offer the evidence tools. Reserving
	// nothing while the tools stay registered is the one direction that breaks
	// the invariant, because assembled context plus evidence could then exceed
	// the input target.
	DocumentKnowledgeReachable bool
}

// Breakdown reports what the budget was spent on. It exists so a reader can tell
// "the summary was empty" from "the summary was dropped to make room", and so a
// regression in one block shows up as a number rather than as a missing sentence.
type Breakdown struct {
	RequiredTokens      int
	ToolTokens          int
	TaskStateTokens     int
	MasteryTokens       int
	GlobalMemoryTokens  int
	TopicMemoryTokens   int
	EpisodeMemoryTokens int
	SummaryTokens       int
	HistoryTokens       int
	AssembledTokens     int
	EvidenceReserved    int
	TargetTokens        int

	// SummaryTruncated says the stored summary was longer than what the budget
	// allowed and reached the model cut short.
	SummaryTruncated bool
	// SummaryDropped says there was a summary but no room for any of it, which is a
	// different outcome from a shortened one and must not be reported as the same.
	SummaryDropped bool

	// HistoryCandidates and HistoryKept count whole exchanges, so a window that
	// shrank is visible as a rate rather than only as a token figure.
	HistoryCandidates int
	HistoryKept       int
	HistoryDropped    int
}

// Build lays out one learning-agent prompt inside the budget and returns it with the
// breakdown of how the budget went.
//
// The order is deliberate. Mandatory parts are checked first because they can only be
// refused, not shortened. Then the summary, because it stands in for messages that are
// no longer sent verbatim and losing it costs the oldest context. Then the exchanges,
// newest first, because the end of the conversation is the part a model should weight
// most. The evidence allowance is never spent here: it is what the turn has not asked
// for yet.
//
// Nothing here deletes a stored message. A block that does not fit is left out of this
// one request, which is a read-side decision.
func Build(parts Parts, config Config) ([]model.Message, Breakdown, error) {
	pairs := completePairs(parts.History)
	// The evidence allowance is the one budget line the reference project made
	// conditional: a run whose evidence tools are not even registered spends
	// the reserve on nothing, and it does so on exactly the questions that most
	// need conversation history. The caller decides reachability on the same
	// predicate that gates the tool offers, so the two can never disagree.
	evidenceReserved := 0
	if parts.DocumentKnowledgeReachable {
		evidenceReserved = config.EvidenceMaxTokens
	}
	breakdown := Breakdown{
		RequiredTokens:    MessageTokens(parts.SystemPrompt) + MessageTokens(parts.Question),
		ToolTokens:        ToolDefinitionTokens(parts.Tools),
		EvidenceReserved:  evidenceReserved,
		TargetTokens:      config.InputTargetTokens,
		HistoryCandidates: len(pairs),
	}
	breakdown.RequiredTokens += breakdown.ToolTokens
	if strings.TrimSpace(parts.TaskState) != "" {
		// Task state is required, priced like any system message, and refused
		// rather than trimmed when it does not fit.
		breakdown.TaskStateTokens = MessageTokens(parts.TaskState)
		breakdown.RequiredTokens += breakdown.TaskStateTokens
	}

	// The evidence allowance is held back whether or not a tool is likely to run
	// on a reachable turn. The reference project decided this per question from a
	// keyword list, which is a guess about the model's next move; holding it on
	// every reachable turn costs nothing on a turn that stays conversational and
	// is the only way a turn that searches late still fits.
	available := config.InputTargetTokens - breakdown.EvidenceReserved
	if available < 0 {
		available = 0
	}
	if breakdown.RequiredTokens > available {
		return nil, breakdown, fmt.Errorf("%w: required %d against %d available",
			ErrRequiredExceedsBudget, breakdown.RequiredTokens, available)
	}
	remaining := available - breakdown.RequiredTokens

	// Optional v2 layers are trimmed independently in production priority order:
	// mastery, global/topic/episode context, then summary, then history.
	layer := func(text string, cap int) string {
		if strings.TrimSpace(text) == "" || cap <= 0 || remaining <= 0 {
			return ""
		}
		value := PrefixWithinTokens(strings.TrimSpace(text), minInt(cap, remaining))
		cost := MessageTokens(value)
		if cost > remaining {
			return ""
		}
		remaining -= cost
		return value
	}
	mastery := layer(parts.Mastery, config.MasteryMaxTokens)
	if mastery != "" {
		breakdown.MasteryTokens = MessageTokens(mastery)
	}
	globalMemory := layer(parts.GlobalMemory, config.GlobalMemoryMaxTokens)
	if globalMemory != "" {
		breakdown.GlobalMemoryTokens = MessageTokens(globalMemory)
	}
	topicMemory := layer(parts.TopicMemory, config.TopicMemoryMaxTokens)
	if topicMemory != "" {
		breakdown.TopicMemoryTokens = MessageTokens(topicMemory)
	}
	summary := ""
	if text := strings.TrimSpace(parts.Summary); text != "" {
		// The block is the label, the preamble and the text, in one message, so its
		// framing is paid for before any content is.
		envelope := Tokens(responder.SummaryBlock("")) + MessageEnvelopeTokens
		budget := minInt(config.SummaryMaxTokens, remaining) - envelope
		if budget > 0 {
			summary = PrefixWithinTokens(text, budget)
			breakdown.SummaryTruncated = summary != text
			breakdown.SummaryTokens = Tokens(responder.SummaryBlock(summary)) + MessageEnvelopeTokens
		} else {
			// Nothing is left, so the summary goes out as if the session had none.
			// Sending a longer one instead would make the cap a suggestion, and
			// dropping only part of it silently would be a third outcome nobody
			// could see in the numbers.
			breakdown.SummaryDropped = true
		}
		remaining -= breakdown.SummaryTokens
	}

	history := selectExchanges(pairs, historyLimits(config, remaining))
	for _, exchange := range history {
		breakdown.HistoryTokens += MessageTokens(exchange.Content)
	}
	breakdown.HistoryKept = len(history) / 2
	breakdown.HistoryDropped = breakdown.HistoryCandidates - breakdown.HistoryKept
	remaining -= breakdown.HistoryTokens

	// Episode is the final derived layer: it can consume only what summary and
	// recent history left. This is the v2 allocation order; it must not displace
	// either block merely because the episode card itself fits its own cap.
	episodeMemory := layer(parts.EpisodeMemory, config.EpisodeMaxTokens)
	if episodeMemory != "" {
		breakdown.EpisodeMemoryTokens = MessageTokens(episodeMemory)
	}

	messages := responder.PromptMessages(parts.SystemPrompt, summary, history, parts.Question)
	// Optional layer messages are inserted after the policy and before summary
	// and recent history, preserving v2 priority without granting them policy authority.
	optional := make([]model.Message, 0, 4)
	for _, text := range []string{mastery, globalMemory, topicMemory, episodeMemory} {
		if strings.TrimSpace(text) != "" {
			optional = append(optional, model.Message{Role: model.RoleSystem, Content: text})
		}
	}
	if len(optional) > 0 {
		insert := 0
		if len(messages) > 0 && messages[0].Role == model.RoleSystem {
			insert = 1
		}
		shifted := make([]model.Message, 0, len(messages)+len(optional))
		shifted = append(shifted, messages[:insert]...)
		shifted = append(shifted, optional...)
		shifted = append(shifted, messages[insert:]...)
		messages = shifted
	}
	if breakdown.TaskStateTokens > 0 {
		// Right after the policy, before the summary and the exchanges: the task
		// state says what the learner is mid-way through, which frames everything
		// below it. Built into a fresh slice so the layout's own backing array is
		// never reshuffled in place.
		insert := 0
		if len(messages) > 0 && messages[0].Role == model.RoleSystem {
			insert = 1
		}
		shifted := make([]model.Message, 0, len(messages)+1)
		shifted = append(shifted, messages[:insert]...)
		shifted = append(shifted, model.Message{Role: model.RoleSystem, Content: parts.TaskState})
		shifted = append(shifted, messages[insert:]...)
		messages = shifted
	}
	breakdown.AssembledTokens = RequestTokens(model.Request{Messages: messages, Tools: parts.Tools})
	if breakdown.AssembledTokens > available {
		// The per-block arithmetic and the request-wide measurement disagree. Trust
		// the wider one and refuse, rather than sending a request that only one of
		// the two checks cleared.
		return nil, breakdown, fmt.Errorf("%w: assembled %d against %d available",
			ErrAssembledExceedsTarget, breakdown.AssembledTokens, available)
	}
	return messages, breakdown, nil
}

// CheckRequest is the pre-flight measure taken on a request as it will actually be
// sent, including the calls a turn has already accumulated. It answers a different
// question from Build: Build decides what a turn starts with, and this decides whether
// the next call may be made at all.
//
// defaultMaxOutputTokens is what the provider configuration will apply to a request
// that names no limit of its own, so an override set anywhere in the code is measured
// rather than assumed away.
func CheckRequest(request model.Request, config Config, defaultMaxOutputTokens int) error {
	estimated := RequestTokens(request)
	available := config.InputTargetTokens
	if estimated > available {
		return fmt.Errorf("%w: %d estimated input against an input target of %d",
			ErrAssembledExceedsTarget, estimated, available)
	}
	maxOutput := request.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutputTokens
	}
	if maxOutput > 0 && estimated+maxOutput > config.WindowTokens {
		return fmt.Errorf("%w: %d estimated input plus an output limit of %d against a declared window of %d",
			ErrWindowExceeded, estimated, maxOutput, config.WindowTokens)
	}
	return nil
}

// exchangeLimits are the three ceilings a recent window answers to at once. They are
// kept apart because each one sees a failure the others cannot: a count is blind to one
// enormous answer, a rune count cannot price a token bill, and a token estimate is wider
// than runes for Latin-script text. The selection applies all three and the tightest
// wins, which is what lets a token budget be added without the window growing.
type exchangeLimits struct {
	pairs  int
	runes  int
	tokens int
}

// historyLimits derives them for one turn, where the token ceiling is also capped by
// whatever the mandatory parts and the injected summary left behind.
func historyLimits(config Config, remaining int) exchangeLimits {
	limits := exchangeLimits{
		pairs:  1 << 30,
		runes:  1 << 30,
		tokens: 1 << 30,
	}
	if config.HistoryMaxTokens > 0 {
		limits.tokens = minInt(config.HistoryMaxTokens, remaining)
	} else {
		limits.tokens = remaining
	}
	if config.HistoryMaxMessages > 0 {
		limits.pairs = config.HistoryMaxMessages / 2
	}
	if config.HistoryMaxRunes > 0 {
		limits.runes = config.HistoryMaxRunes
	}
	if limits.pairs < 1 {
		limits.pairs = 1
	}
	return limits
}

// selectExchanges keeps the newest complete exchanges that fit every ceiling.
//
// It walks backwards and stops at the first pair that does not fit instead of skipping
// it: a window with a hole in the middle reads as a broken conversation, and the
// reference project's alternative - cutting the newest message in half to squeeze it
// in - is the one thing this project refuses to do, because a question whose answer
// was dropped invites the model to answer it again.
func selectExchanges(candidates [][]responder.Turn, limits exchangeLimits) []responder.Turn {
	if limits.tokens <= 0 || len(candidates) == 0 {
		return nil
	}
	kept := make([]responder.Turn, 0, len(candidates)*2)
	usedTokens, usedRunes := 0, 0
	for index := len(candidates) - 1; index >= 0; index-- {
		pair := candidates[index]
		if len(kept)/2+1 > limits.pairs {
			break
		}
		cost := MessageTokens(pair[0].Content) + MessageTokens(pair[1].Content)
		runes := len([]rune(pair[0].Content)) + len([]rune(pair[1].Content))
		if usedTokens+cost > limits.tokens || usedRunes+runes > limits.runes {
			break
		}
		usedTokens += cost
		usedRunes += runes
		kept = append([]responder.Turn{pair[0], pair[1]}, kept...)
	}
	return kept
}

// completePairs keeps only user→assistant pairs, walking forward and stepping over an
// orphan by one message rather than by a whole window. A trailing half-exchange is
// dropped: a question with no answer behind it must not be presented to the model as an
// exchange that was settled, and a mid-list orphan must not shift every pair after it.
func completePairs(history []responder.Turn) [][]responder.Turn {
	pairs := make([][]responder.Turn, 0, len(history)/2)
	for index := 0; index+1 < len(history); {
		question, answer := history[index], history[index+1]
		if question.Role != model.RoleUser || answer.Role != model.RoleAssistant {
			index++
			continue
		}
		pairs = append(pairs, []responder.Turn{question, answer})
		index += 2
	}
	return pairs
}

func minInt(left, right int) int {
	if right < left {
		return right
	}
	return left
}
