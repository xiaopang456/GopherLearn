package main

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// memoryAccessFor is the binary's whole decision about what the memory read side may reach, and
// this is where that decision is pinned. The permission cannot be reviewed by reading main: a
// grant that looks installed and is not, or one that is installed and grants too much, both read
// the same in a startup sequence. So the function is called here, over a real store, in the three
// states that matter.

// memoryWiringFixture is one learner with one learning-agent conversation, one message that
// provenance can point at, and two confirmed memories: a global fact that must always be readable,
// and a topic memory under the subject a quiz would name.
type memoryWiringFixture struct {
	store     repository.Repository
	userID    string
	sessionID string
	globalID  string
	topicID   string
	subject   string
}

func newMemoryWiringFixture(t *testing.T) memoryWiringFixture {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "wiring@example.test", DisplayName: "wiring",
		PasswordHash: "hash-not-used"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "学习会话",
		Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{
		Role: "user", Content: "AOF 的 fsync 该选哪个"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := memoryWiringFixture{store: store, userID: user.ID, sessionID: session.ID,
		subject: domain.NormalizeLearningTopicKey("Redis 持久化")}
	memory := service.NewMemoryService(store)
	for _, draft := range []domain.MemoryDraft{
		{Scope: domain.MemoryScopeGlobal, Content: "全局：解释 fsync 时用中文", Confidence: 1},
		{Scope: domain.MemoryScopeTopic, TopicKey: fixture.subject,
			Content: "redis 主题：AOF 的 fsync 选 everysec", Confidence: 1},
	} {
		draft.SourceSessionID = session.ID
		draft.SourceMessageIDs = []string{message.ID}
		stored, err := memory.CreateMemoryDraft(ctx, user.ID, draft)
		if err != nil {
			t.Fatal(err)
		}
		confirmed, err := memory.ConfirmMemoryDraft(ctx, user.ID, stored.MemoryID)
		if err != nil {
			t.Fatal(err)
		}
		if draft.Scope == domain.MemoryScopeGlobal {
			fixture.globalID = confirmed.MemoryID
		} else {
			fixture.topicID = confirmed.MemoryID
		}
	}
	return fixture
}

// setConsent records the learner's decision the way an HTTP request would: through the service, on
// the conversation, with nothing else named.
func (f memoryWiringFixture) setConsent(t *testing.T, enabled bool) {
	t.Helper()
	if _, err := service.NewSessionService(f.store).
		CommitTopicMemory(t.Context(), f.userID, f.sessionID, enabled); err != nil {
		t.Fatalf("commit consent %v: %v", enabled, err)
	}
}

// openQuizOnThisConversation leaves a live quiz titled the way LearningService.Start leaves one.
func (f memoryWiringFixture) openQuizOnThisConversation(t *testing.T) {
	t.Helper()
	if _, err := f.store.CreateLearningSession(t.Context(), &domain.LearningSession{
		UserID: f.userID, SessionID: f.sessionID, Topic: "Redis 持久化",
		TopicKey:        f.subject,
		Status:          domain.LearningSessionCreated,
		CurrentQuestion: domain.LearningQuestionOrdinalFirst,
		TotalQuestions:  domain.LearningQuestionCount,
		Version:         1,
		ExpiresAt:       time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f memoryWiringFixture) read(t *testing.T) []service.MemoryHit {
	t.Helper()
	access, err := memoryAccessFor(f.store)
	if err != nil {
		t.Fatal(err)
	}
	hits, _, err := access.Recall(t.Context(), service.MemoryQuery{
		UserID: f.userID, SessionID: f.sessionID, Query: "fsync", MaxResults: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func readIDs(hits []service.MemoryHit) string {
	parts := make([]string, 0, len(hits))
	for _, hit := range hits {
		parts = append(parts, hit.Memory.MemoryID)
	}
	return strings.Join(parts, ",")
}

// TestBinaryInstallsTheTopicAuthorizer is the "is the feature wired" half: with a consent and a
// live quiz, the read side the process builds does reach the topic memory. A main that quietly kept
// the default grant would pass every HTTP and service test in this repository, because those test
// the grant they construct themselves - which is exactly why this one goes through the entry point.
func TestBinaryInstallsTheTopicAuthorizer(t *testing.T) {
	fixture := newMemoryWiringFixture(t)
	fixture.setConsent(t, true)
	fixture.openQuizOnThisConversation(t)
	if got := readIDs(fixture.read(t)); !strings.Contains(got, fixture.topicID) {
		t.Fatalf("the assembled read side never reached the authorized topic memory: %s", got)
	}
}

// TestBinaryStillRefusesWhatTheLearnerNeverAllowed is the other half, and the one that matters for
// shipping the line above: installing the authorizer must not turn into granting topic memory.
// Consent off, and consent on with no quiz, both have to answer with exactly the two layers that
// were always readable.
func TestBinaryStillRefusesWhatTheLearnerNeverAllowed(t *testing.T) {
	t.Run("no consent, quiz open", func(t *testing.T) {
		fixture := newMemoryWiringFixture(t)
		fixture.openQuizOnThisConversation(t)
		if got := readIDs(fixture.read(t)); got != fixture.globalID {
			t.Fatalf("an unanswered switch read %s, want only the learner's own global fact", got)
		}
	})
	t.Run("consent on, no quiz", func(t *testing.T) {
		fixture := newMemoryWiringFixture(t)
		fixture.setConsent(t, true)
		if got := readIDs(fixture.read(t)); got != fixture.globalID {
			t.Fatalf("a consent with no open quiz read %s, want only the global fact", got)
		}
	})
	t.Run("consent taken back after a read", func(t *testing.T) {
		fixture := newMemoryWiringFixture(t)
		fixture.setConsent(t, true)
		fixture.openQuizOnThisConversation(t)
		if !strings.Contains(readIDs(fixture.read(t)), fixture.topicID) {
			t.Fatal("the test itself lost the topic memory before the revocation")
		}
		fixture.setConsent(t, false)
		if got := readIDs(fixture.read(t)); got != fixture.globalID {
			t.Fatalf("the next read after switching off still returned %s", got)
		}
	})
}
