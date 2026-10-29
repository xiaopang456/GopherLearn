package model

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeLearningTopicKey(t *testing.T) {
	tests := map[string]string{
		"  Redis   缓存  ": "redis-缓存",
		"Gin/中间件":        "gin-中间件",
		"RPC_粘包---处理":    "rpc-粘包-处理",
		"***":            "",
	}
	for input, want := range tests {
		if got := NormalizeLearningTopicKey(input); got != want {
			t.Fatalf("NormalizeLearningTopicKey(%q) = %q, want %q", input, got, want)
		}
	}
	long := strings.Repeat("a", 100)
	if got := NormalizeLearningTopicKey(long); len([]rune(got)) != 80 {
		t.Fatalf("normalized long topic length = %d, want 80", len([]rune(got)))
	}
}

func TestLearningSessionStateMachine(t *testing.T) {
	valid := []struct{ from, to LearningSessionStatus }{
		{LearningSessionCreated, LearningSessionAsking},
		{LearningSessionAsking, LearningSessionWaitingAnswer},
		{LearningSessionWaitingAnswer, LearningSessionGrading},
		{LearningSessionGrading, LearningSessionWaitingAnswer},
		{LearningSessionGrading, LearningSessionCompleted},
		{LearningSessionWaitingAnswer, LearningSessionCancelled},
	}
	for _, transition := range valid {
		if !transition.from.CanTransition(transition.to) {
			t.Fatalf("expected transition %s -> %s", transition.from, transition.to)
		}
	}
	invalid := []struct{ from, to LearningSessionStatus }{
		{LearningSessionCreated, LearningSessionWaitingAnswer},
		{LearningSessionWaitingAnswer, LearningSessionCompleted},
		{LearningSessionCompleted, LearningSessionWaitingAnswer},
		{LearningSessionCancelled, LearningSessionCreated},
	}
	for _, transition := range invalid {
		if transition.from.CanTransition(transition.to) {
			t.Fatalf("unexpected transition %s -> %s", transition.from, transition.to)
		}
	}
}

func TestLearningQuestionRequiresBoundedEvidence(t *testing.T) {
	question := &LearningQuestion{
		ID: "question-1", UserID: 1, LearningSessionID: "learning-1", Ordinal: 1,
		Type: LearningQuestionShortAnswer, Question: "Redis 的过期策略是什么？",
		AssessmentGoal:  "理解 Redis 过期删除策略",
		ReferenceAnswer: "惰性删除与定期删除", Rubric: []string{"说明两类删除策略"},
		Evidence: []LearningEvidence{{
			EvidenceID: 1, DocumentID: "doc-1", DocumentName: "redis.md", DocVersion: 2,
			ChunkID: "chunk-1", StartLine: 10, EndLine: 20, ContentHash: "hash", Snapshot: "证据正文",
		}},
	}
	if err := question.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
	question.Evidence[0].Snapshot = strings.Repeat("中", 6001)
	if err := question.ValidateForCreate(); err == nil {
		t.Fatal("expected oversized evidence snapshot to fail")
	}

	activeKey := LearningActiveKey(1, "chat-1")
	session := &LearningSession{
		ID: "learning-1", UserID: 1, SessionID: "chat-1", AgentRunID: "run-1",
		Topic: "Redis", TopicKey: "redis", Status: LearningSessionCreated,
		CurrentQuestion: 1, TotalQuestions: 3, Version: 1, ActiveKey: &activeKey,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := session.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
}
