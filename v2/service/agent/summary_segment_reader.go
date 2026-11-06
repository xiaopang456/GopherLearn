package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"GopherAI/model"
)

// The read side of the layered summary.
//
// The write side produces two things: the board, which says what is true now,
// and the segments, which say what happened then. The read side puts the
// board in front of the model every turn, the newest segments with it, and
// the few older segments the server believes this question is about.
//
// The reader is deliberately not the writer's repository interface. A reader
// needs fewer rights than a writer, and - more importantly - the read side
// has to keep working when the write path is off. The off mode sends exactly
// what it sent before, and the shadow mode writes without being read: a
// reader that could only work when the write path exists would make those two
// modes into the same mode.

// SummarySegmentReader is the storage the layered summary's read side needs.
type SummarySegmentReader interface {
	// GetWhiteboard returns the session's board, or model.ErrWhiteboardNotFound
	// when the session has none.
	GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error)
	// GetSummarySegment returns one segment of the session by its sequence
	// number, or model.ErrSummarySegmentNotFound when the session has no such
	// segment.
	GetSummarySegment(ctx context.Context, userID int64, sessionID string, seq int) (*model.SessionSummarySegment, error)
	// ListLatestSummarySegments returns the newest segments, newest first.
	ListLatestSummarySegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error)
	// ListSummarySegments returns the session's segments, oldest first.
	ListSummarySegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error)
	// GetSummarySegmentWatermark returns the message id the session's
	// segments cover.
	GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error)
}

// LayeredSummarySnapshot is what one turn's read side found.
type LayeredSummarySnapshot struct {
	// Board is the session's current state, when it has one.
	Board *model.SessionWhiteboard
	// BoardDocument is the parsed board. It is the zero value when the
	// session has no board, or when the stored one cannot be read.
	BoardDocument model.SessionWhiteboardDocument
	// Resident are the segments that travel with every turn.
	Resident []model.SessionSummarySegment
	// Retrieved are the segments the server chose for this question.
	Retrieved []model.SessionSummarySegment
	// CoveredMessageID is the highest message id the session's segments
	// cover. Everything below it is compressed; everything above it is still
	// raw conversation.
	CoveredMessageID uint
}

// LayeredSummaryProvider reads the layered summary for one turn.
type LayeredSummaryProvider interface {
	LayeredSummary(ctx context.Context, input RunContext) (LayeredSummarySnapshot, error)
}

// ResidentSegmentCount derives how many of the newest segments travel with
// every turn, from the block that renders them.
//
// It is the preselector's rule, not a second one: a segment is indivisible,
// the block is the budget, and the count is the division. The same two numbers
// that size the retrieved block size this one, so a configuration cannot end up
// asking for two residents in a block that holds one - which is the defect the
// old independent constant produced once already, when the default was fixed at
// two and a scaled resident block of 310 tokens held a single 300-token
// segment.
//
// One is the floor for the same reason the preselector has one: nothing
// selected and nothing allowed are different states, and only the second is a
// configuration mistake.
func ResidentSegmentCount(residentTokens, segmentTokens int) int {
	segments, _, _ := SummarySegmentPreselectLimits(residentTokens, segmentTokens)
	return segments
}

// DatabaseLayeredSummaryProvider reads the layered summary out of MySQL.
type DatabaseLayeredSummaryProvider struct {
	reader    SummarySegmentReader
	preselect *SummarySegmentPreselector
	recent    SummarySegmentRecentUse
	// migrate converts a session the old path summarised into the new
	// tables. It is optional: without it an old session waits for the
	// background to write its first segment.
	migrate  SummarySegmentMigration
	resident int
}

// NewDatabaseLayeredSummaryProvider builds the reader.
//
// The resident count is required and has no default: it is derived from the
// block that renders it (ResidentSegmentCount), and a default here would be a
// second answer to a question the budget already answers.
func NewDatabaseLayeredSummaryProvider(reader SummarySegmentReader, preselect *SummarySegmentPreselector,
	resident int) (*DatabaseLayeredSummaryProvider, error) {
	if reader == nil {
		return nil, fmt.Errorf("summary segment reader is required")
	}
	if resident <= 0 {
		return nil, fmt.Errorf("summary segment resident count is required: derive it from the block that renders it")
	}
	return &DatabaseLayeredSummaryProvider{reader: reader, preselect: preselect, resident: resident}, nil
}

// UseRecentUse attaches the record of the segments the last turns reached for.
//
// It is a setter rather than a constructor argument because the record is
// optional: without it the read side sends the newest segments and nothing
// else.
func (p *DatabaseLayeredSummaryProvider) UseRecentUse(recent SummarySegmentRecentUse) {
	if p == nil {
		return
	}
	p.recent = recent
}

// UseMigrator attaches the conversion of an old summary into the new tables.
//
// It is a setter rather than a constructor argument because the conversion
// is a deployment's business: a deployment that has never run the old path
// has nothing to convert.
func (p *DatabaseLayeredSummaryProvider) UseMigrator(migrator SummarySegmentMigration) {
	if p == nil {
		return
	}
	p.migrate = migrator
}

// layeredState is one read of the session's new state.
//
// The three parts are read together because they say the same thing from
// three sides: the board is what is true now, the segments are what the
// session did, and the watermark is how far the segments reach.
type layeredState struct {
	board     *model.SessionWhiteboard
	resident  []model.SessionSummarySegment
	watermark uint
}

// readLayeredState reads the board, the resident segments and the watermark.
//
// A read that fails is a failure and not a partial state: the caller keeps
// what it had.
func (p *DatabaseLayeredSummaryProvider) readLayeredState(ctx context.Context, input RunContext) (layeredState, error) {
	state := layeredState{}
	board, err := p.reader.GetWhiteboard(ctx, input.UserID, input.SessionID)
	if err != nil && !errors.Is(err, model.ErrWhiteboardNotFound) {
		return state, fmt.Errorf("load session whiteboard: %w", err)
	}
	state.board = board
	resident, err := p.reader.ListLatestSummarySegments(ctx, input.UserID, input.SessionID, p.resident)
	if err != nil {
		return state, fmt.Errorf("load resident summary segments: %w", err)
	}
	state.resident = resident
	watermark, _, err := p.reader.GetSummarySegmentWatermark(ctx, input.UserID, input.SessionID)
	if err != nil {
		return state, fmt.Errorf("load summary segment watermark: %w", err)
	}
	state.watermark = watermark
	return state, nil
}

// layeredStateCarriesCoverage says whether the segments carry the coverage
// the watermark claims.
//
// The watermark says how far the summary reaches, and the raw messages below
// it are dropped from the window. A session with no segments has no
// coverage, so its watermark is zero. A session with segments reaches at
// least as far as its newest segment, and the watermark is the highest end
// the segments reach.
//
// A state that says otherwise was read across a write. Believing it would
// drop a stretch of the conversation from the window without putting
// anything in its place.
func layeredStateCarriesCoverage(resident []model.SessionSummarySegment, watermark uint) bool {
	if len(resident) == 0 {
		return watermark == 0
	}
	return resident[0].EndMessageID >= watermark
}

// LayeredSummary reads the board, the resident segments and the segments this
// question is about.
//
// A board that cannot be read is not a failure: the board is an optimisation
// over the raw conversation, and the raw conversation is still there. A
// preselection that cannot run costs the retrieved segments and nothing else.
func (p *DatabaseLayeredSummaryProvider) LayeredSummary(ctx context.Context, input RunContext) (LayeredSummarySnapshot, error) {
	snapshot := LayeredSummarySnapshot{}
	if p == nil || p.reader == nil {
		return snapshot, fmt.Errorf("summary segment reader is not initialized")
	}
	if input.UserID <= 0 || strings.TrimSpace(input.SessionID) == "" {
		return snapshot, fmt.Errorf("layered summary identity is required")
	}
	state, err := p.readLayeredState(ctx, input)
	if err != nil {
		return snapshot, err
	}
	board, resident, watermark := state.board, state.resident, state.watermark
	if !layeredStateCarriesCoverage(resident, watermark) {
		// The state was read across a write: the segments that reach the
		// watermark are not the ones in the snapshot. Nothing is claimed as
		// covered, so the raw messages below it are still sent.
		watermark = 0
	}
	// A session the old path summarised has neither a board nor a segment.
	// Converting it here gives it the same shape as a new one, and the turn
	// does not have to wait for the background to catch up.
	if p.migrate != nil && (board == nil || len(resident) == 0) {
		mayHaveChanged, migrateErr := p.migrate.Migrate(ctx, input)
		if migrateErr != nil {
			// The conversion is an optimisation over a conversation that is
			// still there. A failure costs the board and the first segment,
			// and the turn goes on with the raw conversation.
			log.Printf("[agent-context] summary segment migration failed: session=%s error_type=%T",
				input.SessionID, migrateErr)
		} else if mayHaveChanged {
			// The conversion wrote, or lost the race to a writer that did.
			// Either way the state has to be read again, and it is taken
			// only when it is whole: a board that could not be read and a
			// segment list that could not be read would leave the window
			// claiming a coverage it does not carry, and the raw messages
			// below the watermark would be dropped without anything in
			// their place.
			reread, rereadErr := p.readLayeredState(ctx, input)
			switch {
			case rereadErr != nil:
				log.Printf("[agent-context] summary segment migration reread failed: session=%s error_type=%T",
					input.SessionID, rereadErr)
			case !layeredStateCarriesCoverage(reread.resident, reread.watermark):
				// The read crossed a write, and the segments that reach
				// the watermark are not in the snapshot. The state stays
				// where it was, so the raw messages below the watermark
				// are still sent and the turn does not lose that part of
				// the conversation.
				log.Printf("[agent-context] summary segment migration reread inconsistent: session=%s",
					input.SessionID)
			default:
				board, resident, watermark = reread.board, reread.resident, reread.watermark
			}
		}
	}
	if board != nil {
		snapshot.Board = board
		document, decodeErr := model.DecodeSessionWhiteboardDocument(board.Content)
		if decodeErr != nil {
			log.Printf("[agent-context] session whiteboard unreadable: session=%s error_type=%T",
				input.SessionID, decodeErr)
		} else {
			snapshot.BoardDocument = document
		}
	}
	snapshot.CoveredMessageID = watermark
	snapshot.Resident = resident
	if err := p.addRecentUse(ctx, input, &snapshot); err != nil {
		// The recent-use record is a memory, not a source of truth: losing it
		// costs the learner the segments they were asking about, and the
		// turn still goes on.
		log.Printf("[agent-context] summary segment recent use unavailable: session=%s error_type=%T",
			input.SessionID, err)
	}
	if p.preselect == nil {
		return snapshot, nil
	}
	candidates, err := p.reader.ListSummarySegments(ctx, input.UserID, input.SessionID, SummarySegmentCandidateLimit)
	if err != nil {
		return snapshot, fmt.Errorf("load summary segment candidates: %w", err)
	}
	retrieved, err := p.preselect.Select(ctx, input, candidates, resident)
	if err != nil {
		log.Printf("[agent-context] summary segment preselect degraded: session=%s error_type=%T",
			input.SessionID, err)
		return snapshot, nil
	}
	snapshot.Retrieved = retrieved
	return snapshot, nil
}

// addRecentUse puts the segments the last turns reached for into the resident
// set.
//
// The newest segments are not always the ones the learner is asking about. A
// learner who keeps going back to the same stretch of the conversation wants
// that stretch in front of them again, and by the time the newest segments
// have moved past it the only thing that brings it back is this record.
//
// The resident budget still decides: the segments that do not fit are cut, and
// the newest ones are kept first.
func (p *DatabaseLayeredSummaryProvider) addRecentUse(ctx context.Context, input RunContext,
	snapshot *LayeredSummarySnapshot) error {
	if p.recent == nil || len(snapshot.Resident) == 0 {
		return nil
	}
	seqs, err := p.recent.ListRecentUse(ctx, input.UserID, input.SessionID, SummarySegmentRecentUseLimit)
	if err != nil {
		return err
	}
	resident := make(map[int]struct{}, len(snapshot.Resident))
	for _, segment := range snapshot.Resident {
		resident[segment.Seq] = struct{}{}
	}
	for _, seq := range seqs {
		if _, already := resident[seq]; already {
			continue
		}
		segment, err := p.reader.GetSummarySegment(ctx, input.UserID, input.SessionID, seq)
		if err != nil {
			// A segment that is gone is not a reason to fail the turn: the
			// record outlived the segment it names.
			continue
		}
		if segment == nil {
			continue
		}
		resident[seq] = struct{}{}
		snapshot.Resident = append(snapshot.Resident, *segment)
	}
	return nil
}

// EmptyLayeredSummaryProvider is the read side that finds nothing.
// It is what a deployment without the layered summary gets, and it is a
// provider rather than a nil check so that the builder has one code path
// instead of two.
type EmptyLayeredSummaryProvider struct{}

func (EmptyLayeredSummaryProvider) LayeredSummary(context.Context, RunContext) (LayeredSummarySnapshot, error) {
	return LayeredSummarySnapshot{}, nil
}
