package memory

import (
	"context"
	"encoding/json"
	"errors"
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
		"duplicate: the proposal adds nothing",
		"Report the comparison in uncovered_points",
		"never answer duplicate with a non-empty list",
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

// TestTopicRelationClassifierRefusesADuplicateThatAlsoReportsPoints is the
// correction the review asked for. A judge that answers duplicate while also
// naming points it claims the note does not cover has contradicted itself,
// and the server must not pick a side: the named points can be blank, can
// repeat what the note already says, or can be facts the proposal never
// made. None of them is evidence that the proposal adds something, so the
// judgement is refused - the candidate stays retryable, the watermark is
// held, and no card is made this round.
func TestTopicRelationClassifierRefusesADuplicateThatAlsoReportsPoints(t *testing.T) {
	cases := []struct {
		name   string
		points string
	}{
		{"空白", `["   "]`},
		{"旧笔记已有的事实", `["中间件按注册顺序执行"]`},
		{"候选根本没说过的事实", `["Abort 会中断当前请求"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := &memoryChat{response: toolMessage(topicConflictTool,
				`{"relation":"duplicate","target_memory_id":"note-1","uncovered_points":`+tc.points+`,"reason":"同一件事"}`)}
			classifier, _ := NewTopicRelationClassifier(chat)
			result, err := classifier.Classify(context.Background(),
				ExplicitMemoryInput{Scope: model.MemoryScopeTopic, TopicKey: "gin-mw", Subject: "中间件链", Content: "中间件是按注册顺序依次执行的。"},
				[]model.LongTermMemory{{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gin-mw", Subject: "中间件链", Content: "中间件按注册顺序执行。", Status: model.LongTermMemoryActive}})
			if !errors.Is(err, ErrInvalidTopicRelation) {
				t.Fatalf("err=%v result=%+v, want the inconsistent judgement refused", err, result)
			}
			if result.Relation != "" || result.TargetMemoryID != "" {
				t.Fatalf("result=%+v, want no relation out of a refused judgement", result)
			}
		})
	}
}

// TestTopicRelationClassifierKeepsAConsistentSupplement keeps the half of the
// rule the refusal must not touch: a judge that answers complementary and
// names the point it adds is consistent, so the candidate keeps its card and
// the note it compared against.
func TestTopicRelationClassifierKeepsAConsistentSupplement(t *testing.T) {
	chat := &memoryChat{response: toolMessage(topicConflictTool,
		`{"relation":"complementary","target_memory_id":"note-1","uncovered_points":["Abort 跳过后续 handler，但不会中断当前请求"],"reason":"旧笔记没有覆盖 Abort 的这一条"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	result, err := classifier.Classify(context.Background(),
		ExplicitMemoryInput{Scope: model.MemoryScopeTopic, TopicKey: "gin-mw", Subject: "中间件链", Content: "中间件链按顺序执行，Abort 只跳过后续 handler。"},
		[]model.LongTermMemory{{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gin-mw", Subject: "中间件链", Content: "中间件按注册顺序执行。", Status: model.LongTermMemoryActive}})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result.Relation != TopicRelationComplementary {
		t.Fatalf("relation=%+v, want a consistent supplement kept", result)
	}
	if result.TargetMemoryID != "note-1" {
		t.Fatalf("target=%q, want the compared note", result.TargetMemoryID)
	}
}

// TestTopicRelationClassifierKeepsAPureRestatementDuplicate is the other half
// of the rule: a proposal that adds nothing reports no uncovered point and
// stays a duplicate, so the card is dropped and the existing note stands.
func TestTopicRelationClassifierKeepsAPureRestatementDuplicate(t *testing.T) {
	chat := &memoryChat{response: toolMessage(topicConflictTool,
		`{"relation":"duplicate","target_memory_id":"note-1","uncovered_points":[],"reason":"同一件事，只是换了措辞"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	result, err := classifier.Classify(context.Background(),
		ExplicitMemoryInput{Scope: model.MemoryScopeTopic, TopicKey: "gin-mw", Subject: "中间件链", Content: "中间件是按注册顺序依次执行的。"},
		[]model.LongTermMemory{{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gin-mw", Subject: "中间件链", Content: "中间件按注册顺序执行。", Status: model.LongTermMemoryActive}})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result.Relation != TopicRelationDuplicate {
		t.Fatalf("relation=%+v, want a pure restatement to stay a duplicate", result)
	}
}

// TestTopicRelationClassifierAcceptsAJudgeThatReportsNoCoverageField keeps
// the contract tolerant: a judge that answers without the coverage field is
// read as "adds nothing", so an older answer shape keeps its meaning instead
// of failing the whole judgement.
func TestTopicRelationClassifierAcceptsAJudgeThatReportsNoCoverageField(t *testing.T) {
	chat := &memoryChat{response: toolMessage(topicConflictTool,
		`{"relation":"duplicate","target_memory_id":"note-1","reason":"同一件事"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	result, err := classifier.Classify(context.Background(),
		ExplicitMemoryInput{Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "软删除", Content: "默认查询会过滤软删除记录。"},
		[]model.LongTermMemory{{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "软删除", Content: "DeletedAt 让默认查询过滤已删记录。", Status: model.LongTermMemoryActive}})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if result.Relation != TopicRelationDuplicate {
		t.Fatalf("relation=%+v, want duplicate", result)
	}
}

// TestTopicRelationReasonStaysInsideItsBound keeps the record faithful: a
// consistent answer is passed on as the judge wrote it, and it still fits the
// bound the reason field carries.
func TestTopicRelationReasonStaysInsideItsBound(t *testing.T) {
	long := strings.Repeat("同一件事，只是换了措辞。", 40)
	chat := &memoryChat{response: toolMessage(topicConflictTool,
		`{"relation":"complementary","target_memory_id":"note-1","uncovered_points":["旧笔记没有覆盖的一点"],"reason":"`+long+`"}`)}
	classifier, _ := NewTopicRelationClassifier(chat)
	result, err := classifier.Classify(context.Background(),
		ExplicitMemoryInput{Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "软删除", Content: "默认查询会过滤软删除记录。"},
		[]model.LongTermMemory{{ID: "note-1", Scope: model.MemoryScopeTopic, TopicKey: "gorm", Subject: "软删除", Content: "DeletedAt 让默认查询过滤已删记录。", Status: model.LongTermMemoryActive}})
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !validGlobalReason(result.Reason) {
		t.Fatalf("reason is not valid for the server: %q", result.Reason)
	}
	if result.Reason != long {
		t.Fatalf("reason=%q, want the judge's reason as written", result.Reason)
	}
}
