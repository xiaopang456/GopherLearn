package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// The capability ceiling closes the learner's own state. It does not close the
// conversation's own record: the board and the segments say what this session
// decided, and a turn that switched the library off is still a turn in this
// session. An ordinary conversation is exactly that turn - a legacy chat
// session resolves to knowledge_disabled - so if the ceiling reached the
// board and the segments, ordinary chat would lose the only context it has.
//
// The pairing is what makes this test worth having. Both policies run through
// the same wiring and both halves are asserted: the switched-off turn has to
// keep the conversation's blocks and lose the personal ones, and the control
// has to show the personal readers being reached at all. Either half alone
// passes on a wiring that does nothing.
func TestTheConversationBlocksSurviveATurnThatSwitchedKnowledgeOff(t *testing.T) {
	for _, tc := range []struct {
		name          string
		policy        rag.KnowledgePolicy
		personalReads bool
	}{
		{name: "switched off", policy: rag.KnowledgeDisabled},
		{name: "switched on", policy: rag.KnowledgePreferred, personalReads: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &fakeSegmentReader{
				board: testBoard(t, 60, []model.SessionWhiteboardEntry{
					{Key: "duration", Text: "每次总时长 = 48 分钟", Value: "48 分钟", SourceMessageIDs: []uint{45}},
				}),
				watermark: 90,
				segments: []model.SessionSummarySegment{
					testSegment(t, 1, 1, 30, "学习时长调整", []model.SessionSummarySegmentPoint{
						{Text: "每次学习总时长定为 30 分钟", EvidenceMessageIDs: []uint{12}},
					}, nil),
					testSegment(t, 2, 31, 60, "RPC 拆包", []model.SessionSummarySegmentPoint{
						{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{45}},
					}, nil),
					testSegment(t, 3, 61, 90, "学习时长调整", []model.SessionSummarySegmentPoint{
						{Text: "总时长改为 48 分钟", EvidenceMessageIDs: []uint{75}},
					}, []model.SessionSummarySegmentChange{{Key: "duration", OldValue: "30 分钟", NewValue: "48 分钟", NewMessageIDs: []uint{75}}}),
				},
			}
			history := &fakeHistoryReader{messages: []model.Message{
				{ID: 91, IsUser: true, Content: "刚才那个时长"},
			}}
			tasks, mastery, memories := &countedTaskProvider{}, &countedMasteryProvider{}, &countedMemoryProvider{}
			config := layeredTestConfig()
			builder, err := NewContextBuilder(config, history, tasks, mastery, memories, EmptySummaryProvider{})
			if err != nil {
				t.Fatalf("build context builder: %v", err)
			}
			preselector, err := NewSummarySegmentPreselector(nil, nil)
			if err != nil {
				t.Fatalf("build preselector: %v", err)
			}
			provider, err := NewDatabaseLayeredSummaryProvider(reader, preselector, 2)
			if err != nil {
				t.Fatalf("build layered provider: %v", err)
			}
			if err := builder.UseLayeredSummaryProvider(provider); err != nil {
				t.Fatalf("attach layered provider: %v", err)
			}

			window, err := builder.Build(context.Background(), RunContext{
				UserID: 7, SessionID: "session", Question: "我一开始定的学习时长是多少",
				KnowledgePolicy: tc.policy,
			})
			if err != nil {
				t.Fatalf("build window: %v", err)
			}

			// The conversation's own blocks, under either policy.
			shown := renderWindowForTest(window)
			for _, want := range []string{
				"Current state (server state;",
				"Summary segments (compressed record",
				"每次总时长 = 48 分钟",
				"总时长改为 48 分钟",
			} {
				if !strings.Contains(shown, want) {
					t.Fatalf("%s: %q did not reach the window:\n%s", tc.name, want, shown)
				}
			}
			if window.Budget.Whiteboard == 0 {
				t.Fatalf("%s: the board arrived but was not charged: %+v", tc.name, window.Budget)
			}
			if window.Budget.ResidentSegments == 0 {
				t.Fatalf("%s: the resident segments arrived but were not charged: %+v", tc.name, window.Budget)
			}

			// The personal half, under the switch.
			counts := map[string]int32{
				"task state": tasks.calls.Load(), "mastery": mastery.calls.Load(),
				"long-term memory": memories.calls.Load(),
			}
			if !tc.personalReads {
				for what, got := range counts {
					if got != 0 {
						t.Fatalf("%s: the switched-off turn read %s %d times", tc.name, what, got)
					}
				}
				for _, leaked := range []string{"复习 TCP", "掌握度 42", "偏好带例子"} {
					if strings.Contains(shown, leaked) {
						t.Fatalf("%s: the switched-off turn rendered %q into the window", tc.name, leaked)
					}
				}
				// No evidence reserve is held back for tools this turn cannot
				// have. That room is what the board and the segments are paid
				// out of on an ordinary conversation.
				if window.Budget.EvidenceReserved != 0 {
					t.Fatalf("%s: reserved %d tokens of evidence for a turn that cannot search",
						tc.name, window.Budget.EvidenceReserved)
				}
				return
			}
			for what, got := range counts {
				if got == 0 {
					t.Fatalf("%s: the control read no %s, so the zeros above prove nothing", tc.name, what)
				}
			}
			if window.Budget.EvidenceReserved != config.EvidenceReserve {
				t.Fatalf("%s: evidence reserved %d, want %d", tc.name, window.Budget.EvidenceReserved, config.EvidenceReserve)
			}
		})
	}
}
