package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
)

// TestLiveSmokeOneGeneration makes exactly one real model call through the
// bounded model, which is the path every agent turn takes.
//
// It is opt-in, because it costs money and it needs a provider:
//
//	GOPHERLEARN_LIVE_SMOKE=1
//	GOPHERLEARN_LIVE_SMOKE_BASE_URL=https://api.deepseek.com
//	GOPHERLEARN_LIVE_SMOKE_MODEL=deepseek-chat
//	GOPHERLEARN_LIVE_SMOKE_KEY=<the provider key>
//
// A local counting proxy can sit in front of the provider: the base URL
// points at the proxy's chat lane and the key is whatever the proxy expects,
// because the proxy replaces the Authorization header with the real one. The
// test makes one call and the proxy's cap makes sure that one is the only
// one.
//
// The test asserts the answer is text and that the turn recorded exactly one
// generation: a change to the retry path must not turn one turn into two
// calls when the provider answers.
func TestLiveSmokeOneGeneration(t *testing.T) {
	if os.Getenv("GOPHERLEARN_LIVE_SMOKE") != "1" {
		t.Skip("the live smoke is opt-in: set GOPHERLEARN_LIVE_SMOKE=1")
	}
	baseURL := strings.TrimSpace(os.Getenv("GOPHERLEARN_LIVE_SMOKE_BASE_URL"))
	model := strings.TrimSpace(os.Getenv("GOPHERLEARN_LIVE_SMOKE_MODEL"))
	key := strings.TrimSpace(os.Getenv("GOPHERLEARN_LIVE_SMOKE_KEY"))
	if baseURL == "" || model == "" || key == "" {
		t.Fatal("the live smoke needs a base URL, a model and a key")
	}

	chat, err := einoopenai.NewChatModel(context.Background(), &einoopenai.ChatModelConfig{
		BaseURL: baseURL,
		Model:   model,
		APIKey:  key,
	})
	if err != nil {
		t.Fatalf("the chat model could not be built: %v", err)
	}

	steps := newMemoryStepRepository()
	state := newRetryState(steps, 4)
	bounded := &boundedModel{inner: chat, state: state}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	message, err := bounded.Generate(ctx, []*schema.Message{schema.UserMessage("用一句话说明什么是向量检索。")})
	if err != nil {
		t.Fatalf("the live generation failed: %v", err)
	}
	if message == nil || strings.TrimSpace(message.Content) == "" {
		t.Fatal("the live generation returned no text")
	}
	if state.modelCalls != 1 {
		t.Fatalf("one answer must spend one generation, modelCalls=%d", state.modelCalls)
	}
	t.Logf("live smoke: one generation answered with %d characters", len(message.Content))
}
