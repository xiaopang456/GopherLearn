package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
)

// plannedConfig is the shipped budget with only the read mode changed, so any
// difference this file observes is the read path and not the budget.
func plannedConfig(mode memoryrecall.ReadMode) ContextConfig {
	config := DefaultContextConfig()
	config.EpisodeRecallMode = EpisodeRecallInject
	config.MemoryReadMode = mode
	return config
}

func plannedEpisode(id string, age time.Duration) model.EpisodeMemory {
	return model.EpisodeMemory{
		ID: id, UserID: 7, SessionID: "s-" + id, Title: "会话 " + id,
		Document: model.EpisodeDocument{
			Synopsis:  "一次关于 " + id + " 的讨论",
			Decisions: []model.EpisodeEvidence{{Text: "决定 " + id, EvidenceMessageIDs: []uint{3}}},
			Topics:    []model.EpisodeTopic{{Key: "主题" + id, Label: "主题" + id}},
		},
		TopicKeys: model.StringList{"主题" + id}, SearchText: "会话 " + id,
		SourceLastMessageID: 3, LastMessageAt: time.Now().Add(-age), Version: 1,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed,
	}
}

func plannedInput() RunContext {
	return RunContext{
		UserID: 7, UserName: "learner", SessionID: "s-now", Question: "漏桶我拿什么打的比方？",
		EpisodeRecallReady: true,
	}
}

type recordingPlanned struct {
	recall PlannedMemoryRecall
	err    error
	calls  int
	// at records when the provider ran relative to the builder returning, which
	// is how "the plan happens before the answering model sees anything" is
	// checked structurally rather than asserted.
	finished bool
}

func (r *recordingPlanned) PlanAndVerify(context.Context, RunContext) (PlannedMemoryRecall, error) {
	r.calls++
	if r.finished {
		return PlannedMemoryRecall{}, errors.New("called after the window was assembled")
	}
	return r.recall, r.err
}

// Under model_planned_v1 the old readers are not consulted at all. Calling them
// first and letting a model re-rank what they returned would leave the
// pre-STABILIZE-032 trigger rules deciding what the new path can ever see.
func TestThePlannedModeDoesNotConsultTheOldReaders(t *testing.T) {
	memory := &countingMemories{}
	episodes := &countingEpisodes{}
	planned := &recordingPlanned{recall: PlannedMemoryRecall{
		Global: []string{"[global] 偏好: 先结论"}, Topic: []string{"[topic] 漏桶: 澡堂水池"},
		Episodes: []model.EpisodeMemory{plannedEpisode("a", time.Hour)},
	}}
	builder, err := NewContextBuilder(plannedConfig(memoryrecall.ModeModelPlannedV1),
		&fakeHistoryReader{}, nil, nil, memory, nil)
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	builder.UseEpisodeProvider(episodes)
	builder.UsePlannedMemoryProvider(planned)
	window, err := builder.Build(context.Background(), plannedInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	planned.finished = true
	if planned.calls != 1 {
		t.Fatalf("the planned provider ran %d times, expected once", planned.calls)
	}
	if memory.calls != 0 || episodes.calls != 0 {
		t.Fatalf("the old readers ran (memory=%d episode=%d) under %s",
			memory.calls, episodes.calls, memoryrecall.ModeModelPlannedV1)
	}
	if window.Budget.TopicUserMemory == 0 || window.Budget.EpisodeMemory == 0 {
		t.Fatal("the planned result did not reach the assembled window")
	}
}

type countingMemories struct{ calls int }

func (c *countingMemories) RelevantMemories(context.Context, RunContext) (MemoryRecall, error) {
	c.calls++
	return MemoryRecall{Topic: []string{"[topic] 旧路径不该出现"}}, nil
}

type countingEpisodes struct{ calls int }

func (c *countingEpisodes) RelevantEpisodes(context.Context, RunContext) ([]model.EpisodeMemory, error) {
	c.calls++
	return []model.EpisodeMemory{plannedEpisode("legacy", time.Hour)}, nil
}

// The failure is closed and it is not the turn's failure. The learner keeps the
// current conversation, the summary and the knowledge base; only personalisation
// is lost.
func TestAPlannedFailureCostsMemoryAndNotTheTurn(t *testing.T) {
	planned := &recordingPlanned{err: errors.New("planner unavailable")}
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, SessionID: "s-now", IsUser: true, Content: "上一轮我问了什么"},
		{ID: 2, SessionID: "s-now", IsUser: false, Content: "上一轮的回答"},
	}}
	builder, err := NewContextBuilder(plannedConfig(memoryrecall.ModeModelPlannedV1),
		history, staticTaskState("任务状态"), nil, nil, staticSummary("会话摘要"))
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	builder.UsePlannedMemoryProvider(planned)
	window, err := builder.Build(context.Background(), plannedInput())
	if err != nil {
		t.Fatalf("a memory outage must not fail the turn: %v", err)
	}
	if window.Budget.TopicUserMemory != 0 || window.Budget.EpisodeMemory != 0 || window.Budget.GlobalUserMemory != 0 {
		t.Fatal("a failed planned read must inject no long-term memory at all")
	}
	if window.Budget.Summary == 0 || window.Budget.RecentMessages == 0 || window.Budget.TaskState == 0 {
		t.Fatal("the rest of the context must survive a memory outage")
	}
}

// A builder configured for the planned mode with no planned provider is a wiring
// error. It fails loudly rather than serving turns with no long-term memory under
// a mode chosen for its memory behaviour.
func TestThePlannedModeRefusesToRunWithoutItsProvider(t *testing.T) {
	builder, err := NewContextBuilder(plannedConfig(memoryrecall.ModeModelPlannedV1),
		&fakeHistoryReader{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	if _, err := builder.Build(context.Background(), plannedInput()); err == nil {
		t.Fatal("the planned mode must refuse to build without a planned provider")
	}
}

// The episode ceiling is the context builder's, unchanged. The planned path may
// propose more; it may not raise the budget.
func TestThePlannedModeKeepsTheThreeEpisodeCeiling(t *testing.T) {
	proposed := []model.EpisodeMemory{}
	for index := 0; index < 8; index++ {
		proposed = append(proposed, plannedEpisode(fmt.Sprintf("%d", index), time.Duration(index)*time.Hour))
	}
	planned := &recordingPlanned{recall: PlannedMemoryRecall{Episodes: proposed}}
	builder, err := NewContextBuilder(plannedConfig(memoryrecall.ModeModelPlannedV1),
		&fakeHistoryReader{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	builder.UsePlannedMemoryProvider(planned)
	window, err := builder.Build(context.Background(), plannedInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(window.EpisodeIDs) > 3 {
		t.Fatalf("%d episodes were injected, the ceiling is 3", len(window.EpisodeIDs))
	}
	if window.Budget.EpisodeMemory > DefaultContextConfig().MaxEpisodeTokens {
		t.Fatalf("the episode block used %d tokens, over the %d budget",
			window.Budget.EpisodeMemory, DefaultContextConfig().MaxEpisodeTokens)
	}
}

// Everything that is not long-term memory is byte-identical to what legacy_v2
// assembles from the same inputs. If the two differed, an answer-level result
// could not be attributed to the memory block.
func TestNonMemoryContextIsByteIdenticalToLegacy(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 1, SessionID: "s-now", IsUser: true, Content: "第一句"},
		{ID: 2, SessionID: "s-now", IsUser: false, Content: "第二句"},
		{ID: 3, SessionID: "s-now", IsUser: true, Content: "第三句"},
	}}
	build := func(mode memoryrecall.ReadMode, attach func(*DatabaseContextBuilder)) ContextWindow {
		reader := &fakeHistoryReader{messages: history.messages}
		builder, err := NewContextBuilder(plannedConfig(mode), reader,
			staticTaskState("当前任务：复习"), nil, nil, staticSummary("这是会话摘要"))
		if err != nil {
			t.Fatalf("builder: %v", err)
		}
		attach(builder)
		window, err := builder.Build(context.Background(), plannedInput())
		if err != nil {
			t.Fatalf("build under %s: %v", mode, err)
		}
		return window
	}
	legacy := build(memoryrecall.ModeLegacyV2, func(*DatabaseContextBuilder) {})
	planned := build(memoryrecall.ModeModelPlannedV1, func(builder *DatabaseContextBuilder) {
		builder.UsePlannedMemoryProvider(&recordingPlanned{})
	})
	if len(legacy.Messages) != len(planned.Messages) {
		t.Fatalf("legacy assembled %d messages and the planned mode %d",
			len(legacy.Messages), len(planned.Messages))
	}
	for index := range legacy.Messages {
		if legacy.Messages[index].Content != planned.Messages[index].Content {
			t.Fatalf("message %d differs between the modes:\nlegacy:  %q\nplanned: %q",
				index, legacy.Messages[index].Content, planned.Messages[index].Content)
		}
		if legacy.Messages[index].Role != planned.Messages[index].Role {
			t.Fatalf("message %d has a different role between the modes", index)
		}
	}
	if legacy.Budget != planned.Budget {
		t.Fatalf("the budget breakdown differs:\nlegacy:  %+v\nplanned: %+v", legacy.Budget, planned.Budget)
	}
}

// The budgets the design refuses to change are the ones the mode inherits.
func TestThePlannedModeChangesNoBudget(t *testing.T) {
	shipped := DefaultContextConfig()
	planned := plannedConfig(memoryrecall.ModeModelPlannedV1)
	if planned.MaxGlobalUserMemoryTokens != shipped.MaxGlobalUserMemoryTokens ||
		planned.MaxTopicUserMemoryTokens != shipped.MaxTopicUserMemoryTokens ||
		planned.MaxEpisodeTokens != shipped.MaxEpisodeTokens ||
		planned.MaxSummaryTokens != shipped.MaxSummaryTokens ||
		planned.MaxMasteryTokens != shipped.MaxMasteryTokens ||
		planned.InputTargetTokens != shipped.InputTargetTokens {
		t.Fatal("the planned mode must inherit every shipped budget unchanged")
	}
	if err := planned.Validate(); err != nil {
		t.Fatalf("the planned mode must be a valid configuration: %v", err)
	}
}

// The two blocks keep their own budgets and their own order. Global is injected
// before topic, exactly as it was, so a mode change cannot silently reorder the
// prompt.
func TestGlobalStillPrecedesTopicUnderThePlannedMode(t *testing.T) {
	planned := &recordingPlanned{recall: PlannedMemoryRecall{
		Global: []string{"[global; tags=preference] 顺序: 先结论"},
		Topic:  []string{"[topic; tags=backend; direct personal evidence] 漏桶: 澡堂水池"},
	}}
	builder, err := NewContextBuilder(plannedConfig(memoryrecall.ModeModelPlannedV1),
		&fakeHistoryReader{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	builder.UsePlannedMemoryProvider(planned)
	window, err := builder.Build(context.Background(), plannedInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	globalAt, topicAt := -1, -1
	for index, message := range window.Messages {
		if strings.HasPrefix(message.Content, "Global user memories") {
			globalAt = index
		}
		if strings.HasPrefix(message.Content, "Topic user memories") {
			topicAt = index
		}
	}
	if globalAt < 0 || topicAt < 0 {
		t.Fatal("both memory blocks must be present")
	}
	if globalAt > topicAt {
		t.Fatal("global memory must still be injected before topic memory")
	}
}

// Nothing the planner or the verifier said reaches what a client can see. The
// window carries numbers and ids for internal use; the catalogue, the plan and
// the model's raw output never appear in an assembled message.
func TestPlannerOutputNeverReachesTheAssembledMessages(t *testing.T) {
	planned := &recordingPlanned{recall: PlannedMemoryRecall{
		Topic:    []string{"[topic; tags=backend; direct personal evidence] 漏桶: 澡堂水池"},
		Episodes: []model.EpisodeMemory{plannedEpisode("a", time.Hour)},
	}}
	builder, err := NewContextBuilder(plannedConfig(memoryrecall.ModeModelPlannedV1),
		&fakeHistoryReader{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("builder: %v", err)
	}
	builder.UsePlannedMemoryProvider(planned)
	window, err := builder.Build(context.Background(), plannedInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, message := range window.Messages {
		for _, leak := range []string{
			"memory.read-plan", "memory.verdict", "select_memory", "judge_candidates",
			"schema_version", "candidate_id", "plan_hash", "catalogue_version",
		} {
			if strings.Contains(message.Content, leak) {
				t.Fatalf("the assembled context leaked %q", leak)
			}
		}
	}
}
