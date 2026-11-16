package conversation

import (
	"fmt"
	"strings"

	"GopherAI/model"
)

// SessionSnapshot holds one conversation and the metadata needed to restore
// it. Message IDs are preserved because summary evidence refers to them.
type SessionSnapshot struct {
	Session  model.Session         `json:"session"`
	Messages []model.Message       `json:"messages"`
	Summary  *model.SessionSummary `json:"summary,omitempty"`
}

// Snapshot holds the conversations of one attempt.
//
// It used to hold exactly one conversation. It now holds a list, because a
// comparison of two cross-session memory paths needs the retrieval to have
// more than one conversation to choose from: an attempt whose database holds
// only the conversation the question is about measures nothing about
// retrieval, because there is nothing to retrieve between.
//
// The list is ordered, and the order is the order the conversations were
// prepared in. The first one is the attempt's own conversation; the question
// is asked in a further one, which never carries any of them.
type Snapshot struct {
	Sessions []SessionSnapshot `json:"sessions"`
}

// Primary is the attempt's own conversation.
//
// An attempt that carries more than one conversation still has exactly one
// that the question is about, and the harness needs to name it.
func (s Snapshot) Primary() (SessionSnapshot, bool) {
	if len(s.Sessions) == 0 {
		return SessionSnapshot{}, false
	}
	return s.Sessions[0], true
}

// primaryOf is the attempt's own conversation, for the call sites that want
// one value out of it.
func primaryOf(s Snapshot) SessionSnapshot {
	primary, _ := s.Primary()
	return primary
}

// SessionIDs names the conversations of the attempt, in order.
func (s Snapshot) SessionIDs() []string {
	out := make([]string, 0, len(s.Sessions))
	for _, session := range s.Sessions {
		out = append(out, session.Session.ID)
	}
	return out
}

// MessageCount is how many messages the attempt carries.
func (s Snapshot) MessageCount() int {
	total := 0
	for _, session := range s.Sessions {
		total += len(session.Messages)
	}
	return total
}

// Validate rejects foreign rows, reordered messages, dangling summary
// evidence, and a set of conversations that does not belong to one learner.
func (s Snapshot) Validate() error {
	if len(s.Sessions) == 0 {
		return fmt.Errorf("the attempt carries no conversation")
	}
	var ownerID int64
	var ownerName string
	seenSessions := map[string]struct{}{}
	// The message id is the table's primary key and it is global, so two
	// conversations of one attempt must not share a message id.
	seenMessages := map[uint]struct{}{}
	for index, session := range s.Sessions {
		if err := session.Validate(); err != nil {
			return fmt.Errorf("conversation %d: %w", index+1, err)
		}
		if _, exists := seenSessions[session.Session.ID]; exists {
			return fmt.Errorf("conversation %d: the id is already in the attempt", index+1)
		}
		seenSessions[session.Session.ID] = struct{}{}
		if index == 0 {
			ownerID, ownerName = session.Session.UserID, session.Session.UserName
		} else if session.Session.UserID != ownerID || session.Session.UserName != ownerName {
			return fmt.Errorf("conversation %d: belongs to another learner", index+1)
		}
		for _, message := range session.Messages {
			if _, exists := seenMessages[message.ID]; exists {
				return fmt.Errorf("conversation %d: message %d is already in the attempt", index+1, message.ID)
			}
			seenMessages[message.ID] = struct{}{}
		}
	}
	return nil
}

// Validate rejects foreign rows, reordered messages, and dangling summary
// evidence in one conversation.
func (s SessionSnapshot) Validate() error {
	if err := s.Session.ValidateForCreate(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if s.Session.Mode != model.SessionModeAgent {
		return fmt.Errorf("session is not an agent session")
	}
	ids := make(map[uint]model.Message, len(s.Messages))
	var previous uint
	for i, m := range s.Messages {
		if m.ID == 0 || m.ID <= previous || m.SessionID != s.Session.ID || m.UserName != s.Session.UserName || strings.TrimSpace(m.Content) == "" {
			return fmt.Errorf("invalid message %d", i+1)
		}
		if m.IsUser != (i%2 == 0) {
			return fmt.Errorf("invalid message role at %d", i+1)
		}
		// These fields are omitted by the product JSON tags. Reject instead of
		// silently losing run provenance or episode citations during export.
		if m.PersistenceKey != nil || m.AgentRunID != nil || m.AgentRunRole != nil || len(m.EpisodeSources) != 0 {
			return fmt.Errorf("message %d has unsupported provenance", i+1)
		}
		previous = m.ID
		ids[m.ID] = m
	}
	if len(s.Messages) == 0 || len(s.Messages)%2 != 0 {
		return fmt.Errorf("history must contain complete turns")
	}
	if s.Summary == nil {
		return nil
	}
	if err := s.Summary.Validate(); err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	if s.Summary.SessionID != s.Session.ID || s.Summary.UserID != s.Session.UserID {
		return fmt.Errorf("summary belongs to another session")
	}
	if _, ok := ids[s.Summary.CoveredMessageID]; !ok {
		return fmt.Errorf("summary watermark is not a message")
	}
	doc, err := model.DecodeSessionSummaryDocument(s.Summary.Content)
	if err != nil {
		return fmt.Errorf("summary content: %w", err)
	}
	for _, group := range []model.SessionSummaryEvidenceList{doc.ConfirmedDecisions, doc.OpenLoops, doc.UserCorrections} {
		for _, item := range group {
			for _, id := range item.EvidenceMessageIDs {
				m, ok := ids[id]
				if !ok || !m.IsUser || id > s.Summary.CoveredMessageID {
					return fmt.Errorf("invalid summary evidence id %d", id)
				}
			}
		}
	}
	return nil
}
