package summary

import (
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// Stored message roles. The domain keeps roles as plain text, so this package names
// the two it recognizes rather than reaching into another package for the constants.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// Why a refresh was not run. These are reported rather than invented: a skipped
// refresh is normal, and the reason says whether the conversation is simply too short
// or the stored boundary cannot be trusted.
const (
	// SkipBelowThreshold means neither threshold has been passed: not enough
	// uncovered messages, and not enough uncovered material to estimate as tokens.
	SkipBelowThreshold = "below_threshold"
	// SkipNothingToCompress means everything uncovered still fits in the verbatim
	// window, or the input budget leaves no whole pair to compress.
	SkipNothingToCompress = "nothing_beyond_the_kept_window"
	// SkipInputTooLarge means the oldest exchange alone does not fit the summarizing
	// input budget once the previous summary is counted. Nothing can be compressed
	// without dropping a whole exchange from the summary's coverage, so the session
	// keeps its old boundary and says why rather than reporting a routine skip.
	SkipInputTooLarge = "oldest_exchange_exceeds_the_summary_input"
	// SkipBoundaryMissing means the stored summary points at a message this session
	// no longer contains. Refreshing is refused rather than guessed at: the only
	// alternatives are re-summarizing messages already inside the text, or assuming
	// the boundary is somewhere newer - and either could rewrite history. The
	// conversation service ends the whole turn on this code, because the same unknown
	// coverage also makes the window impossible to cut honestly.
	SkipBoundaryMissing = "summary_boundary_missing"
)

// messagePair is one complete exchange, by index into the stored message slice.
type messagePair struct{ question, answer int }

// Plan is what one refresh would do.
type Plan struct {
	// Compress holds the complete pairs that go into the new summary, oldest first.
	Compress []responder.Turn
	// CompressIDs names each entry of Compress: the stored message id the turn
	// was built from, in the same order. The structured summary cites these ids
	// as evidence_message_ids, so the summarizer's payload must show them - a
	// model cannot cite an id it was never shown. User entries are the only ones
	// eligible to be cited as evidence; assistant entries exist so their source
	// handles can update the references field.
	CompressIDs []string
	// CompressSources carries, per entry of Compress, the citations stored on
	// that message. Only assistant messages carry any; the summarizer renders
	// them as opaque source handles for the references field, never as prose.
	CompressSources [][]domain.Source
	// CoveredMessageID is the assistant message this refresh ends at: the new
	// summary's boundary. It is a stored id, so the next turn can find the exact
	// same place again.
	CoveredMessageID string
	// CompressedMessages counts entries in Compress, and UncoveredMessages counts
	// every complete message past the boundary before the verbatim window was
	// subtracted. Both are reported so a reader can tell "too short" from "nothing
	// left over".
	CompressedMessages int
	UncoveredMessages  int
	// UncoveredTokens is the estimate over those same exchanges, which is the figure
	// the token threshold is compared with. It is a measurement of what would be sent,
	// not a claim about the model's tokenizer.
	UncoveredTokens int
	// TriggeredBy says which threshold the refresh passed: "messages", "tokens", or
	// "" when it ran neither.
	TriggeredBy string
	// Skipped is "" when a refresh should run, and one of the Skip constants when it
	// must not.
	Skipped string
}

// Build plans one refresh over the stored messages of one session.
//
// The conversation is read in append order, and only complete user→assistant pairs
// qualify. A user message with no answer behind it - the turn that failed, or the
// question being answered right now - is left out entirely: it is neither compressed
// into a summary nor presented as an answered exchange. Nothing is deleted from the
// store on account of it; this is a read-side decision.
//
// The pairs are taken from the oldest side of what is uncovered, so the summary always
// covers one contiguous stretch from the previous boundary forward. Trimming from the
// other end would leave a hole, and a summary with a hole in it cannot be extended
// again without guessing what fell in it.
//
// previous is the summary already stored for this session. The planner needs it because
// that text is part of the request the refresh makes: a budget measured only over the
// new exchanges would let the payload grow with every refresh while each individual
// step looked compliant.
//
// controls decides which of the migrated rules this turn applies. A refresh planned with
// the legacy set is byte-for-byte the decision this project made before the migration:
// same trigger, same message and rune bounds, no token bound and no cooldown.
func Build(messages []domain.Message, boundaryMessageID, previous string, policy Policy,
	controls Controls,
) Plan {
	policy = policy.Normalize()
	start := 0
	if boundary := strings.TrimSpace(boundaryMessageID); boundary != "" {
		index := indexOfMessage(messages, boundary)
		if index < 0 {
			return Plan{Skipped: SkipBoundaryMissing}
		}
		// The boundary itself is already summarized; resume after it.
		start = index + 1
	}

	pairs := completePairs(messages[start:])
	for index := range pairs {
		pairs[index].question += start
		pairs[index].answer += start
	}

	plan := Plan{UncoveredMessages: len(pairs) * 2}
	byMessages := plan.UncoveredMessages >= policy.TriggerMessages
	plan.UncoveredTokens = countTokens(messages, previous, pairs)
	byTokens := controls.TokenTrigger && plan.UncoveredTokens >= policy.TriggerTokens
	// The message threshold is compared as it always was - reached, not exceeded - and
	// the token threshold the same way, so the two read alike: either one on its own
	// makes a refresh due, and the label records which one did. The token half only
	// exists where the migrated controls were asked for; a chat turn is triggered by
	// message count exactly as it was before, and cannot be made to pay for a refresh
	// by a number it never had.
	switch {
	case byMessages:
		plan.TriggeredBy = "messages"
	case byTokens:
		plan.TriggeredBy = "tokens"
	default:
		plan.Skipped = SkipBelowThreshold
		return plan
	}
	if len(pairs)-recentPairCount(policy.KeepRecentMessages) <= 0 {
		// Past a threshold but with nothing beyond the verbatim tail: the newest
		// exchanges stay word for word, which is the whole point of keeping them.
		plan.Skipped = SkipNothingToCompress
		return plan
	}
	compressPairs := len(pairs) - recentPairCount(policy.KeepRecentMessages)
	// Trim the newest compressed pairs until the set fits the input budget. Refusing
	// to send part of a pair is the point: the budget is measured over whole
	// exchanges, so the result is never a truncated turn.
	for compressPairs > 0 && !fitsBudget(pairs[:compressPairs], messages, previous, policy, controls) {
		compressPairs--
	}
	if compressPairs <= 0 {
		// Distinguish "there was nothing to compress" from "the oldest exchange alone
		// cannot be sent". The first is normal; the second means this session can never
		// compact again on its own, and reading it as the first is how a growing blind
		// spot gets reported as a healthy skip.
		if len(pairs) > 0 && !fitsBudget(pairs[:1], messages, previous, policy, controls) {
			plan.Skipped = SkipInputTooLarge
			return plan
		}
		plan.Skipped = SkipNothingToCompress
		return plan
	}

	plan.Compress = make([]responder.Turn, 0, compressPairs*2)
	plan.CompressIDs = make([]string, 0, compressPairs*2)
	plan.CompressSources = make([][]domain.Source, 0, compressPairs*2)
	for _, entry := range pairs[:compressPairs] {
		plan.Compress = append(plan.Compress,
			responder.Turn{Role: roleUser, Content: messages[entry.question].Content},
			responder.Turn{Role: roleAssistant, Content: messages[entry.answer].Content},
		)
		plan.CompressIDs = append(plan.CompressIDs, messages[entry.question].ID, messages[entry.answer].ID)
		plan.CompressSources = append(plan.CompressSources, sourcesOf(messages[entry.question]), sourcesOf(messages[entry.answer]))
	}
	plan.CompressedMessages = len(plan.Compress)
	plan.CoveredMessageID = messages[pairs[compressPairs-1].answer].ID
	return plan
}

// sourcesOf copies a message's citations, or returns nil when it has none. A
// nil entry keeps the parallel slices aligned without implying "empty handle
// list" to the renderer.
func sourcesOf(message domain.Message) []domain.Source {
	if len(message.Sources) == 0 {
		return nil
	}
	return append([]domain.Source(nil), message.Sources...)
}

// completePairs walks stored messages in order and returns every user→assistant pair.
// An orphan steps forward by one message rather than skipping a whole window, so a
// failed turn in the middle does not hide the exchanges after it.
func completePairs(messages []domain.Message) []messagePair {
	pairs := make([]messagePair, 0, len(messages)/2+1)
	for index := 0; index+1 < len(messages); {
		if messages[index].Role == roleUser && messages[index+1].Role == roleAssistant {
			pairs = append(pairs, messagePair{question: index, answer: index + 1})
			index += 2
			continue
		}
		index++
	}
	return pairs
}

// countTokens estimates what a refresh over these exchanges would cost, measured with
// the same ruler that prices the request about to be sent - both messages, the framing
// and the previous summary included. The planner deliberately does not keep its own
// approximation of the labels: an estimate that is close to the payload but computed a
// different way is the exact shape of bug where a plan says "fits" and the request does
// not.
func countTokens(messages []domain.Message, previous string, pairs []messagePair) int {
	return requestCost(previous, materialTurns(messages, pairs), materialIDs(messages, pairs), materialSources(messages, pairs))
}

// materialTurns flattens stored pairs into the sequence the renderer consumes.
func materialTurns(messages []domain.Message, pairs []messagePair) []responder.Turn {
	turns := make([]responder.Turn, 0, len(pairs)*2)
	for _, entry := range pairs {
		turns = append(turns,
			responder.Turn{Role: roleUser, Content: messages[entry.question].Content},
			responder.Turn{Role: roleAssistant, Content: messages[entry.answer].Content},
		)
	}
	return turns
}

// materialIDs names the same sequence, from the same rows.
func materialIDs(messages []domain.Message, pairs []messagePair) []string {
	ids := make([]string, 0, len(pairs)*2)
	for _, entry := range pairs {
		ids = append(ids, messages[entry.question].ID, messages[entry.answer].ID)
	}
	return ids
}

// materialSources carries the same sequence's citations.
func materialSources(messages []domain.Message, pairs []messagePair) [][]domain.Source {
	sources := make([][]domain.Source, 0, len(pairs)*2)
	for _, entry := range pairs {
		sources = append(sources, sourcesOf(messages[entry.question]), sourcesOf(messages[entry.answer]))
	}
	return sources
}

// fitsBudget reports whether one candidate compress set can be sent.
//
// Messages and runes bound every refresh, as they always did. The estimated-token bound
// counts the whole request - the previous summary and the framing included - and only
// applies where the migrated controls were asked for, so a chat turn is still trimmed by
// the two limits it has always had and nothing else.
func fitsBudget(pairs []messagePair, messages []domain.Message, previous string, policy Policy,
	controls Controls,
) bool {
	if len(pairs)*2 > policy.MaxInputMessages {
		return false
	}
	used := 0
	for _, entry := range pairs {
		used += len([]rune(messages[entry.question].Content)) + len([]rune(messages[entry.answer].Content))
		if used > policy.MaxInputRunes {
			return false
		}
	}
	if !controls.TokenInputCap {
		return true
	}
	return requestCost(previous, materialTurns(messages, pairs), materialIDs(messages, pairs),
		materialSources(messages, pairs)) <= policy.MaxInputTokens
}

func indexOfMessage(messages []domain.Message, id string) int {
	for index := range messages {
		if messages[index].ID == id {
			return index
		}
	}
	return -1
}
