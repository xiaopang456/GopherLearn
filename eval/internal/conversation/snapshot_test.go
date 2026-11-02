package conversation

import (
	"GopherAI/model"
	"testing"
)

func validSnapshot() Snapshot {
	return Snapshot{Session: model.Session{ID: "test-session", UserID: 7, UserName: "learner", Mode: model.SessionModeAgent}, Messages: []model.Message{{ID: 1, SessionID: "test-session", UserName: "learner", Content: "hello", IsUser: true}, {ID: 2, SessionID: "test-session", UserName: "learner", Content: "hi"}}, Summary: &model.SessionSummary{UserID: 7, SessionID: "test-session", CoveredMessageID: 2, Version: 1, Content: `{"schema_version":2,"current_goal":"","confirmed_decisions":[{"text":"learning","evidence_message_ids":[1]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`}}
}
func TestSnapshotChecksReferences(t *testing.T) {
	if err := validSnapshot().Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Snapshot){
		"foreign message":    func(s *Snapshot) { s.Messages[0].SessionID = "foreign" },
		"foreign owner":      func(s *Snapshot) { s.Summary.UserID = 8 },
		"dangling watermark": func(s *Snapshot) { s.Summary.CoveredMessageID = 5 },
		"assistant evidence": func(s *Snapshot) {
			s.Summary.Content = `{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[2]}]}`
		},
		"future evidence": func(s *Snapshot) {
			s.Summary.CoveredMessageID = 1
			s.Summary.Content = `{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[2]}]}`
		},
		"reordered ids": func(s *Snapshot) { s.Messages[1].ID = 1 },
		"partial turn":  func(s *Snapshot) { s.Messages = s.Messages[:1] },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			s := validSnapshot()
			change(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}
