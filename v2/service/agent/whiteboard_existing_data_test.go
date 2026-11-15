package agent

// What a board already holds is not rewritten by a mode change.
//
// A board that was written in version 2 or version 3 keeps its version and
// keeps the origins it holds. The mode decides what a new proposal may put
// on the board; it does not decide what the board already says.
//
// The two questions were the same question for a while, and the board was
// the worse for it: a deployment that turned the mode off wrote a version 1
// document over a version 2 one, and an entry the learner had taken on lost
// the saying of it and became the learner's own.

import (
	"testing"

	"GopherAI/model"
)

// oldBoardWithOrigin is a board that already holds an entry the learner took
// a proposal of the assistant's on.
//
// A version 1 board could only hold what the learner said, so it holds the
// learner's own entry alone.
func oldBoardWithOrigin(version int) model.SessionWhiteboardDocument {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: version,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 52),
		},
	}
	if version >= model.SessionWhiteboardSchemaV2 {
		board.Plan = []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 51),
		}
	}
	return board
}

// The version a board was written in is the version it keeps.
//
// A version 2 board written by a deployment that once recorded the
// assistant's products keeps version 2 when the mode is turned off: the
// origins in it are the learner's own words, and a version 1 document
// written over it would drop them without trace.
func TestAVersion2BoardKeepsItsVersionWhenTheModeIsOff(t *testing.T) {
	current := oldBoardWithOrigin(model.SessionWhiteboardSchemaV2)
	batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
		},
	}, batch, learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV2 {
		t.Fatalf("the board is written in version %d, want the version it already had (%d)",
			result.Board.SchemaVersion, model.SessionWhiteboardSchemaV2)
	}
	if got := boardValues(result.Board)["pages"]; got != "41 页/次" {
		t.Fatalf("the board holds %q, want the learner's new value", got)
	}
}

// The same holds for a version 3 board, and the statements it carries are
// the learner's own words: they stay.
func TestAVersion3BoardKeepsItsVersionAndItsStatementsWhenTheModeIsOff(t *testing.T) {
	current := oldBoardWithOrigin(model.SessionWhiteboardSchemaV3)
	current.Unverified = []model.SessionWhiteboardAnchor{
		{
			Key: "pages", Value: "45 页/次", MessageID: 93,
			Text: "改成 45 页/次怎么样", Complete: true,
			Reason: model.SessionWhiteboardAnchorCitationInvalid, UpdatedAtMessageID: 93,
		},
	}
	batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
		},
	}, batch, learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV3 {
		t.Fatalf("the board is written in version %d, want version %d",
			result.Board.SchemaVersion, model.SessionWhiteboardSchemaV3)
	}
	if len(result.Board.Unverified) != 1 {
		t.Fatalf("the board carries %d statements, want the one it had", len(result.Board.Unverified))
	}
	if result.Board.Unverified[0].MessageID != 93 {
		t.Fatalf("the statement points at message %d, want 93", result.Board.Unverified[0].MessageID)
	}
}

// A deployment that never turned the mode on keeps the document it always
// wrote.
func TestAVersion1BoardStaysVersion1WhenTheModeIsOff(t *testing.T) {
	current := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1,
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 26 页", "26 页/次", "", 52),
		},
	}
	batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
		},
	}, batch, learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV1 {
		t.Fatalf("the board is written in version %d, want version %d",
			result.Board.SchemaVersion, model.SessionWhiteboardSchemaV1)
	}
}

// The version moves forward when the mode asks for a newer one.
func TestTheModeThatRecordsTheProductsWritesVersion3OverAVersion2(t *testing.T) {
	current := oldBoardWithOrigin(model.SessionWhiteboardSchemaV2)
	batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if result.Board.SchemaVersion != model.SessionWhiteboardSchemaV3 {
		t.Fatalf("the board is written in version %d, want version %d",
			result.Board.SchemaVersion, model.SessionWhiteboardSchemaV3)
	}
}

// An entry the proposal did not mention keeps the saying of it.
//
// The learner took a proposal of the assistant's on, and the board says so.
// Turning the mode off stops new products of the assistant from reaching the
// board; it does not rewrite the entries that are already there.
func TestAnUnchangedAcceptedEntryKeepsItsOriginWhenTheModeIsOff(t *testing.T) {
	current := oldBoardWithOrigin(model.SessionWhiteboardSchemaV2)
	batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, batch,
		learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Plan) != 1 {
		t.Fatalf("the plan holds %d entries, want the one it had", len(result.Board.Plan))
	}
	entry := result.Board.Plan[0]
	if entry.EntryOrigin() != model.SessionWhiteboardOriginAssistantAccepted {
		t.Fatalf("the entry's origin = %q, want the accepted proposal's", entry.EntryOrigin())
	}
	if entry.Value != "三道" {
		t.Fatalf("the entry holds %q, want the value it had", entry.Value)
	}
}

// The same entry keeps its origin when the mode is on, and it keeps its
// message as well: the reader has to be able to go back to it.
func TestAnUnchangedAcceptedEntryKeepsItsOriginWhenTheModeIsOn(t *testing.T) {
	current := oldBoardWithOrigin(model.SessionWhiteboardSchemaV3)
	batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{}, batch,
		assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Plan) != 1 {
		t.Fatalf("the plan holds %d entries, want the one it had", len(result.Board.Plan))
	}
	entry := result.Board.Plan[0]
	if entry.EntryOrigin() != model.SessionWhiteboardOriginAssistantAccepted {
		t.Fatalf("the entry's origin = %q, want the accepted proposal's", entry.EntryOrigin())
	}
	if !whiteboardCitesID(entry.SourceMessageIDs, 51) {
		t.Fatalf("the entry cites %v, want the message it came from", entry.SourceMessageIDs)
	}
}

// Keeping what the board holds is a different thing from taking what a new
// proposal brings.
//
// The mode that does not record the assistant's products does not take a
// proposal of the assistant's on, even a proven one.
func TestTheModeThatIsOffDoesNotTakeANewAcceptedProposal(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1}
	batch := withLearnerMessage(
		assistantBatch(90, "给我三道动态规划的练习", 91, "1. 最长递增子序列\n2. 0-1 背包\n3. 区间 DP 入门"),
		92, "好，就按这三道来")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 91, 92),
		},
	}, batch, learnerOnlyPolicy(model.SessionWhiteboardSchemaV1))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 0 {
		t.Fatalf("the board holds %+v, want the proposal of the assistant's left off it",
			result.Board.Constraints)
	}
	if result.Diagnostics.AssistantProposals != 1 {
		t.Fatalf("the diagnostics count %d proposals of the assistant's, want 1",
			result.Diagnostics.AssistantProposals)
	}
}

// The mode that does record them takes it on, with both messages cited.
func TestTheModeThatIsOnStillTakesAnAcceptedProposal(t *testing.T) {
	current := model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV3}
	batch := withLearnerMessage(
		assistantBatch(90, "给我三道动态规划的练习", 91, "1. 最长递增子序列\n2. 0-1 背包\n3. 区间 DP 入门"),
		92, "好，就按这三道来")
	result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
		Constraints: []model.SessionWhiteboardEntry{
			ownEntry("practice_plan", "按这三道练习来", "三道", model.SessionWhiteboardOriginAssistantAccepted, 91, 92),
		},
	}, batch, assistantProductsPolicy(model.SessionWhiteboardSchemaV3))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Board.Constraints) != 1 {
		t.Fatalf("the board holds %d entries, want the accepted proposal on it",
			len(result.Board.Constraints))
	}
	entry := result.Board.Constraints[0]
	if entry.EntryOrigin() != model.SessionWhiteboardOriginAssistantAccepted {
		t.Fatalf("the entry's origin = %q, want the accepted proposal's", entry.EntryOrigin())
	}
	if !whiteboardCitesID(entry.SourceMessageIDs, 91) || !whiteboardCitesID(entry.SourceMessageIDs, 92) {
		t.Fatalf("the entry cites %v, want the proposal and the message that took it on",
			entry.SourceMessageIDs)
	}
}

// The learner's own words reach an old board in either mode, and they reach
// it the same way.
//
// The mode is about what the assistant produced. It is not about the
// learner's own facts, and a board that was written before the mode was
// turned on is not a board the learner's new value cannot reach.
func TestTheLearnersOwnUpdateReachesAnOldBoardInEveryMode(t *testing.T) {
	versions := []int{
		model.SessionWhiteboardSchemaV1,
		model.SessionWhiteboardSchemaV2,
		model.SessionWhiteboardSchemaV3,
	}
	policies := []struct {
		name   string
		policy WhiteboardPolicy
	}{
		{"the mode that records the assistant's products", assistantProductsPolicy(model.SessionWhiteboardSchemaV3)},
		{"the mode that does not", learnerOnlyPolicy(model.SessionWhiteboardSchemaV1)},
	}
	for _, version := range versions {
		for _, policy := range policies {
			current := oldBoardWithOrigin(version)
			batch := learnerBatch(map[uint]string{95: "改成每次 41 页吧"}, 95)
			result, err := ApplyWhiteboardProposal(current, WhiteboardProposal{
				Constraints: []model.SessionWhiteboardEntry{
					ownEntry("pages", "每次读 41 页", "41 页/次", "", 95),
				},
			}, batch, policy.policy)
			if err != nil {
				t.Fatalf("version %d, %s: %v", version, policy.name, err)
			}
			if got := boardValues(result.Board)["pages"]; got != "41 页/次" {
				t.Fatalf("version %d, %s: the board holds %q, want the learner's new value",
					version, policy.name, got)
			}
			if result.Board.SchemaVersion < version {
				t.Fatalf("version %d, %s: the board was written in version %d",
					version, policy.name, result.Board.SchemaVersion)
			}
		}
	}
}
