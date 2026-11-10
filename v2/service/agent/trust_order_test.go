package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/model"
)

// The trust order decides which of two contradicting sources is right.
//
// It names the sources the window carries, and only those: a rule that points
// at a block the model cannot see is a rule the model cannot obey, and a rule
// that leaves out a block it can see leaves it to guess.

// trustOrderLevels reads the numbered levels out of a rendered trust order.
func trustOrderLevels(t *testing.T, prompt string) []string {
	t.Helper()
	levels := make([]string, 0, 5)
	for _, line := range strings.Split(prompt, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || !strings.HasPrefix(trimmed, "Trust order") {
			continue
		}
		for _, candidate := range strings.Split(prompt, "\n") {
			candidate = strings.TrimSpace(candidate)
			index := strings.Index(candidate, ". ")
			if index <= 0 {
				continue
			}
			number := candidate[:index]
			if number == "1" || number == "2" || number == "3" || number == "4" || number == "5" {
				levels = append(levels, candidate[index+2:])
			}
		}
		break
	}
	return levels
}

func TestTheTrustOrderNamesOnlyTheSourcesTheWindowCarries(t *testing.T) {
	cases := []struct {
		name   string
		blocks TrustOrderBlocks
		want   []string
	}{
		{
			name:   "the raw conversation alone",
			blocks: TrustOrderBlocks{RecentMessages: true},
			want:   []string{trustOrderRecentMessages},
		},
		{
			name: "the session's own blocks",
			blocks: TrustOrderBlocks{
				RecentMessages: true, CurrentState: true, SummarySegments: true,
			},
			want: []string{
				trustOrderRecentMessages, trustOrderCurrentState, trustOrderSummarySegments,
			},
		},
		{
			name: "the long-term defaults",
			blocks: TrustOrderBlocks{
				RecentMessages: true, LongTermMemory: true,
			},
			want: []string{trustOrderRecentMessages, trustOrderLongTermMemory},
		},
		{
			name: "the earlier conversations",
			blocks: TrustOrderBlocks{
				RecentMessages: true, EpisodeMemory: true,
			},
			want: []string{trustOrderRecentMessages, trustOrderEpisodeMemory},
		},
		{
			name:   "every source at once",
			blocks: TrustOrderBlocks{RecentMessages: true, CurrentState: true, SummarySegments: true, LongTermMemory: true, EpisodeMemory: true},
			want: []string{
				trustOrderRecentMessages, trustOrderCurrentState, trustOrderSummarySegments,
				trustOrderLongTermMemory, trustOrderEpisodeMemory,
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rendered := renderTrustOrder(testCase.blocks)
			levels := trustOrderLevels(t, rendered)
			if len(levels) != len(testCase.want) {
				t.Fatalf("levels = %v, want %v", levels, testCase.want)
			}
			for index, want := range testCase.want {
				if levels[index] != want {
					t.Fatalf("level %d = %q, want %q", index+1, levels[index], want)
				}
			}
		})
	}
}

// The levels are numbered from one without gaps.
//
// A gap would read as a level that was skipped rather than one that does not
// exist, and the model would not know which of the two it was looking at.

func TestTheTrustOrderIsNumberedWithoutGaps(t *testing.T) {
	rendered := renderTrustOrder(TrustOrderBlocks{RecentMessages: true, EpisodeMemory: true})
	if !strings.Contains(rendered, "\n1. "+trustOrderRecentMessages) {
		t.Fatalf("the first level is not numbered one: %q", rendered)
	}
	if !strings.Contains(rendered, "\n2. "+trustOrderEpisodeMemory) {
		t.Fatalf("the second level is not numbered two: %q", rendered)
	}
	if strings.Contains(rendered, "\n3. ") {
		t.Fatalf("the order has a level that does not exist: %q", rendered)
	}
}

// The profile and the topic notes are defaults, not facts about this
// session, and the rule that settles a conflict between them and this
// session is written only when they are in the window.

func TestTheLongTermDefaultRuleFollowsTheProfileBlock(t *testing.T) {
	withProfile := renderTrustOrder(TrustOrderBlocks{RecentMessages: true, LongTermMemory: true})
	if !strings.Contains(withProfile, "long-term defaults, recorded on an earlier day") {
		t.Fatalf("the long-term default rule is missing: %q", withProfile)
	}
	if !strings.Contains(withProfile, "this session wins") {
		t.Fatalf("the long-term default rule does not say which one wins: %q", withProfile)
	}
	withoutProfile := renderTrustOrder(TrustOrderBlocks{RecentMessages: true})
	if strings.Contains(withoutProfile, "long-term defaults") {
		t.Fatalf("the long-term default rule is written without the profile: %q", withoutProfile)
	}
}

// The window is what decides the trust order.
//
// A turn that carries the board, the segments and the learner's topic notes
// writes four levels and the rule that settles a conflict with the profile;
// a turn that carries none of them writes only the raw conversation.

func TestTheLayeredWindowWritesTheTrustOrderForItsOwnBlocks(t *testing.T) {
	reader := &fakeSegmentReader{
		board: testBoard(t, 60, []model.SessionWhiteboardEntry{
			{Key: "duration", Text: "每次总时长 = 48 分钟", Value: "48 分钟", SourceMessageIDs: []uint{45}},
		}),
		watermark: 90,
		segments: []model.SessionSummarySegment{
			testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
				{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
			}, nil),
		},
	}
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 91, IsUser: true, Content: "现在改成 48 分钟"},
	}}
	builder, err := NewContextBuilder(layeredTestConfig(), history, staticTaskState(""), nil,
		staticMemories{"讲解偏好：先说结论"}, EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build context builder: %v", err)
	}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, nil, 2)
	if err != nil {
		t.Fatalf("build layered provider: %v", err)
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		t.Fatalf("attach layered provider: %v", err)
	}
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "现在每次学习多久",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}
	prompt := window.Messages[0].Content
	levels := trustOrderLevels(t, prompt)
	want := []string{
		trustOrderRecentMessages, trustOrderCurrentState, trustOrderSummarySegments,
		trustOrderLongTermMemory,
	}
	if len(levels) != len(want) {
		t.Fatalf("levels = %v, want %v", levels, want)
	}
	for index, level := range want {
		if levels[index] != level {
			t.Fatalf("level %d = %q, want %q", index+1, levels[index], level)
		}
	}
	if !strings.Contains(prompt, "long-term defaults, recorded on an earlier day") {
		t.Fatalf("the window carries the profile but not its rule: %q", prompt)
	}
	// The episodes are not in this window, so they are not in the order.
	if strings.Contains(prompt, trustOrderEpisodeMemory) {
		t.Fatalf("the window names a source it does not carry: %q", prompt)
	}
}

// A turn with no long-term memory in it says nothing about the profile.
//
// The rule is not harmless when the block is absent: it would tell the model
// to arbitrate between the session and a block it cannot see.

func TestTheLayeredWindowWithoutLongTermMemorySaysNothingAboutTheProfile(t *testing.T) {
	reader := &fakeSegmentReader{
		board: testBoard(t, 60, []model.SessionWhiteboardEntry{
			{Key: "duration", Text: "每次总时长 = 48 分钟", Value: "48 分钟", SourceMessageIDs: []uint{45}},
		}),
		watermark: 90,
	}
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 91, IsUser: true, Content: "现在改成 48 分钟"},
	}}
	builder, err := NewContextBuilder(layeredTestConfig(), history, staticTaskState(""), nil,
		staticMemories{}, EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build context builder: %v", err)
	}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, nil, 2)
	if err != nil {
		t.Fatalf("build layered provider: %v", err)
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		t.Fatalf("attach layered provider: %v", err)
	}
	window, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "现在每次学习多久",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}
	prompt := window.Messages[0].Content
	if strings.Contains(prompt, trustOrderLongTermMemory) {
		t.Fatalf("the window names the profile without carrying it: %q", prompt)
	}
	if strings.Contains(prompt, "long-term defaults") {
		t.Fatalf("the window carries the profile rule without the profile: %q", prompt)
	}
	levels := trustOrderLevels(t, prompt)
	want := []string{trustOrderRecentMessages, trustOrderCurrentState}
	if len(levels) != len(want) {
		t.Fatalf("levels = %v, want %v", levels, want)
	}
}
