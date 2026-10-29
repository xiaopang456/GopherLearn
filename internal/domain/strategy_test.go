package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveAnswerStrategy(t *testing.T) {
	cases := []struct {
		name          string
		mode          SessionMode
		knowledge     KnowledgeOptions
		want          AnswerStrategy
		wantKnowledge KnowledgeOptions
		wantErr       error
	}{
		{
			name:          "chat without knowledge",
			mode:          SessionModeChat,
			knowledge:     KnowledgeOptions{},
			want:          AnswerStrategyDirectChat,
			wantKnowledge: KnowledgeOptions{},
		},
		{
			name:          "chat with knowledge",
			mode:          SessionModeChat,
			knowledge:     KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}},
			want:          AnswerStrategyKnowledgeQA,
			wantKnowledge: KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}},
		},
		{
			name:          "learning agent",
			mode:          SessionModeLearningAgent,
			knowledge:     KnowledgeOptions{},
			want:          AnswerStrategyAgent,
			wantKnowledge: KnowledgeOptions{},
		},
		{
			name:      "learning agent rejects the chat toggle",
			mode:      SessionModeLearningAgent,
			knowledge: KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}},
			wantErr:   ErrInvalidKnowledgeOptions,
		},
		{
			name:      "learning agent rejects orphan collection ids",
			mode:      SessionModeLearningAgent,
			knowledge: KnowledgeOptions{CollectionIDs: []string{"collection-1"}},
			wantErr:   ErrInvalidKnowledgeOptions,
		},
		{
			name:      "chat with knowledge needs a collection",
			mode:      SessionModeChat,
			knowledge: KnowledgeOptions{Enabled: true},
			wantErr:   ErrInvalidKnowledgeOptions,
		},
		{
			name:      "disabled knowledge cannot carry collections",
			mode:      SessionModeChat,
			knowledge: KnowledgeOptions{CollectionIDs: []string{"collection-1"}},
			wantErr:   ErrInvalidKnowledgeOptions,
		},
		{
			name:      "removed mode fast_chat is rejected",
			mode:      SessionMode("fast_chat"),
			knowledge: KnowledgeOptions{},
			wantErr:   ErrInvalidSessionMode,
		},
		{
			name:      "knowledge_qa is not a session mode",
			mode:      SessionMode("knowledge_qa"),
			knowledge: KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}},
			wantErr:   ErrInvalidSessionMode,
		},
		{
			name:      "empty mode is rejected",
			mode:      SessionMode(""),
			knowledge: KnowledgeOptions{},
			wantErr:   ErrInvalidSessionMode,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			strategy, knowledge, err := ResolveAnswerStrategy(testCase.mode, testCase.knowledge)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("error = %v, want %v", err, testCase.wantErr)
			}
			if testCase.wantErr != nil {
				if strategy != "" || !reflect.DeepEqual(knowledge, KnowledgeOptions{}) {
					t.Fatalf("returned %q %#v with an error", strategy, knowledge)
				}
				return
			}
			if strategy != testCase.want {
				t.Fatalf("strategy = %q, want %q", strategy, testCase.want)
			}
			if !strategy.IsValid() {
				t.Fatalf("strategy %q is not a member of the closed set", strategy)
			}
			if !reflect.DeepEqual(knowledge, testCase.wantKnowledge) {
				t.Fatalf("knowledge = %#v, want %#v", knowledge, testCase.wantKnowledge)
			}
		})
	}
}

func TestKnowledgeOptionsNormalizeTrimsDeduplicatesAndKeepsOrder(t *testing.T) {
	normalized, err := KnowledgeOptions{
		Enabled:       true,
		CollectionIDs: []string{" b ", "a", "b", "  c  ", "a"},
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b", "a", "c"}
	if !reflect.DeepEqual(normalized.CollectionIDs, want) {
		t.Fatalf("collection ids = %#v, want %#v", normalized.CollectionIDs, want)
	}
	if !normalized.Enabled {
		t.Fatal("normalized options lost the enabled flag")
	}
}

func TestKnowledgeOptionsNormalizeRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name      string
		knowledge KnowledgeOptions
	}{
		{"enabled without collections", KnowledgeOptions{Enabled: true}},
		{"enabled with only blank collections", KnowledgeOptions{Enabled: true, CollectionIDs: []string{"   ", ""}}},
		{"one blank collection among valid ones", KnowledgeOptions{Enabled: true, CollectionIDs: []string{"a", "  "}}},
		{"disabled with collections", KnowledgeOptions{CollectionIDs: []string{"a"}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.knowledge.Normalize(); !errors.Is(err, ErrInvalidKnowledgeOptions) {
				t.Fatalf("error = %v, want ErrInvalidKnowledgeOptions", err)
			}
		})
	}
}

func TestKnowledgeOptionsNormalizeAcceptsTheDocumentedDisabledDefault(t *testing.T) {
	// A client omitting "knowledge" and one sending
	// {"enabled":false,"collection_ids":[]} must mean the same thing.
	normalized, err := KnowledgeOptions{Enabled: false, CollectionIDs: []string{}}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, KnowledgeOptions{}) {
		t.Fatalf("normalized = %#v, want %#v", normalized, KnowledgeOptions{})
	}
}

func TestResolveAnswerStrategyDoesNotAliasReturnedCollections(t *testing.T) {
	input := KnowledgeOptions{Enabled: true, CollectionIDs: []string{"collection-1"}}
	_, resolved, err := ResolveAnswerStrategy(SessionModeChat, input)
	if err != nil {
		t.Fatal(err)
	}
	resolved.CollectionIDs[0] = "tampered"
	if input.CollectionIDs[0] != "collection-1" {
		t.Fatalf("resolution shared its slice with the caller: %#v", input.CollectionIDs)
	}
}
