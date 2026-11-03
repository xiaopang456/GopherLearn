package conversation

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/entry"
	agent "GopherAI/service/agent"
	"context"
	"github.com/cloudwego/eino/schema"
	"io"
	"path/filepath"
	"testing"
)

func TestOfflineWireUsesProductionSDK(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	settings, err := offlineSettings(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roles, err := LoadRolePrompts(root, memoryrecall.ReadMode("legacy_v2"))
	if err != nil {
		t.Fatal(err)
	}
	ledger := &ModelLedger{}
	server := offlineWire(ledger, roles, &offlineChat{})
	defer server.Close()
	settings.Entry.ChatBaseURL = server.URL + "/v1"
	model, err := entry.ProductionDependencies(settings).ChatModel(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	input := []*schema.Message{schema.SystemMessage(agent.SystemPromptFor(memoryrecall.ReadMode("legacy_v2"))), schema.UserMessage("synthetic question")}
	response, err := model.Generate(context.Background(), input)
	if err != nil || response.Content != "synthetic offline answer" {
		t.Fatalf("generate response=%+v err=%v", response, err)
	}
	stream, err := model.Stream(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	content := ""
	for {
		part, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content += part.Content
	}
	if content != "synthetic offline answer" || len(ledger.Calls()) != 2 || !ledger.Complete() {
		t.Fatalf("wire content=%q calls=%+v", content, ledger.Calls())
	}
	settings.Config.RagEmbeddingBaseURL = server.URL + "/v1"
	embedder, err := entry.ProductionDependencies(settings).Embedder(context.Background(), settings.Config)
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := embedder.EmbedStrings(context.Background(), []string{"synthetic input", "second input"})
	if err != nil || len(vectors) != 2 || len(vectors[0]) != 1024 {
		t.Fatalf("embedding transport vectors=%d err=%v", len(vectors), err)
	}
	calls := ledger.Calls()
	if len(calls) != 3 || calls[2].Role != "embedding" || !ledger.Complete() {
		t.Fatal("embedding not accounted", calls)
	}
	if calls[0].RequestedMaxTokens != nil || calls[0].RequestedMaxCompletionTokens != nil {
		t.Fatal("product factory output cap changed; recheck budgeting assumptions")
	}
}
