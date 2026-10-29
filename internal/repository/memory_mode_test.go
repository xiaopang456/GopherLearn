package repository

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

func TestSessionModesAreValidatedAndPersisted(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "mode@example.com", DisplayName: "Mode"})
	if err != nil {
		t.Fatal(err)
	}

	modes := []domain.SessionMode{
		domain.SessionModeChat,
		domain.SessionModeLearningAgent,
	}
	for _, mode := range modes {
		input := &domain.Session{UserID: user.ID, Title: string(mode), Mode: mode}
		created, err := store.CreateSession(ctx, input)
		if err != nil {
			t.Fatalf("create %q: %v", mode, err)
		}
		input.Mode = domain.SessionMode("tampered_after_create")
		if created.Mode != mode {
			t.Fatalf("created mode=%q, want %q", created.Mode, mode)
		}
		loaded, err := store.GetSession(ctx, user.ID, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Mode != mode {
			t.Fatalf("loaded mode=%q, want %q", loaded.Mode, mode)
		}
	}

	items, err := store.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(modes) {
		t.Fatalf("listed %d sessions, want %d", len(items), len(modes))
	}
	for index, mode := range modes {
		if items[index].Mode != mode {
			t.Errorf("listed mode[%d]=%q, want %q", index, items[index].Mode, mode)
		}
	}
}

func TestRetiredModesAreRejectedAsSessionModes(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "retired@example.com", DisplayName: "Retired"})
	if err != nil {
		t.Fatal(err)
	}

	retired := []domain.SessionMode{
		"fast_chat",
		// knowledge_qa is an answer strategy now, never a stored mode.
		"knowledge_qa",
		"",
		"unsupported",
		"agent",
		"direct_chat",
		"Chat",
	}
	for _, mode := range retired {
		if _, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "retired", Mode: mode}); err != ErrInvalidArgument {
			t.Errorf("mode %q error=%v, want ErrInvalidArgument", mode, err)
		}
	}
	items, err := store.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("rejected modes wrote %d sessions", len(items))
	}
}

func TestInvalidSessionModeDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "invalid-mode@example.com", DisplayName: "Mode"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, &domain.Session{
		UserID: user.ID,
		Title:  "invalid",
		Mode:   domain.SessionMode("unsupported"),
	}); err != ErrInvalidArgument {
		t.Fatalf("invalid mode error=%v, want ErrInvalidArgument", err)
	}
	items, err := store.ListSessions(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("invalid mode wrote %d sessions", len(items))
	}
}

func TestSessionModeHasNoUpdateEntryPoint(t *testing.T) {
	for _, candidate := range []reflect.Type{
		reflect.TypeOf((*Repository)(nil)).Elem(),
		reflect.TypeOf(&InMemoryRepository{}),
	} {
		for method := 0; method < candidate.NumMethod(); method++ {
			name := candidate.Method(method).Name
			if strings.Contains(name, "Update") || strings.Contains(name, "Set") || strings.Contains(name, "Patch") {
				t.Fatalf("%s exposes %q, which could rewrite a stored session", candidate, name)
			}
		}
	}

	ctx := context.Background()
	store := NewInMemoryRepository()
	user, err := store.CreateUser(ctx, &domain.User{Email: "immutable@example.com", DisplayName: "Immutable"})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "Chat", Mode: domain.SessionModeChat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessage(ctx, user.ID, created.ID, &domain.Message{Role: "user", Content: "question"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetSession(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Mode = domain.SessionModeLearningAgent
	loaded.Title = "rewritten"
	reloaded, err := store.GetSession(ctx, user.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Mode != domain.SessionModeChat || reloaded.Title != "Chat" {
		t.Fatalf("stored session changed through a returned copy: %#v", reloaded)
	}
}
