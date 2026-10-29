package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// ProposeMemoryDraft is the door an agent uses. These tests hold it to one sentence: it may
// only ever add a pending draft belonging to the caller, citing evidence that exists, and it
// may do nothing else - no confirmation, no recall, no second copy of the same claim.

// failingMemoryStore forwards everything but lets one call answer with a chosen error, so a
// test can watch what the service does with a store it cannot trust.
type failingMemoryStore struct {
	repository.Repository
	failWith error
	calls    int
}

func (f *failingMemoryStore) CreateMemoryDraftOnce(_ context.Context, _ *domain.MemoryDraft) (*domain.Memory, bool, error) {
	f.calls++
	return nil, false, f.failWith
}

func TestProposeMemoryDraftTakesTheCallersIdentity(t *testing.T) {
	ctx := t.Context()
	service, _, userID, sessionID, messageID := memoryFixture(t)

	// The payload names a different account. It is overwritten, not consulted: filing a
	// memory into somebody else's name would be a privilege issue, not a validation detail.
	proposed, created, err := service.ProposeMemoryDraft(ctx, userID, domain.MemoryDraft{
		UserID: "user-someone-else", Scope: domain.MemoryScopeGlobal, Content: "回答控制在一段以内",
		Confidence: 0.1, SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("a first proposal reported itself as a replay")
	}
	if proposed.UserID != userID {
		t.Fatalf("stored under %q, want the authenticated caller %q", proposed.UserID, userID)
	}
	// The caller also cannot choose how certain the claim is: a proposal from a live
	// conversation is asserted by the server at 1, so nothing can be stored and then
	// silently hidden by the recall floor.
	if proposed.Confidence != 1 {
		t.Fatalf("confidence = %v, want the server-owned 1", proposed.Confidence)
	}
	if _, err := service.Memory(ctx, "user-someone-else", proposed.MemoryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the named account can read it: %v, want not found", err)
	}
	if pending, err := service.PendingDrafts(ctx, userID); err != nil || len(pending) != 1 {
		t.Fatalf("caller's pending listing = %+v err %v", pending, err)
	}
}

func TestProposeMemoryDraftRefusesEveryMissingOrForeignSource(t *testing.T) {
	ctx := t.Context()
	service, store, userID, sessionID, messageID := memoryFixture(t)

	otherSession, err := store.CreateSession(ctx, &domain.Session{UserID: userID, Title: "另一段对话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	otherMessage, err := store.AppendMessage(ctx, userID, otherSession.ID, &domain.Message{Role: "user", Content: "另一段对话里的话"})
	if err != nil {
		t.Fatal(err)
	}
	// A real session belonging to somebody else: the refusal must be indistinguishable
	// from "no such session", so proposing a memory cannot be used to probe ids.
	intruder, err := store.CreateUser(ctx, &domain.User{Email: "victim@example.test", DisplayName: "victim", PasswordHash: "hash-not-used"})
	if err != nil {
		t.Fatal(err)
	}
	intruderSession, err := store.CreateSession(ctx, &domain.Session{UserID: intruder.ID, Title: "别人的会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		draft domain.MemoryDraft
		want  error
	}{
		{"no content", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, SourceSessionID: sessionID,
			SourceMessageIDs: []string{messageID}}, ErrInvalidRequest},
		{"no session at all", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "一条事实",
			SourceMessageIDs: []string{messageID}}, ErrInvalidRequest},
		{"no source message", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "一条事实",
			SourceSessionID: sessionID}, ErrInvalidRequest},
		{"source message from another conversation", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal,
			Content: "一条事实", SourceSessionID: sessionID, SourceMessageIDs: []string{otherMessage.ID}}, ErrInvalidRequest},
		{"unknown session", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "一条事实",
			SourceSessionID: "session-does-not-exist", SourceMessageIDs: []string{messageID}}, ErrNotFound},
		{"another account's session", domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "一条事实",
			SourceSessionID: intruderSession.ID, SourceMessageIDs: []string{messageID}}, ErrNotFound},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := service.ProposeMemoryDraft(ctx, userID, testCase.draft); !errors.Is(err, testCase.want) {
				t.Fatalf("= %v, want %v", err, testCase.want)
			}
		})
	}
	// None of those attempts stored anything, so a refusal cannot leave a half-truth behind.
	if pending, err := service.PendingDrafts(ctx, userID); err != nil || len(pending) != 0 {
		t.Fatalf("pending drafts after the refusals = %+v err %v, want none", pending, err)
	}

	// A session deleted after the draft was written: the proposal survives as pending, but
	// confirming it is refused, because the evidence it cites no longer exists to check.
	stored, _, err := service.ProposeMemoryDraft(ctx, userID, domain.MemoryDraft{
		Scope: domain.MemoryScopeGlobal, Content: "这段对话被删掉之后我仍然在等确认",
		Confidence: 1, SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, userID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfirmMemoryDraft(ctx, userID, stored.MemoryID); !errors.Is(err, ErrMemoryEvidenceMissing) {
		t.Fatalf("confirm after the evidence was deleted = %v, want %v", err, ErrMemoryEvidenceMissing)
	}
	if pending, err := service.PendingDrafts(ctx, userID); err != nil || len(pending) != 1 {
		t.Fatalf("the draft vanished with its evidence: %+v err %v, want it still listed", pending, err)
	}
	// Proposing that same claim again now fails too: the store will not accept a source it
	// cannot verify, and an unverifiable proposal must not be filed as a fresh draft.
	if _, _, err := service.ProposeMemoryDraft(ctx, userID, domain.MemoryDraft{
		Scope: domain.MemoryScopeGlobal, Content: "这段对话被删掉之后我仍然在等确认",
		Confidence: 1, SourceSessionID: sessionID, SourceMessageIDs: []string{messageID},
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-proposing against a deleted session = %v, want not found", err)
	}
}

// TestProposeMemoryDraftIsPendingIdempotentAndGoesNoFurther is the whole capability boundary
// in one test: one row, pending, replayed rather than duplicated, and - read off the store's
// own call log - no confirmation, no active listing, no single-row read either.
func TestProposeMemoryDraftIsPendingIdempotentAndGoesNoFurther(t *testing.T) {
	ctx := t.Context()
	watched := &memoryWatchingStore{Repository: repository.NewInMemoryRepository()}
	store := watched
	user, err := store.CreateUser(ctx, &domain.User{Email: "agent@example.test", DisplayName: "agent", PasswordHash: "hash-not-used"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "会话", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	message, err := store.AppendMessage(ctx, user.ID, session.ID, &domain.Message{Role: "user", Content: "证据"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewMemoryService(watched)
	claim := domain.MemoryDraft{
		Scope: domain.MemoryScopeTopic, TopicKey: "redis", Content: "AOF 选 everysec",
		SourceSessionID: session.ID, SourceMessageIDs: []string{message.ID},
	}

	before := watched.operationCount()
	first, created, err := service.ProposeMemoryDraft(ctx, user.ID, claim)
	if err != nil || !created {
		t.Fatalf("first proposal = created %v err %v", created, err)
	}
	if first.Status != domain.MemoryStatusPending {
		t.Fatalf("status = %q, want pending", first.Status)
	}
	second, replayed, err := service.ProposeMemoryDraft(ctx, user.ID, claim)
	if err != nil {
		t.Fatal(err)
	}
	if replayed {
		t.Fatal("an identical claim stored a second row, so the same proposal could pile up")
	}
	if second.MemoryID != first.MemoryID {
		t.Fatalf("replay returned %q, want the waiting row %q", second.MemoryID, first.MemoryID)
	}
	if pending, err := service.PendingDrafts(ctx, user.ID); err != nil || len(pending) != 1 {
		t.Fatalf("pending listing = %+v err %v, want exactly one", pending, err)
	}
	if active, err := service.ConfirmedMemories(ctx, user.ID); err != nil || len(active) != 0 {
		t.Fatalf("active listing = %+v err %v, want none: a proposal never takes effect on its own", active, err)
	}
	// The log is the proof of scope. "propose" twice, then the two listings this test calls
	// by hand - and nothing else. In particular no confirm, no recall-side active listing,
	// no per-row read: the proposal path cannot touch the states it is not allowed to see.
	operations := strings.Join(watched.operationsSince(before), ",")
	if operations != "propose,propose,list_by_status,list_by_status" {
		t.Fatalf("store operations during the proposal path = %q", operations)
	}
}

func TestProposeMemoryDraftCarriesStoreFailuresUpUntampered(t *testing.T) {
	ctx := t.Context()
	_, store, userID, sessionID, messageID := memoryFixture(t)
	draft := domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "一条事实",
		Confidence: 1, SourceSessionID: sessionID, SourceMessageIDs: []string{messageID}}

	for name, pair := range map[string][2]error{
		"a not-found stays a not-found":     {repository.ErrNotFound, ErrNotFound},
		"an invalid argument stays invalid": {repository.ErrInvalidArgument, ErrInvalidRequest},
		"a conflict stays a conflict":       {repository.ErrConflict, ErrConflict},
		"anything unknown is internal":      {errors.New("connection reset"), ErrInternal},
	} {
		t.Run(name, func(t *testing.T) {
			failing := &failingMemoryStore{Repository: store, failWith: pair[0]}
			if _, _, err := NewMemoryService(failing).ProposeMemoryDraft(ctx, userID, draft); !errors.Is(err, pair[1]) {
				t.Fatalf("= %v, want %v", err, pair[1])
			}
			if failing.calls != 1 {
				t.Fatalf("the store was called %d times, want one attempt and no retry", failing.calls)
			}
		})
	}
}

func TestProposeMemoryDraftNeedsACallerAndAStore(t *testing.T) {
	ctx := t.Context()
	_, store, _, sessionID, messageID := memoryFixture(t)
	draft := domain.MemoryDraft{Scope: domain.MemoryScopeGlobal, Content: "一条事实", Confidence: 1,
		SourceSessionID: sessionID, SourceMessageIDs: []string{messageID}}

	if _, _, err := NewMemoryService(nil).ProposeMemoryDraft(ctx, "user-1", draft); !errors.Is(err, ErrInternal) {
		t.Fatalf("no store = %v, want the internal error rather than a silent no-op", err)
	}
	for _, blank := range []string{"", "   "} {
		if _, _, err := NewMemoryService(store).ProposeMemoryDraft(ctx, blank, draft); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("blank user %q = %v, want unauthorized", blank, err)
		}
	}
}
