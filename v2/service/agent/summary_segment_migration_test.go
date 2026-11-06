package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
)

// The lazy conversion of an old rolling summary.
//
// A session that was summarised by the old path has a row in the old table
// and nothing in the new ones. The conversion gives it the same shape as a
// new one, without a migration script and without a model call.

// fakeLegacyReader is the old summary's row.
type fakeLegacyReader struct {
	summary *model.SessionSummary
	err     error
}

func (f *fakeLegacyReader) GetSummary(context.Context, int64, string) (*model.SessionSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.summary, nil
}

// fakeMigrationWriter records what the conversion wrote.
type fakeMigrationWriter struct {
	segment *model.SessionSummarySegment
	board   *model.SessionWhiteboard
	err     error
	// onWrite lets a test make the store reflect the write, the way the real
	// one does.
	onWrite func(*model.SessionSummarySegment, *model.SessionWhiteboard)
}

func (f *fakeMigrationWriter) SaveSummarySegmentAndWhiteboard(_ context.Context,
	segment *model.SessionSummarySegment, board *model.SessionWhiteboard, _ uint64) error {
	if f.err != nil {
		return f.err
	}
	f.segment = segment
	f.board = board
	if f.onWrite != nil {
		f.onWrite(segment, board)
	}
	return nil
}

// legacyDocument is an old summary with one item in every group.
func legacyDocument() model.SessionSummaryDocument {
	return model.SessionSummaryDocument{
		CurrentGoal: "把 RPC 拆包学明白",
		ConfirmedDecisions: model.SessionSummaryEvidenceList{
			{Text: "决定使用长度字段拆包", EvidenceMessageIDs: []uint{12}},
		},
		CompletedItems: []string{"读完了长度前缀"},
		OpenLoops: model.SessionSummaryEvidenceList{
			{Text: "半包还没验证", EvidenceMessageIDs: []uint{20}},
		},
		UserCorrections: model.SessionSummaryEvidenceList{
			{Text: "不是粘包，是半包", EvidenceMessageIDs: []uint{30}},
		},
		KeyErrors:  []string{"把粘包和半包搞混了"},
		References: []string{"Go  net 包文档"},
	}
}

// The seven groups go where they belong.
//
// The board holds what is true now; the segment holds everything as text,
// because that is the only place the session's progress is still answerable.
func TestTheConversionPutsTheOldGroupsWhereTheyBelong(t *testing.T) {
	board, segment, ok := ConvertLegacySummary(legacyDocument(), 40)
	if !ok {
		t.Fatal("the document was not converted")
	}
	if board.Goal == nil || board.Goal.Text != "把 RPC 拆包学明白" {
		t.Fatalf("the goal is %+v", board.Goal)
	}
	if board.Goal.UpdatedAtMessageID != 40 {
		t.Fatalf("the goal's message is %d, want the covered message", board.Goal.UpdatedAtMessageID)
	}
	if len(board.Constraints) != 2 {
		t.Fatalf("the board holds %d constraints, want the decision and the correction", len(board.Constraints))
	}
	if board.Constraints[0].Key != "decision.1" || board.Constraints[1].Key != "correction.1" {
		t.Fatalf("the constraint keys are %q and %q", board.Constraints[0].Key, board.Constraints[1].Key)
	}
	if len(board.Constraints[0].SourceMessageIDs) != 1 || board.Constraints[0].SourceMessageIDs[0] != 12 {
		t.Fatalf("the decision's evidence is %v", board.Constraints[0].SourceMessageIDs)
	}
	if len(board.Plan) != 1 || board.Plan[0].State != "done" {
		t.Fatalf("the plan is %+v", board.Plan)
	}
	if len(board.OpenItems) != 2 {
		t.Fatalf("the board holds %d open items, want the loop and the error", len(board.OpenItems))
	}
	if board.OpenItems[1].Key != "error.1" {
		t.Fatalf("the error's key is %q", board.OpenItems[1].Key)
	}
	if len(segment.Points) != 6 {
		t.Fatalf("the segment holds %d points, want one per text group", len(segment.Points))
	}
	if len(segment.Names) != 1 || segment.Names[0] != "Go  net 包文档" {
		t.Fatalf("the segment's names are %v", segment.Names)
	}
	if segment.Topic != "历史摘要" {
		t.Fatalf("the segment's topic is %q", segment.Topic)
	}
}

// The references do not go on the board.
//
// A reference is a document the session touched. It is progress inside the
// session, not a fact about it, and the board is where the facts are.
func TestTheReferencesStayOutOfTheBoard(t *testing.T) {
	board, _, ok := ConvertLegacySummary(legacyDocument(), 40)
	if !ok {
		t.Fatal("the document was not converted")
	}
	for _, entry := range append(append(board.Constraints, board.Plan...), board.OpenItems...) {
		if entry.Text == "Go  net 包文档" {
			t.Fatal("a reference reached the board")
		}
	}
}

// The groups are trimmed to what the new tables accept.
//
// A converted session with less in it is still a session that works, and one
// that cannot be written is a session that does not.
func TestTheConversionTrimsToWhatTheTablesAccept(t *testing.T) {
	document := model.SessionSummaryDocument{CurrentGoal: "目标"}
	for index := 0; index < 40; index++ {
		document.ConfirmedDecisions = append(document.ConfirmedDecisions,
			model.SessionSummaryEvidence{Text: "决定", EvidenceMessageIDs: []uint{uint(index + 1)}})
	}
	board, segment, ok := ConvertLegacySummary(document, 40)
	if !ok {
		t.Fatal("the document was not converted")
	}
	if len(board.Constraints) > model.SessionWhiteboardItemLimit {
		t.Fatalf("the board holds %d constraints, the limit is %d",
			len(board.Constraints), model.SessionWhiteboardItemLimit)
	}
	if len(segment.Points) > model.SessionSummarySegmentPointLimit {
		t.Fatalf("the segment holds %d points, the limit is %d",
			len(segment.Points), model.SessionSummarySegmentPointLimit)
	}
}

// A session the old path summarised is converted when it is read.
func TestTheMigratorConvertsASessionTheOldPathSummarised(t *testing.T) {
	reader := &fakeSegmentReader{}
	legacy := &fakeLegacyReader{summary: &model.SessionSummary{
		UserID: 7, SessionID: "session", Content: mustMarshalSummary(t, legacyDocument()), CoveredMessageID: 40,
	}}
	writer := &fakeMigrationWriter{}
	migrator, err := NewSummarySegmentMigrator(reader, legacy, writer)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := migrator.Migrate(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !converted {
		t.Fatal("the session was not converted")
	}
	if writer.segment == nil || writer.board == nil {
		t.Fatal("nothing was written")
	}
	if writer.segment.Seq != 1 || writer.segment.StartMessageID != 0 || writer.segment.EndMessageID != 40 {
		t.Fatalf("the segment covers %d..%d at seq %d",
			writer.segment.StartMessageID, writer.segment.EndMessageID, writer.segment.Seq)
	}
	if writer.board.CoveredMessageID != 40 {
		t.Fatalf("the board covers %d, want 40", writer.board.CoveredMessageID)
	}
}

// A session that already has a board is left alone.
//
// The new path owns it now, and a conversion would overwrite what the
// background wrote.
func TestTheMigratorLeavesASessionThatAlreadyHasABoardAlone(t *testing.T) {
	reader := &fakeSegmentReader{board: testBoard(t, 60, nil)}
	legacy := &fakeLegacyReader{summary: &model.SessionSummary{
		UserID: 7, SessionID: "session", Content: mustMarshalSummary(t, legacyDocument()), CoveredMessageID: 40,
	}}
	writer := &fakeMigrationWriter{}
	migrator, err := NewSummarySegmentMigrator(reader, legacy, writer)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := migrator.Migrate(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if converted {
		t.Fatal("a session with a board was converted")
	}
	if writer.segment != nil {
		t.Fatal("something was written to a session that already has a board")
	}
}

// A session the old path never summarised is not converted.
func TestTheMigratorLeavesASessionWithoutAnOldSummaryAlone(t *testing.T) {
	reader := &fakeSegmentReader{}
	legacy := &fakeLegacyReader{err: sessiondao.ErrSummaryNotFound}
	writer := &fakeMigrationWriter{}
	migrator, err := NewSummarySegmentMigrator(reader, legacy, writer)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := migrator.Migrate(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if converted {
		t.Fatal("a session without an old summary was converted")
	}
}

// A summary that cannot be read is not converted, and the turn goes on.
//
// The session keeps the conversation it has, and the background writes its
// first segment when it gets to it.
func TestTheMigratorLeavesAnUnreadableSummaryAlone(t *testing.T) {
	reader := &fakeSegmentReader{}
	legacy := &fakeLegacyReader{summary: &model.SessionSummary{
		UserID: 7, SessionID: "session", Content: "这不是 JSON", CoveredMessageID: 40,
	}}
	writer := &fakeMigrationWriter{}
	migrator, err := NewSummarySegmentMigrator(reader, legacy, writer)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := migrator.Migrate(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if converted {
		t.Fatal("an unreadable summary was converted")
	}
	if writer.segment != nil {
		t.Fatal("something was written for an unreadable summary")
	}
}

// A write that loses the race is not an error.
//
// The writer that won holds the board, and that board is the one that counts.
func TestTheMigratorAcceptsALostRace(t *testing.T) {
	reader := &fakeSegmentReader{}
	legacy := &fakeLegacyReader{summary: &model.SessionSummary{
		UserID: 7, SessionID: "session", Content: mustMarshalSummary(t, legacyDocument()), CoveredMessageID: 40,
	}}
	writer := &fakeMigrationWriter{err: ErrSummarySegmentWhiteboardConflict}
	migrator, err := NewSummarySegmentMigrator(reader, legacy, writer)
	if err != nil {
		t.Fatal(err)
	}
	converted, err := migrator.Migrate(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if converted {
		t.Fatal("a lost race was reported as a conversion")
	}
}

// mustMarshalSummary encodes an old summary document.
func mustMarshalSummary(t *testing.T, document model.SessionSummaryDocument) string {
	t.Helper()
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode summary: %v", err)
	}
	return string(content)
}

// The provider falls back to the raw conversation when there is nothing to
// read.
//
// That is the path every session takes before the background has written its
// first segment, and the path an old session takes when its summary cannot be
// converted.
func TestTheProviderFallsBackToTheRawConversation(t *testing.T) {
	reader := &fakeSegmentReader{watermark: 0}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, nil, DefaultResidentSummarySegments)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := provider.LayeredSummary(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("layered summary: %v", err)
	}
	if snapshot.Board != nil || len(snapshot.Resident) != 0 || len(snapshot.Retrieved) != 0 {
		t.Fatalf("the snapshot carries state: board=%v resident=%d", snapshot.Board, len(snapshot.Resident))
	}
	if snapshot.CoveredMessageID != 0 {
		t.Fatalf("the conversation starts after %d, want nothing covered", snapshot.CoveredMessageID)
	}
}

// The migration is not an error when it cannot run.
func TestTheMigrationFailureIsNotAFailureOfTheTurn(t *testing.T) {
	reader := &fakeSegmentReader{watermark: 0}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, nil, DefaultResidentSummarySegments)
	if err != nil {
		t.Fatal(err)
	}
	provider.UseMigrator(failingMigration{})
	snapshot, err := provider.LayeredSummary(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("layered summary: %v", err)
	}
	if snapshot.CoveredMessageID != 0 {
		t.Fatalf("the conversation starts after %d", snapshot.CoveredMessageID)
	}
}

// failingMigration is a conversion that could not run.
type failingMigration struct{}

func (failingMigration) Migrate(context.Context, RunContext) (bool, error) {
	return false, errors.New("the store was unreachable")
}

// An old session keeps working.
//
// It is a session the old path summarised, and the applied mode reads the new
// state. The conversion gives it a board and a first segment on the first
// turn, so the turn does not have to send the whole conversation and does not
// have to wait for the background.
func TestAnOldSessionKeepsWorking(t *testing.T) {
	reader := &fakeSegmentReader{watermark: 0}
	legacy := &fakeLegacyReader{summary: &model.SessionSummary{
		UserID: 7, SessionID: "session", Content: mustMarshalSummary(t, legacyDocument()), CoveredMessageID: 40,
	}}
	writer := &fakeMigrationWriter{}
	writer.onWrite = func(segment *model.SessionSummarySegment, board *model.SessionWhiteboard) {
		reader.segments = []model.SessionSummarySegment{*segment}
		reader.board = &model.SessionWhiteboard{
			UserID: 7, SessionID: "session", Content: board.Content,
			CoveredMessageID: board.CoveredMessageID, Version: 1,
		}
		reader.watermark = segment.EndMessageID
	}
	migrator, err := NewSummarySegmentMigrator(reader, legacy, writer)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewDatabaseLayeredSummaryProvider(reader, nil, DefaultResidentSummarySegments)
	if err != nil {
		t.Fatal(err)
	}
	provider.UseMigrator(migrator)
	snapshot, err := provider.LayeredSummary(context.Background(),
		RunContext{UserID: 7, SessionID: "session", Question: "拆包"})
	if err != nil {
		t.Fatalf("layered summary: %v", err)
	}
	if snapshot.Board == nil {
		t.Fatal("the converted session has no board")
	}
	if snapshot.BoardDocument.Goal == nil || snapshot.BoardDocument.Goal.Text != "把 RPC 拆包学明白" {
		t.Fatalf("the board's goal is %+v", snapshot.BoardDocument.Goal)
	}
	if len(snapshot.Resident) != 1 || snapshot.Resident[0].Seq != 1 {
		t.Fatalf("the session holds %d resident segments", len(snapshot.Resident))
	}
	if snapshot.CoveredMessageID != 40 {
		t.Fatalf("the conversation starts after %d, want 40", snapshot.CoveredMessageID)
	}
}
