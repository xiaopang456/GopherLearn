package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The clarification card.
//
// It is the shape the learner is shown when the model could not tell which
// conversation they meant, and it is the same shape the memory draft card
// is: a card under the assistant's message, with the choices the learner can
// make.
//
// It is on the message rather than only in the run's result because the
// learner answers it in their NEXT message, and the answer has to be
// resolvable against the candidates that were offered.

// ClarificationCard is what the learner is shown.
type ClarificationCard struct {
	// ClarificationID identifies this clarification.
	ClarificationID string `json:"clarificationId"`
	// Question is the question that is waiting for an answer.
	Question string `json:"question"`
	// Prompt says what the learner may do.
	Prompt string `json:"prompt,omitempty"`
	// Candidates are the conversations that were offered.
	Candidates []ClarificationCandidate `json:"candidates"`
	// AllowFreeText says the learner may answer with their own words.
	AllowFreeText bool `json:"allowFreeText"`
	// AllowHistoryBrowse says the learner may pick from their OWN list of
	// conversations instead of from the candidates.
	//
	// It is the second of the clarification's three operations, and it is
	// what makes a target that never made it into a candidate list
	// reachable.
	AllowHistoryBrowse bool `json:"allowHistoryBrowse"`
}

// ClarificationCandidate is one conversation the learner may pick.
type ClarificationCandidate struct {
	// SessionID is the conversation.
	SessionID string `json:"sessionId"`
	// Title is the conversation's own title.
	Title string `json:"title"`
	// Topics are the subjects the conversation was filed under.
	Topics []string `json:"topics,omitempty"`
	// LastTalkedAt is when it was last talked in.
	LastTalkedAt string `json:"lastTalkedAt,omitempty"`
}

// NewClarificationID mints the identifier of a clarification.
//
// It is a random identifier rather than a counter, because two turns of two
// learners may ask at the same moment and a counter would have them share
// one.
func NewClarificationID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		// A clarification without an id cannot be answered by id, and
		// the learner answering it by number or title still works. The
		// id is a convenience, not the mechanism.
		return ""
	}
	return hex.EncodeToString(buffer)
}

// ClarificationTokens is what a clarification costs the answer.
//
// It is the cost of the rendered list: the candidates are short, and the
// question they carry is the learner's own.
func ClarificationTokens(clarification *model.SessionClarification) int {
	if clarification == nil {
		return 0
	}
	cost := rag.EstimateTokens(clarification.Question) + 8
	for _, candidate := range clarification.Candidates {
		cost += rag.EstimateTokens(candidate.Label()) + 4
		for _, topic := range candidate.Topics {
			cost += rag.EstimateTokens(topic) + 1
		}
	}
	return cost
}

// ClarificationCardOf renders the card the learner is shown.
func ClarificationCardOf(clarification *model.SessionClarification) *ClarificationCard {
	if clarification == nil {
		return nil
	}
	card := &ClarificationCard{
		ClarificationID:    clarification.ClarificationID,
		Question:           clarification.Question,
		Prompt:             clarification.Prompt,
		AllowFreeText:      clarification.AllowFreeText,
		AllowHistoryBrowse: clarification.AllowHistoryBrowse,
		Candidates:         make([]ClarificationCandidate, 0, len(clarification.Candidates)),
	}
	for _, candidate := range clarification.Candidates {
		entry := ClarificationCandidate{
			SessionID: candidate.SessionID,
			Title:     candidate.Title,
			Topics:    append([]string{}, candidate.Topics...),
		}
		if !candidate.LastTalkedAt.IsZero() {
			entry.LastTalkedAt = candidate.LastTalkedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		card.Candidates = append(card.Candidates, entry)
	}
	return card
}

// CloneClarificationCard copies a card.
func CloneClarificationCard(value *ClarificationCard) *ClarificationCard {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Candidates = append([]ClarificationCandidate{}, value.Candidates...)
	return &copy
}

// pendingClarification is the question the model is waiting for an answer
// to.
//
// It is pending when the LAST thing the assistant said was a question: the
// learner answers it in this turn, and this turn's question is their answer.
// A clarification that has an assistant message after it was already
// answered, and answering it again would answer a question the learner is
// no longer asking.
func pendingClarification(history []model.Message) *model.SessionClarification {
	for index := len(history) - 1; index >= 0; index-- {
		message := history[index]
		if message.IsUser {
			continue
		}
		return message.Clarification
	}
	return nil
}

// maxClarificationTokens bounds the clarification block.
//
// The block is a few lines: which question was waiting, what the learner
// said, and - when they picked one - which conversation they picked. A
// hundred tokens is enough for that, and a bound is what keeps a long
// learner's own words from turning the block into the largest thing in the
// window.
const maxClarificationTokens = 200

// maxLockedSessionTokens bounds the evidence of one locked conversation.
//
// It is bigger than the clarification block's cap because the evidence IS
// the answer's input: a block that says which conversation the learner meant
// is a few lines, and a block that says what is in it is the conversation's
// own compressed body. Two thousand tokens is a whole session's summary.
const maxLockedSessionTokens = 2000

// clarificationContinuationPrompt is what a turn that continues a clarified
// question is told about the tools it may use.
//
// It is a separate block and NOT a line of the base prompt, on purpose: the
// base prompt's bytes are the ones the published measurements were taken
// against, and a rule that matters only on a turn that continues a
// clarification must not move them.
//
// Each line names ONE tool, and the renderer drops the line when this turn
// does not have that tool. So the model is told to use the tools this turn
// actually offers, and is never told to use one it cannot see - which is the
// whole of J12: the summary browse is registered only when the summary is
// in the applied mode, and the segment search only when the segment index
// is there. A rule that named both unconditionally would be a rule the
// model could not obey half the time.
const clarificationContinuationPrompt = `
You are continuing a question the learner asked in an earlier turn, after they told you which conversation they meant.
- Read that conversation's summary with browse_summary_segments, naming the conversation in its session parameter.
- When that browse does not reach the answer, find the stretch of turns you are looking for with search_history_segments.
- When neither the summary nor the segment search carries the detail, search the conversation's own words with search_session_history: a short keyword that would appear in the sentence you need, not a whole question sentence.
- When a tool asks which conversation to read, pass the conversation ID exactly as the locked block writes it, not the conversation's title. A title is not a conversation id: a read that names the title is refused, and the answer then has to be guessed instead of read.`

// The clarification block the next turn carries.
//
// It is what tells the model that the learner answered a question, what the
// question was, and what the answer was. Without it the reply is a bare
// word in the conversation and the model has to guess that it answered
// anything.
//
// It says DIFFERENTLY depending on what the reply was, and the difference
// is the point:
//
//   - The reply names one of the candidates. Then the learner has chosen a
//     conversation, and the answer is the waiting question in that
//     conversation. The block says so, and it says how to open it.
//   - The reply names none of them. Then the learner has NOT refused and has
//     NOT necessarily chosen anything: "the one where we talked about
//     context on Monday" is a better clue, "I mean this chat" points at the
//     conversation in front of them, and "actually, how do I create a
//     channel?" is a new question. The block hands the model the waiting
//     question as BACKGROUND and the learner's own words, and lets it work
//     out which of the three it is.
//
// The second case deliberately carries no instruction. A block that said
// "search again" would send the model off through the learner's history to
// answer a question the learner may have abandoned, and a block that said
// "answer the waiting question" would make the model answer a question the
// learner replaced. The model is the one that can read the conversation, and
// the server cannot: it sees the reply, not what the reply meant.
func clarificationBlock(clarification *model.SessionClarification, reply string) string {
	if clarification == nil {
		return ""
	}
	chosen, matched := clarification.Resolve(reply)
	var b strings.Builder
	b.WriteString("The learner was asked which conversation they meant.\n")
	b.WriteString(fmt.Sprintf("The question that was waiting for an answer was: %s\n", clarification.Question))
	if matched && chosen != nil {
		b.WriteString(fmt.Sprintf("They chose the conversation %s (conversation id %s).\n",
			chosen.Label(), chosen.SessionID))
		b.WriteString("Answer the waiting question in that conversation: open it with the " +
			"cross-session search tool, then read it with the history-reading tools this " +
			"turn offers.")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("They answered: %q\n", strings.TrimSpace(reply)))
	b.WriteString("That reply may name a different conversation, point at this one, or ask " +
		"something else. Work out which, and answer accordingly.")
	return b.String()
}
