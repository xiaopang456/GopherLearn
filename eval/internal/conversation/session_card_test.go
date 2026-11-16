package conversation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The arm has to say which path it measures and where it writes, or it
// measures nothing: an arm that names no namespace would write into the
// index the other arm reads.
func TestCardArmRefusesAnArmThatWouldMeasureNothing(t *testing.T) {
	if err := (&CardArm{}).Validate(); err == nil {
		t.Fatal("an arm with no mode was accepted")
	}
	if err := (&CardArm{Mode: "partial"}).Validate(); err == nil {
		t.Fatal("a mode the product does not know was accepted")
	}
	if err := (&CardArm{Mode: CardArmModeOn}).Validate(); err == nil {
		t.Fatal("the card arm was accepted without its own namespace")
	}
	if err := (&CardArm{Mode: CardArmModeOn, IndexName: "idx", KeyPrefix: "p:"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&CardArm{Mode: CardArmModeOff}).Validate(); err != nil {
		t.Fatal(err)
	}
	if (&CardArm{Mode: CardArmModeOn, IndexName: "idx", KeyPrefix: "p:"}).Enabled() != true {
		t.Fatal("the card arm did not report itself as enabled")
	}
	if (&CardArm{Mode: CardArmModeOff}).Enabled() {
		t.Fatal("the old arm reported itself as the card path")
	}
}

// The question is asked in another conversation of the same learner. The
// identifier is derived, so that no run can ask its question in the
// conversation the memory came from by accident.
func TestTheQuestionSessionIsAnotherConversation(t *testing.T) {
	source := "synthetic-evaluation"
	question := QuestionSessionID(source)
	if question == source || !strings.HasPrefix(question, "eval-q-") {
		t.Fatalf("the question session %q is not another conversation", question)
	}
	if question != QuestionSessionID(source) {
		t.Fatal("the question session changed between two readings of the same source")
	}
	if question == QuestionSessionID("another-session") {
		t.Fatal("two sources share a question session")
	}
	// The product keeps a session id in a varchar(36).
	if len(question) > 36 {
		t.Fatalf("the question session id %q does not fit the product's column", question)
	}
}

// The counts come from the product's own step record. A number that was
// recomputed from the answer text would be a reading of the text.
func TestTheArmCountsWhatTheAnswerLookedAt(t *testing.T) {
	steps := []model.AgentStep{
		{Type: model.AgentStepRetrieve, SourceIDs: []string{"card-1", "card-1"}},
		{Type: model.AgentStepTool, ToolName: cardToolBrowseSummary,
			ArgumentsSummary: `{"segments":[2,3]}`, ResultSummary: "two segments"},
		{Type: model.AgentStepTool, ToolName: cardToolSearchHistory,
			ArgumentsSummary: `{"query":"rpc"}`, ResultSummary: `[{"message_id":11},{"message_id":12}]`},
		{Type: model.AgentStepTool, ToolName: cardToolReadHistory,
			ArgumentsSummary: `{"message_ids":[12,13]}`, ResultSummary: "ok"},
		{Type: model.AgentStepTool, ToolName: "something_else", ArgumentsSummary: `{"segments":[9]}`},
	}
	evidence := ReadCardArmSteps(steps)
	if len(evidence.Tools) != 4 {
		t.Fatalf("tools=%+v", evidence.Tools)
	}
	browsed, hits, readback := evidence.Browsed, evidence.Hits, evidence.ReadBack
	if len(browsed) != 2 || browsed[0] != 2 || browsed[1] != 3 {
		t.Fatalf("browsed=%+v", browsed)
	}
	if len(hits) != 2 || hits[0] != 11 || hits[1] != 12 {
		t.Fatalf("hits=%+v", hits)
	}
	if len(readback) != 2 || readback[0] != 12 || readback[1] != 13 {
		t.Fatalf("readback=%+v", readback)
	}
}

// The recall of the OLD arm comes from the provenance the product wrote on the
// assistant message, not from the prompt. The old path fills the Episode
// ancestors of the message; the card path does not, because a card has no
// Episode ancestors.
func TestTheOldArmReadsItsRecallFromTheMessageProvenance(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.Session{}, &model.Message{}); err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: "eval-q-1", UserID: 7, UserName: "l4_test", Mode: model.SessionModeAgent}
	if err = db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	message := model.Message{ID: 5, SessionID: "eval-q-1", UserName: "l4_test", IsUser: false,
		Content: "the answer", EpisodeSources: model.EpisodeSourceRefs{{EpisodeID: "episode-1", Version: 1}}}
	if err = db.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	sources, err := ReadMessageEpisodeSources(context.Background(), db, 7, "eval-q-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0] != "episode-1" {
		t.Fatalf("recall=%+v", sources)
	}
	// A session of another learner has no sources to read.
	other := model.Session{ID: "eval-q-2", UserID: 8, UserName: "l4_other", Mode: model.SessionModeAgent}
	if err = db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	sources, err = ReadMessageEpisodeSources(context.Background(), db, 7, "eval-q-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("foreign session accepted: %+v", sources)
	}
}

// The switch goes into the product's own configuration, and the quiet window
// is left alone: the harness's long window is what keeps the product's own
// publications out of the attempt.
func TestApplyCardArmPutsTheSwitchIntoTheProductConfiguration(t *testing.T) {
	repository := filepath.Join("..", "..", "..")
	storage := t.TempDir()
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Config.AgentConfig.SessionCardMode != CardArmModeOff {
		t.Fatal("the product's default is not the old path")
	}
	quiet := settings.Config.AgentConfig.EpisodeQuietWindowSeconds
	arm := &CardArm{Mode: CardArmModeOn, IndexName: "gopherai_session_card_arm_idx", KeyPrefix: "gopherai:session_card:arm:"}
	if err = ApplyCardArm(settings, arm); err != nil {
		t.Fatal(err)
	}
	if settings.Config.AgentConfig.SessionCardMode != CardArmModeOn ||
		settings.Config.AgentConfig.SessionCardIndexName != arm.IndexName ||
		settings.Config.AgentConfig.SessionCardIndexKeyPrefix != arm.KeyPrefix {
		t.Fatalf("the switch did not reach the configuration: %+v", settings.Config.AgentConfig)
	}
	if settings.Config.AgentConfig.EpisodeQuietWindowSeconds != quiet {
		t.Fatal("the arm changed the quiet window")
	}
	if err = settings.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = ApplyCardArm(nil, arm); err == nil {
		t.Fatal("settings were not required")
	}
}

// The whole card arm, end to end, on a real MySQL and a real Redis.
//
// The arm has to write a card, wait for it to be indexed and ask its question
// in another conversation - and the audit has to be able to tell the
// preparation phase from the query phase, or the card's own work would be
// read as a violation.
func TestTheCardArmWritesACardAndAsksInAnotherConversation(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL and Redis images")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repository := filepath.Join("..", "..", "..")
	turns := make([]Turn, 4)
	for i := range turns {
		turns[i] = testTurn(i)
	}
	warmup, err := PrepareOfflineProductReport(ctx, repository, SingleHistory("synthetic-evaluation", turns), AttemptInput{})
	if err != nil {
		t.Fatal(err)
	}
	arm := &CardArm{Mode: CardArmModeOn,
		IndexName: "gopherai_session_card_eval_idx", KeyPrefix: "gopherai:session_card:eval:",
		WaitTimeout: 90 * time.Second}
	result, err := RunOfflineProduct(ctx, repository, warmup.Snapshot, AttemptInput{
		Questions:   []Question{{Text: "我们之前讨论过的那个练习安排是什么？"}},
		SessionCard: arm,
	})
	if err != nil {
		t.Fatalf("the card arm failed: %v; audit=%+v", err, result.Audit)
	}
	if result.SessionCard == nil {
		t.Fatal("the attempt recorded no card evidence")
	}
	evidence := result.SessionCard
	if !evidence.Published {
		t.Fatalf("the projection was not published: %+v", evidence)
	}
	if !evidence.Ready || evidence.Card == nil {
		t.Fatalf("no card was indexed: %+v", evidence)
	}
	if evidence.JobErrorCode != "" {
		t.Fatalf("the card was refused: %s", evidence.JobErrorCode)
	}
	if evidence.SourceSessionID == evidence.QuestionSessionID {
		t.Fatal("the question was asked in the conversation the memory came from")
	}
	if evidence.QuestionSessionID != QuestionSessionID(warmup.Snapshot.Sessions[0].Session.ID) {
		t.Fatalf("the question session %q is not the derived one", evidence.QuestionSessionID)
	}
	if evidence.SourceMessages == 0 || evidence.SourceCharacters == 0 {
		t.Fatalf("the attempt did not record the size of its source: %+v", evidence)
	}
	if evidence.Card.SourceLastMessageID == 0 {
		t.Fatalf("the card does not say how far it reaches: %+v", evidence.Card)
	}
	if !result.BackgroundAudit.Valid {
		t.Fatalf("the audit did not certify the run: %+v", result.Audit)
	}
	if result.Audit.ExecutedInQueryPhase {
		t.Fatal("something was generated in the query phase")
	}
	if result.Audit.Prepared.IsZero() {
		t.Fatal("the preparation phase was not recorded")
	}
	// The card's own call is preparation work, and the answer's call is
	// answer work. The two must not be folded into one another.
	prepareCalls, answerCalls := 0, 0
	for _, call := range result.Calls {
		switch call.Phase {
		case "prepare":
			prepareCalls++
		case "answer":
			answerCalls++
		}
	}
	if prepareCalls == 0 {
		t.Fatalf("the card's own call was not attributed to the preparation phase: %+v", result.Calls)
	}
	if answerCalls == 0 {
		t.Fatalf("the answer's call was not attributed to the answer phase: %+v", result.Calls)
	}
}
