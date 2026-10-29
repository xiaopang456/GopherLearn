package topicconsent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const testSecret = "stabilize050-test-signing-secret-0123456789"

func testSigner(t *testing.T) *Signer {
	t.Helper()
	signer, err := NewSigner(testSecret, 10*time.Minute)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	signer.UseClock(func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) })
	return signer
}

func testCandidates() []Candidate {
	return []Candidate{
		{TopicKey: "jwt-鉴权", DisplayName: "JWT 鉴权", Source: SourceGradedTopic},
		{TopicKey: "session-状态机", DisplayName: "Session 状态机", Source: SourceRecentNotes},
		{TopicKey: "go-并发", DisplayName: "Go 并发", Source: SourceLexicalMatch},
	}
}

func issued(t *testing.T, signer *Signer) string {
	t.Helper()
	ticket, _, err := signer.Issue(7, "sess-1", "req-1", "登录态怎么校验？", testCandidates())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	return ticket
}

func baseRequest(ticket string, selected ...string) VerifyRequest {
	return VerifyRequest{Ticket: ticket, Selected: selected, UserID: 7, SessionID: "sess-1",
		RequestID: "req-1", Question: "登录态怎么校验？"}
}

func TestSignerRefusesAWeakOrMissingSecret(t *testing.T) {
	for _, secret := range []string{"", "   ", strings.Repeat("a", MinSecretLength-1)} {
		if _, err := NewSigner(secret, time.Minute); err == nil {
			t.Fatalf("secret %q must be refused", secret)
		}
	}
	if _, err := NewSigner(testSecret, 2*time.Hour); err == nil {
		t.Fatal("an unbounded ticket lifetime must be refused")
	}
}

func TestValidTicketAuthorisesOnlyTheSelectedCandidates(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	keys, err := signer.Verify(baseRequest(ticket, "jwt-鉴权"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(keys) != 1 || keys[0] != "jwt-鉴权" {
		t.Fatalf("keys = %v", keys)
	}
}

func TestSelectingTwoCandidatesAuthorisesBothInCanonicalOrder(t *testing.T) {
	signer := testSigner(t)
	keys, err := signer.Verify(baseRequest(issued(t, signer), "session-状态机", "jwt-鉴权"))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// The verifier canonicalises a set of permissions. Click order must not
	// change the resulting context or the ticket's meaning.
	want := []string{"jwt-鉴权", "session-状态机"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

// A ticket with nothing ticked is a real answer - "use none of them" - and must
// not be confused with an absent ticket or with a failure.
func TestSelectingNothingIsAValidAnswer(t *testing.T) {
	signer := testSigner(t)
	keys, err := signer.Verify(baseRequest(issued(t, signer)))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want none", keys)
	}
}

func TestMissingTicketIsNotAnAuthorisation(t *testing.T) {
	signer := testSigner(t)
	if _, err := signer.Verify(VerifyRequest{UserID: 7, SessionID: "sess-1", RequestID: "req-1",
		Question: "登录态怎么校验？"}); err == nil {
		t.Fatal("an absent ticket must not authorise anything")
	}
}

// Every binding is checked, and each one is checked for the reason it exists:
// a ticket must not survive a different user, session, request or question.
func TestEveryBindingIsRechecked(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)

	cases := map[string]VerifyRequest{
		"cross user":     {Ticket: ticket, Selected: []string{"jwt-鉴权"}, UserID: 8, SessionID: "sess-1", RequestID: "req-1", Question: "登录态怎么校验？"},
		"cross session":  {Ticket: ticket, Selected: []string{"jwt-鉴权"}, UserID: 7, SessionID: "sess-2", RequestID: "req-1", Question: "登录态怎么校验？"},
		"cross request":  {Ticket: ticket, Selected: []string{"jwt-鉴权"}, UserID: 7, SessionID: "sess-1", RequestID: "req-2", Question: "登录态怎么校验？"},
		"cross question": {Ticket: ticket, Selected: []string{"jwt-鉴权"}, UserID: 7, SessionID: "sess-1", RequestID: "req-1", Question: "粘包怎么处理？"},
	}
	for name, request := range cases {
		if _, err := signer.Verify(request); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestForgedAndTamperedTicketsAreRefused(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	payload, signature, _ := strings.Cut(ticket, ".")

	other, err := NewSigner("a-different-secret-of-sufficient-length-0123456789", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	forged, _, err := other.Issue(7, "sess-1", "req-1", "登录态怎么校验？", testCandidates())
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"another signer":   forged,
		"tampered payload": payload + "x." + signature,
		"stripped":         payload,
		"empty signature":  payload + ".",
	} {
		if _, err := signer.Verify(baseRequest(bad, "jwt-鉴权")); err == nil {
			t.Fatalf("%s must be refused", name)
		}
	}
}

func TestExpiredTicketIsRefused(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	signer.UseClock(func() time.Time { return time.Date(2026, 9, 6, 12, 11, 0, 0, time.UTC) })
	if _, err := signer.Verify(baseRequest(ticket, "jwt-鉴权")); err == nil {
		t.Fatal("an expired ticket must be refused")
	}
}

func TestKeysOutsideTheOfferedCandidatesAreRefused(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	if _, err := signer.Verify(baseRequest(ticket, "redis-分布式锁")); err == nil {
		t.Fatal("a key that was never offered must be refused")
	}
}

func TestMoreThanTwoTopicsAreRefused(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	if _, err := signer.Verify(baseRequest(ticket, "jwt-鉴权", "session-状态机", "go-并发")); err == nil {
		t.Fatal("three topics must be refused")
	}
	// Duplicates must not buy a third slot either.
	keys, err := signer.Verify(baseRequest(ticket, "jwt-鉴权", "jwt-鉴权"))
	if err != nil || len(keys) != 1 {
		t.Fatalf("duplicates must collapse, got keys=%v err=%v", keys, err)
	}
}

// The same request id may be retried: the run path depends on it, and a ticket
// consumed on first use would break every replayed request.
func TestSameRequestIsIdempotent(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := signer.Verify(baseRequest(ticket, "jwt-鉴权")); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
}

func TestQuestionHashIgnoresOnlyWhitespace(t *testing.T) {
	if QuestionHash("登录态 怎么校验？") != QuestionHash("  登录态\n怎么校验？  ") {
		t.Fatal("re-wrapped whitespace must not change the question identity")
	}
	if QuestionHash("登录态怎么校验？") == QuestionHash("粘包怎么处理？") {
		t.Fatal("different questions must not share a hash")
	}
}

func TestTicketDoesNotCarryTheLearnersWords(t *testing.T) {
	signer := testSigner(t)
	ticket := issued(t, signer)
	if strings.Contains(ticket, "登录态") {
		t.Fatal("the ticket travels through the browser and must not carry the question text")
	}
}

// ---- suggestions ----

func staticTopics(topics ...Topic) TopicLister {
	return TopicListerFunc(func(context.Context, int64) ([]Topic, error) { return topics, nil })
}

func TestSuggesterOffersOnlyLexicallyRelatedTopics(t *testing.T) {
	suggester, err := NewSuggester(staticTopics(
		Topic{Key: "jwt-鉴权", Name: "JWT 鉴权", Graded: true},
		Topic{Key: "redis-分布式锁", Name: "Redis 分布式锁", Notes: 3},
	), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	got, err := suggester.Suggest(context.Background(), 7, "jwt 的签名是怎么校验的？")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].TopicKey != "jwt-鉴权" {
		t.Fatalf("candidates = %+v", got)
	}
	if got[0].Source != SourceGradedTopic {
		t.Fatalf("source = %q", got[0].Source)
	}
}

// Padding the list with the learner's other subjects would reintroduce
// "nearest available topic" in the interface after removing it from the server.
func TestSuggesterReturnsNothingRatherThanPadding(t *testing.T) {
	suggester, err := NewSuggester(staticTopics(
		Topic{Key: "jwt-鉴权", Name: "JWT 鉴权", Graded: true},
		Topic{Key: "redis-分布式锁", Name: "Redis 分布式锁", Notes: 9},
	), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	got, err := suggester.Suggest(context.Background(), 7, "今天天气怎么样？")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unrelated question must offer nothing, got %+v", got)
	}
}

func TestSuggesterNeverExceedsItsLimit(t *testing.T) {
	topics := []Topic{}
	for _, name := range []string{"jwt-鉴权", "jwt-刷新", "jwt-过期", "jwt-签名", "jwt-载荷", "jwt-算法"} {
		topics = append(topics, Topic{Key: name, Name: name, Notes: 1})
	}
	suggester, err := NewSuggester(staticTopics(topics...), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	got, err := suggester.Suggest(context.Background(), 7, "jwt 怎么用")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > MaxCandidates {
		t.Fatalf("returned %d candidates, limit is %d", len(got), MaxCandidates)
	}
}

func TestSuggesterRefusesAnUntrustedUser(t *testing.T) {
	suggester, err := NewSuggester(staticTopics(Topic{Key: "jwt-鉴权", Name: "JWT"}), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := suggester.Suggest(context.Background(), 0, "jwt"); err == nil {
		t.Fatal("a suggestion without a trusted user id must be refused")
	}
}

func TestCatalogueBrowseFindsTheTopicTheSemanticConfirmationMissed(t *testing.T) {
	suggester, err := NewSuggester(staticTopics(
		Topic{Key: "session-状态机", Name: "Session 状态机", Graded: true},
		Topic{Key: "docker-容器编排", Name: "Docker 容器编排", Notes: 2},
		Topic{Key: "etcd-服务发现", Name: "etcd 服务发现"},
	), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	page, err := suggester.Browse(context.Background(), 7, "Docker", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Candidates) != 1 || page.Candidates[0].TopicKey != "docker-容器编排" {
		t.Fatalf("candidates = %+v", page.Candidates)
	}
	if page.Candidates[0].Source != SourceCatalogueBrowse {
		t.Fatalf("browse source leaked topic state: %q", page.Candidates[0].Source)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"mastery", "weak", "noteCount", "score", "content", "recent_notes", "graded_topic"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("catalogue response leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestCatalogueBrowsePaginatesAllTopicsInStableOrder(t *testing.T) {
	topics := []Topic{}
	for _, key := range []string{"topic-c", "topic-a", "topic-b"} {
		topics = append(topics, Topic{Key: key, Name: key})
	}
	suggester, err := NewSuggester(staticTopics(topics...), MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	first, err := suggester.Browse(context.Background(), 7, "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := suggester.Browse(context.Background(), 7, "", first.NextOffset, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{first.Candidates[0].TopicKey, first.Candidates[1].TopicKey, second.Candidates[0].TopicKey}
	want := []string{"topic-a", "topic-b", "topic-c"}
	if strings.Join(got, ",") != strings.Join(want, ",") || !first.HasMore || second.HasMore {
		t.Fatalf("pages = %+v / %+v", first, second)
	}
}

func TestResolveSelectionRechecksCurrentOwnershipAndIsAllOrNothing(t *testing.T) {
	current := []Topic{{Key: "jwt-鉴权", Name: "JWT"}, {Key: "docker-容器编排", Name: "Docker"}}
	lister := TopicListerFunc(func(_ context.Context, userID int64) ([]Topic, error) {
		if userID != 7 {
			return []Topic{{Key: "foreign", Name: "Foreign"}}, nil
		}
		return append([]Topic(nil), current...), nil
	})
	suggester, err := NewSuggester(lister, MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := suggester.ResolveSelection(context.Background(), 7, []string{"jwt-鉴权", "docker-容器编排"})
	if err != nil || len(resolved) != 2 {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	current = current[:1] // Docker was removed after the browse response.
	if _, err := suggester.ResolveSelection(context.Background(), 7, []string{"jwt-鉴权", "docker-容器编排"}); err == nil {
		t.Fatal("a stale mixed selection must be refused rather than partially authorised")
	}
	if _, err := suggester.ResolveSelection(context.Background(), 7, []string{"foreign"}); err == nil {
		t.Fatal("another user's topic must not be resolved")
	}
}
