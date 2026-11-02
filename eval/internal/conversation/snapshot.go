package conversation

import (
	"fmt"
	"strings"

	"GopherAI/model"
)

// Snapshot holds only a single conversation and the metadata needed to restore it.
// Message IDs are preserved because summary evidence refers to them.
type Snapshot struct {
	Session  model.Session         `json:"session"`
	Messages []model.Message       `json:"messages"`
	Summary  *model.SessionSummary `json:"summary,omitempty"`
}

// Validate rejects foreign rows, reordered messages, and dangling summary evidence.
func (s Snapshot) Validate() error {
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
