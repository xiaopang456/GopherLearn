package agent

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/model"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// A turn that switched its capabilities off must not read anything personal or pay
// for any retrieval - and the ledger cannot prove that, because every one of these
// reads happens before a tool exists. So each reader counts its own calls, and the
// counts are compared under both switch positions, in every memory read mode that
// reaches them.
//
// The positive control is what makes the zero meaningful: a provider nobody wired
// up would report zero under both policies.

type countedTaskProvider struct{ calls atomic.Int32 }

func (p *countedTaskProvider) CurrentTaskState(context.Context, RunContext) (string, error) {
	p.calls.Add(1)
	return "当前任务：复习 TCP", nil
}

type countedMasteryProvider struct{ calls atomic.Int32 }

func (p *countedMasteryProvider) CurrentMastery(context.Context, RunContext) (string, error) {
	p.calls.Add(1)
	return "该主题掌握度 42", nil
}

type countedTopicResolver struct{ calls atomic.Int32 }

func (p *countedTopicResolver) ProposeTopicKeys(context.Context, RunContext) ([]string, error) {
	p.calls.Add(1)
	return []string{"topic-counted"}, nil
}

type countedEpisodeProvider struct{ calls atomic.Int32 }

func (p *countedEpisodeProvider) RelevantEpisodes(context.Context, RunContext) ([]model.EpisodeMemory, error) {
	p.calls.Add(1)
	return []model.EpisodeMemory{testEpisode("episode-counted", "讨论过粘包", time.Now().UTC())}, nil
}

type countedMemoryProvider struct{ calls atomic.Int32 }

func (p *countedMemoryProvider) RelevantMemories(context.Context, RunContext) (MemoryRecall, error) {
	p.calls.Add(1)
	return MemoryRecall{Global: []string{"用户偏好带例子的解释"}}, nil
}

func TestCapabilityFreeTurnReadsNoPersonalStateInAnyMode(t *testing.T) {
	for _, mode := range []memoryrecall.ReadMode{
		memoryrecall.DefaultReadMode(),
		memoryrecall.ModeEpisodeV3Lexical,
		memoryrecall.ModeEpisodeV3Semantic,
	} {
		for _, tc := range []struct {
			name   string
			policy rag.KnowledgePolicy
		}{
			{name: "switched off", policy: rag.KnowledgeDisabled},
			{name: "switched on", policy: rag.KnowledgePreferred},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				config := testContextConfig()
				config.MemoryReadMode = mode
				config.EpisodeRecallMode = EpisodeRecallInject
				tasks, mastery, topics, episodes, memories := &countedTaskProvider{}, &countedMasteryProvider{}, &countedTopicResolver{}, &countedEpisodeProvider{}, &countedMemoryProvider{}
				builder, err := NewContextBuilder(config, stubHistory{}, tasks, mastery, memories, nil)
				if err != nil {
					t.Fatal(err)
				}
				builder.UseEpisodeProvider(episodes)
				builder.UseTopicKeyResolver(topics)

				window, err := builder.Build(context.Background(), RunContext{
					UserID: 7, SessionID: "session", Question: "TCP 为什么没有消息边界",
					KnowledgePolicy: tc.policy, EpisodeRecallReady: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				counts := map[string]int32{
					"task state": tasks.calls.Load(), "mastery": mastery.calls.Load(),
					"topic resolver": topics.calls.Load(), "episodes": episodes.calls.Load(),
					"long-term memory": memories.calls.Load(),
				}
				shown := renderWindowForTest(window)
				if tc.policy == rag.KnowledgeDisabled {
					for what, got := range counts {
						if got != 0 {
							t.Fatalf("a capability-free turn read %s %d times under mode %s", what, got, mode)
						}
					}
					for _, leaked := range []string{"复习 TCP", "掌握度 42", "topic-counted", "偏好带例子", "讨论过粘包"} {
						if strings.Contains(shown, leaked) {
							t.Fatalf("a capability-free turn rendered %q into the window under mode %s", leaked, mode)
						}
					}
					return
				}
				// The control half: the same wiring does reach these readers when
				// the turn has capabilities, so the zeros above are the ceiling
				// and not an unused seam.
				if tasks.calls.Load() == 0 || mastery.calls.Load() == 0 {
					t.Fatalf("the control read nothing at all under mode %s; the zeros prove nothing", mode)
				}
				if mode.UsesTopicResolver() && topics.calls.Load() == 0 {
					t.Fatalf("mode %s is supposed to resolve topic keys, but the resolver was never called", mode)
				}
				if episodes.calls.Load() == 0 || memories.calls.Load() == 0 {
					t.Fatalf("mode %s read no episode/memory state; the wiring under test is not the production wiring", mode)
				}
			})
		}
	}
}

func renderWindowForTest(window ContextWindow) string {
	var text strings.Builder
	for _, message := range window.Messages {
		if message != nil {
			text.WriteString(message.Content)
			text.WriteString("\n")
		}
	}
	return text.String()
}

// The shadow reader lives in the service rather than the builder, so the builder's
// ceiling does not reach it: it calls the same retriever, and under a semantic mode
// that means a query embedding.
func TestServiceSkipsEpisodeShadowForACapabilityFreeTurn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     rag.KnowledgePolicy
		wantShadow int32
	}{
		{name: "switched off", policy: rag.KnowledgeDisabled, wantShadow: 0},
		{name: "switched on", policy: rag.KnowledgePreferred, wantShadow: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var observed atomic.Int32
			repository := newMemoryRunRepository(agentTestSession())
			chatModel := &scriptedChatModel{respond: func(_ context.Context, _ int) (*schema.Message, error) {
				return schema.AssistantMessage("好的", nil), nil
			}}
			runner := mustRunner(t, chatModel, StaticToolRegistry{}, repository, DefaultRunnerConfig())
			service, err := NewService(repository, repository, &memoryMessageStore{}, runner)
			if err != nil {
				t.Fatal(err)
			}
			service.SetEpisodeShadowObserver(EpisodeShadowObserverFunc(func(context.Context, int64, string, string) error {
				observed.Add(1)
				return nil
			}))
			request := agentTestRequest()
			request.KnowledgePolicy = tc.policy
			if _, err := service.Run(context.Background(), request, DiscardEvents); err != nil {
				t.Fatal(err)
			}
			if observed.Load() != tc.wantShadow {
				t.Fatalf("%s: shadow observed %d times, want %d", tc.name, observed.Load(), tc.wantShadow)
			}
		})
	}
}
