package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/common/rag"
	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// The blocks that change on every turn are sent behind the conversation
// history. The reason is the prefix cache: DeepSeek caches on a complete
// prefix, so a block that changes every turn and sits in front of the history
// invalidates that history's cache on every turn.
//
// What this file pins down is the other half of that decision: the change
// moves blocks and nothing else. The budget each block receives, the read-back
// authority the model is given, and the bytes the model sees are the same
// before and after - only the order differs.

// orderSummary is a summary provider that carries the fields the read-back
// authority needs: a version, a watermark and a document with evidence ids.
type orderSummary struct{ content string }

func (s orderSummary) SessionSummary(context.Context, RunContext) (SummarySnapshot, error) {
	return SummarySnapshot{Content: s.content, CoveredMessageID: 12, Version: 3}, nil
}

const orderSummaryContent = `{"schema_version":2,"current_goal":"学完 RPC 粘包",` +
	`"confirmed_decisions":[{"text":"使用长度字段拆包","evidence_message_ids":[11,12]}],` +
	`"open_loops":[{"text":"补并发半包测试","evidence_message_ids":[13]}]}`

// orderWindow builds one window of a fixed shape. The per-turn blocks are
// parameters, so a test can change them between two turns and see exactly
// what the change moved.
func orderWindow(t *testing.T, global, topic string, chunk string, history []model.Message) ContextWindow {
	t.Helper()
	config := testContextConfig()
	config.MemoryReadMode = memoryrecall.ModeAgentMemorySourcePriorityV3
	config.EpisodeRecallMode = EpisodeRecallInject
	config.InputTargetTokens = 8192
	config.ModelWindowTokens = 16384
	config.EvidenceReserve = 800
	config.MaxEpisodeTokens = 900

	builder, err := NewContextBuilder(config, &fakeHistoryReader{messages: history},
		staticTaskState("active learning task"),
		staticMastery("mastery instruction"),
		MemoryProviderFunc(func(context.Context, RunContext) (MemoryRecall, error) {
			return MemoryRecall{Global: []string{global}, Topic: []string{topic}}, nil
		}),
		orderSummary{content: orderSummaryContent})
	if err != nil {
		t.Fatal(err)
	}
	builder.UseEpisodeProvider(EpisodeProviderFunc(func(context.Context, RunContext) ([]model.EpisodeMemory, error) {
		return []model.EpisodeMemory{
			testEpisode("episode-1", "第一次讨论", time.Date(2026, 8, 1, 8, 0, 0, 0, time.UTC)),
		}, nil
	}))
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "半包怎么处理", EpisodeRecallReady: true,
		Relevance: &RelevanceOutcome{
			Status: RelevanceHit, Threshold: 0.5, TopScore: 0.9,
			Chunks: []RelevanceChunk{{
				Citation: 1, Identity: "doc-1", DocID: "doc-1", DocVersion: 1,
				Source: rag.Source{FileName: "notes.md"}, Content: chunk,
			}},
			NextCitation: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return window
}

// orderBlock names the block a message belongs to, by what it says rather
// than by where it sits. Naming them this way is what lets the same input be
// compared before and after the change.
func orderBlock(index int, message *schema.Message, last int) string {
	content := message.Content
	switch {
	case index == 0:
		return "system"
	case index == last:
		return "question"
	case message.Role != schema.System:
		return "history"
	case strings.Contains(content, "active learning task"):
		return "task_state"
	case strings.Contains(content, "mastery instruction"):
		return "mastery"
	case strings.HasPrefix(content, "Session summary"):
		return "summary"
	case strings.HasPrefix(content, "The server ran a search"):
		return "pre_retrieval"
	case strings.HasPrefix(content, "Global user memories"):
		return "global_memory"
	case strings.HasPrefix(content, "Topic user memories"):
		return "topic_memory"
	case strings.HasPrefix(content, "Relevant cross-session episodes"):
		return "episode_memory"
	case strings.HasPrefix(content, "Current conversation evidence"):
		return "current_evidence"
	default:
		return "unknown"
	}
}

func orderBlocks(window ContextWindow) []string {
	blocks := make([]string, 0, len(window.Messages))
	for index, message := range window.Messages {
		blocks = append(blocks, orderBlock(index, message, len(window.Messages)-1))
	}
	return blocks
}

func orderHistory() []model.Message {
	return []model.Message{
		{ID: 13, IsUser: true, Content: "半包测试怎么写"},
		{ID: 14, IsUser: false, Content: "先补并发用例"},
	}
}

// The blocks that change every turn must all sit behind the conversation
// history, and the history must sit behind the blocks that rarely change.
func TestContextBuilderSendsPerTurnBlocksBehindTheConversationHistory(t *testing.T) {
	window := orderWindow(t, "偏好举例讲解", "粘包主题笔记", "粘包是消息边界问题", orderHistory())
	want := []string{
		"system", "task_state", "mastery", "summary",
		"history", "history",
		"pre_retrieval", "global_memory", "topic_memory", "episode_memory",
		"question",
	}
	got := orderBlocks(window)
	if len(got) != len(want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("blocks = %v, want %v", got, want)
		}
	}
}

// The budget each block receives must not move. The figures below were taken
// from the same input built by the previous ordering, and a change in any of
// them means this change stopped being a reordering.
func TestContextBuilderKeepsEveryBlockTheSameSizeWhenTheOrderChanges(t *testing.T) {
	window := orderWindow(t, "偏好举例讲解", "粘包主题笔记", "粘包是消息边界问题", orderHistory())
	budget := window.Budget
	if budget.System != 3889 || budget.TaskState != 11 || budget.Mastery != 10 ||
		budget.GlobalUserMemory != 38 || budget.TopicUserMemory != 38 ||
		budget.EpisodeMemory != 146 || budget.EpisodeCount != 1 || budget.Summary != 144 ||
		budget.RecentMessages != 21 || budget.CurrentEvidence != 0 ||
		budget.PreRetrievalEvidence != 191 || budget.CurrentQuestion != 10 ||
		budget.EvidenceReserved != 609 || budget.Assembled != 4498 || budget.ProjectedInput != 5107 {
		t.Fatalf("block sizes moved: %+v", budget)
	}
	if budget.DroppedMessages != 0 || budget.TruncatedMessages != 0 {
		t.Fatalf("history was trimmed: %+v", budget)
	}
	if budget.ProjectedInput > budget.InputTarget {
		t.Fatalf("budget overflow: %+v", budget)
	}
}

// The read-back authority must follow what was rendered, not where the block
// sits. The same ids are granted before and after the change.
func TestContextBuilderKeepsTheReadBackGrantsWhenTheOrderChanges(t *testing.T) {
	window := orderWindow(t, "偏好举例讲解", "粘包主题笔记", "粘包是消息边界问题", orderHistory())
	summaryGrants := window.SummaryHistoryGrants
	if len(summaryGrants) != 2 {
		t.Fatalf("summary grants = %+v", summaryGrants)
	}
	for index, grant := range summaryGrants {
		wantID := uint(11 + index)
		if grant.MessageID != wantID || grant.SourceKind != HistorySourceSummary ||
			grant.SourceSessionID != "session" || grant.SummaryVersion != 3 || grant.SummaryWatermark != 12 {
			t.Fatalf("summary grant %d = %+v", index, grant)
		}
	}
	episodeGrants := window.EpisodeHistoryGrants
	if len(episodeGrants) != 2 {
		t.Fatalf("episode grants = %+v", episodeGrants)
	}
	for index, grant := range episodeGrants {
		wantID := uint(11 + index*2)
		if grant.MessageID != wantID || grant.SourceKind != HistorySourceEpisode ||
			grant.SourceSessionID != "episode-source-episode-1" ||
			grant.EpisodeID != "episode-1" || grant.EpisodeVersion != 1 {
			t.Fatalf("episode grant %d = %+v", index, grant)
		}
	}
}

// Two turns of one conversation must share a byte-identical prefix up to the
// end of the conversation history.
//
// This is the whole point of the ordering: the second turn's prompt starts
// with the first turn's prompt, so the history the provider cached is still a
// cache hit. With the per-turn blocks in front, the second turn's prompt
// differs from the first turn's before the history is even reached, and
// nothing the history holds can be reused.
func TestContextBuilderKeepsTheHistoryPrefixByteIdenticalAcrossTurns(t *testing.T) {
	first := orderWindow(t, "偏好举例讲解", "粘包主题笔记", "粘包是消息边界问题", orderHistory())
	second := orderWindow(t, "换一种说法", "另一个主题", "半包的处理办法",
		append(orderHistory(), model.Message{ID: 15, IsUser: false, Content: "用长度字段包一层"}))

	// The end of the conversation history in the first turn: the last message
	// before the per-turn blocks begin.
	end := 0
	for index, message := range first.Messages {
		if index == len(first.Messages)-1 {
			break
		}
		if message.Role != schema.System {
			end = index + 1
		}
	}
	if end == 0 {
		t.Fatalf("the first turn carries no conversation history: %v", orderBlocks(first))
	}
	if end > len(second.Messages) {
		t.Fatalf("the second turn is shorter than the first: %v", orderBlocks(second))
	}
	for index := 0; index < end; index++ {
		if first.Messages[index].Role != second.Messages[index].Role ||
			first.Messages[index].Content != second.Messages[index].Content {
			t.Fatalf("prefix differs at %d: %q vs %q", index,
				first.Messages[index].Content, second.Messages[index].Content)
		}
	}
	// The per-turn blocks must actually differ, or the assertion above would
	// hold for a window that never changes.
	tail := false
	for index := end; index < len(first.Messages) && index < len(second.Messages); index++ {
		if first.Messages[index].Content != second.Messages[index].Content {
			tail = true
			break
		}
	}
	if !tail {
		t.Fatalf("the per-turn blocks did not change between the two turns")
	}
}
