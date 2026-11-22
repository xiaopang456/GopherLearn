package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// The decision that one refused card does not throw the whole preparation
// away is exercised here WITHOUT a product, a database and a model.
//
// The decision is the one that decides whether the run survives a refused
// card, and it is the decision that a reader of the report has to be able to
// trust. It used to live inside the real preparation, where it could only be
// reached with a paid provider behind it - which means it had no regression
// guard at all: anyone could turn "one refusal stops everything" back on and
// no test would have said so.

// TestOneRefusedConversationDoesNotStopThePreparation says that a refused
// card costs the reader that one conversation and nothing else.
func TestOneRefusedConversationDoesNotStopThePreparation(t *testing.T) {
	conversations := []string{"s-1", "s-2", "s-3"}
	projection := &ArmProjection{Mode: CardArmModeOn, MemoryKind: string(model.EpisodeContentSessionCard),
		Sessions: conversations, Targets: map[string]uint{}}
	visited := []string{}
	publish := func(_ context.Context, sessionID string) armStep {
		visited = append(visited, sessionID)
		if sessionID == "s-2" {
			return armStep{published: true, due: true, target: 42, waited: 7,
				code: "card_source_boundary",
				err:  errors.New("the card names a message the conversation does not hold")}
		}
		return armStep{published: true, due: true, target: 10, waited: 3}
	}
	followed := []string{}
	followUp := func(_ context.Context, sessionID string, _ uint) error {
		followed = append(followed, sessionID)
		return nil
	}
	if err := driveArmProjection(context.Background(), projection, conversations, publish, followUp); err != nil {
		t.Fatalf("one refused card must not stop the preparation: %v", err)
	}
	if len(visited) != len(conversations) {
		t.Fatalf("every conversation must be published: visited %v", visited)
	}
	if len(followed) != 2 {
		t.Fatalf("the refused conversation must not be read back: followed %v", followed)
	}
	if len(projection.Failures) != 1 {
		t.Fatalf("exactly one failure must be recorded: %+v", projection.Failures)
	}
	failure := projection.Failures[0]
	if failure.SessionID != "s-2" {
		t.Fatalf("the failure must name the conversation that was refused: %+v", failure)
	}
	if failure.ErrorCode != "card_source_boundary" {
		t.Fatalf("the failure must carry the job's own code: %+v", failure)
	}
	if failure.TargetMessageID != 42 {
		t.Fatalf("the failure must say how far the refused card was asked to reach: %+v", failure)
	}
	if projection.Published != 3 || projection.Due != 3 {
		t.Fatalf("the refused card was published and due as well: %+v", projection)
	}
	if projection.Targets["s-2"] != 42 || projection.Targets["s-3"] != 10 {
		t.Fatalf("every conversation's target must be recorded: %+v", projection.Targets)
	}
}

// TestEveryRefusedConversationStopsThePreparation says that tolerance does
// not become blindness: a preparation that produced no memory at all is not
// a measurement.
func TestEveryRefusedConversationStopsThePreparation(t *testing.T) {
	conversations := []string{"s-1", "s-2"}
	projection := &ArmProjection{Mode: CardArmModeOn, MemoryKind: string(model.EpisodeContentSessionCard),
		Sessions: conversations, Targets: map[string]uint{}}
	publish := func(_ context.Context, _ string) armStep {
		return armStep{published: true, due: true, target: 11, code: "card_source_boundary",
			err: errors.New("the card names a message the conversation does not hold")}
	}
	err := driveArmProjection(context.Background(), projection, conversations, publish, nil)
	if err == nil {
		t.Fatal("a preparation without a single memory must stop")
	}
	if !strings.Contains(err.Error(), "every conversation's memory was refused") {
		t.Fatalf("the stop must say why: %v", err)
	}
	if projection.Error != "every conversation's memory was refused" {
		t.Fatalf("the projection must carry the reason: %q", projection.Error)
	}
	if len(projection.Failures) != len(conversations) {
		t.Fatalf("every refusal must be recorded: %+v", projection.Failures)
	}
}

// TestThePreparationStopsWhenItsOwnCardInputCannotBeRead says that a
// preparation which cannot say what the card was written from is not a
// measurement either.
func TestThePreparationStopsWhenItsOwnCardInputCannotBeRead(t *testing.T) {
	projection := &ArmProjection{Mode: CardArmModeOn, MemoryKind: string(model.EpisodeContentSessionCard),
		Sessions: []string{"s-1"}, Targets: map[string]uint{}}
	publish := func(_ context.Context, _ string) armStep {
		return armStep{published: true, due: true, target: 11}
	}
	followUp := func(_ context.Context, _ string, _ uint) error {
		return errors.New("the card input could not be read")
	}
	err := driveArmProjection(context.Background(), projection, []string{"s-1"}, publish, followUp)
	if err == nil || !strings.Contains(err.Error(), "the card input could not be read") {
		t.Fatalf("the failure must travel out: %v", err)
	}
}

// TestTheFailingPreparationKeepsItsProjectionCallsAndUsage says that the
// failing exit of a preparation keeps everything the successful one keeps.
//
// The projection, the calls it cost and the usage it made are the evidence of
// what the preparation did. A preparation that failed and threw them away
// would leave a reader with a refusal and no way to judge it.
func TestTheFailingPreparationKeepsItsProjectionCallsAndUsage(t *testing.T) {
	projection := &ArmProjection{Mode: CardArmModeOn, MemoryKind: string(model.EpisodeContentSessionCard),
		Sessions: []string{"s-1", "s-2"}, Targets: map[string]uint{},
		Failures: []ArmFailure{{SessionID: "s-2", ErrorCode: "card_source_boundary",
			Error: "the card names a message the conversation does not hold", TargetMessageID: 42}}}
	report := &WarmupReport{Calls: []ModelCall{{ID: 1, Role: "summary", Method: "generate"}}}
	snapshot := &Snapshot{}
	projectionCalls := []ModelCall{{ID: 2, Role: "background", Method: "generate",
		Usage: &schema.TokenUsage{TotalTokens: 12}}}
	if err := attachProjectionEvidence(report, snapshot, projection, projectionCalls); err != nil {
		t.Fatalf("the usage must be readable: %v", err)
	}
	if report.Projection == nil || len(report.Projection.Failures) != 1 {
		t.Fatalf("the projection must stay on the report: %+v", report.Projection)
	}
	if len(report.Calls) != 2 {
		t.Fatalf("the projection's calls must stay on the report: %d", len(report.Calls))
	}
	if len(report.UsageByRole) == 0 {
		t.Fatal("the usage by role must stay on the report")
	}
	if snapshot.Projection == nil {
		t.Fatal("the archive must carry the preparation's record")
	}
	if len(snapshot.Projection.Failures) != 1 ||
		snapshot.Projection.Failures[0].SessionID != "s-2" ||
		snapshot.Projection.Failures[0].ErrorCode != "card_source_boundary" {
		t.Fatalf("the archive must carry the refused conversations: %+v", snapshot.Projection)
	}
}

// TestTheQuerySeesTheRefusedConversations says that a query which reuses the
// archive is told which conversations the preparation could not write.
//
// A query that quietly dropped them would report a smaller comparison than
// the one that was actually run, and the items of those conversations would
// disappear with them.
func TestTheQuerySeesTheRefusedConversations(t *testing.T) {
	snapshot := Snapshot{
		Sessions: []SessionSnapshot{{Session: model.Session{ID: "s-1", UserID: 7, UserName: "learner"}}},
		Projection: &ArmProjection{Mode: CardArmModeOn, MemoryKind: string(model.EpisodeContentSessionCard),
			Targets: map[string]uint{},
			Failures: []ArmFailure{{SessionID: "s-2", ErrorCode: "card_source_boundary",
				Error: "the card names a message the conversation does not hold", TargetMessageID: 42}}},
	}
	arm := &CardArm{Mode: CardArmModeOn, IndexName: "cards", KeyPrefix: "cards:"}
	evidence := armEvidenceFromSnapshot(snapshot, arm)
	if evidence == nil {
		t.Fatal("the evidence must be built")
	}
	if len(evidence.Failures) != 1 {
		t.Fatalf("the refused conversations must reach the query: %+v", evidence.Failures)
	}
	if evidence.Failures[0].SessionID != "s-2" ||
		evidence.Failures[0].ErrorCode != "card_source_boundary" ||
		evidence.Failures[0].TargetMessageID != 42 {
		t.Fatalf("the refusal must reach the query whole: %+v", evidence.Failures[0])
	}
}

// TestTheCardBodiesAreWrittenToThePrivateDirectory says that the card
// generations' own bodies are kept: they are the one thing the ledger's
// hashes cannot reconstruct.
//
// The file is written before the sandbox is closed, it is private, and it is
// never overwritten: a second write would destroy the first one's evidence.
func TestTheCardBodiesAreWrittenToThePrivateDirectory(t *testing.T) {
	dir := t.TempDir()
	exchanges := []CardExchange{{SessionID: "s-1", TargetMessageID: 42,
		Request: `{"prompt":"write the card"}`, Response: `{"card":"refused"}`, HTTPStatus: 200}}
	if err := WriteCardEvidence(dir, exchanges); err != nil {
		t.Fatalf("the bodies must be written: %v", err)
	}
	path := filepath.Join(dir, CardEvidenceFileName)
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the bodies must be on disk: %v", err)
	}
	var read []CardExchange
	if err := json.Unmarshal(encoded, &read); err != nil {
		t.Fatalf("the bodies must be readable: %v", err)
	}
	if len(read) != 1 || read[0].SessionID != "s-1" || read[0].Request != exchanges[0].Request ||
		read[0].Response != exchanges[0].Response {
		t.Fatalf("the bodies must be kept whole: %+v", read)
	}
	// The file is private: it holds the raw bodies of a paid run.
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("the bodies must be on disk: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("the bodies must be private: %v", info.Mode().Perm())
	}
	// A second write must not destroy the first one's evidence.
	if err := WriteCardEvidence(dir, exchanges); err == nil {
		t.Fatal("the bodies must never be overwritten")
	}
	// A phase that looked for the bodies and found none says so.
	empty := t.TempDir()
	if err := WriteCardEvidence(empty, nil); err != nil {
		t.Fatalf("an empty finding must still be written: %v", err)
	}
	encoded, err = os.ReadFile(filepath.Join(empty, CardEvidenceFileName))
	if err != nil {
		t.Fatalf("an empty finding must be on disk: %v", err)
	}
	if strings.TrimSpace(string(encoded)) != "[]" {
		t.Fatalf("an empty finding must read as an empty list: %s", encoded)
	}
	// A run without a private directory writes nothing at all.
	if err := WriteCardEvidence("", exchanges); err != nil {
		t.Fatalf("a run without a directory must not fail: %v", err)
	}
}
