package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"GopherAI/model"
)

// The layered summary keeps the newest raw messages in the window, whatever
// its watermark says it covered.
//
// The watermark says how far the segments reach, and the raw conversation
// starts after it. A watermark that reaches the newest message therefore
// leaves the turn with no raw conversation at all: "what did I just say"
// would be answered from a compressed record of it. The floor keeps the
// newest few messages in the learner's own words on every turn, the same
// number the rolling summary's writer keeps unsummarized, and the same
// content being both raw and compressed is the accepted price.
func TestTheNewestRawMessagesSurviveAFullWatermark(t *testing.T) {
	messages := make([]model.Message, 0, 30)
	for index := 1; index <= 30; index++ {
		messages = append(messages, model.Message{ID: uint(index), IsUser: index%2 == 1,
			Content: fmt.Sprintf("第 %d 条：每天读 15 页，材料提前 5 天", index)})
	}
	// The watermark covers every message there is: the segments are the
	// authority, and without a floor the raw conversation would be empty.
	reader := &fakeSegmentReader{
		board:     testBoard(t, 28, nil),
		watermark: 30,
		segments: []model.SessionSummarySegment{
			testSegment(t, 1, 1, 30, "每天的读量", []model.SessionSummarySegmentPoint{
				{Text: "每天读 15 页", EvidenceMessageIDs: []uint{1}},
			}, nil),
		},
	}
	history := &fakeHistoryReader{messages: messages}
	preselector, err := NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		t.Fatalf("build preselector: %v", err)
	}
	window := buildLayeredWindow(t, reader, history, nil, preselector)

	raw := 0
	for _, message := range window.Messages {
		if strings.Contains(message.Content, "第 ") && strings.Contains(message.Content, " 条") {
			raw++
		}
	}
	if raw == 0 {
		t.Fatal("the newest raw messages did not survive a watermark that covers them")
	}
	if raw > layeredTestConfig().MinRecentMessages {
		t.Fatalf("the window carries %d raw messages; the floor is %d and the watermark covered the rest",
			raw, layeredTestConfig().MinRecentMessages)
	}
	// The floor is the newest ones, not the oldest.
	sawNewest, sawOldest := false, false
	for _, message := range window.Messages {
		if strings.Contains(message.Content, "第 30 条") {
			sawNewest = true
		}
		if strings.Contains(message.Content, "第 1 条") {
			sawOldest = true
		}
	}
	if !sawNewest || sawOldest {
		t.Fatalf("the floor kept the wrong end: newest=%t oldest=%t", sawNewest, sawOldest)
	}
	// And the budget says what the guarantee cost.
	if window.Budget.RecentMessageFloor == 0 {
		t.Fatal("the floor was not reported in the budget")
	}
	if window.Budget.RecentMessages < window.Budget.RecentMessageFloor {
		t.Fatalf("the raw messages cost %d and the floor guaranteed %d; the blocks ate the floor",
			window.Budget.RecentMessages, window.Budget.RecentMessageFloor)
	}
}

// The floor is a reservation against the compressed blocks, not a spend
// before them: the blocks spend what is left after the floor was promised,
// so a turn that cannot hold both keeps the learner's own words and shrinks
// the compression instead of the other way round.
func TestTheFloorIsSpentBeforeTheBlocks(t *testing.T) {
	messages := make([]model.Message, 0, 30)
	for index := 1; index <= 30; index++ {
		messages = append(messages, model.Message{ID: uint(index), IsUser: index%2 == 1,
			Content: fmt.Sprintf("第 %d 条：每天读 15 页，材料提前 5 天，编号 B 短号总结", index)})
	}
	// A board big enough that the budget has to choose between it and the
	// raw floor: the limit per category is twenty entries at a dozen tokens
	// each, and the plan category holds all of them.
	entries := make([]model.SessionWhiteboardEntry, 0, 20)
	for index := 0; index < 20; index++ {
		entries = append(entries, model.SessionWhiteboardEntry{
			Key: fmt.Sprintf("k%02d", index), Text: "每天读 15 页，材料提前 5 天", Value: "15 页",
			SourceMessageIDs: []uint{3},
		})
	}
	reader := &fakeSegmentReader{
		board:     testBoard(t, 28, entries),
		watermark: 30,
		segments: []model.SessionSummarySegment{
			testSegment(t, 1, 1, 30, "每天的读量", []model.SessionSummarySegmentPoint{
				{Text: "每天读 15 页", EvidenceMessageIDs: []uint{1}},
			}, nil),
		},
	}
	build := func(t *testing.T, floor int) ContextWindow {
		t.Helper()
		history := &fakeHistoryReader{messages: messages}
		config := layeredTestConfig()
		config.EvidenceReserve = 0
		config.InputTargetTokens = 2200
		config.MinRecentMessages = floor
		builder, err := NewContextBuilder(config, history, staticTaskState(""), nil,
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
			UserID: 7, SessionID: "session", Question: "我一开始定的读量是多少",
		})
		if err != nil {
			t.Fatalf("build window: %v", err)
		}
		return window
	}

	// Without the floor the board takes the room and the raw conversation
	// goes: this is the behaviour the floor exists to prevent, and the numbers
	// above are chosen so it actually happens.
	without := build(t, 0)
	if without.Budget.Whiteboard == 0 || without.Budget.RecentMessages > 0 {
		t.Fatalf("the tight budget did not force a choice (board=%d recent=%d), so the test proves nothing",
			without.Budget.Whiteboard, without.Budget.RecentMessages)
	}
	// With the floor the choice goes the other way: the board is the one that
	// gives, and the newest raw messages are all in the window.
	with := build(t, DefaultMinRecentMessages)
	if with.Budget.RecentMessages < with.Budget.RecentMessageFloor {
		t.Fatalf("the raw messages cost %d and the floor guaranteed %d; the board ate the floor",
			with.Budget.RecentMessages, with.Budget.RecentMessageFloor)
	}
	if with.Budget.Whiteboard >= without.Budget.Whiteboard {
		t.Fatalf("the board spent %d with the floor and %d without; the floor did not reach the board",
			with.Budget.Whiteboard, without.Budget.Whiteboard)
	}
}
