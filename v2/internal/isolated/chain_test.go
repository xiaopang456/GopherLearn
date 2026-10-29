package isolated

import (
	"GopherAI/common/rag"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	"GopherAI/service/knowledge"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"strings"
	"testing"
	"time"
)

func toolCall(name, args string) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "call-" + name, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}}}
}
func requestText(chat *captureModel) string { data, _ := json.Marshal(chat.all()); return string(data) }
func readAnswer(chat *captureModel) (*schema.Message, error) {
	requests := chat.all()
	if len(requests) == 0 {
		return nil, errors.New("no request")
	}
	for _, message := range requests[len(requests)-1].Messages {
		if message.Role != schema.Tool {
			continue
		}
		var out struct {
			Messages []struct {
				Content   string `json:"content"`
				Reference string `json:"reference"`
			}
		}
		if json.Unmarshal([]byte(message.Content), &out) == nil && len(out.Messages) > 0 {
			return schema.AssistantMessage(out.Messages[0].Content+" ["+out.Messages[0].Reference+"]", nil), nil
		}
	}
	return schema.AssistantMessage("没有获准的历史证据", nil), nil
}
func TestHistoryCapabilityAbsentWithoutGrantThroughHTTP(t *testing.T) {
	s := testSettings(t)
	app, _, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-no-grant")
	runHTTP(t, app, token, session, "run-no-grant", "你好")
	assertHistoryCapability(t, chat, false)
}
func toolMode(s *Settings) {
	c := &s.Config.AgentConfig
	c.MemoryReadMode = "agent_memory_source_priority_v2"
	c.MaxToolCalls = 6
	c.MaxModelCalls = 7
	c.MaxGraphSteps = 16
	c.MemoryToolMaxCalls = 2
	c.MemoryToolTopK = 5
	c.MemoryToolCandidateDepth = 20
	c.MemoryToolMinConfidence = 0.75
	c.MemoryToolTimeoutMS = 3000
	c.TopicConsentSecret = "synthetic-topic-consent-key-at-least-32-chars"
}

func TestSummaryGrantGeneratedByOriginalContextAndReadThroughHTTP(t *testing.T) {
	s := testSettings(t)
	toolMode(s)
	var chat *captureModel
	var messageID uint
	app, db, c, token := newFixture(t, s, func(_ context.Context, n int) (*schema.Message, error) {
		if n == 1 {
			return toolCall("read_history_message", fmt.Sprintf(`{"message_ids":[%d]}`, messageID)), nil
		}
		return readAnswer(chat)
	})
	chat = c
	session := createSession(t, app, token, "create-summary")
	message := model.Message{SessionID: session, UserName: "offline-user", Content: "原始约束：仅使用离线资料。", IsUser: true}
	if err := db.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	messageID = message.ID
	content, _ := json.Marshal(model.SessionSummaryDocument{SchemaVersion: model.SessionSummarySchemaV2, CurrentGoal: "核对原始约束", ConfirmedDecisions: model.SessionSummaryEvidenceList{{Text: "只使用离线资料", EvidenceMessageIDs: []uint{messageID}}}})
	if err := db.Create(&model.SessionSummary{UserID: 1, SessionID: session, Content: string(content), CoveredMessageID: messageID, Version: 1, TokenEstimate: 100}).Error; err != nil {
		t.Fatal(err)
	}
	result := runHTTP(t, app, token, session, "run-summary", "请核对当前对话中我的原话")
	if !strings.Contains(fmt.Sprint(result["answer"]), "仅使用离线资料") || result["toolCalls"] != float64(1) {
		t.Fatalf("summary/read chain: %+v requests %s", result, requestText(chat))
	}
	assertHistoryCapability(t, chat, true)
	text := requestText(chat)
	if !strings.Contains(text, "核对原始约束") || !strings.Contains(text, "reference") {
		t.Fatalf("summary injection or issued evidence absent: %s", text)
	}
}
func TestEpisodeInjectAndShadowUseOriginalRecallAndEvidence(t *testing.T) {
	for _, recall := range []string{"shadow_only", "inject_enabled"} {
		t.Run(recall, func(t *testing.T) {
			s := testSettings(t)
			s.Config.AgentConfig.EpisodeRecallMode = recall
			var chat *captureModel
			var id uint
			app, db, c, token := newFixture(t, s, func(_ context.Context, n int) (*schema.Message, error) {
				if n == 1 && recall == "inject_enabled" {
					return toolCall("read_history_message", fmt.Sprintf(`{"message_ids":[%d]}`, id)), nil
				}
				return readAnswer(chat)
			})
			chat = c
			prior := createSession(t, app, token, "create-prior")
			message := model.Message{SessionID: prior, UserName: "offline-user", Content: "我决定离线核对原实现。", IsUser: true}
			if err := db.Create(&message).Error; err != nil {
				t.Fatal(err)
			}
			id = message.ID
			episode := model.EpisodeMemory{ID: "offline-episode", UserID: 1, SessionID: prior, Title: "之前的决定", Document: model.EpisodeDocument{Synopsis: "上次决定", SourceHandles: []string{"message:1"}, Decisions: []model.EpisodeEvidence{{Text: "离线核对原实现", EvidenceMessageIDs: []uint{id}}}}, SearchText: "离线核对原实现", TopicKeys: model.StringList{}, SourceLastMessageID: id, LastMessageAt: time.Now().Add(-time.Hour), Version: 1, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}
			if err := db.Create(&episode).Error; err != nil {
				t.Fatal(err)
			}
			session := createSession(t, app, token, "create-current")
			result := runHTTP(t, app, token, session, "run-episode", "上次我的原话是什么")
			if recall == "inject_enabled" {
				if !strings.Contains(fmt.Sprint(result["answer"]), "我决定离线核对原实现") {
					t.Fatalf("episode read chain: %+v %s", result, requestText(chat))
				}
			} else {
				if strings.Contains(fmt.Sprint(result["answer"]), "我决定离线核对原实现") {
					t.Fatal("shadow authorized raw read")
				}
			}
			assertHistoryCapability(t, chat, recall == "inject_enabled")
			want := float64(0)
			if recall == "inject_enabled" {
				want = 1
			}
			if result["toolCalls"] != want {
				t.Fatalf("original history calls: %+v", result)
			}
		})
	}
}
func TestDocumentReadUsesStoredOriginalNotSnippetThroughHTTP(t *testing.T) {
	s := testSettings(t)
	app, db, chat, token := newFixture(t, s, func(_ context.Context, n int) (*schema.Message, error) {
		switch n {
		case 1:
			return toolCall("knowledge_search", `{"query":"TCP"}`), nil
		case 2:
			return toolCall("document_read", `{"document_id":"offline-doc","document_version":1,"start_line":1,"end_line":3}`), nil
		default:
			return schema.AssistantMessage("原文说明 ORIGIN_ONLY_BOUNDARY [1]", nil), nil
		}
	})
	storage, err := knowledge.NewLocalFileStorage(s.Config.KnowledgeConfig.StorageDir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := storage.SaveTemp(context.Background(), 1, "offline-doc", strings.NewReader("# TCP\nSNIPPET_ONLY\nORIGIN_ONLY_BOUNDARY\n"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := storage.Publish(stored.TempPath, 1, "offline-doc", 1, "md")
	if err != nil {
		t.Fatal(err)
	}
	doc := model.KnowledgeDocument{ID: "offline-doc", UserID: 1, OriginalName: "TCP.md", StoragePath: path, FileType: "md", ContentHash: stored.ContentHash, FileSize: stored.FileSize, Version: 1, ActiveVersion: 1, IndexStatus: model.KnowledgeStatusReady}
	doc.PrepareForCreate()
	if err := db.Create(&doc).Error; err != nil {
		t.Fatal(err)
	}
	chat.chunks.rows = []rag.RedisChunkSearchResult{{RedisKey: "offline-vector", ChunkID: "chunk-1", DocID: "offline-doc", DocVersion: 1, ChunkIndex: 0, ChunkType: "prose", SourceFile: "TCP.md", TitlePath: "TCP", StartLine: 1, EndLine: 2, RawContent: "SNIPPET_ONLY", EmbeddingContent: "SNIPPET_ONLY", Distance: 0.01, LexicalScore: 1}}
	session := createSession(t, app, token, "create-document")
	result := runHTTP(t, app, token, session, "run-document", "讲解TCP原文")
	if result["toolCalls"] != float64(2) {
		var steps []model.AgentStep
		db.Find(&steps)
		var runs []model.AgentRun
		db.Find(&runs)
		t.Fatalf("document read chain: %+v runs %+v steps %+v %s", result, runs, steps, requestText(chat))
	}
	if !strings.Contains(requestText(chat), "ORIGIN_ONLY_BOUNDARY") {
		t.Fatalf("actual model never received source-only text: %s", requestText(chat))
	}
}
func TestContextTestDefaultIsNotDeploymentDefault(t *testing.T) {
	if agentService.DefaultContextConfig().InputTargetTokens != 20000 {
		t.Fatal("original test default changed")
	}
	if testSettings(t).Config.AgentConfig.ContextInputTargetTokens != 28000 {
		t.Fatal("original config default changed")
	}
}
