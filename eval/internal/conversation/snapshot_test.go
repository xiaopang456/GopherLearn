package conversation

import (
	"testing"

	"GopherAI/model"
)

// validSnapshot is one conversation of one learner: the shape the harness
// still checks for, and the shape a bundle of them is made of.
func validSnapshot() Snapshot {
	return Snapshot{Sessions: []SessionSnapshot{{
		Session: model.Session{ID: "test-session", UserID: 7, UserName: "learner", Mode: model.SessionModeAgent},
		Messages: []model.Message{
			{ID: 1, SessionID: "test-session", UserName: "learner", Content: "hello", IsUser: true},
			{ID: 2, SessionID: "test-session", UserName: "learner", Content: "hi"},
		},
		Summary: &model.SessionSummary{UserID: 7, SessionID: "test-session", CoveredMessageID: 2, Version: 1,
			Content: `{"schema_version":2,"current_goal":"","confirmed_decisions":[{"text":"learning","evidence_message_ids":[1]}],"completed_items":[],"open_loops":[],"user_corrections":[],"key_errors":[],"references":[]}`},
	}}}
}

func TestSnapshotChecksReferences(t *testing.T) {
	if err := validSnapshot().Validate(); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*Snapshot){
		"foreign message":    func(s *Snapshot) { s.Sessions[0].Messages[0].SessionID = "foreign" },
		"foreign owner":      func(s *Snapshot) { s.Sessions[0].Summary.UserID = 8 },
		"dangling watermark": func(s *Snapshot) { s.Sessions[0].Summary.CoveredMessageID = 5 },
		"assistant evidence": func(s *Snapshot) {
			s.Sessions[0].Summary.Content = `{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[2]}]}`
		},
		"future evidence": func(s *Snapshot) {
			s.Sessions[0].Summary.CoveredMessageID = 1
			s.Sessions[0].Summary.Content = `{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[2]}]}`
		},
		"reordered ids":  func(s *Snapshot) { s.Sessions[0].Messages[1].ID = 1 },
		"partial turn":   func(s *Snapshot) { s.Sessions[0].Messages = s.Sessions[0].Messages[:1] },
		"run provenance": func(s *Snapshot) { v := "run"; s.Sessions[0].Messages[0].AgentRunID = &v },
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

// A bundle has to belong to one learner, and its conversations must not share
// a message id: the message id is the table's primary key.
func TestSnapshotChecksTheBundle(t *testing.T) {
	bundle := validSnapshot()
	second := bundle.Sessions[0]
	second.Session.ID = "other-session"
	second.Messages = []model.Message{
		{ID: 3, SessionID: "other-session", UserName: "learner", Content: "hello", IsUser: true},
		{ID: 4, SessionID: "other-session", UserName: "learner", Content: "hi"},
	}
	second.Summary = nil
	bundle.Sessions = append(bundle.Sessions, second)
	if err := bundle.Validate(); err != nil {
		t.Fatalf("a bundle of one learner was refused: %v", err)
	}
	if len(bundle.SessionIDs()) != 2 || bundle.SessionIDs()[0] != "test-session" {
		t.Fatalf("session ids=%+v", bundle.SessionIDs())
	}
	if bundle.MessageCount() != 4 {
		t.Fatalf("message count=%d", bundle.MessageCount())
	}

	// Another learner is refused.
	foreign := bundle
	foreign.Sessions = append([]SessionSnapshot(nil), bundle.Sessions...)
	other := foreign.Sessions[1]
	other.Session.UserID = 8
	foreign.Sessions[1] = other
	if err := foreign.Validate(); err == nil {
		t.Fatal("a bundle of two learners was accepted")
	}

	// A message id that is already in the bundle is refused.
	clash := bundle
	clash.Sessions = append([]SessionSnapshot(nil), bundle.Sessions...)
	same := clash.Sessions[1]
	same.Messages = []model.Message{
		{ID: 1, SessionID: "other-session", UserName: "learner", Content: "hello", IsUser: true},
		{ID: 4, SessionID: "other-session", UserName: "learner", Content: "hi"},
	}
	clash.Sessions[1] = same
	if err := clash.Validate(); err == nil {
		t.Fatal("a bundle with two conversations of one message id was accepted")
	}
}
