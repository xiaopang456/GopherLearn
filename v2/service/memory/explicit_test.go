package memory

import (
	"GopherAI/model"
	"errors"
	"testing"
	"time"
)

func TestNormalizeExplicitInputEnforcesScopeExpiryAndSensitiveBoundary(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		input ExplicitMemoryInput
		valid bool
	}{
		{name: "global stable", input: ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "讲解偏好", Content: "先讲整体链路", Durability: model.MemoryDurabilityStable, IdempotencyKey: "one"}, valid: true},
		{name: "topic requires key", input: ExplicitMemoryInput{Scope: model.MemoryScopeTopic, Subject: "RPC", Content: "我不理解粘包", Durability: model.MemoryDurabilityStable, IdempotencyKey: "two"}},
		{name: "global rejects topic key", input: ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "RPC", Content: "我不理解粘包", TopicKey: "rpc", Durability: model.MemoryDurabilityStable, IdempotencyKey: "three"}},
		{name: "expired time bound", input: ExplicitMemoryInput{Scope: model.MemoryScopeTopic, Subject: "阶段目标", Content: "本周复习 RPC", TopicKey: "rpc", Durability: model.MemoryDurabilityTimeBound, ValidUntil: timePointer(now.Add(-time.Minute)), IdempotencyKey: "four"}},
		{name: "future time bound", input: ExplicitMemoryInput{Scope: model.MemoryScopeTopic, Subject: "阶段目标", Content: "本周复习 RPC", TopicKey: "rpc", Durability: model.MemoryDurabilityTimeBound, ValidUntil: timePointer(now.Add(time.Hour)), IdempotencyKey: "five"}, valid: true},
		{name: "sensitive secret", input: ExplicitMemoryInput{Scope: model.MemoryScopeGlobal, Subject: "token", Content: "api_key=private-value", Durability: model.MemoryDurabilityStable, IdempotencyKey: "six"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeExplicitInput(9, test.input, now)
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && !errors.Is(err, ErrInvalidExplicitWrite) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestNormalizeExplicitInputCanonicalizesTopicKey(t *testing.T) {
	now := time.Now()
	input, err := normalizeExplicitInput(7, ExplicitMemoryInput{
		Scope: model.MemoryScopeTopic, Subject: "HTTP boundary", Content: "I confuse framing modes",
		TopicKey: " NimbusStream_913 ", Durability: model.MemoryDurabilityStable, IdempotencyKey: "topic-key",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if input.TopicKey != "nimbusstream-913" {
		t.Fatalf("topic key=%q", input.TopicKey)
	}
}

func TestNormalizeExplicitInputDerivesFixedConflictTagsWhenOmitted(t *testing.T) {
	now := time.Now()
	input, err := normalizeExplicitInput(7, ExplicitMemoryInput{
		Scope: model.MemoryScopeGlobal, Subject: "回答偏好", Content: "回答先给结论，再解释原因。",
		Durability: model.MemoryDurabilityStable, IdempotencyKey: "derived-tags",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Tags) == 0 || input.Tags[0] != "preference" {
		t.Fatalf("derived tags=%v", input.Tags)
	}
}

func TestNormalizeExplicitInputRecognizesAnswerOrderAsPreference(t *testing.T) {
	now := time.Now()
	input, err := normalizeExplicitInput(7, ExplicitMemoryInput{
		Scope: model.MemoryScopeGlobal, Subject: "回答顺序", Content: "请先说结论，然后解释原因。",
		Durability: model.MemoryDurabilityStable, IdempotencyKey: "answer-order",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tag := range input.Tags {
		found = found || tag == "preference"
	}
	if !found {
		t.Fatalf("derived tags=%v", input.Tags)
	}
}

func timePointer(value time.Time) *time.Time { return &value }
