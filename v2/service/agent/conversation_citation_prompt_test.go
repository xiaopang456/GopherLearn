package agent

import (
	"context"
	"strings"
	"testing"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
)

// The rule that stops a model from borrowing the document citation syntax for
// its own list has to reach both windows of the layered comparison.
//
// The two arms differ in what the window carries - the board and the summary
// segments on one side, the rolling summary on the other - and the prompt is
// assembled per turn from the mode's text plus whatever contract the blocks
// need. A rule that reached only one of them would make the arms two
// different methods, and the comparison between them would be measuring the
// prompt rather than the memory.
//
// So the test builds one window of each shape and asks the same question of
// both: is the rule in the system message the model is sent?
func TestTheConversationCitationRuleIsInBothWindows(t *testing.T) {
	history := &fakeHistoryReader{messages: []model.Message{
		{ID: 91, IsUser: true, Content: "刚才那个时长"},
	}}
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
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	layered := buildLayeredWindow(t, reader, history, nil, preselector)

	builder, err := NewContextBuilder(layeredTestConfig(), history, staticTaskState(""), nil,
		staticMemories{}, EmptySummaryProvider{})
	if err != nil {
		t.Fatalf("build context builder: %v", err)
	}
	truncated, err := builder.Build(context.Background(), RunContext{
		UserID: 7, SessionID: "session", Question: "我一开始定的学习时长是多少",
	})
	if err != nil {
		t.Fatalf("build window: %v", err)
	}

	for _, testCase := range []struct {
		name   string
		window ContextWindow
	}{
		{name: "layered", window: layered},
		{name: "truncated", window: truncated},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			system := testCase.window.Messages[0].Content
			for _, required := range []string{
				"write no bracketed numbers",
				"message_ids=...",
			} {
				if !strings.Contains(system, required) {
					t.Fatalf("the system prompt is missing %q", required)
				}
			}
		})
	}

	// The two windows are the two shapes, or the loop above would be asserting
	// one window twice. The layered arm's prompt carries the contract the
	// board and the segments are read under; the other arm's does not.
	if !strings.Contains(layered.Messages[0].Content, "Trust order (when two sources disagree") {
		t.Fatal("the layered window does not carry the layered contract")
	}
	if strings.Contains(truncated.Messages[0].Content, "Trust order (when two sources disagree") {
		t.Fatal("the window without the layered blocks carries the layered contract")
	}
}

// The rule names no tool, so the renderer keeps it in a turn that has no
// read-back tool at all.
//
// The renderer drops a rule that talks about a capability this turn does not
// have, because a rule about a tool the model cannot see is a rule it cannot
// obey. A rule that names none is kept - and this one has to be, because the
// turn that answers from the conversation is exactly the turn that may have
// no history tool.
func TestTheConversationCitationRuleSurvivesATurnWithoutTheHistoryTool(t *testing.T) {
	system := RenderSystemPrompt(SystemPromptFor(memoryrecall.DefaultReadMode()), PromptCapabilities{})
	for _, required := range []string{
		"write no bracketed numbers",
		"message_ids=...",
	} {
		if !strings.Contains(system, required) {
			t.Fatalf("the rendered prompt is missing %q", required)
		}
	}
}
