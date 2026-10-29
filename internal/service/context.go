package service

import (
	"time"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// Context budget defaults. A model call is charged per token and a session can
// hold thousands of messages, so the recent window is bounded by both a count and
// a size: either limit alone can be defeated by the other (12 short greetings, or
// two enormous answers).
const (
	DefaultContextMaxMessages = 12
	DefaultContextMaxRunes    = 6000

	roleUser      = "user"
	roleAssistant = "assistant"
)

// ContextBudget bounds how much stored history one turn sends to a model.
type ContextBudget struct {
	MaxMessages int
	MaxRunes    int

	// Window is the budget a learning-agent turn is assembled under, and the only
	// window rule that strategy has. Its history ceilings carry the same message and
	// rune limits above, so the migration added a token dimension to the existing
	// window rather than replacing it.
	//
	// It sits here rather than on the agent because the conversation service is the one
	// that reads stored history, and a turn whose service trimmed by runes while its
	// agent trimmed by tokens would give the learner a recollection that depends on
	// which rule got there last.
	//
	// A chat or knowledge turn does not read it: those strategies keep the message and
	// rune limits above, unchanged.
	Window contextwindow.Config

	// AgentTurnDeadline is the one clock an agent turn spends from: the summary
	// refresh and the model loop draw on it together instead of each starting a fresh
	// timer. Zero leaves the turn on the caller's context, which is what every other
	// strategy has always had and what a hand-built budget keeps.
	AgentTurnDeadline time.Duration
}

// Normalize replaces unusable limits with the defaults. Zero means "not
// configured", never "send nothing".
func (b ContextBudget) Normalize() ContextBudget {
	if b.MaxMessages <= 0 {
		b.MaxMessages = DefaultContextMaxMessages
	}
	if b.MaxRunes <= 0 {
		b.MaxRunes = DefaultContextMaxRunes
	}
	if b.AgentTurnDeadline < 0 {
		b.AgentTurnDeadline = 0
	}
	// An illegal window is not clamped here: it is refused by name at assembly, where
	// a startup can still fail. Normalizing a negative to a default would report an
	// operator's typo as their choice.
	window, err := b.Window.Resolve()
	if err != nil {
		window = contextwindow.DefaultConfig()
	}
	b.Window = window
	return b
}

// agentCandidates returns what one learning-agent turn may choose history from: the
// stored messages after the summary's boundary, newest `HistoryFetchLimit` of them,
// converted without deciding anything.
//
// This is a read bound and not a window. The ceilings that decide what reaches the
// model - messages, runes and estimated tokens - all live in the one selection that
// contextwindow applies, because it is the only place that can see the system prompt,
// the tool offers and the injected summary alongside the exchanges. Trimming here as
// well would put two owners on one transcript, and the two would disagree as soon as a
// session's text was neither CJK nor short.
func agentCandidates(messages []domain.Message, budget ContextBudget) []responder.Turn {
	source := messages
	if limit := budget.Window.HistoryFetchLimit; limit > 0 && len(source) > limit {
		// It counts messages rather than exchanges because that is the unit the store
		// returns. A candidate slice that starts mid-exchange is no problem: the
		// selection matches roles before it pairs anything.
		source = source[len(source)-limit:]
	}
	turns := make([]responder.Turn, 0, len(source))
	for _, message := range source {
		turns = append(turns, responder.Turn{Role: message.Role, Content: message.Content})
	}
	return turns
}

// recentTurns selects the stored messages a model turn may see, newest first by
// cost but returned in conversation order.
//
// Only completed user→assistant pairs qualify. A turn whose model call failed -
// or whose assistant message could not be stored - leaves a user message with no
// answer behind it, and feeding that back would show the model a question it has
// already been asked but never answered, inviting it to answer the same thing
// twice with no memory of the failure. Nothing is deleted: this is a read-side
// filter over an append-only transcript, and the history endpoint still returns
// every stored message as it is.
//
// Selection walks backwards and stops at the first pair that does not fit rather
// than skipping it and reaching further back, because a window with a hole in the
// middle reads as a broken conversation.
//
// The current question is not part of this window: the caller passes it separately
// and the history is read before the new user message is stored, so the question
// reaches the model exactly once.
func recentTurns(messages []domain.Message, budget ContextBudget) []responder.Turn {
	budget = budget.Normalize()
	type completed struct {
		question int
		answer   int
	}
	// Collected newest-first, so the loop below can stop at the first pair that
	// does not fit and still hand back an oldest-first window.
	pairs := make([]completed, 0, budget.MaxMessages/2+1)
	used := 0
	for index := len(messages) - 1; index >= 1; {
		answer := messages[index]
		question := messages[index-1]
		if answer.Role != roleAssistant || question.Role != roleUser {
			// An orphan: an unanswered question, or an answer whose question is
			// gone. Step one message and look for the next complete pair.
			index--
			continue
		}
		pairRunes := len([]rune(question.Content)) + len([]rune(answer.Content))
		if len(pairs)*2+2 > budget.MaxMessages || used+pairRunes > budget.MaxRunes {
			break
		}
		used += pairRunes
		pairs = append(pairs, completed{question: index - 1, answer: index})
		index -= 2
	}
	selected := make([]responder.Turn, 0, len(pairs)*2)
	for index := len(pairs) - 1; index >= 0; index-- {
		pair := pairs[index]
		selected = append(selected,
			responder.Turn{Role: messages[pair.question].Role, Content: messages[pair.question].Content},
			responder.Turn{Role: messages[pair.answer].Role, Content: messages[pair.answer].Content},
		)
	}
	return selected
}
