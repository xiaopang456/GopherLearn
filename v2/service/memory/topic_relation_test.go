package memory

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"GopherAI/model"
)

// TestTopicRelationClassifierCarriesTopicAndFinalScope is the input audit for
// the production topic judgement: the payload names the topic, the proposed
// note, and the active notes of that topic - and a snapshot mixing another
// topic's row is rejected rather than compared.
func TestTopicRelationClassifierCarriesTopicAndFinalScope(t *testing.T) {
	chat := &memoryChat{response: toolMessage(topicIndependentTool, `{"reason":"another matter"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	incoming := ExplicitMemoryInput{
		Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "软删除",
		Content: "GORM 默认查询会过滤软删除记录，要用 Unscoped 才能看到。",
	}
	existing := []model.LongTermMemory{
		{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "软删除", Content: "DeletedAt 让默认查询自动过滤已删记录。", Status: model.LongTermMemoryActive},
		{ID: "note-x", Scope: model.MemoryScopeTopic, TopicKey: "go-basics", Subject: "切片", Content: "别的话题的笔记", Status: model.LongTermMemoryActive},
	}
	if _, err := classifier.Classify(context.Background(), incoming, existing); err == nil {
		t.Fatalf("a mixed-topic snapshot must be rejected, not compared")
	}
	if _, err := classifier.Classify(context.Background(), incoming, existing[:1]); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(chat.messages) != 2 {
		t.Fatalf("messages=%d, want system plus user", len(chat.messages))
	}
	var payload struct {
		TopicKey     string `json:"topic_key"`
		ProposedNote struct {
			Subject string `json:"subject"`
			Content string `json:"content"`
		} `json:"proposed_note"`
		ActiveNotes []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"active_notes"`
	}
	if err := json.Unmarshal([]byte(chat.messages[1].Content), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.TopicKey != "gorm" || payload.ProposedNote.Content != incoming.Content {
		t.Fatalf("payload=%+v, want the final topic and the proposed note", payload)
	}
	if len(payload.ActiveNotes) != 1 || payload.ActiveNotes[0].ID != "note-1" {
		t.Fatalf("notes=%+v, want only the final topic's active rows", payload.ActiveNotes)
	}
	for _, fragment := range []string{
		"every supplied note is from that same topic",
		"Different wording or a different analogy never makes it distinct",
		"A note about another subject on this topic is independent",
	} {
		if !strings.Contains(chat.messages[0].Content, fragment) {
			t.Fatalf("prompt rules missing %q", fragment)
		}
	}
}

// TestTopicRelationClassifierRejectsInventedTarget mirrors the global scope's
// own guard: the judge may only point at a note the server supplied.
func TestTopicRelationClassifierRejectsInventedTarget(t *testing.T) {
	chat := &memoryChat{response: toolMessage(topicConflictTool, `{"relation":"duplicate","target_memory_id":"invented","reason":"same"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	_, err := classifier.Classify(context.Background(),
		ExplicitMemoryInput{Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "s", Content: "c"},
		[]model.LongTermMemory{{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "s", Content: "c", Status: model.LongTermMemoryActive}})
	if err == nil {
		t.Fatalf("an invented target must fail closed")
	}
}
