package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

func TestSessionServiceCreatesAndListsFinalModes(t *testing.T) {
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "modes@example.com", DisplayName: "Modes"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(store)

	modes := []domain.SessionMode{domain.SessionModeChat, domain.SessionModeLearningAgent}
	for _, mode := range modes {
		created, err := service.Create(context.Background(), user.ID, "  "+string(mode)+"  ", mode)
		if err != nil {
			t.Fatalf("create %q: %v", mode, err)
		}
		if created.Mode != mode {
			t.Fatalf("created mode=%q, want %q", created.Mode, mode)
		}
		if created.Title != string(mode) {
			t.Fatalf("title=%q was not trimmed", created.Title)
		}
	}
	items, err := service.List(context.Background(), user.ID)
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

func TestSessionServiceRejectsRemovedAndUnknownModesWithoutWriting(t *testing.T) {
	store := repository.NewInMemoryRepository()
	user, err := store.CreateUser(context.Background(), &domain.User{Email: "rejected@example.com", DisplayName: "Rejected"})
	if err != nil {
		t.Fatal(err)
	}
	service := NewSessionService(store)

	rejected := []domain.SessionMode{
		"",
		"fast_chat",
		"knowledge_qa",
		"direct_chat",
		"agent",
		"unsupported",
	}
	for _, mode := range rejected {
		if _, err := service.Create(context.Background(), user.ID, "title", mode); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("mode %q error=%v, want ErrInvalidRequest", mode, err)
		}
	}
	items, err := service.List(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("rejected modes wrote %d sessions", len(items))
	}
}

func TestSessionServiceHasNoModeUpdateEntryPoint(t *testing.T) {
	serviceType := reflect.TypeOf(&SessionService{})
	for method := 0; method < serviceType.NumMethod(); method++ {
		name := serviceType.Method(method).Name
		if strings.Contains(name, "Update") || strings.Contains(name, "Set") || strings.Contains(name, "Patch") {
			t.Fatalf("SessionService exposes %q, which could rewrite a stored mode", name)
		}
	}
}
