package agent

import (
	"GopherAI/common/rag"
	"context"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
)

// The read side of the ceiling has its own test; this is the write side, and it is the
// half that costs money later rather than now.
//
// Both publishers only append a debounced queue row, so a turn that enqueued one looks
// free while it runs. The episode worker then projects the session with the chat model
// and embeds it, and the memory worker extracts from the run the same way. A turn the
// user switched off has to leave neither behind, or "this turn used no capability" is
// true of the answer and false of the bill.
//
// The third arm is the one with teeth: an explicit "请记住" question is the only case
// where the real memory publisher enqueues from a run that called no tool, so a gate
// that merely relied on toolCalls being zero would pass the first arm and fail this one.
func TestServiceHandsACapabilityFreeTurnToNoBackgroundPipeline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     rag.KnowledgePolicy
		question   string
		wantWrites int32
	}{
		{name: "switched off", policy: rag.KnowledgeDisabled, question: "解释 Context 取消", wantWrites: 0},
		{name: "switched off but asking to be remembered", policy: rag.KnowledgeDisabled, question: "请记住：我理解 TCP 粘包的本质是缓冲区边界不明确", wantWrites: 0},
		{name: "switched on", policy: rag.KnowledgePreferred, question: "解释 Context 取消", wantWrites: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var episodes, memories atomic.Int32
			repository := newMemoryRunRepository(agentTestSession())
			chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
				return schema.AssistantMessage("好的", nil), nil
			}}
			runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
			service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
			if err != nil {
				t.Fatal(err)
			}
			// Counting the hand-off, not what the workers do with it: each publisher has
			// its own reasons for declining a run, and those are tested where they live.
			service.SetEpisodePublisher(EpisodeEventPublisherFunc(func(context.Context, int64, string, uint) error {
				episodes.Add(1)
				return nil
			}))
			service.SetMemoryPublisher(MemoryEventPublisherFunc(func(context.Context, int64, string, string, string, int, bool) error {
				memories.Add(1)
				return nil
			}))
			request := agentTestRequest()
			request.KnowledgePolicy = tc.policy
			request.Question = tc.question
			if _, err := service.Run(context.Background(), request, DiscardEvents); err != nil {
				t.Fatal(err)
			}
			if got := episodes.Load() + memories.Load(); got != tc.wantWrites {
				t.Fatalf("%s: enqueued %d background jobs (episode=%d memory=%d), want %d",
					tc.name, got, episodes.Load(), memories.Load(), tc.wantWrites)
			}
		})
	}
}

// A run that failed is handed to no pipeline either. The failure path publishes for the
// same reason the success path does, so it needs the same gate; without this a turn that
// asked to be remembered and then failed would still be extracted from.
func TestServiceHandsAFailedCapabilityFreeTurnToNoBackgroundPipeline(t *testing.T) {
	var episodes, memories atomic.Int32
	repository := newMemoryRunRepository(agentTestSession())
	chatModel := &scriptedChatModel{respond: func(context.Context, int) (*schema.Message, error) {
		return nil, context.DeadlineExceeded
	}}
	runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
	service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	service.SetEpisodePublisher(EpisodeEventPublisherFunc(func(context.Context, int64, string, uint) error {
		episodes.Add(1)
		return nil
	}))
	service.SetMemoryPublisher(MemoryEventPublisherFunc(func(context.Context, int64, string, string, string, int, bool) error {
		memories.Add(1)
		return nil
	}))
	request := agentTestRequest()
	request.KnowledgePolicy = rag.KnowledgeDisabled
	request.Question = "请记住：我理解 TCP 粘包的本质是缓冲区边界不明确"
	if _, err := service.Run(context.Background(), request, DiscardEvents); err == nil {
		t.Fatal("a failing run was reported as a success, so this test proved nothing")
	}
	if got := episodes.Load() + memories.Load(); got != 0 {
		t.Fatalf("a failed capability-free turn enqueued %d background jobs (episode=%d memory=%d)", got, episodes.Load(), memories.Load())
	}
}
