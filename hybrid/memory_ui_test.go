package hybrid

import (
	"GopherAI/model"
	"context"
	"github.com/cloudwego/eino/schema"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestMemoryFrontendPageAndAgentCardThroughOriginalHTTP(t *testing.T) {
	app, db, _, agent, _, _ := httpFixture(t)
	agent.respond = func(_ context.Context, n int) (*schema.Message, error) {
		if n%2 == 1 {
			args := `{"scope":"topic","topic_key":"redis","subject":"个人理解","content":"我用队列理解 Redis","tags":["redis"],"durability":"stable"}`
			if n == 5 {
				args = `{"scope":"topic","topic_key":"redis","subject":"过期目标","content":"我计划练习 Redis 队列","tags":["redis"],"durability":"time_bound","valid_until":"2030-01-01T00:00:00Z"}`
			}
			return &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "real-memory-ui-call", Type: "function", Function: schema.FunctionCall{Name: "propose_memory", Arguments: args}}}}, nil
		}
		if n == 6 {
			// Expiry injection on the actual tool-created row, before the old UI card
			// is acted upon. No synthetic draft/identity/grant or repository substitute.
			expired := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
			if err := db.Model(&model.MemoryDraft{}).Where("subject = ? AND user_id = ?", "过期目标", 1).Updates(map[string]any{"valid_until": expired, "created_at": expired.Add(-time.Hour)}).Error; err != nil {
				return nil, err
			}
		}
		return schema.AssistantMessage("已生成原待确认草稿", nil), nil
	}
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	cmd := exec.Command("node", "../web/test/helpers/hybridMemoryUiClient.mjs")
	cmd.Env = append(os.Environ(), "HYBRID_MEMORY_UI_URL="+server.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("actual frontend page/card chain: %v\n%s", err, output)
	}
	var drafts []model.MemoryDraft
	if err := db.Order("created_at").Find(&drafts).Error; err != nil || len(drafts) != 3 {
		t.Fatalf("original draft records %v %v", drafts, err)
	}
	var deleted int64
	db.Model(&model.LongTermMemory{}).Where("status = ?", model.LongTermMemoryDeleted).Count(&deleted)
	if deleted != 1 {
		t.Fatalf("original deleted memory %d", deleted)
	}
}
