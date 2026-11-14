package conversation

// The whiteboard around a batch, and the authorisation that came out of it.
//
// Two questions are asked here, and they are asked about the same run.
//
// The first is what the board looked like around each batch: what it held
// before the batch wrote into it and what it held afterwards. Without the
// two snapshots side by side, a reader cannot say whether a value the model
// proposed reached the board - and the counters cannot say it either, because
// a counter of applied entries does not say which one.
//
// The second is where an authorised message came from. The product grants a
// message under one kind whether the board confirmed it or only kept the
// learner's words findable, so the split has to be made where the grant is
// read: a message the board confirmed and a message the board only
// preserved are different facts, and a reader who cannot tell them apart
// cannot tell a confirmed value from a preserved one.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"GopherAI/config"
	"GopherAI/model"
	agent "GopherAI/service/agent"
)

// The board around a batch is kept.
//
// The board before the batch and the board after it are two facts about two
// moments, and a run that keeps only the second one cannot say what a batch
// did to the board.
func TestTheBoardAroundTheBatchIsKept(t *testing.T) {
	recorder := &recordingChatModel{inner: &scriptedModel{bodies: []string{evidenceProposal()}}}
	generator := evidenceGenerator(t, recorder)
	outcome := batchOutcome{}
	evidence := newBatchEvidenceCollector(config.AgentConfig{SummarySegmentDebugDump: true}, "run-1")

	if _, err := runSegmentBatchAttempts(context.Background(), generator, recorder,
		evidenceInput(), 1, 1, 0, &outcome, evidence, model.SessionWhiteboardDocument{}); err != nil {
		t.Fatalf("run the batch: %v", err)
	}
	records := evidence.set().Records
	if len(records) != 1 {
		t.Fatalf("the run kept %d records, want 1", len(records))
	}
	record := records[0]
	if record.WhiteboardBefore == nil {
		t.Fatal("the record keeps no board before the batch")
	}
	if record.WhiteboardAfter == nil {
		t.Fatal("the record keeps no board after the batch")
	}
	if len(record.WhiteboardBefore.Constraints) != 0 {
		t.Fatalf("the board before the batch holds %+v, want nothing",
			record.WhiteboardBefore.Constraints)
	}
	// The entry of the proposal was refused, so the board after the batch
	// holds no entry either - but it holds the learner's own words, and the
	// snapshot says so apart from the entries.
	if len(record.WhiteboardAfter.Constraints) != 0 {
		t.Fatalf("the board after the batch holds %+v, want no entry",
			record.WhiteboardAfter.Constraints)
	}
	if len(record.WhiteboardAfter.Anchors) != 1 {
		t.Fatalf("the board after the batch carries %+v, want the learner's own words",
			record.WhiteboardAfter.Anchors)
	}
	anchor := record.WhiteboardAfter.Anchors[0]
	if anchor.MessageID != 95 {
		t.Fatalf("the statement names message %d, want the learner's own message",
			anchor.MessageID)
	}
	if anchor.Reason == "" {
		t.Fatalf("the statement carries no reason: %+v", anchor)
	}
}

// An authorised message says which card drew the authorisation.
//
// The board draws one kind of authorisation for two different things: the
// message an entry holds and the message an unconfirmed statement keeps
// findable. The summary segment draws a third. A reader has to be able to
// tell them apart, because a message the board confirmed is a value the board
// holds and a message the board only preserved is a value the board refused.
func TestTheAuthorisationSaysWhichCardDrewIt(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			{Key: "words_per_day", Text: "背单词每天 30 个", Value: "30 个/天", SourceMessageIDs: []uint{96}},
		},
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "words_per_day", Value: "45 个/天", MessageID: 95, Reason: model.SessionWhiteboardAnchorQuoteNotInMessage},
		},
	}
	grants := []agent.HistoryMessageGrant{
		{MessageID: 95, SourceKind: agent.HistorySourceWhiteboard, SourceSessionID: "session-1"},
		{MessageID: 96, SourceKind: agent.HistorySourceWhiteboard, SourceSessionID: "session-1"},
		{MessageID: 97, SourceKind: agent.HistorySourceSummarySegment, SourceSessionID: "session-1"},
	}
	_ = board
	window := agent.ContextWindow{WhiteboardAnchorMessageIDs: []uint{95}, WhiteboardEntryMessageIDs: []uint{96}}

	origins := grantOrigins(grants, window)
	if len(origins[grantOriginWhiteboardAnchor]) != 1 ||
		origins[grantOriginWhiteboardAnchor][0] != 95 {
		t.Fatalf("the whiteboard anchor's messages = %v, want the one the statement kept",
			origins[grantOriginWhiteboardAnchor])
	}
	if len(origins[grantOriginWhiteboardEntry]) != 1 ||
		origins[grantOriginWhiteboardEntry][0] != 96 {
		t.Fatalf("the whiteboard entry's messages = %v, want the one the entry holds",
			origins[grantOriginWhiteboardEntry])
	}
	if len(origins[grantOriginSummarySegment]) != 1 ||
		origins[grantOriginSummarySegment][0] != 97 {
		t.Fatalf("the summary segment's messages = %v, want the one the segment quoted",
			origins[grantOriginSummarySegment])
	}
}

// A message the board keeps under an entry and a message the board keeps
// under a statement are two different messages.
//
// They are both the board's, and the product grants both under one kind. The
// split is the one that says whether the board confirmed a value or only
// preserved the learner's words.
func TestAMessageUnderAnEntryAndAMessageUnderAStatementAreToldApart(t *testing.T) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV3,
		Constraints: []model.SessionWhiteboardEntry{
			{Key: "words_per_day", Text: "背单词每天 30 个", Value: "30 个/天", SourceMessageIDs: []uint{96}},
		},
		Unverified: []model.SessionWhiteboardAnchor{
			{Key: "words_per_day", Value: "45 个/天", MessageID: 95, Reason: model.SessionWhiteboardAnchorQuoteNotInMessage},
		},
	}
	grants := []agent.HistoryMessageGrant{
		{MessageID: 95, SourceKind: agent.HistorySourceWhiteboard, SourceSessionID: "session-1"},
		{MessageID: 96, SourceKind: agent.HistorySourceWhiteboard, SourceSessionID: "session-1"},
	}
	origins := grantOrigins(grants, agent.ContextWindow{WhiteboardAnchorMessageIDs: []uint{board.Unverified[0].MessageID}, WhiteboardEntryMessageIDs: board.Constraints[0].SourceMessageIDs})
	for _, origin := range []string{grantOriginWhiteboardAnchor, grantOriginWhiteboardEntry} {
		if len(origins[origin]) != 1 {
			t.Fatalf("%s carries %v, want one message", origin, origins[origin])
		}
	}
	if origins[grantOriginWhiteboardAnchor][0] == origins[grantOriginWhiteboardEntry][0] {
		t.Fatal("the statement and the entry are the same message")
	}
}

// emptyOriginHistory keeps the test focused on the persisted board.
type emptyOriginHistory struct{}

func (emptyOriginHistory) ListRecentAfter(context.Context, string, uint, int) ([]model.Message, error) {
	return nil, nil
}

func TestFinalReadFileReportsOnlyRenderedBoardOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, entryText       string
		wantEntry, wantAnchor bool
	}{
		{"both", "short", true, true}, {"entry only", "short", true, false}, {"anchor only", strings.Repeat("long ", 100), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			board := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3, Constraints: []model.SessionWhiteboardEntry{{Key: "k", Text: tc.entryText, Value: "v", SourceMessageIDs: []uint{7}}}, Unverified: []model.SessionWhiteboardAnchor{{Key: "k", Value: "v", MessageID: 7, Reason: model.SessionWhiteboardAnchorNoClaim}}}
			provider := &stubLayeredProvider{snapshot: agent.LayeredSummarySnapshot{Board: &model.SessionWhiteboard{Version: 1, CoveredMessageID: 7}, BoardDocument: board, CoveredMessageID: 7}}
			found := false
			for budget := 1; budget < 500; budget++ {
				cfg := agent.DefaultContextConfig()
				cfg.MaxWhiteboardTokens = budget
				cfg.MinRecentMessages = 0
				builder, err := agent.NewContextBuilder(cfg, emptyOriginHistory{}, nil, nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = builder.UseLayeredSummaryProvider(provider); err != nil {
					t.Fatal(err)
				}
				run := agent.RunContext{UserID: 1, SessionID: "s", Question: "hello"}
				window, err := builder.Build(context.Background(), run)
				if err != nil {
					t.Fatal(err)
				}
				if (len(window.WhiteboardEntryMessageIDs) > 0) != tc.wantEntry || (len(window.WhiteboardAnchorMessageIDs) > 0) != tc.wantAnchor {
					continue
				}
				grants, set := windowGrantSet(window, run)
				if !set.Covers(7, "s") {
					t.Fatal("not authorised")
				}
				read := LayeredRead{GrantOrigins: grantOrigins(grants, window), PersistedWhiteboard: whiteboardSnapshotOf(board), DisplayedWhiteboardEntryKeys: window.WhiteboardEntryKeys, DisplayedWhiteboardAnchorMessageIDs: window.WhiteboardAnchorMessageIDs}
				annotatePersistedVisibility(read.PersistedWhiteboard, window)
				dir := t.TempDir()
				if err = WriteLayeredReadFiles(dir, read); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(dir, "read.json"))
				if err != nil {
					t.Fatal(err)
				}
				var decoded LayeredRead
				if err = json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if (len(decoded.GrantOrigins[grantOriginWhiteboardEntry]) > 0) != tc.wantEntry || (len(decoded.GrantOrigins[grantOriginWhiteboardAnchor]) > 0) != tc.wantAnchor {
					t.Fatalf("origins=%v", decoded.GrantOrigins)
				}
				if decoded.PersistedWhiteboard == nil || len(decoded.PersistedWhiteboard.Constraints) != 1 || len(decoded.PersistedWhiteboard.Anchors) != 1 {
					t.Fatal("persisted snapshot lost")
				}
				if *decoded.PersistedWhiteboard.Constraints[0].Displayed != tc.wantEntry || *decoded.PersistedWhiteboard.Anchors[0].Displayed != tc.wantAnchor {
					t.Fatal("display status differs from window")
				}
				found = true
				break
			}
			if !found {
				t.Fatal("no budget created requested case")
			}
		})
	}
}
