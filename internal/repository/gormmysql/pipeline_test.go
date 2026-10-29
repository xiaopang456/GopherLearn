package gormmysql

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// captureResponder is the model boundary for these tests: it records the
// resolved turn and answers deterministically without any provider.
type captureResponder struct {
	requests chan responder.Request
	answer   string
	chunks   []string
}

func (c *captureResponder) Respond(ctx context.Context, request responder.Request) (responder.Answer, error) {
	if c.requests != nil {
		c.requests <- request
	}
	return responder.Answer{Content: c.answer}, nil
}

func (c *captureResponder) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	if c.requests != nil {
		c.requests <- request
	}
	for _, chunk := range c.chunks {
		if err := emit(chunk); err != nil {
			return responder.Answer{}, err
		}
	}
	return responder.Answer{Content: c.answer}, nil
}

// TestTurnPersistsThroughReopenedDatabase is the point of phase 2A: a full
// message turn must still be there after the process that wrote it is gone.
func TestTurnPersistsThroughReopenedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	db := openTestDB(t, path)
	repo := NewRepository(db)
	ctx := context.Background()

	user, err := repo.CreateUser(ctx, &domain.User{Email: "restart@example.com", DisplayName: "Owner", PasswordHash: "hash"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}

	assistant := &captureResponder{requests: make(chan responder.Request, 4), answer: "answer", chunks: []string{"ans", "wer"}}
	messages := service.NewMessageService(repo, assistant, service.WithCollectionAccess(allowAllCollections{}))

	pair, err := messages.Send(ctx, user.ID, session.ID, "question", domain.KnowledgeOptions{
		Enabled:       true,
		CollectionIDs: []string{" collection-1 ", "collection-1", "collection-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pair.User.Content != "question" || pair.Assistant.Content != "answer" {
		t.Fatalf("unexpected turn pair: %#v", pair)
	}
	resolved := <-assistant.requests
	if resolved.SessionMode != domain.SessionModeChat || resolved.AnswerStrategy != domain.AnswerStrategyKnowledgeQA {
		t.Fatalf("sync turn resolved to %#v", resolved)
	}
	if !resolved.KnowledgeEnabled || strings.Join(resolved.KnowledgeCollectionIDs, ",") != "collection-1,collection-2" {
		t.Fatalf("knowledge options were not normalized through the SQL path: %#v", resolved)
	}

	var events []string
	if err := messages.Stream(ctx, user.ID, session.ID, "second", domain.KnowledgeOptions{}, func(event service.StreamEvent) error {
		events = append(events, event.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(events, ",") != "message.started,message.delta,message.delta,message.completed" {
		t.Fatalf("stream events = %v", events)
	}
	streamed := <-assistant.requests
	if streamed.AnswerStrategy != domain.AnswerStrategyDirectChat {
		t.Fatalf("stream turn resolved to %q, want direct_chat", streamed.AnswerStrategy)
	}

	closeDB(db)

	reopened := NewRepository(openTestDB(t, path))
	stored, err := reopened.ListMessages(ctx, user.ID, session.ID)
	if err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	got := make([]string, 0, len(stored))
	for _, message := range stored {
		got = append(got, message.Role+":"+message.Content)
	}
	want := []string{"user:question", "assistant:answer", "user:second", "assistant:answer"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("transcript after reopen = %v, want %v", got, want)
	}
	sessions, err := reopened.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Mode != domain.SessionModeChat || sessions[0].ID != session.ID {
		t.Fatalf("sessions after reopen = %#v", sessions)
	}
}

func TestCorruptStoredModeFailsAsInternalNotBadRequest(t *testing.T) {
	repo, db := newTestRepository(t)
	ctx := context.Background()
	user := mustCreateUser(t, repo, "internal-not-400@example.com")
	session := mustCreateSession(t, repo, user.ID, domain.SessionModeChat)

	if err := db.Exec("UPDATE sessions SET mode = 'knowledge_qa' WHERE id = ?", session.ID).Error; err != nil {
		t.Fatal(err)
	}
	messages := service.NewMessageService(repo, &captureResponder{answer: "answer"}, service.WithCollectionAccess(allowAllCollections{}))
	_, err := messages.Send(ctx, user.ID, session.ID, "question", domain.KnowledgeOptions{})
	if !errors.Is(err, service.ErrInternal) {
		t.Fatalf("error = %v, want ErrInternal", err)
	}
	if errors.Is(err, service.ErrInvalidRequest) {
		t.Fatal("a stored-data fault must not be blamed on the client request")
	}
	if count := countRows(t, db, "SELECT COUNT(*) FROM messages"); count != 0 {
		t.Fatalf("a turn against corrupt data persisted %d messages", count)
	}
}

// allowAllCollections stands in for the knowledge authorization gate in these
// durability tests, which are about what survives a restart rather than who may
// read which collection.
type allowAllCollections struct{}

func (allowAllCollections) EnsureReadable(context.Context, string, []string) error { return nil }
