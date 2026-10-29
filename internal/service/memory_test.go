package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// memoryFixture builds a user, one session with a couple of messages, and the memory
// service over the same store, so a test can talk about real provenance rather than
// invented ids.
func memoryFixture(t *testing.T) (*MemoryService, repository.Repository, string, string, string) {
	t.Helper()
	ctx := t.Context()
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "我平时用 Go 写服务"})
	if err != nil {
		t.Fatal(err)
	}
	return NewMemoryService(store), store, user.ID, session.ID, message.ID
}

func draftFor(userID, sessionID, messageID string) domain.MemoryDraft {
	return domain.MemoryDraft{
		UserID:           userID,
		Scope:            domain.MemoryScopeGlobal,
		Content:          "用户用 Go 写后端服务",
		Confidence:       0.8,
		SourceSessionID:  sessionID,
		SourceMessageIDs: []string{messageID},
	}
}

func TestMemoryConfirmationFlow(t *testing.T) {
	ctx := t.Context()
	service, _, userID, sessionID, messageID := memoryFixture(t)

	// A draft is stored pending and is visible to its owner, but it is not yet one of the
	// memories the system would act on.
	stored, err := service.CreateMemoryDraft(ctx, userID, draftFor("", sessionID, messageID))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != domain.MemoryStatusPending {
		t.Fatalf("status = %q, want pending", stored.Status)
	}
	if pending, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeGlobal, ""); err != nil || len(pending) != 0 {
		t.Fatalf("active listing before confirmation = %+v err %v, want empty", pending, err)
	}

	confirmed, err := service.ConfirmMemoryDraft(ctx, userID, stored.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != domain.MemoryStatusActive {
		t.Fatalf("confirmed status = %q", confirmed.Status)
	}
	active, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].MemoryID != stored.MemoryID || active[0].Content != stored.Content {
		t.Fatalf("active listing = %+v, want the confirmed memory", active)
	}
}

// TestOwnerListingsCoverWhatTheOwnerMustDecideOn pins the two read paths a confirmation
// surface needs. The prompt-facing listing deliberately refuses to enumerate topic memory
// without a key; the owner still has to see their own topic drafts, so the management read
// is a separate method rather than a widened version of the recall read.
func TestOwnerListingsCoverWhatTheOwnerMustDecideOn(t *testing.T) {
	ctx := t.Context()
	service, store, userID, sessionID, messageID := memoryFixture(t)

	other, err := store.CreateUser(ctx, &domain.User{Email: "other@example.com", DisplayName: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := store.CreateSession(ctx, &domain.Session{UserID: other.ID, Title: "别人的会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := store.AppendMessage(ctx, other.ID, otherSession.ID, &domain.Message{Role: "user", Content: "与我无关"})
	if err != nil {
		t.Fatal(err)
	}

	globalDraft, err := service.CreateMemoryDraft(ctx, userID, draftFor("", sessionID, messageID))
	if err != nil {
		t.Fatal(err)
	}
	topicDraft, err := service.CreateMemoryDraft(ctx, userID, domain.MemoryDraft{
		Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "RDB 不是缓存的唯一选项",
		Confidence: 0.7, SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := service.CreateMemoryDraft(ctx, other.ID, draftFor("", otherSession.ID, otherMessage.ID))
	if err != nil {
		t.Fatal(err)
	}

	pending, err := service.PendingDrafts(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].MemoryID != globalDraft.MemoryID || pending[1].MemoryID != topicDraft.MemoryID {
		t.Fatalf("pending listing = %+v, want this user's two drafts in creation order", pending)
	}
	for _, item := range pending {
		if item.Status != domain.MemoryStatusPending {
			t.Fatalf("the pending listing carried a %q row", item.Status)
		}
	}
	if theirs, err := service.PendingDrafts(ctx, other.ID); err != nil || len(theirs) != 1 || theirs[0].MemoryID != foreign.MemoryID {
		t.Fatalf("the other account's pending listing = %+v err %v, want only its own draft", theirs, err)
	}

	// Confirming the topic memory makes it visible to its owner without the owner having to
	// name the key again - which is precisely what the recall-side listing refuses to do.
	confirmedTopic, err := service.ConfirmMemoryDraft(ctx, userID, topicDraft.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeTopic, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a topic listing with no key = %v, want an invalid request", err)
	}
	confirmed, err := service.ConfirmedMemories(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(confirmed) != 1 || confirmed[0].MemoryID != confirmedTopic.MemoryID || confirmed[0].Scope != domain.MemoryScopeTopic {
		t.Fatalf("confirmed listing = %+v, want the topic memory the user just confirmed", confirmed)
	}

	// A state transition moves a row between the two listings and never leaves it in both.
	rejected, err := service.RejectMemoryDraft(ctx, userID, globalDraft.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != domain.MemoryStatusArchived {
		t.Fatalf("rejected status = %q", rejected.Status)
	}
	afterPending, err := service.PendingDrafts(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPending) != 0 {
		t.Fatalf("pending listing after the decision = %+v, want empty", afterPending)
	}
	afterConfirmed, err := service.ConfirmedMemories(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range afterConfirmed {
		if item.MemoryID == rejected.MemoryID {
			t.Fatalf("an archived row appeared in the confirmed listing")
		}
		if item.Status != domain.MemoryStatusActive {
			t.Fatalf("the confirmed listing carried a %q row", item.Status)
		}
	}

	for name, call := range map[string]func(context.Context, string) ([]domain.Memory, error){
		"pending": service.PendingDrafts,
		"active":  service.ConfirmedMemories,
	} {
		if _, err := call(ctx, "   "); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("%s listing with no user = %v, want unauthorized", name, err)
		}
	}
	empty := NewMemoryService(nil)
	if _, err := empty.PendingDrafts(ctx, userID); !errors.Is(err, ErrInternal) {
		t.Fatalf("pending listing without a store = %v, want the internal error", err)
	}
	if _, err := empty.ConfirmedMemories(ctx, userID); !errors.Is(err, ErrInternal) {
		t.Fatalf("confirmed listing without a store = %v, want the internal error", err)
	}
}

// TestMemoryTransitionErrorsAreStable pins the four answers a confirmation surface needs
// to tell apart. Each case is checked by identity, because a message text is not an API.
func TestMemoryTransitionErrorsAreStable(t *testing.T) {
	ctx := t.Context()
	service, store, userID, sessionID, messageID := memoryFixture(t)

	confirmed := mustConfirmedDraft(t, service, userID, sessionID, messageID, "主题已确认", "已确认")
	other, err := store.CreateUser(ctx, &domain.User{Email: "intruder@example.com", DisplayName: "Intruder"})
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := store.CreateSession(ctx, &domain.Session{UserID: other.ID, Title: "别人的会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := store.AppendMessage(ctx, other.ID, otherSession.ID, &domain.Message{Role: "user", Content: "别人的话"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateMemoryDraft(ctx, other.ID, draftFor(other.ID, otherSession.ID, otherMessage.ID))
	if err != nil {
		t.Fatal(err)
	}
	rejected := mustRejectedDraft(t, service, userID, sessionID, messageID, "主题已拒绝", "已拒绝")
	archived := mustConfirmedDraft(t, service, userID, sessionID, messageID, "主题已归档", "已归档")
	if _, err := service.ArchiveMemory(ctx, userID, archived.MemoryID); err != nil {
		t.Fatal(err)
	}
	draft := mustDraftStored(t, service, userID, sessionID, messageID, "主题候选", "还是候选")
	// Two memories in one topic slot: confirming the second retires the first, which is
	// the state the superseded case needs.
	supersededOld := mustConfirmedDraft(t, service, userID, sessionID, messageID, "同一主题", "旧版本")
	newer := mustDraftStored(t, service, userID, sessionID, messageID, "同一主题", "新版本")
	if _, err := service.ConfirmMemoryDraft(ctx, userID, newer.MemoryID); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"confirming twice", func() error { _, err := service.ConfirmMemoryDraft(ctx, userID, confirmed.MemoryID); return err }, ErrMemoryAlreadyConfirmed},
		{"confirming another user's memory", func() error {
			_, err := service.ConfirmMemoryDraft(ctx, other.ID, confirmed.MemoryID)
			return err
		}, ErrNotFound},
		{"confirming a rejected draft", func() error { _, err := service.ConfirmMemoryDraft(ctx, userID, rejected.MemoryID); return err }, ErrMemoryArchived},
		{"confirming an archived memory", func() error { _, err := service.ConfirmMemoryDraft(ctx, userID, archived.MemoryID); return err }, ErrMemoryArchived},
		{"confirming a superseded memory", func() error { _, err := service.ConfirmMemoryDraft(ctx, userID, supersededOld.MemoryID); return err }, ErrMemorySuperseded},
		{"archiving a draft instead of rejecting it", func() error { _, err := service.ArchiveMemory(ctx, userID, draft.MemoryID); return err }, ErrMemoryNotActive},
		{"rejecting what is already confirmed", func() error { _, err := service.RejectMemoryDraft(ctx, userID, confirmed.MemoryID); return err }, ErrMemoryAlreadyConfirmed},
		{"confirming a memory that does not exist", func() error {
			_, err := service.ConfirmMemoryDraft(ctx, userID, "mem-nope")
			return err
		}, ErrNotFound},
		{"reading another user's memory", func() error { _, err := service.Memory(ctx, other.ID, confirmed.MemoryID); return err }, ErrNotFound},
		{"archiving another user's memory", func() error { _, err := service.ArchiveMemory(ctx, other.ID, confirmed.MemoryID); return err }, ErrNotFound},
		{"deleting another user's memory", func() error { return service.DeleteMemory(ctx, other.ID, confirmed.MemoryID) }, ErrNotFound},
		{"an anonymous caller", func() error { _, err := service.ConfirmMemoryDraft(ctx, "  ", confirmed.MemoryID); return err }, ErrUnauthorized},
		{"an anonymous draft", func() error {
			_, err := service.CreateMemoryDraft(ctx, "", draftFor(userID, sessionID, messageID))
			return err
		}, ErrUnauthorized},
		{"a malformed draft", func() error {
			bad := draftFor(userID, sessionID, messageID)
			bad.Scope = "mood"
			_, err := service.CreateMemoryDraft(ctx, userID, bad)
			return err
		}, ErrInvalidRequest},
		{"an empty memory id", func() error { _, err := service.ConfirmMemoryDraft(ctx, userID, " "); return err }, ErrInvalidRequest},
		{"a topic listing without a key", func() error { _, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeTopic, ""); return err }, ErrInvalidRequest},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.call(); !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
	// A stranger's listing must not show the owner's memories either direction.
	found, err := service.ActiveMemories(ctx, other.ID, domain.MemoryScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("the stranger's listing = %+v, want empty", found)
	}
}

// TestMemoryDraftIsFiledForTheAuthenticatedUserOnly checks that a caller cannot route a
// memory into somebody else's account by naming them in the payload.
func TestMemoryDraftIsFiledForTheAuthenticatedUserOnly(t *testing.T) {
	ctx := t.Context()
	service, store, userID, sessionID, messageID := memoryFixture(t)
	victim, err := store.CreateUser(ctx, &domain.User{Email: "victim@example.com", DisplayName: "Victim"})
	if err != nil {
		t.Fatal(err)
	}

	draft := draftFor(victim.ID, sessionID, messageID)
	stored, err := service.CreateMemoryDraft(ctx, userID, draft)
	if err != nil {
		t.Fatal(err)
	}
	if stored.UserID != userID {
		t.Fatalf("stored for %q, want the authenticated user %q", stored.UserID, userID)
	}
	if found, err := service.ActiveMemories(ctx, victim.ID, domain.MemoryScopeGlobal, ""); err != nil || len(found) != 0 {
		t.Fatalf("the named victim can already see it: %+v err %v", found, err)
	}
}

// TestConfirmingAfterTheEvidenceIsGone reports the missing conversation rather than
// promoting a memory nobody can check any more.
func TestConfirmingAfterTheEvidenceIsGone(t *testing.T) {
	ctx := t.Context()
	service, store, userID, sessionID, messageID := memoryFixture(t)
	stored := mustDraftStored(t, service, userID, sessionID, messageID, "", "证据将被删除")
	if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmMemoryDraft(ctx, userID, stored.MemoryID); !errors.Is(err, ErrMemoryEvidenceMissing) {
		t.Fatalf("error = %v, want the evidence-missing refusal", err)
	}
	if current, err := service.Memory(ctx, userID, stored.MemoryID); err != nil || current.Status != domain.MemoryStatusPending {
		t.Fatalf("the draft changed anyway: %+v err %v", current, err)
	}
}

// TestConcurrentConfirmationsLetExactlyOneThrough is the race the conditional write exists
// for: two callers read the same pending draft, and only one transition may take effect.
func TestConcurrentConfirmationsLetExactlyOneThrough(t *testing.T) {
	ctx := t.Context()
	service, _, userID, sessionID, messageID := memoryFixture(t)
	stored := mustDraftStored(t, service, userID, sessionID, messageID, "", "同时点确认")

	const callers = 8
	start := make(chan struct{})
	errs := make([]error, callers)
	results := make([]*domain.Memory, callers)
	var group sync.WaitGroup
	for index := 0; index < callers; index++ {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			<-start
			results[slot], errs[slot] = service.ConfirmMemoryDraft(ctx, userID, stored.MemoryID)
		}(index)
	}
	close(start)
	group.Wait()

	succeeded := 0
	for index := 0; index < callers; index++ {
		switch {
		case errs[index] == nil:
			succeeded++
			if results[index].Status != domain.MemoryStatusActive {
				t.Fatalf("a successful confirmation returned %+v", results[index])
			}
		case errors.Is(errs[index], ErrMemoryAlreadyConfirmed), errors.Is(errs[index], ErrConflict):
			// The loser is told the row already moved. Which of the two it is depends on
			// how fast the winner's write landed, and both are stable answers.
		default:
			t.Fatalf("caller %d got an unexpected error: %v", index, errs[index])
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d confirmations took effect, want exactly one", succeeded)
	}
	active, err := service.ActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].MemoryID != stored.MemoryID {
		t.Fatalf("active memories = %+v, want the one confirmed draft", active)
	}
}

// TestMemoryOperationsAreAbsentFromEveryTurn is the boundary this phase promises: a stored
// memory - pending, active, or archived - is not conversation context. The answer path is
// watched at the store, so the claim is about behaviour rather than about nobody having
// written the code yet: if a later change reaches for memory while answering, this fails.
func TestMemoryOperationsAreAbsentFromEveryTurn(t *testing.T) {
	ctx := t.Context()
	watched := &memoryWatchingStore{Repository: repository.NewInMemoryRepository()}
	user, err := watched.CreateUser(ctx, &domain.User{Email: "owner@example.com", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []domain.SessionMode{domain.SessionModeChat, domain.SessionModeLearningAgent} {
		session, err := watched.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		message, err := watched.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "我平时用 Go"})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range []struct{ role, content string }{{"assistant", "收到"}, {"user", "再来一条"}, {"assistant", "好的"}} {
			if _, err := watched.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: entry.role, Content: entry.content}); err != nil {
				t.Fatal(err)
			}
		}

		// The memory is really stored and really confirmed, so what the turn below
		// avoids is available rather than hypothetical.
		draft := draftFor(user.ID, session.ID, message.ID)
		draft.Content = "秘密记忆内容"
		stored, err := watched.CreateMemoryDraft(ctx, &draft)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := watched.ConfirmMemoryDraft(ctx, user.ID, stored.MemoryID); err != nil {
			t.Fatal(err)
		}
		// Prove the memory is recallable before proving a turn does not recall it: its
		// confidence is at the production floor and its scope is authorized, so what
		// keeps it out of the answer path is the absence of a reader, not a filter.
		recallable, err := watched.ListActiveMemories(ctx, user.ID, domain.MemoryScopeGlobal, "")
		if err != nil || len(recallable) != 1 || recallable[0].Content != draft.Content {
			t.Fatalf("the fixture memory is not recallable: %+v err %v", recallable, err)
		}

		before := watched.operationCount()
		assistant := &fixedResponder{answer: "回答", respondRequest: make(chan responder.Request, 1), streamRequest: make(chan responder.Request, 1)}
		service := NewMessageService(watched, assistant, WithCollectionAccess(openAccess{}))
		if _, err := service.Send(ctx, user.ID, session.ID, "问题", domain.KnowledgeOptions{}); err != nil {
			t.Fatal(err)
		}
		request := <-assistant.respondRequest
		if events := turnText(request); strings.Contains(events, "秘密记忆内容") {
			t.Fatalf("the %s turn was answered with memory content in it: %s", mode, events)
		}
		var seen []StreamEvent
		if err := service.Stream(ctx, user.ID, session.ID, "流式问题", domain.KnowledgeOptions{}, func(event StreamEvent) error {
			seen = append(seen, event)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		streamed := <-assistant.streamRequest
		if strings.Contains(turnText(streamed), "秘密记忆内容") {
			t.Fatalf("the streamed %s turn carried memory content: %s", mode, turnText(streamed))
		}
		if len(seen) == 0 {
			t.Fatal("the stream produced no events at all")
		}
		if ops := watched.operationsSince(before); len(ops) != 0 {
			t.Fatalf("answering a %s turn touched long-term memory: %v", mode, ops)
		}
	}
}

func turnText(request responder.Request) string {
	parts := []string{request.Content, request.Summary}
	for _, turn := range request.History {
		parts = append(parts, turn.Content)
	}
	return strings.Join(parts, "\n")
}

func mustDraftStored(t *testing.T, service *MemoryService, userID, sessionID, messageID, topic, content string) *domain.Memory {
	t.Helper()
	draft := draftFor("", sessionID, messageID)
	draft.Content = content
	if topic != "" {
		// A topic of its own keeps each fixture memory in its own single-active slot, so
		// confirming one cannot silently supersede another and change what a later case
		// is asserting.
		draft.Scope = domain.MemoryScopeTopic
		draft.TopicKey = topic
	}
	stored, err := service.CreateMemoryDraft(t.Context(), userID, draft)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func mustConfirmedDraft(t *testing.T, service *MemoryService, userID, sessionID, messageID, topic, content string) *domain.Memory {
	t.Helper()
	stored := mustDraftStored(t, service, userID, sessionID, messageID, topic, content)
	confirmed, err := service.ConfirmMemoryDraft(t.Context(), userID, stored.MemoryID)
	if err != nil {
		t.Fatalf("confirm %q: %v", content, err)
	}
	return confirmed
}

func mustRejectedDraft(t *testing.T, service *MemoryService, userID, sessionID, messageID, topic, content string) *domain.Memory {
	t.Helper()
	stored := mustDraftStored(t, service, userID, sessionID, messageID, topic, content)
	rejected, err := service.RejectMemoryDraft(t.Context(), userID, stored.MemoryID)
	if err != nil {
		t.Fatalf("reject %q: %v", content, err)
	}
	return rejected
}

// memoryWatchingStore counts every long-term memory operation, so a test can assert that
// the conversation path performs none. It wraps the real store rather than replacing it,
// so nothing about the answer path's behaviour changes.
type memoryWatchingStore struct {
	repository.Repository
	mu  sync.Mutex
	ops []string
}

func (s *memoryWatchingStore) note(op string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, op)
}

func (s *memoryWatchingStore) operationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ops)
}

func (s *memoryWatchingStore) operationsSince(before int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops[before:]...)
}

func (s *memoryWatchingStore) CreateMemoryDraft(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, error) {
	s.note("create")
	return s.Repository.CreateMemoryDraft(ctx, draft)
}

// CreateMemoryDraftOnce is counted too: it is the newest way a row can appear in this
// table, and the conversation path is supposed to perform no memory operation at all.
func (s *memoryWatchingStore) CreateMemoryDraftOnce(ctx context.Context, draft *domain.MemoryDraft) (*domain.Memory, bool, error) {
	s.note("propose")
	return s.Repository.CreateMemoryDraftOnce(ctx, draft)
}

func (s *memoryWatchingStore) GetMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	s.note("get")
	return s.Repository.GetMemory(ctx, userID, memoryID)
}

func (s *memoryWatchingStore) ConfirmMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	s.note("confirm")
	return s.Repository.ConfirmMemoryDraft(ctx, userID, memoryID)
}

func (s *memoryWatchingStore) RejectMemoryDraft(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	s.note("reject")
	return s.Repository.RejectMemoryDraft(ctx, userID, memoryID)
}

func (s *memoryWatchingStore) ArchiveMemory(ctx context.Context, userID, memoryID string) (*domain.Memory, error) {
	s.note("archive")
	return s.Repository.ArchiveMemory(ctx, userID, memoryID)
}

func (s *memoryWatchingStore) ListActiveMemories(ctx context.Context, userID string, scope domain.MemoryScope, topicKey string) ([]domain.Memory, error) {
	s.note("list")
	return s.Repository.ListActiveMemories(ctx, userID, scope, topicKey)
}

// ListMemoriesByStatus is counted like the rest: the owner's management read is the newest
// memory path, and a conversation turn that used it would otherwise slip past the counter
// that exists to prove the answer path performs no memory work at all.
func (s *memoryWatchingStore) ListMemoriesByStatus(ctx context.Context, userID string, status domain.MemoryStatus) ([]domain.Memory, error) {
	s.note("list_by_status")
	return s.Repository.ListMemoriesByStatus(ctx, userID, status)
}

func (s *memoryWatchingStore) DeleteMemory(ctx context.Context, userID, memoryID string) error {
	s.note("delete")
	return s.Repository.DeleteMemory(ctx, userID, memoryID)
}
