package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The clarification.
//
// It is what a turn looks like when the model cannot answer without knowing
// WHICH conversation the learner meant.
//
// The conversations of one learner look alike from the outside: four of them
// may each have arranged three exercises, and the question "what was the
// second one" names none of them. The model finds all four, and it cannot
// pick one - so it asks, and this is what it asked.
//
// The learner answers in one of two ways, and both are supported:
//
//   - They pick one of the candidates. The reply is the candidate's id, its
//     number in the list, or its title.
//   - They write their own clue - a title, a time, a topic. That is not one
//     of the candidates, and it is not a refusal either: it is a better
//     search, and the search is allowed to reach conversations that were not
//     in the list.
//
// The learner does not have to ask again. The pending question travels with
// the clarification, and the next turn answers THAT question in the
// conversation that was chosen.
type SessionClarification struct {
	// ClarificationID identifies this clarification.
	ClarificationID string `json:"clarification_id"`
	// Question is the question that was waiting for an answer.
	//
	// It is the learner's own question, kept whole: the next turn answers
	// it, and a question that has to be reconstructed from a summary is a
	// question that was already answered wrongly once.
	Question string `json:"question"`
	// Prompt says what the learner may do, in the words of the model.
	Prompt string `json:"prompt,omitempty"`
	// Candidates are the conversations that were offered.
	//
	// They are the ones the model found and the ones the learner may
	// pick. A conversation outside this list may still be reached - by the
	// learner's own clue - but it was not offered.
	Candidates []SessionClarificationCandidate `json:"candidates"`
	// AllowFreeText says the learner may answer with their own words.
	AllowFreeText bool `json:"allow_free_text"`
	// CreatedAt is when the clarification was asked.
	CreatedAt time.Time `json:"created_at"`
}

// SessionClarificationCandidate is one conversation the learner may pick.
type SessionClarificationCandidate struct {
	// SessionID is the conversation.
	SessionID string `json:"session_id"`
	// Title is the conversation's own title.
	Title string `json:"title"`
	// Topics are the subjects the conversation was filed under.
	Topics []string `json:"topics,omitempty"`
	// LastTalkedAt is when it was last talked in.
	LastTalkedAt time.Time `json:"last_talked_at,omitempty"`
}

// Label is what the learner is shown.
//
// It names the conversation and when it happened, because a title alone does
// not tell two conversations of the same family apart.
func (c SessionClarificationCandidate) Label() string {
	title := strings.TrimSpace(c.Title)
	if title == "" {
		title = c.SessionID
	}
	if c.LastTalkedAt.IsZero() {
		return title
	}
	return fmt.Sprintf("%s（%s）", title, c.LastTalkedAt.Local().Format("2006-01-02"))
}

// Resolve reads the learner's reply and says which conversation it named.
//
// The second return value says whether a candidate was named at all. A reply
// that named none is the learner's own clue, and the caller has to treat it
// as a search rather than as a refusal.
//
// The order of the readings is the order of how sure they are: the id is
// exact, the number is what the button carries, and the title is a reading
// of what the learner typed.
func (c *SessionClarification) Resolve(reply string) (*SessionClarificationCandidate, bool) {
	if c == nil {
		return nil, false
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return nil, false
	}
	for index := range c.Candidates {
		candidate := &c.Candidates[index]
		if candidate.SessionID != "" && reply == candidate.SessionID {
			return candidate, true
		}
	}
	if number, err := strconv.Atoi(reply); err == nil {
		if number >= 1 && number <= len(c.Candidates) {
			return &c.Candidates[number-1], true
		}
	}
	lowered := strings.ToLower(reply)
	for index := range c.Candidates {
		candidate := &c.Candidates[index]
		title := strings.ToLower(strings.TrimSpace(candidate.Title))
		if title == "" {
			continue
		}
		if lowered == title || strings.Contains(lowered, title) {
			return candidate, true
		}
	}
	return nil, false
}

// Rendered is the list of candidates as the learner reads them.
//
// It is the same list the model writes into its answer, so that a client
// which cannot draw buttons still shows the learner the same choices.
func (c *SessionClarification) Rendered() string {
	if c == nil || len(c.Candidates) == 0 {
		return ""
	}
	lines := make([]string, 0, len(c.Candidates))
	for index, candidate := range c.Candidates {
		line := fmt.Sprintf("%d. %s", index+1, candidate.Label())
		if topics := candidate.topicsText(); topics != "" {
			line += " — " + topics
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// topicsText is the candidate's subjects as one line.
func (c SessionClarificationCandidate) topicsText() string {
	out := make([]string, 0, len(c.Topics))
	for _, topic := range c.Topics {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}
		out = append(out, topic)
	}
	return strings.Join(out, "、")
}

// Validate refuses a clarification that cannot be shown.
func (c *SessionClarification) Validate() error {
	if c == nil {
		return fmt.Errorf("clarification is nil")
	}
	if strings.TrimSpace(c.Question) == "" {
		return fmt.Errorf("clarification has no question")
	}
	if len(c.Candidates) == 0 {
		return fmt.Errorf("clarification has no candidates")
	}
	if len(c.Candidates) > 8 {
		return fmt.Errorf("clarification has too many candidates")
	}
	for _, candidate := range c.Candidates {
		if strings.TrimSpace(candidate.SessionID) == "" {
			return fmt.Errorf("a clarification candidate has no session")
		}
	}
	return nil
}
