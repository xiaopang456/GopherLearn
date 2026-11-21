package isolated

import (
	"context"
	"testing"

	"GopherAI/model"
	agentService "GopherAI/service/agent"

	"github.com/cloudwego/eino/schema"
)

// The thin card path.
//
// It replaces the Episode path and nothing else: the conversation's own
// board and summary segments still travel with every turn, and the
// cross-session tools are still there for the turns that need them.
//
// Both facts are asserted here because both are invisible from the outside:
// a path that quietly stopped assembling the current session's summary would
// still answer questions, and a path that quietly dropped the cross-session
// tools would still answer questions - just the wrong ones.

// thinCardSettings is the shipped configuration with the thin card on.
func thinCardSettings(t *testing.T) *Settings {
	t.Helper()
	s := testSettings(t)
	s.Config.AgentConfig.SessionCardMode = "thin"
	s.Config.AgentConfig.SummarySegmentMode = "on"
	s.Config.AgentConfig.SessionCardThinIndexName = "gopherai_thin_card_test_idx"
	s.Config.AgentConfig.SessionCardThinIndexKeyPrefix = "gopherai:thin_card:test:"
	return s
}

// The current conversation's own state still reaches the window under the
// thin card.
//
// The thin card is a way of finding an EARLIER conversation. It is not a
// replacement for the current one, and a turn that stopped assembling the
// board and the segments would leave the model with the raw messages only -
// which is the state the whole summary path exists to avoid.
func TestTheThinCardPathStillAssemblesTheCurrentConversationsState(t *testing.T) {
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-thin-window")

	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	mustSeedLayeredState(t, db, row.UserID, session)

	window, err := app.Context.Build(context.Background(), agentService.RunContext{
		UserID: row.UserID, SessionID: session, Question: "我最初每天读多少页",
	})
	if err != nil {
		t.Fatalf("build the window: %v", err)
	}
	var board, segments bool
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		if len(message.Content) >= len("Current state") && message.Content[:len("Current state")] == "Current state" {
			board = true
		}
		if len(message.Content) >= len("Summary segments") && message.Content[:len("Summary segments")] == "Summary segments" {
			segments = true
		}
	}
	if !board {
		t.Fatalf("the board did not reach the window under the thin card (%d messages)", len(window.Messages))
	}
	if !segments {
		t.Fatalf("the segments did not reach the window under the thin card (%d messages)", len(window.Messages))
	}
}

// The cross-session tools are on the table under the thin card.
//
// A thin card that no tool can open is a card nobody ever finds, and the
// turn that needed it would have nothing to offer the learner.
func TestTheThinCardPathOffersTheCrossSessionTools(t *testing.T) {
	s := thinCardSettings(t)
	app, db, _, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
		return schema.AssistantMessage("你好", nil), nil
	})
	session := createSession(t, app, token, "create-thin-tools")

	var row model.Session
	if err := db.Where("id = ?", session).First(&row).Error; err != nil {
		t.Fatalf("read the session: %v", err)
	}
	names, err := app.Runner.ToolNames(context.Background(), agentService.RunContext{
		RunID: "run-thin-tools", UserID: row.UserID, SessionID: session,
		Question: "上次你给我排的那三道题里，第二道练的是什么？",
	})
	if err != nil {
		t.Fatalf("read the tool table: %v", err)
	}
	for _, wanted := range []string{"search_session_cards", "search_history_segments",
		"read_session_whiteboard", "ask_which_conversation", "read_history_message"} {
		found := false
		for _, name := range names {
			if name == wanted {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the tool %q is not on the table under the thin card: %v", wanted, names)
		}
	}
}
