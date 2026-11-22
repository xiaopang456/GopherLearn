package conversation

// The layered summary of the real preparation path.
//
// The card path reads the session's own compressed state: the summary segments
// and the whiteboard. The rolling summary that the preparation phase writes is
// a different document, and it is not what the card reads.
//
// So a preparation that only wrote the rolling summary produced a card whose
// evidence was the raw messages alone: the segments were empty and the board
// was absent, and the card said so by what it did NOT carry. That is a real
// measurement of a different thing, and it is not what the card path claims to
// measure.
//
// This file drives the PRODUCT'S OWN segment producer over the preparation's
// conversations, so the segments and the board exist before the card is
// projected. Nothing here reimplements the producer: the generator, the
// whiteboard rules, the budget and the write path are the product's, and the
// harness only says which batch to summarize.

import (
	"context"
	"fmt"

	"GopherAI/config"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"gorm.io/gorm"
)

// LayeredSummaryRecord is what the preparation wrote for one conversation.
//
// It is evidence, not a log line: the numbers say whether the card's input
// really held the compressed state, and a preparation that claims the card
// path but cannot show a segment is a preparation that measured something
// else.
type LayeredSummaryRecord struct {
	// SessionID is the conversation the segments belong to.
	SessionID string `json:"session_id"`
	// Segments is how many summary segments were written.
	Segments int `json:"segments"`
	// Whiteboard says the board was written. It is a bool and not a count:
	// the board is one row per conversation.
	Whiteboard bool `json:"whiteboard"`
	// CoveredMessageID is how far the written state reaches. It is the
	// message id the segments cover, and it is what says the card's evidence
	// was compressed rather than raw.
	CoveredMessageID uint `json:"covered_message_id"`
	// WhiteboardCoveredMessageID is how far the board reaches. The board
	// covers what the segments cover, and the two are told apart because
	// they are two facts.
	WhiteboardCoveredMessageID uint `json:"whiteboard_covered_message_id"`
	// Points is how many summary points the written segments carry. A
	// segment with no points is a segment that says nothing.
	Points int `json:"points"`
	// Error says why this conversation got no layered summary.
	//
	// The preparation goes on when one conversation fails: the other thirty-
	// one were summarized well, and throwing them away would measure an
	// empty set instead of a degraded one. The conversation this belongs to
	// has no segment and no board, so its card is written from the raw
	// messages - and this field is where that shows.
	Error string `json:"error,omitempty"`
}

// WriteLayeredSummaries summarizes every conversation of the preparation with
// the product's own producer.
//
// The batch is the conversation's whole stretch: from the first message to the
// last one the preparation wrote. That is what the card's evidence loader
// reads - every segment of the session whose end is inside the card's window -
// and it is why the batch is not split: a card that saw half of a
// conversation's segments would be describing a conversation it did not read.
//
// The write path is the product's, so the board's rules about sources, limits
// and deletions hold here as they hold in a deployment. The harness does not
// touch the board's content.
//
//	WriteLayeredSummaries(ctx, s, userID, sessionIDs, chat, settings)
func WriteLayeredSummaries(ctx context.Context, s *Sandbox, userID int64, sessionIDs []string,
	chat modelcomponent.ToolCallingChatModel, settings SegmentSettings) ([]LayeredSummaryRecord, error) {
	if chat == nil {
		return nil, fmt.Errorf("the layered summary needs a model")
	}
	generator, err := agent.NewModelSegmentGenerator(chat, settings.MaxInputTokens, settings.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	generator.UseAssistantContent(settings.AssistantContent)
	generator.UseSegmentBudget(settings.SegmentTokens)
	return WriteLayeredSummariesWithGenerator(ctx, s, userID, sessionIDs, generator, settings)
}

// WriteLayeredSummariesWithGenerator summarizes the conversations with a
// generator the caller supplies.
//
// The seam exists so that the failure path can be exercised: a generator that
// refuses one batch is what the real model does when it writes a document the
// decoder does not know, and a preparation that stops at the first refusal
// would throw away the conversations that were summarized well.
func WriteLayeredSummariesWithGenerator(ctx context.Context, s *Sandbox, userID int64, sessionIDs []string,
	generator agent.SegmentGenerator, settings SegmentSettings) ([]LayeredSummaryRecord, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("the evaluation database is unavailable")
	}
	if userID <= 0 || len(sessionIDs) == 0 {
		return nil, fmt.Errorf("the layered summary needs an identity and a conversation")
	}
	if generator == nil {
		return nil, fmt.Errorf("the layered summary needs a generator")
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	records := make([]LayeredSummaryRecord, 0, len(sessionIDs))
	written := 0
	for _, sessionID := range sessionIDs {
		record, err := writeOneLayeredSummary(ctx, s, userID, sessionID, generator, settings)
		if err != nil {
			// One conversation's failure does not void the other thirty-one.
			//
			// The product refuses a summary it cannot store - a document with
			// a field the decoder does not know, or a summary that does not
			// fit the budget it is read under - and both refusals are
			// deliberate. A preparation that stopped at the first refusal
			// would throw away the conversations that were summarized well,
			// and the arm would measure an empty set instead of a degraded
			// one.
			//
			// So the failure is recorded and the preparation goes on. The
			// conversation it belongs to has no layered summary, and its
			// card is written from the raw messages - which is a
			// DIFFERENT card, and the record says so.
			record.Error = err.Error()
			records = append(records, record)
			continue
		}
		written++
		records = append(records, record)
	}
	if written == 0 {
		// Tolerance must not become blindness. A preparation where NO
		// conversation got a layered summary is not measuring the card path
		// at all, and it has to stop before it spends the rest of the run.
		return records, fmt.Errorf("no conversation got a summary segment: the layered summary is broken, not one conversation")
	}
	return records, nil
}

// countLayeredSummaryFailures counts the conversations that got no layered
// summary.
//
// The number is the finding: a card arm where a quarter of the cards were
// written from the raw messages is a different measurement from one where
// they all were, and a reader has to be able to see it without counting.
func countLayeredSummaryFailures(records []LayeredSummaryRecord) int {
	failures := 0
	for _, record := range records {
		if record.Error != "" || record.Segments == 0 {
			failures++
		}
	}
	return failures
}

// writeOneLayeredSummary summarizes one conversation and records what was
// written.
//
// The batch is read from the conversation itself: the first message's id and
// the last message's id. Nothing else decides where the batch starts and ends,
// so the segments cover exactly the turns the preparation wrote.
func writeOneLayeredSummary(ctx context.Context, s *Sandbox, userID int64, sessionID string,
	generator agent.SegmentGenerator, settings SegmentSettings) (LayeredSummaryRecord, error) {
	record := LayeredSummaryRecord{SessionID: sessionID}
	var first, last uint
	if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		// The batch is the conversation's own stretch. The messages belong to
		// the session, and the session belongs to the user - the message row
		// itself carries no user column, so the ownership is checked on the
		// session.
		var owned int64
		if err := tx.Model(&model.Session{}).
			Where("id = ? AND user_id = ?", sessionID, userID).Count(&owned).Error; err != nil {
			return err
		}
		if owned != 1 {
			return fmt.Errorf("the conversation %s is not this learner's", sessionID)
		}
		var ids []uint
		if err := tx.Model(&model.Message{}).
			Where("session_id = ?", sessionID).
			Order("id ASC").Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("the conversation %s has no message to summarize", sessionID)
		}
		first, last = ids[0], ids[len(ids)-1]
		return nil
	}); err != nil {
		return record, err
	}

	repository := newSegmentRepository(s)
	producer, err := agent.NewModelSummarySegmentProducer(repository, repository, generator,
		agent.SummarySegmentProducerConfig{
			MaxMessages:     settings.MaxMessages,
			MaxInputTokens:  settings.MaxInputTokens,
			MaxOutputTokens: settings.MaxOutputTokens,
			FetchLimit:      settings.FetchLimit,
		})
	if err != nil {
		return record, err
	}
	job := &model.SessionSummarySegmentJob{
		UserID: userID, SessionID: sessionID,
		StartMessageID: first, DesiredMessageID: last, BatchMessageID: last,
	}
	if _, err := producer.ProduceSegment(ctx, job); err != nil {
		return record, fmt.Errorf("the layered summary of %s failed: %w", sessionID, err)
	}

	// What was written is read back, so the record says what is in the
	// database and not what the producer reported. A producer that wrote
	// nothing and said it did would otherwise be taken at its word.
	if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		var segments []model.SessionSummarySegment
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Order("seq ASC").Find(&segments).Error; err != nil {
			return err
		}
		record.Segments = len(segments)
		for _, segment := range segments {
			if segment.EndMessageID > record.CoveredMessageID {
				record.CoveredMessageID = segment.EndMessageID
			}
			content, decodeErr := model.DecodeSessionSummarySegmentContent(segment.Content)
			if decodeErr != nil {
				continue
			}
			record.Points += len(content.Points)
		}
		var board model.SessionWhiteboard
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			First(&board).Error; err == nil {
			record.Whiteboard = true
			record.WhiteboardCoveredMessageID = board.CoveredMessageID
		} else if err != gorm.ErrRecordNotFound {
			return err
		}
		return nil
	}); err != nil {
		return record, err
	}
	return record, nil
}

// segmentRepository is the product's own segment store, reached through the
// evaluation's guarded transaction.
//
// The methods are the product's: the producer reads the turns, reads the board
// and writes the segment with the board through the DAO, so the board's rules
// about ownership, versions and coverage hold exactly as they hold in a
// deployment. The adapter only carries the transaction.
type segmentRepository struct {
	sandbox *Sandbox
}

func newSegmentRepository(s *Sandbox) *segmentRepository {
	return &segmentRepository{sandbox: s}
}

func (r *segmentRepository) ListSummarySegmentMessages(ctx context.Context, userID int64, sessionID string,
	startMessageID, endMessageID uint, limit int) ([]model.Message, error) {
	var out []model.Message
	err := (guardedStore{sandbox: r.sandbox}).with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = sessiondao.NewRepository(tx).ListSummarySegmentMessages(ctx, userID, sessionID, startMessageID, endMessageID, limit)
		return err
	})
	return out, err
}

func (r *segmentRepository) GetWhiteboard(ctx context.Context, userID int64, sessionID string) (*model.SessionWhiteboard, error) {
	var out *model.SessionWhiteboard
	err := (guardedStore{sandbox: r.sandbox}).with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = sessiondao.NewRepository(tx).GetWhiteboard(ctx, userID, sessionID)
		return err
	})
	return out, err
}

func (r *segmentRepository) GetSummarySegmentWatermark(ctx context.Context, userID int64, sessionID string) (uint, int, error) {
	var covered uint
	var seq int
	err := (guardedStore{sandbox: r.sandbox}).with(ctx, func(tx *gorm.DB) error {
		var err error
		covered, seq, err = sessiondao.NewRepository(tx).GetSummarySegmentWatermark(ctx, userID, sessionID)
		return err
	})
	return covered, seq, err
}

func (r *segmentRepository) GetLatestSummarySegment(ctx context.Context, userID int64, sessionID string) (*model.SessionSummarySegment, error) {
	var out *model.SessionSummarySegment
	err := (guardedStore{sandbox: r.sandbox}).with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = sessiondao.NewRepository(tx).GetLatestSummarySegment(ctx, userID, sessionID)
		return err
	})
	return out, err
}

func (r *segmentRepository) SaveSummarySegmentAndWhiteboard(ctx context.Context, segment *model.SessionSummarySegment,
	board *model.SessionWhiteboard, expectedVersion uint64) error {
	return (guardedStore{sandbox: r.sandbox}).with(ctx, func(tx *gorm.DB) error {
		return sessiondao.NewRepository(tx).SaveSummarySegmentAndWhiteboard(ctx, segment, board, expectedVersion)
	})
}

func (r *segmentRepository) NextSummarySegmentSeq(ctx context.Context, userID int64, sessionID string) (int, error) {
	var out int
	err := (guardedStore{sandbox: r.sandbox}).with(ctx, func(tx *gorm.DB) error {
		var err error
		out, err = sessiondao.NewRepository(tx).NextSummarySegmentSeq(ctx, userID, sessionID)
		return err
	})
	return out, err
}

// CardInputEvidence says what the card was written FROM.
//
// The card path reads the conversation's own compressed state: the summary
// segments and the whiteboard. A card written from the raw messages alone is
// a different measurement - the compressed state was not there - and a report
// that could not tell the two apart would claim the card path was measured
// when it was not.
//
// The numbers are read out of the database the card was projected in, and the
// watermark is the card row's own: it is what the card itself says it
// covered.
type CardInputEvidence struct {
	// Segments is how many summary segments the conversation had inside the
	// card's window.
	Segments int `json:"segments"`
	// Whiteboard says the conversation had a whiteboard inside the card's
	// window.
	Whiteboard bool `json:"whiteboard"`
	// SegmentPoints is how many summary points those segments carry. A
	// segment with no points says nothing, so the count is the number of
	// points and not the number of segments.
	SegmentPoints int `json:"segment_points"`
	// SummaryWatermark is how far the compressed state the card read reaches.
	// It is the card row's own watermark, and it is zero when the card was
	// written from the raw messages alone.
	SummaryWatermark uint `json:"summary_watermark"`
	// RawMessages is how many raw messages the card was shown. A card that
	// read a compressed state shows the tail the compression did not cover,
	// and a card that read nothing shows the whole conversation.
	RawMessages int `json:"raw_messages"`
}

// ReadCardInput reads the compressed state one conversation had inside the
// card's window.
//
// The window is the card's own target: the card may not reach past it, and
// the evidence loader reads exactly what is inside it. So the count here is
// the count the card saw.
func ReadCardInput(ctx context.Context, s *Sandbox, userID int64, sessionID string,
	targetMessageID uint) (CardInputEvidence, error) {
	evidence := CardInputEvidence{}
	if s == nil || s.db == nil {
		return evidence, fmt.Errorf("the evaluation database is unavailable")
	}
	if userID <= 0 || sessionID == "" {
		return evidence, fmt.Errorf("the card input needs an identity and a conversation")
	}
	err := (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		var segments []model.SessionSummarySegment
		if err := tx.Where("user_id = ? AND session_id = ? AND end_message_id <= ?", userID, sessionID, targetMessageID).
			Order("seq ASC").Find(&segments).Error; err != nil {
			return err
		}
		evidence.Segments = len(segments)
		for _, segment := range segments {
			content, decodeErr := model.DecodeSessionSummarySegmentContent(segment.Content)
			if decodeErr != nil {
				continue
			}
			evidence.SegmentPoints += len(content.Points)
			if segment.EndMessageID > evidence.SummaryWatermark {
				evidence.SummaryWatermark = segment.EndMessageID
			}
		}
		var board model.SessionWhiteboard
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			First(&board).Error; err == nil {
			if board.CoveredMessageID <= targetMessageID {
				evidence.Whiteboard = true
				if board.CoveredMessageID > evidence.SummaryWatermark {
					evidence.SummaryWatermark = board.CoveredMessageID
				}
			}
		} else if err != gorm.ErrRecordNotFound {
			return err
		}
		// The raw messages the card was shown are the ones the compressed
		// state does not cover. The messages belong to the session, and the
		// session belongs to the user - the message row itself carries no
		// user column.
		var raw int64
		if err := tx.Model(&model.Message{}).
			Where("session_id = ? AND id > ? AND id <= ?",
				sessionID, evidence.SummaryWatermark, targetMessageID).
			Count(&raw).Error; err != nil {
			return err
		}
		evidence.RawMessages = int(raw)
		return nil
	})
	return evidence, err
}

// LayeredSummarySettings reads the layered summary's numbers out of the
// product's own configuration.
//
// The numbers are the product's, not the harness's: a harness that used its
// own would measure a deployment that does not exist. The effective input
// budget is the product's own effective one, because the mode that records the
// assistant's products reads far more than the mode that does not.
func LayeredSummarySettings(cfg *config.Config) SegmentSettings {
	if cfg == nil {
		return SegmentSettings{}
	}
	agent := cfg.AgentConfig
	return SegmentSettings{
		MaxMessages:      agent.SummarySegmentMaxMessages,
		FetchLimit:       agent.SummarySegmentFetchLimit,
		MaxInputTokens:   agent.EffectiveSummarySegmentMaxInputTokens(),
		MaxOutputTokens:  agent.SummarySegmentMaxOutputTokens,
		SegmentTokens:    agent.EffectiveSegmentTokens(),
		AssistantContent: agent.SummaryAssistantContentEnabled(),
	}
}

// SegmentSettings is what the layered summary needs.
//
// The numbers are the product's own configuration, read from the same settings
// the product boots with. A harness that used different ones would measure a
// deployment that does not exist.
type SegmentSettings struct {
	// MaxMessages is how many turns one batch may hold.
	MaxMessages int
	// FetchLimit is how many turns are read to fill the batch. It is at
	// least MaxMessages.
	FetchLimit int
	// MaxInputTokens and MaxOutputTokens are the generator's limits.
	MaxInputTokens  int
	MaxOutputTokens int
	// SegmentTokens is what one rendered segment may cost.
	SegmentTokens int
	// AssistantContent says the summary records what the assistant produced
	// as well as what the learner said.
	AssistantContent bool
}

// Validate checks the settings.
func (s SegmentSettings) Validate() error {
	if s.MaxMessages < 1 || s.MaxMessages > 512 {
		return fmt.Errorf("the layered summary's batch size is invalid")
	}
	if s.FetchLimit < s.MaxMessages || s.FetchLimit > 1024 {
		return fmt.Errorf("the layered summary's fetch limit is invalid")
	}
	if s.MaxInputTokens < 512 || s.MaxInputTokens > 100000 {
		return fmt.Errorf("the layered summary's input limit is invalid")
	}
	if s.MaxOutputTokens < 128 || s.MaxOutputTokens > 8000 {
		return fmt.Errorf("the layered summary's output limit is invalid")
	}
	if s.SegmentTokens < 1 {
		return fmt.Errorf("the layered summary's segment budget is invalid")
	}
	return nil
}
