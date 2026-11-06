package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
)

// The lazy conversion of an old rolling summary.
//
// A session that was summarised by the old path has a row in the old table
// and nothing in the new ones. Converting it when the session is read gives
// that session the same shape as a new one - a board and a first segment -
// without a migration script, without a model call, and without waiting for
// the background to catch up.
//
// The conversion is a pure function over the stored document. What the old
// summary said becomes the board's current facts and the first segment's
// text. Nothing is inferred: a group the old summary did not have becomes an
// empty group, not a guess.
//
// A document this schema cannot read is not converted. The session then goes
// on with the raw conversation, which is what it had before, and the
// background writes its first segment when it gets to it.

// SummarySegmentMigrationWriter writes the converted board and segment.
//
// It is the same write the generator uses: the board and the segment claim
// the same stretch of the conversation, so they land together or not at all.
type SummarySegmentMigrationWriter interface {
	SaveSummarySegmentAndWhiteboard(ctx context.Context, segment *model.SessionSummarySegment,
		board *model.SessionWhiteboard, expectedVersion uint64) error
}

// LegacySummaryReader reads the old rolling summary.
//
// It is the stored row and nothing else: the migration must not regenerate a
// summary, because a summary written now would be a summary of the old path
// that the applied mode has already stopped using.
type LegacySummaryReader interface {
	GetSummary(ctx context.Context, userID int64, sessionID string) (*model.SessionSummary, error)
}

// SummarySegmentMigration is the conversion of an old summary into the new
// tables.
//
// It is an interface so that the read side does not depend on how the
// conversion is carried out, and so that a deployment without one can leave
// it unset.
type SummarySegmentMigration interface {
	// Migrate converts the session's old summary if it has one and the new
	// tables are empty.
	//
	// It reports whether the session's new state may have changed, so the
	// caller knows whether it has to read the state again. A conversion that
	// wrote changed it, and so did a conversion that lost the race to a
	// writer that wrote. A session that had nothing to convert leaves the
	// state where it was.
	Migrate(ctx context.Context, input RunContext) (bool, error)
}

// SummarySegmentMigrator converts one session's old summary.
type SummarySegmentMigrator struct {
	reader SummarySegmentReader
	legacy LegacySummaryReader
	writer SummarySegmentMigrationWriter
	now    func() time.Time
}

// NewSummarySegmentMigrator builds a migrator.
func NewSummarySegmentMigrator(reader SummarySegmentReader, legacy LegacySummaryReader,
	writer SummarySegmentMigrationWriter) (*SummarySegmentMigrator, error) {
	if reader == nil || legacy == nil || writer == nil {
		return nil, fmt.Errorf("summary segment migrator dependencies are required")
	}
	return &SummarySegmentMigrator{reader: reader, legacy: legacy, writer: writer, now: time.Now}, nil
}

// Migrate converts the session's old summary if it has one and the new
// tables are empty.
//
// It reports whether the session's new state may have changed, so the caller
// knows whether it has to read the state again. A session that already has a
// board or a segment is left alone: the new path owns it now.
func (m *SummarySegmentMigrator) Migrate(ctx context.Context, input RunContext) (bool, error) {
	if m == nil || m.reader == nil || m.legacy == nil || m.writer == nil {
		return false, nil
	}
	existing, err := m.reader.GetWhiteboard(ctx, input.UserID, input.SessionID)
	if err != nil && !errors.Is(err, model.ErrWhiteboardNotFound) {
		return false, fmt.Errorf("read session whiteboard: %w", err)
	}
	if existing != nil {
		return false, nil
	}
	segments, err := m.reader.ListLatestSummarySegments(ctx, input.UserID, input.SessionID, 1)
	if err != nil {
		return false, fmt.Errorf("read summary segments: %w", err)
	}
	if len(segments) > 0 {
		return false, nil
	}
	summary, err := m.legacy.GetSummary(ctx, input.UserID, input.SessionID)
	if err != nil {
		if errors.Is(err, sessiondao.ErrSummaryNotFound) {
			// The session was never summarised by the old path. There is
			// nothing to convert.
			return false, nil
		}
		return false, fmt.Errorf("read legacy summary: %w", err)
	}
	if summary == nil || strings.TrimSpace(summary.Content) == "" {
		return false, nil
	}
	document, err := model.DecodeSessionSummaryDocument(summary.Content)
	if err != nil {
		// A summary this schema cannot read is not converted. That includes
		// the plain text an ordinary conversation's summary was stored as:
		// turning it into the structured form is another tool's job, and
		// doing it here as well would convert it twice.
		//
		// The session keeps the conversation it has, and the background
		// writes its first segment when it gets to it.
		log.Printf("[summary-segment] legacy summary not convertible: session=%s error_type=%T",
			input.SessionID, err)
		return false, nil
	}
	boardDocument, segmentContent, ok := ConvertLegacySummary(document, summary.CoveredMessageID, summary.Version)
	if !ok {
		return false, nil
	}
	boardContent, err := model.MarshalSessionWhiteboardDocument(boardDocument)
	if err != nil {
		return false, fmt.Errorf("encode migrated whiteboard: %w", err)
	}
	segmentContentText, err := model.MarshalSessionSummarySegmentContent(segmentContent)
	if err != nil {
		return false, fmt.Errorf("encode migrated summary segment: %w", err)
	}
	now := m.now()
	segment := &model.SessionSummarySegment{
		UserID:          input.UserID,
		SessionID:       input.SessionID,
		Seq:             1,
		StartMessageID:  0,
		EndMessageID:    summary.CoveredMessageID,
		StartAt:         now,
		EndAt:           now,
		Topic:           segmentContent.Topic,
		Content:         segmentContentText,
		NamesAndNumbers: model.MarshalSessionSummarySegmentTerms(segmentContent.Names, segmentContent.Numbers),
		IndexStatus:     model.SessionSummarySegmentIndexPending,
	}
	board := &model.SessionWhiteboard{
		UserID:           input.UserID,
		SessionID:        input.SessionID,
		Content:          boardContent,
		CoveredMessageID: summary.CoveredMessageID,
	}
	// The board has no version yet, so the write expects version zero. A
	// writer that won the race is not an error: its board is the one that
	// counts, and the state has changed, so the caller reads it again.
	if err := m.writer.SaveSummarySegmentAndWhiteboard(ctx, segment, board, 0); err != nil {
		if errors.Is(err, ErrSummarySegmentWhiteboardConflict) {
			return true, nil
		}
		return false, fmt.Errorf("write migrated summary segment: %w", err)
	}
	return true, nil
}

// ConvertLegacySummary turns an old summary document into a board and a first
// segment.
//
// The mapping is the plan's:
//
//   - the goal becomes the board's goal;
//   - the confirmed decisions and the corrections become the board's
//     constraints, because both are "in force and to be obeyed";
//   - the completed items become the board's plan, marked done;
//   - the open loops and the key errors become the board's open items;
//   - the references do not go on the board at all: a reference is a
//     document the session touched, which is progress inside the session
//     rather than a fact about it.
//
// The first segment keeps everything as text, because that is the only place
// the session's progress is still answerable: "what did we talk about then"
// has no other source.
//
// The groups are trimmed to what the new tables allow. A group that reaches
// its limit keeps its newest entries, because the newest is what the learner
// is most likely to ask about.
func ConvertLegacySummary(document model.SessionSummaryDocument, coveredMessageID uint, migratedFromVersion uint64) (model.SessionWhiteboardDocument, model.SessionSummarySegmentContent, bool) {
	board := model.SessionWhiteboardDocument{
		SchemaVersion: model.SessionWhiteboardSchemaV1, MigratedFromVersion: migratedFromVersion,
	}
	segment := model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: "历史摘要",
		MigratedFromVersion: migratedFromVersion,
	}
	if goal := strings.TrimSpace(document.CurrentGoal); goal != "" {
		board.Goal = &model.SessionWhiteboardGoal{
			Text:               boundLegacyText(goal),
			UpdatedAtMessageID: coveredMessageID,
		}
		segment.Points = append(segment.Points, model.SessionSummarySegmentPoint{Text: boundLegacyPointText(goal)})
	}
	for index, decision := range document.ConfirmedDecisions {
		text := strings.TrimSpace(decision.Text)
		if text == "" {
			continue
		}
		board.Constraints = append(board.Constraints, model.SessionWhiteboardEntry{
			Key:                fmt.Sprintf("decision.%d", index+1),
			Text:               boundLegacyText(text),
			SourceMessageIDs:   boundLegacyEvidence(decision.EvidenceMessageIDs, model.SessionWhiteboardEvidenceLimit),
			UpdatedAtMessageID: maxLegacyMessageID(decision.EvidenceMessageIDs),
		})
		segment.Points = append(segment.Points, model.SessionSummarySegmentPoint{
			Text:               boundLegacyPointText(text),
			EvidenceMessageIDs: boundLegacyEvidence(decision.EvidenceMessageIDs, model.SessionSummaryEvidenceLimit),
		})
	}
	for index, correction := range document.UserCorrections {
		text := strings.TrimSpace(correction.Text)
		if text == "" {
			continue
		}
		board.Constraints = append(board.Constraints, model.SessionWhiteboardEntry{
			Key:                fmt.Sprintf("correction.%d", index+1),
			Text:               boundLegacyText(text),
			SourceMessageIDs:   boundLegacyEvidence(correction.EvidenceMessageIDs, model.SessionWhiteboardEvidenceLimit),
			UpdatedAtMessageID: maxLegacyMessageID(correction.EvidenceMessageIDs),
		})
		segment.Points = append(segment.Points, model.SessionSummarySegmentPoint{
			Text:               boundLegacyPointText(text),
			EvidenceMessageIDs: boundLegacyEvidence(correction.EvidenceMessageIDs, model.SessionSummaryEvidenceLimit),
		})
	}
	for _, item := range document.CompletedItems {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		board.Plan = append(board.Plan, model.SessionWhiteboardEntry{
			Text: boundLegacyText(text), State: "done",
		})
		segment.Points = append(segment.Points, model.SessionSummarySegmentPoint{Text: boundLegacyPointText(text)})
	}
	for _, loop := range document.OpenLoops {
		text := strings.TrimSpace(loop.Text)
		if text == "" {
			continue
		}
		board.OpenItems = append(board.OpenItems, model.SessionWhiteboardEntry{
			Text:               boundLegacyText(text),
			SourceMessageIDs:   boundLegacyEvidence(loop.EvidenceMessageIDs, model.SessionWhiteboardEvidenceLimit),
			UpdatedAtMessageID: maxLegacyMessageID(loop.EvidenceMessageIDs),
		})
		segment.Points = append(segment.Points, model.SessionSummarySegmentPoint{
			Text:               boundLegacyPointText(text),
			EvidenceMessageIDs: boundLegacyEvidence(loop.EvidenceMessageIDs, model.SessionSummaryEvidenceLimit),
		})
	}
	for index, item := range document.KeyErrors {
		text := strings.TrimSpace(item)
		if text == "" {
			continue
		}
		board.OpenItems = append(board.OpenItems, model.SessionWhiteboardEntry{
			Key: fmt.Sprintf("error.%d", index+1), Text: boundLegacyText(text),
		})
		segment.Points = append(segment.Points, model.SessionSummarySegmentPoint{Text: boundLegacyPointText(text)})
	}
	for _, reference := range document.References {
		if text := boundLegacyTerm(reference); text != "" {
			segment.Names = append(segment.Names, text)
		}
	}
	// The limits are the new tables' limits. A group that reaches one is cut
	// rather than refused: a converted session with less in it is still a
	// session that works.
	board.Constraints = boundLegacyGroup(board.Constraints)
	board.Plan = boundLegacyGroup(board.Plan)
	board.OpenItems = boundLegacyGroup(board.OpenItems)
	if len(segment.Points) > model.SessionSummarySegmentPointLimit {
		segment.Points = segment.Points[:model.SessionSummarySegmentPointLimit]
	}
	if len(segment.Names) > model.SessionSummarySegmentTermLimit {
		segment.Names = segment.Names[:model.SessionSummarySegmentTermLimit]
	}
	if board.Goal == nil && len(board.Constraints) == 0 && len(board.Plan) == 0 &&
		len(board.OpenItems) == 0 && len(segment.Points) == 0 && len(segment.Names) == 0 {
		// Nothing worth converting. The session goes on with the raw
		// conversation.
		return model.SessionWhiteboardDocument{}, model.SessionSummarySegmentContent{}, false
	}
	return board, segment, true
}

// boundLegacyText shortens one text to what a board entry accepts.
func boundLegacyText(text string) string {
	return boundRunes(strings.TrimSpace(text), model.SessionWhiteboardTextMaxRunes)
}

// boundLegacyPointText shortens one text to what a segment point accepts.
//
// A point is allowed to be longer than a board entry: the board holds what
// is true now and the point holds the text of it, so the point is where the
// longer wording still fits.
func boundLegacyPointText(text string) string {
	return boundRunes(strings.TrimSpace(text), model.SessionSummarySegmentPointMaxRunes)
}

// boundLegacyTerm shortens one name to what a segment accepts.
//
// The old summary let a reference be a thousand characters and a segment
// term is a hundred, so a long reference has to be cut rather than dropped:
// the document it names is still worth having in the segment.
func boundLegacyTerm(text string) string {
	return boundRunes(strings.TrimSpace(text), model.SessionSummarySegmentTermMaxRunes)
}

// boundLegacyEvidence keeps the evidence a new table accepts.
//
// The old summary let one item cite thirty two messages and a board entry
// cites eight, so the citation has to be cut. The newest are kept: they are
// the ones closest to what the session had reached. A zero is dropped and a
// duplicate is dropped, because the new tables refuse both and a refused
// board is a board that was never written.
func boundLegacyEvidence(ids []uint, limit int) []uint {
	if len(ids) == 0 || limit <= 0 {
		return nil
	}
	kept := make([]uint, 0, len(ids))
	seen := make(map[uint]struct{}, len(ids))
	for index := len(ids) - 1; index >= 0 && len(kept) < limit; index-- {
		id := ids[index]
		if id == 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		return nil
	}
	// The ids were collected newest first, and they read better in the order
	// the session produced them.
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	return kept
}

// boundRunes shortens a text to a rune count.
func boundRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return text
}

// boundLegacyGroup keeps a group within the board's limit.
//
// The newest entries are kept: the newest is what the learner is most likely
// to ask about, and the oldest is the one the segment still carries.
func boundLegacyGroup(entries []model.SessionWhiteboardEntry) []model.SessionWhiteboardEntry {
	if len(entries) <= model.SessionWhiteboardItemLimit {
		return entries
	}
	return entries[len(entries)-model.SessionWhiteboardItemLimit:]
}

// maxLegacyMessageID is the highest message id a group of evidence names.
func maxLegacyMessageID(ids []uint) uint {
	highest := uint(0)
	for _, id := range ids {
		if id > highest {
			highest = id
		}
	}
	return highest
}
