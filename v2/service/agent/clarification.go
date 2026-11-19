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
		ClarificationID: clarification.ClarificationID,
		Question:        clarification.Question,
		Prompt:          clarification.Prompt,
		AllowFreeText:   clarification.AllowFreeText,
		Candidates:      make([]ClarificationCandidate, 0, len(clarification.Candidates)),
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

// The clarification block the next turn carries.
//
// It is what tells the model that the learner answered a question, and what
// the answer was. Without it the reply is a bare word in the conversation
// and the model has to guess that it answered anything.
//
// It also says the two things the model would otherwise get wrong: answer
// the PENDING question (the one before the clarification, not the reply), and
// treat the learner's own words as a clue that may reach conversations that
// were not offered.
func clarificationBlock(clarification *model.SessionClarification, reply string) string {
	if clarification == nil {
		return ""
	}
	chosen, matched := clarification.Resolve(reply)
	var b strings.Builder
	b.WriteString("The learner was asked which conversation they meant, and they answered.\n")
	if matched && chosen != nil {
		b.WriteString(fmt.Sprintf("They chose: %s (conversation id %s).\n", chosen.Label(), chosen.SessionID))
	} else {
		b.WriteString(fmt.Sprintf("They wrote their own words: %q.\n", strings.TrimSpace(reply)))
		b.WriteString("Those words are a clue, not a refusal: use them to search again, including " +
			"conversations that were not in the list you offered.\n")
	}
	b.WriteString(fmt.Sprintf("The question that was waiting for an answer was: %s\n", clarification.Question))
	b.WriteString("Answer THAT question, in the conversation they chose. Open it with " +
		"search_session_cards naming its conversation id, then read it with browse_summary_segments " +
		"or read_session_whiteboard.")
	return b.String()
}
