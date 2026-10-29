package topicconsent

import (
	"strings"
	"testing"
	"time"
)

// The first message of a new Agent chat could not use the learner's records,
// because a consent ticket binds a sessionId and the session did not exist
// until the first message was sent. The fix creates the session first. These
// tests pin what that must NOT change: every binding still applies to the first
// message exactly as it does to any later one.

func firstMessageSigner(t *testing.T) *Signer {
	t.Helper()
	signer, err := NewSigner("stabilize055-first-message-secret-0123456789", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	signer.UseClock(func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) })
	return signer
}

func firstMessageCandidates() []Candidate {
	return []Candidate{
		{TopicKey: "rpc粘包处理", DisplayName: "RPC粘包处理", Source: SourceGradedTopic},
		{TopicKey: "codec-注册表", DisplayName: "Codec 注册表", Source: SourceRecentNotes},
	}
}

// A ticket for a freshly created session is ordinary in every respect.
func TestFirstMessageTicketIsBoundLikeAnyOther(t *testing.T) {
	signer := firstMessageSigner(t)
	ticket, claims, err := signer.Issue(7, "brand-new-session", "req-first", "粘包怎么处理？", firstMessageCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if claims.SessionID != "brand-new-session" || claims.RequestID != "req-first" {
		t.Fatalf("claims = %+v", claims)
	}
	keys, err := signer.Verify(VerifyRequest{Ticket: ticket, Selected: []string{"rpc粘包处理"},
		UserID: 7, SessionID: "brand-new-session", RequestID: "req-first", Question: "粘包怎么处理？"})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(keys) != 1 || keys[0] != "rpc粘包处理" {
		t.Fatalf("keys = %v", keys)
	}
}

// Creating the session first must not create a ticket that works anywhere else.
func TestFirstMessageTicketStillFailsEveryCrossBinding(t *testing.T) {
	signer := firstMessageSigner(t)
	ticket, _, err := signer.Issue(7, "session-a", "req-first", "粘包怎么处理？", firstMessageCandidates())
	if err != nil {
		t.Fatal(err)
	}
	base := VerifyRequest{Ticket: ticket, Selected: []string{"rpc粘包处理"},
		UserID: 7, SessionID: "session-a", RequestID: "req-first", Question: "粘包怎么处理？"}

	crossUser := base
	crossUser.UserID = 8
	crossSession := base
	// The most important one for this change: a ticket minted for one brand-new
	// session must not carry over to the next brand-new session.
	crossSession.SessionID = "session-b"
	crossRequest := base
	crossRequest.RequestID = "req-second"
	crossQuestion := base
	crossQuestion.Question = "Codec 注册表怎么用？"

	for name, request := range map[string]VerifyRequest{
		"cross user": crossUser, "cross session": crossSession,
		"cross request": crossRequest, "cross question": crossQuestion,
	} {
		if _, err := signer.Verify(request); err == nil {
			t.Fatalf("%s must still be refused on a first message", name)
		}
	}
}

// An empty session id can never be signed or accepted: "the session does not
// exist yet" must not become a wildcard.
func TestEmptySessionIsNotAWildcard(t *testing.T) {
	signer := firstMessageSigner(t)
	if _, _, err := signer.Issue(7, "", "req-first", "粘包怎么处理？", firstMessageCandidates()); err == nil {
		t.Fatal("a ticket must not be issued without a session id")
	}
	if _, _, err := signer.Issue(7, "   ", "req-first", "粘包怎么处理？", firstMessageCandidates()); err == nil {
		t.Fatal("whitespace is not a session id")
	}
	ticket, _, err := signer.Issue(7, "session-a", "req-first", "粘包怎么处理？", firstMessageCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Verify(VerifyRequest{Ticket: ticket, Selected: []string{"rpc粘包处理"},
		UserID: 7, SessionID: "", RequestID: "req-first", Question: "粘包怎么处理？"}); err == nil {
		t.Fatal("verifying against an empty session must be refused")
	}
}

// The selection rules are unchanged for a first message.
func TestFirstMessageKeepsSelectionLimits(t *testing.T) {
	signer := firstMessageSigner(t)
	ticket, _, err := signer.Issue(7, "session-a", "req-first", "粘包怎么处理？", firstMessageCandidates())
	if err != nil {
		t.Fatal(err)
	}
	verify := func(selected ...string) error {
		_, err := signer.Verify(VerifyRequest{Ticket: ticket, Selected: selected, UserID: 7,
			SessionID: "session-a", RequestID: "req-first", Question: "粘包怎么处理？"})
		return err
	}
	if err := verify(); err != nil {
		t.Fatalf("selecting nothing must remain valid: %v", err)
	}
	if err := verify("rpc粘包处理", "codec-注册表"); err != nil {
		t.Fatalf("two topics must remain valid: %v", err)
	}
	if err := verify("rpc粘包处理", "codec-注册表", "tcp粘包"); err == nil {
		t.Fatal("three topics must still be refused")
	}
	if err := verify("tcp粘包"); err == nil {
		t.Fatal("a key that was never offered must still be refused")
	}
	if err := verify("rpc粘包处理"); err != nil {
		t.Fatalf("one offered topic must remain valid: %v", err)
	}
}

// The ticket still carries no question text, on a first message as anywhere.
func TestFirstMessageTicketCarriesNoQuestionText(t *testing.T) {
	signer := firstMessageSigner(t)
	ticket, _, err := signer.Issue(7, "session-a", "req-first", "粘包怎么处理？", firstMessageCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ticket, "粘包怎么处理") {
		t.Fatal("the ticket must not carry the learner's words")
	}
}
