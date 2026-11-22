package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"GopherAI/dao/message"
	"GopherAI/dao/session"
	"GopherAI/model"
)

// The database side of the session lock.
//
// It reads one conversation of one learner and nothing else: the ownership
// check is the first thing it does, and a conversation that is not the
// learner's is never read at all.

// The bounds of one locked conversation's evidence.
//
// They are small on purpose. The evidence is the answer's input, and the
// answer is about ONE conversation: a card's job is to point at a body, and
// the body that matters is the part that can hold the answer. A locked
// conversation that needs more than this is a conversation the model has to
// be told to read for itself, and the window says so.
const (
	// maxLockedSegments is how many summary segments one locked
	// conversation contributes.
	maxLockedSegments = 24
	// maxLockedMessages is how many of the conversation's own words one
	// locked conversation contributes when it has no summary.
	maxLockedMessages = 40
	// maxLockedTailMessages is how many of the conversation's own words one
	// locked conversation contributes AFTER the summary's last covered
	// message.
	//
	// The summary stops where the compressor got to; a conversation can go on
	// after it, and the answer can be in the stretch the summary never
	// covered. The read is bounded like every other read here: the evidence
	// block's token cap decides how much of the tail is actually rendered.
	maxLockedTailMessages = 40
	// maxLockedEvidenceRunes bounds the rendered evidence.
	maxLockedEvidenceRunes = 4000
	// maxLockedSegmentRunes bounds one rendered segment.
	maxLockedSegmentRunes = 400
	// maxLockedMessageRunes bounds one rendered message.
	maxLockedMessageRunes = 300
	// maxLockedGapWords bounds the words kept from inside segment ranges that
	// the segment's points did not retain.
	maxLockedGapWords = 24
	// maxLockedGapScan bounds one segment-range read.
	maxLockedGapScan = 100
)

// ErrCrossSessionNotFound is a conversation that is not there.
var ErrCrossSessionNotFound = errors.New("the conversation was not found")

// CrossSessionStore is the store a locked conversation is read from.
//
// It is an interface so that the reader can be exercised without a database,
// and so that the ownership question is asked in one place.
type CrossSessionStore interface {
	// GetConversation reads one conversation of one learner.
	//
	// A conversation the learner does not own is not found: the store's
	// read is scoped, and a scope that is checked after the read is not a
	// boundary.
	GetConversation(ctx context.Context, userID int64, sessionID string) (*model.Session, error)
	// ConversationOwnership says whether a conversation exists and whether
	// it is the learner's.
	//
	// It answers a question about ownership and returns no content: the
	// server needs the two apart, because "there is no such conversation"
	// and "that is somebody else's conversation" are different failures.
	ConversationOwnership(ctx context.Context, userID int64, sessionID string) (exists, owned bool, err error)
	// ListSegments reads the conversation's summary segments, oldest first.
	ListSegments(ctx context.Context, userID int64, sessionID string, limit int) ([]model.SessionSummarySegment, error)
	// ListWords reads the conversation's own words, oldest first.
	ListWords(ctx context.Context, userID int64, sessionID string, limit int) ([]model.Message, error)
	// ListWordsAfter reads the conversation's own words after one message id,
	// oldest first. The id is the summary's last covered message: the words
	// after it are the stretch no summary covers.
	ListWordsAfter(ctx context.Context, userID int64, sessionID string, afterID uint, limit int) ([]model.Message, error)
}

// DatabaseCrossSessionStore reads locked conversations from MySQL.
type DatabaseCrossSessionStore struct {
	Sessions *session.Repository
	Messages *message.Repository
}

// NewDatabaseCrossSessionStore builds the store.
func NewDatabaseCrossSessionStore(sessions *session.Repository, messages *message.Repository) (*DatabaseCrossSessionStore, error) {
	if sessions == nil || messages == nil {
		return nil, fmt.Errorf("cross session store dependencies are required")
	}
	return &DatabaseCrossSessionStore{Sessions: sessions, Messages: messages}, nil
}

// GetConversation reads one conversation of one learner.
func (s *DatabaseCrossSessionStore) GetConversation(ctx context.Context, userID int64,
	sessionID string) (*model.Session, error) {
	row, err := s.Sessions.GetByIDAndUserID(ctx, sessionID, userID)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return nil, ErrCrossSessionNotFound
		}
		return nil, err
	}
	return row, nil
}

// ConversationOwnership says whether a conversation exists and whether it is
// the learner's.
func (s *DatabaseCrossSessionStore) ConversationOwnership(ctx context.Context, userID int64,
	sessionID string) (bool, bool, error) {
	_, err := s.Sessions.GetByIDAndUserID(ctx, sessionID, userID)
	if err == nil {
		return true, true, nil
	}
	if !errors.Is(err, session.ErrSessionNotFound) {
		return false, false, err
	}
	// It was not found as the learner's. It is either not there at all, or
	// it belongs to somebody else, and the two are told apart by a read that
	// asks about ownership only.
	exists, err := s.Sessions.SessionExists(ctx, sessionID)
	if err != nil {
		return false, false, err
	}
	return exists, false, nil
}

// ListSegments reads the conversation's summary segments.
func (s *DatabaseCrossSessionStore) ListSegments(ctx context.Context, userID int64,
	sessionID string, limit int) ([]model.SessionSummarySegment, error) {
	return s.Sessions.ListSummarySegments(ctx, userID, sessionID, limit)
}

// ListWords reads the conversation's own words.
func (s *DatabaseCrossSessionStore) ListWords(ctx context.Context, userID int64,
	sessionID string, limit int) ([]model.Message, error) {
	page, err := s.Messages.ListOwnedHistoryPage(ctx, userID, sessionID, 0, limit)
	if err != nil {
		return nil, err
	}
	return page.Messages, nil
}

// ListWordsAfter reads the conversation's own words after one message id.
func (s *DatabaseCrossSessionStore) ListWordsAfter(ctx context.Context, userID int64,
	sessionID string, afterID uint, limit int) ([]model.Message, error) {
	return s.Messages.ListOwnedWordsAfter(ctx, userID, sessionID, afterID, limit)
}

// DatabaseCrossSessionReader locks one conversation and reads its evidence.
type DatabaseCrossSessionReader struct {
	store CrossSessionStore
}

// NewDatabaseCrossSessionReader builds the reader.
func NewDatabaseCrossSessionReader(store CrossSessionStore) (*DatabaseCrossSessionReader, error) {
	if store == nil {
		return nil, fmt.Errorf("cross session store is required")
	}
	return &DatabaseCrossSessionReader{store: store}, nil
}

// LockSession validates that the conversation is the learner's and reads the
// evidence out of it.
//
// The order is the point: the ownership check comes FIRST, and a conversation
// that is not the learner's is refused before anything of it is read. A
// conversation that does not exist and a conversation that belongs to
// somebody else are told apart, because only the second one says that
// somebody reached for it.
func (r *DatabaseCrossSessionReader) LockSession(ctx context.Context, userID int64,
	sessionID string) (*LockedSession, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("cross session reader is not initialized")
	}
	sessionID = strings.TrimSpace(sessionID)
	if userID <= 0 || sessionID == "" {
		return nil, ErrCrossSessionNotFound
	}
	exists, owned, err := r.store.ConversationOwnership(ctx, userID, sessionID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrCrossSessionNotFound
	}
	if !owned {
		return nil, ErrSessionNotOwned
	}
	conversation, err := r.store.GetConversation(ctx, userID, sessionID)
	if err != nil {
		if errors.Is(err, ErrCrossSessionNotFound) {
			return nil, ErrCrossSessionNotFound
		}
		return nil, err
	}
	locked := &LockedSession{SessionID: sessionID, Title: strings.TrimSpace(conversation.Title)}
	segments, err := r.store.ListSegments(ctx, userID, sessionID, maxLockedSegments)
	if err != nil {
		return nil, err
	}
	if rendered, count := renderLockedSegments(segments); rendered != "" {
		locked.Summary, locked.Segments = rendered, count
		locked.Evidence = rendered
		// A summary segment covers a stretch of turns and keeps only its
		// points. Words inside that stretch that no point kept are still the
		// conversation's own record; the block keeps the ones the waiting
		// question mentions.
		locked.Gaps = collectLockedGapWords(ctx, r.store, userID, sessionID, segments)
		// The summary ends where the compressor stopped, and the conversation
		// can go on after it. The words after the last covered message are
		// the stretch no summary carries; they are read only from THIS
		// conversation, and only after the ownership check above has passed.
		//
		// The read is bounded, and how much of it is rendered is decided by
		// the evidence block's token cap, not here: the summary keeps its
		// place first, and the tail is what gets cut when the two do not fit.
		if watermark := summaryWatermark(segments); watermark > 0 {
			tail, tailErr := r.store.ListWordsAfter(ctx, userID, sessionID, watermark, maxLockedTailMessages)
			if tailErr != nil {
				return nil, tailErr
			}
			if renderedTail, tailCount := renderLockedTail(tail); renderedTail != "" {
				locked.Tail, locked.TailMessages = renderedTail, tailCount
				locked.Evidence = rendered + "\n" + renderedTail
			}
		}
		return locked, nil
	}
	words, err := r.store.ListWords(ctx, userID, sessionID, maxLockedMessages)
	if err != nil {
		return nil, err
	}
	locked.Evidence, locked.Messages = renderLockedWords(words), len(words)
	return locked, nil
}

// collectLockedGapWords reads the words inside each summary segment's range
// that the segment's points did not keep.
//
// A segment covers a stretch of turns and keeps only its points; a detail the
// learner later asks about can be in that stretch and absent from the points.
// The block's own rule is that such a detail is the conversation's record too,
// so it is read here (bounded) and filtered by the waiting question when the
// block is rendered.
func collectLockedGapWords(ctx context.Context, store CrossSessionStore, userID int64,
	sessionID string, segments []model.SessionSummarySegment) []LockedGapWord {
	out := make([]LockedGapWord, 0, maxLockedGapWords)
	seen := make(map[uint]struct{}, maxLockedGapWords)
	for _, segment := range segments {
		start, end := segment.StartMessageID, segment.EndMessageID
		if start == 0 || end < start {
			continue
		}
		cited := make(map[uint]string, 8)
		if content, err := model.DecodeSessionSummarySegmentContent(segment.Content); err == nil {
			for _, point := range content.Points {
				text := strings.TrimSpace(point.Text)
				for _, id := range point.EvidenceMessageIDs {
					cited[id] = text
				}
			}
		}
		after := uint(0)
		if start > 1 {
			after = start - 1
		}
		words, err := store.ListWordsAfter(ctx, userID, sessionID, after, maxLockedGapScan)
		if err != nil {
			continue
		}
		for _, word := range words {
			if word.ID < start {
				continue
			}
			if word.ID > end {
				break
			}
			if _, duplicate := seen[word.ID]; duplicate {
				continue
			}
			seen[word.ID] = struct{}{}
			out = append(out, LockedGapWord{Message: word, PointText: cited[word.ID]})
			if len(out) >= maxLockedGapWords {
				return out
			}
		}
	}
	return out
}

// summaryWatermark is the last message the conversation's summary covers.
//
// It is the end of the last segment, not the number of segments: the tail is
// everything the summary never reached.
func summaryWatermark(segments []model.SessionSummarySegment) uint {
	watermark := uint(0)
	for _, segment := range segments {
		if segment.EndMessageID > watermark {
			watermark = segment.EndMessageID
		}
	}
	return watermark
}

// renderLockedSegments renders a conversation's summary as the evidence.
//
// The summary is the compressed body: it says what happened in the
// conversation in the program's own words, and it is where the answer to a
// question about that conversation usually is.
func renderLockedSegments(segments []model.SessionSummarySegment) (string, int) {
	if len(segments) == 0 {
		return "", 0
	}
	lines := make([]string, 0, len(segments))
	for _, segment := range segments {
		content, err := model.DecodeSessionSummarySegmentContent(segment.Content)
		if err != nil {
			continue
		}
		parts := make([]string, 0, len(content.Points))
		for _, point := range content.Points {
			text := strings.TrimSpace(point.Text)
			if text == "" {
				continue
			}
			parts = append(parts, text)
		}
		if len(parts) == 0 {
			continue
		}
		topic := strings.TrimSpace(content.Topic)
		if topic == "" {
			topic = strings.TrimSpace(segment.Topic)
		}
		line := strings.Join(parts, "；")
		if topic != "" {
			line = topic + "：" + line
		}
		lines = append(lines, boundRunes(line, maxLockedSegmentRunes))
	}
	if len(lines) == 0 {
		return "", 0
	}
	return boundRunes(strings.Join(lines, "\n"), maxLockedEvidenceRunes), len(lines)
}

// renderLockedTail renders the words after the summary as the evidence's tail.
//
// It is the same rendering as the no-summary fallback, and the difference is
// in the caller: this stretch is evidence BECAUSE the summary did not reach
// it, not because the conversation has no summary. The total size is not
// bounded here; the block's token cap decides how much of it is rendered.
func renderLockedTail(words []model.Message) (string, int) {
	lines := make([]string, 0, len(words))
	for _, word := range words {
		content := strings.TrimSpace(word.Content)
		if content == "" {
			continue
		}
		who := "learner"
		if !word.IsUser {
			who = "assistant"
		}
		lines = append(lines, who+": "+boundRunes(content, maxLockedMessageRunes))
	}
	if len(lines) == 0 {
		return "", 0
	}
	return strings.Join(lines, "\n"), len(lines)
}

// renderLockedWords renders a conversation's own words as the evidence.
//
// It is the fallback for a conversation with no summary: the learner's own
// words and the assistant's are the body when nothing compressed it.
func renderLockedWords(words []model.Message) string {
	if len(words) == 0 {
		return ""
	}
	lines := make([]string, 0, len(words))
	for _, word := range words {
		content := strings.TrimSpace(word.Content)
		if content == "" {
			continue
		}
		who := "learner"
		if !word.IsUser {
			who = "assistant"
		}
		lines = append(lines, who+": "+boundRunes(content, maxLockedMessageRunes))
	}
	if len(lines) == 0 {
		return ""
	}
	return boundRunes(strings.Join(lines, "\n"), maxLockedEvidenceRunes)
}
