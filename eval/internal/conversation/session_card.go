package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	episodeDao "GopherAI/dao/episode"
	"GopherAI/entry"
	"GopherAI/model"
	episodeService "GopherAI/service/episode"
	"gorm.io/gorm"
)

// The session card arm.
//
// The old arm asks its question in the conversation the memory was written
// from, so what it measures is the memory of the present conversation. A card
// answers a different question - "where was that, and what happened there" -
// and it is read in the next conversation, so the arm has to ask in another
// conversation too.
//
// Three things separate an arm that measured the card path from an arm that
// measured an empty window:
//
//   - the projection is published on purpose and the index is waited for, so
//     the question is asked against a memory that exists;
//   - the question is asked in a second conversation of the same learner, so
//     the retrieval is a cross-session one;
//   - what the answer actually used is written down: the card that was
//     shown, the segments that were browsed, the hits that were searched and
//     the messages that were read back.
//
// The two arms are kept apart by two levers, and both of them have to be
// used: the card index has its own namespace per arm, so one arm's cards are
// not in the other arm's searches, and the learner's identity carries the
// arm, so the two arms do not share a database.

const (
	// CardArmModeOff is the old path: the cross-session memory is an Episode.
	CardArmModeOff = "disabled"
	// CardArmModeOn is the new path: the cross-session memory is a card.
	CardArmModeOn = "enabled"
	// cardArmDefaultWait is how long the arm waits for the card to be
	// indexed when the attempt does not say.
	cardArmDefaultWait = 90 * time.Second
)

// CardArm says how one attempt measures the card path.
type CardArm struct {
	// Mode is the product's own switch.
	Mode string `json:"mode"`
	// IndexName and KeyPrefix are the card index's namespace. They differ per
	// arm: one arm's cards must not be visible to the other arm's searches.
	IndexName string `json:"index_name,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	// WaitTimeout is how long the arm waits for the card to be indexed.
	WaitTimeout time.Duration `json:"-"`
}

// Enabled reports whether this attempt runs the card path.
func (a *CardArm) Enabled() bool {
	return a != nil && strings.TrimSpace(a.Mode) == CardArmModeOn
}

// Validate rejects an arm that would measure nothing.
func (a *CardArm) Validate() error {
	if a == nil {
		return nil
	}
	switch strings.TrimSpace(a.Mode) {
	case CardArmModeOff, CardArmModeOn:
	default:
		return fmt.Errorf("session card mode must be %q or %q", CardArmModeOff, CardArmModeOn)
	}
	if a.Enabled() && (strings.TrimSpace(a.IndexName) == "" || strings.TrimSpace(a.KeyPrefix) == "") {
		return fmt.Errorf("the card arm needs its own index namespace")
	}
	if a.WaitTimeout < 0 {
		return fmt.Errorf("the card arm cannot wait for a negative time")
	}
	return nil
}

// Wait returns how long the arm waits for the card to be indexed.
func (a *CardArm) Wait() time.Duration {
	if a == nil || a.WaitTimeout <= 0 {
		return cardArmDefaultWait
	}
	return a.WaitTimeout
}

// CardRow is the card the arm found, as the database holds it.
type CardRow struct {
	ID                  string `json:"id"`
	SessionID           string `json:"session_id"`
	Version             uint64 `json:"version"`
	IndexStatus         string `json:"index_status"`
	Title               string `json:"title"`
	Synopsis            string `json:"synopsis,omitempty"`
	SearchText          string `json:"search_text,omitempty"`
	SourceLastMessageID uint   `json:"source_last_message_id"`
	SummaryWatermark    uint   `json:"source_summary_watermark"`
	SummaryVersion      uint64 `json:"source_summary_version"`
	CitedMessageIDs     []uint `json:"cited_message_ids,omitempty"`
}

// CardArmEvidence is what the arm observed.
type CardArmEvidence struct {
	Mode string `json:"mode"`
	// SourceSessionID is the conversation the memory was written from.
	SourceSessionID string `json:"source_session_id"`
	// QuestionSessionID is the conversation the question was asked in. It is
	// a different conversation on purpose: a question asked in the
	// conversation the memory came from would find the raw messages anyway.
	QuestionSessionID string `json:"question_session_id"`
	// Published says the arm published the projection job itself, and
	// PublishedTargetMessageID is the message the projection was asked to
	// reach. Due says the arm moved that job into the present, so that the
	// product's own worker picks it up instead of waiting out the quiet
	// window.
	Published                bool `json:"published"`
	PublishedTargetMessageID uint `json:"published_target_message_id,omitempty"`
	Due                      bool `json:"due"`
	// WaitMilliseconds is how long the arm waited for the card to be indexed.
	WaitMilliseconds int64 `json:"wait_ms"`
	// Ready says the card was written and indexed before the question was
	// asked. An attempt that measured a window without a card is not a
	// measurement of the card path.
	Ready bool `json:"ready"`
	// Card is the row that was found, when there was one.
	Card *CardRow `json:"card,omitempty"`
	// JobErrorCode is the projection job's own error code. It is how an
	// attempt says "the card was refused because the source was over the
	// input budget" instead of "the card was never written".
	JobErrorCode string `json:"job_error_code,omitempty"`
	// SourceMessages and SourceCharacters are the size of the source
	// conversation the card was made from. The product's own input budget is
	// counted in tokens inside the projector, so this is the size the
	// attempt can state for itself.
	SourceMessages   int `json:"source_messages"`
	SourceCharacters int `json:"source_characters"`
	// RecallSources are the memory rows the answer was given, and RecallKinds
	// says what kind of row each of them was. A card and an Episode are
	// different documents, and an attempt that does not say which one it
	// read has not said what it measured.
	RecallSources []string          `json:"recall_sources,omitempty"`
	RecallKinds   map[string]string `json:"recall_kinds,omitempty"`
	// IndexName and KeyPrefix are the namespace this arm wrote into.
	IndexName string `json:"index_name,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	// Tools are the tool rounds of the answer, as the product recorded them.
	Tools []CardToolStep `json:"tools,omitempty"`
	// BrowsedSegments, SearchHits and ReadBackMessages are what the answer
	// looked at. They are counted from the tool rounds above, because a
	// number that was recomputed from the answer text would be a guess.
	BrowsedSegments  []int  `json:"browsed_segments,omitempty"`
	SearchHits       []uint `json:"search_hits,omitempty"`
	ReadBackMessages []uint `json:"read_back_messages,omitempty"`
	// Error says why the arm could not get a card, when it could not.
	Error string `json:"error,omitempty"`
}

// CardToolStep is one tool round of the answer phase.
type CardToolStep struct {
	Tool      string `json:"tool"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

// The tool names the card chain uses. They are the product's own names, and
// an attempt that counts what the answer looked at has to name the same
// tools the product registered.
const (
	cardToolBrowseSummary = "browse_summary_segments"
	cardToolSearchHistory = "search_session_history"
	cardToolReadHistory   = "read_history_message"
)

// PublishSessionCardProjection publishes the projection job for one session.
//
// The product publishes this job when a conversation ends. The harness writes
// the conversation without running an agent, so nothing published it, and a
// projection nobody published is a projection that never runs: the arm would
// measure a window without a card and call it a result.
//
// The quiet window is the arm's own. The product's default waits for the
// conversation to go quiet, and an arm that waits for that would wait for
// the length of the attempt.
func PublishSessionCardProjection(ctx context.Context, db *gorm.DB, userID int64, sessionID string,
	targetMessageID uint, quiet time.Duration) error {
	if db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" || targetMessageID == 0 {
		return fmt.Errorf("invalid session card projection identity")
	}
	if quiet < 0 {
		return fmt.Errorf("the projection quiet window cannot be negative")
	}
	publisher, err := episodeService.NewPublisher(episodeDao.NewRepository(db), quiet)
	if err != nil {
		return err
	}
	return publisher.PublishConversationCompleted(ctx, userID, sessionID, targetMessageID)
}

// MakeProjectionDue lets the projection run now.
//
// The product publishes a projection with the quiet window the deployment
// configured, and the harness configures a day so that nothing runs during an
// attempt. The row the arm published therefore sits in the future, and the
// product's own worker - the thing that really writes the card and indexes it
// - never sees it. The row cannot be moved by publishing again either: the
// job's upsert only moves the deadline when the desired message id grows, and
// the arm's target is the session's last message.
//
// So the arm moves the row it published into the present. It writes nothing
// else: the projection, the card and the indexing are all the product's own
// work, run by the product's own worker.
func MakeProjectionDue(ctx context.Context, db *gorm.DB, userID int64, sessionID string) error {
	if db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("invalid projection identity")
	}
	result := db.WithContext(ctx).Model(&model.EpisodeProjectionJob{}).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Update("available_at", time.Now().UTC())
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("the projection job was not published")
	}
	return nil
}

// WaitForSessionCard waits until the session has a card that is indexed.
//
// The projector writes the card and marks it pending; the indexer marks it
// indexed. A question asked between the two would measure a window whose
// card the search cannot see yet, so the arm waits for the second one.
//
// The job's own error code is returned with the failure: a card that was
// refused because the source was over the input budget is a different fact
// from a card that was never written.
func WaitForSessionCard(ctx context.Context, db *gorm.DB, userID int64, sessionID string,
	timeout time.Duration) (*model.EpisodeMemory, string, error) {
	if db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, "", fmt.Errorf("invalid session card identity")
	}
	if timeout <= 0 {
		timeout = cardArmDefaultWait
	}
	deadline := time.Now().Add(timeout)
	for {
		var card model.EpisodeMemory
		err := db.WithContext(ctx).
			Where("user_id = ? AND session_id = ? AND content_kind = ?",
				userID, sessionID, model.EpisodeContentSessionCard).
			Order("version DESC").Take(&card).Error
		if err == nil && card.IndexStatus == model.EpisodeIndexIndexed {
			return &card, "", nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
		if code := projectionJobErrorCode(ctx, db, userID, sessionID); code != "" {
			return nil, code, fmt.Errorf("the session card projection failed: %s", code)
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("the session has no indexed card after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// projectionJobErrorCode reads the projection job's own error code.
//
// The job row is the only place that says why a card was not written: the
// card path has its own codes, and the shared worker writes the code of the
// path that failed.
func projectionJobErrorCode(ctx context.Context, db *gorm.DB, userID int64, sessionID string) string {
	var job model.EpisodeProjectionJob
	err := db.WithContext(ctx).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Order("updated_at DESC").Take(&job).Error
	if err != nil {
		return ""
	}
	return strings.TrimSpace(job.ErrorCode)
}

// CreateQuestionSession opens the conversation the question is asked in.
//
// The card answers "where was that, and what happened there", and it is read
// in the next conversation. A question asked in the conversation the memory
// was written from would find the raw messages anyway, and the arm would
// measure the wrong thing.
func CreateQuestionSession(ctx context.Context, db *gorm.DB, userID int64, userName, sessionID string) error {
	if db == nil || userID <= 0 || strings.TrimSpace(userName) == "" || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("invalid question session identity")
	}
	session := model.Session{ID: sessionID, UserID: userID, UserName: userName,
		Mode: model.SessionModeAgent, Title: "Evaluation question"}
	return db.WithContext(ctx).Create(&session).Error
}

// CardRowOf reads the card row the arm found into the record the attempt
// keeps.
func CardRowOf(card *model.EpisodeMemory) *CardRow {
	if card == nil {
		return nil
	}
	row := &CardRow{
		ID: card.ID, SessionID: card.SessionID, Version: card.Version,
		IndexStatus: string(card.IndexStatus), Title: card.Title,
		SearchText: card.SearchText, SourceLastMessageID: card.SourceLastMessageID,
		SummaryWatermark: card.SourceSummaryWatermark, SummaryVersion: card.SourceSummaryVersion,
	}
	if !card.Card.IsZero() {
		row.Synopsis = card.Card.Synopsis
		row.CitedMessageIDs = card.Card.CitedMessageIDs()
	}
	return row
}

// ReadCardArmSteps reads the tool rounds of the answer and counts what the
// answer looked at.
//
// The counts come from the product's own step record: the tool name is what
// was called, the arguments are what it was called with and the result is
// what it brought back. A count that was recomputed from the answer text
// would be a reading of the text, and the text is not the record.
func ReadCardArmSteps(steps []model.AgentStep) (tools []CardToolStep, browsed []int, hits []uint, readback []uint) {
	for _, step := range steps {
		if step.Type != model.AgentStepTool {
			continue
		}
		tools = append(tools, CardToolStep{Tool: step.ToolName, Arguments: step.ArgumentsSummary,
			Result: step.ResultSummary, Error: step.ErrorMessage})
		switch step.ToolName {
		case cardToolBrowseSummary:
			browsed = append(browsed, browsedSegmentNumbers(step.ArgumentsSummary)...)
		case cardToolSearchHistory:
			hits = append(hits, searchedMessageNumbers(step.ResultSummary)...)
		case cardToolReadHistory:
			readback = append(readback, readBackMessageNumbers(step.ArgumentsSummary)...)
		}
	}
	return tools, browsed, hits, readback
}

var (
	segmentNumberPattern = regexp.MustCompile(`"segments"\s*:\s*\[([^\]]*)\]`)
	numberListPattern    = regexp.MustCompile(`\d+`)
	messageIDPattern     = regexp.MustCompile(`"message_id"\s*:\s*(\d+)`)
)

// browsedSegmentNumbers reads the segment numbers a browse call named.
func browsedSegmentNumbers(arguments string) []int {
	match := segmentNumberPattern.FindStringSubmatch(arguments)
	if len(match) != 2 {
		return nil
	}
	var out []int
	for _, field := range strings.Split(match[1], ",") {
		value, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || value <= 0 {
			continue
		}
		out = append(out, value)
	}
	return out
}

// searchedMessageNumbers reads the message ids a search brought back.
//
// The result is the tool's own observation, truncated by the product, so the
// numbers are read out of it rather than parsed as a whole document.
func searchedMessageNumbers(result string) []uint {
	var out []uint
	for _, match := range messageIDPattern.FindAllStringSubmatch(result, -1) {
		value, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil || value == 0 {
			continue
		}
		out = append(out, uint(value))
	}
	return out
}

// readBackMessageNumbers reads the message ids a read-back named.
func readBackMessageNumbers(arguments string) []uint {
	start := strings.Index(arguments, `"message_ids"`)
	if start < 0 {
		return nil
	}
	rest := arguments[start:]
	open := strings.Index(rest, "[")
	close := strings.Index(rest, "]")
	if open < 0 || close < 0 || close < open {
		return nil
	}
	var out []uint
	for _, field := range numberListPattern.FindAllString(rest[open+1:close], -1) {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil || value == 0 {
			continue
		}
		out = append(out, uint(value))
	}
	return out
}

// ApplyCardArm puts the arm's switch into the product's own configuration.
//
// The product reads the switch once, at startup, and the arm has to be
// visible to it before the configuration is validated: a run that asked for a
// mode the product does not know has to fail before it spends anything.
//
// The quiet window is deliberately left alone. The harness sets it to a day so
// that the product's own publications never run during an attempt, and the arm
// publishes the projection itself with a zero window - the projection it
// drives is the only one that is allowed to run, and it runs in the
// preparation phase, before the question is asked.
func ApplyCardArm(settings *entry.Settings, arm *CardArm) error {
	if settings == nil {
		return fmt.Errorf("the product settings are required")
	}
	if err := arm.Validate(); err != nil {
		return err
	}
	if arm == nil {
		return nil
	}
	settings.Config.AgentConfig.SessionCardMode = strings.TrimSpace(arm.Mode)
	if arm.Enabled() {
		settings.Config.AgentConfig.SessionCardIndexName = strings.TrimSpace(arm.IndexName)
		settings.Config.AgentConfig.SessionCardIndexKeyPrefix = strings.TrimSpace(arm.KeyPrefix)
	}
	return nil
}

// QuestionSessionID is the conversation the card arm asks its question in.
//
// It is derived from the source conversation rather than written by hand, so
// that a run cannot ask the same question in the conversation the memory came
// from. The name is short on purpose: the product keeps a session id in a
// varchar(36), and a source id that already fills that column would not leave
// room for a suffix.
func QuestionSessionID(sourceSessionID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sourceSessionID)))
	return "eval-q-" + hex.EncodeToString(sum[:6])
}

// DriveSessionCard publishes the projection, waits for the card and opens the
// conversation the question is asked in.
//
// The order is the point. The projection is published before the question
// session exists, so the card cannot be written from the question's own
// conversation. The card is waited for before the question is asked, so the
// question is asked against a memory that exists rather than against a window
// that would have had one. The question session is opened last, so it holds
// nothing but the question.
func DriveSessionCard(ctx context.Context, sandbox *Sandbox, snapshot Snapshot, arm *CardArm) (*CardArmEvidence, error) {
	if err := arm.Validate(); err != nil {
		return nil, err
	}
	evidence := &CardArmEvidence{
		Mode: arm.Mode, SourceSessionID: snapshot.Session.ID,
		IndexName: arm.IndexName, KeyPrefix: arm.KeyPrefix,
	}
	var target uint
	for _, message := range snapshot.Messages {
		evidence.SourceMessages++
		evidence.SourceCharacters += len(message.Content)
		if message.ID > target {
			target = message.ID
		}
	}
	if target == 0 {
		evidence.Error = "the source conversation has no messages"
		return evidence, fmt.Errorf("%s", evidence.Error)
	}
	evidence.PublishedTargetMessageID = target
	if err := sandbox.PublishSessionCardProjection(ctx, snapshot.Session.UserID, snapshot.Session.ID, target); err != nil {
		evidence.Error = err.Error()
		return evidence, err
	}
	evidence.Published = true
	if err := sandbox.MakeProjectionDue(ctx, snapshot.Session.UserID, snapshot.Session.ID); err != nil {
		evidence.Error = err.Error()
		return evidence, err
	}
	evidence.Due = true
	started := time.Now()
	card, code, err := sandbox.WaitForSessionCard(ctx, snapshot.Session.UserID, snapshot.Session.ID, arm.Wait())
	evidence.WaitMilliseconds = time.Since(started).Milliseconds()
	if err != nil {
		evidence.JobErrorCode = code
		evidence.Error = err.Error()
		return evidence, err
	}
	evidence.Ready = true
	evidence.Card = CardRowOf(card)
	questionSession := QuestionSessionID(snapshot.Session.ID)
	if err := sandbox.CreateQuestionSession(ctx, snapshot.Session.UserID, snapshot.Session.UserName, questionSession); err != nil {
		evidence.Error = err.Error()
		return evidence, err
	}
	evidence.QuestionSessionID = questionSession
	return evidence, nil
}

// ReadCardArmRecall reads the memory the answer was given.
//
// The recall is a step of the product's own run, and its source ids are the
// memory rows the answer was built on. A card and an Episode are told apart
// by looking the ids up: the run says what it read, and the database says
// what kind of row that was.
func ReadCardArmRecall(steps []model.AgentStep) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, step := range steps {
		if step.Type != model.AgentStepRetrieve {
			continue
		}
		for _, id := range step.SourceIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// ClassifyRecallSources says what kind of memory the answer was given.
//
// A card and an Episode are different documents, and an attempt that does not
// say which one it read has not said what it measured. The answer is read
// from the rows themselves, not from the arm's own name.
func ClassifyRecallSources(ctx context.Context, db *gorm.DB, userID int64, ids []string) (kinds map[string]string, err error) {
	kinds = map[string]string{}
	if db == nil || len(ids) == 0 {
		return kinds, nil
	}
	var rows []model.EpisodeMemory
	if err = db.WithContext(ctx).Where("user_id = ? AND id IN ?", userID, ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		kinds[row.ID] = string(row.ContentKind)
	}
	return kinds, nil
}

// recordCardArmEvidence reads what the answer looked at.
//
// The counts come from the product's own step record: the tool name is what
// was called, the arguments are what it was called with and the result is
// what it brought back. A count that was recomputed from the answer text
// would be a reading of the text, and the text is not the record.
func recordCardArmEvidence(evidence *CardArmEvidence, sandbox *Sandbox, userID int64, questions []QuestionResult) error {
	if evidence == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, asked := range questions {
		tools, browsed, hits, readback := ReadCardArmSteps(asked.Steps)
		evidence.Tools = append(evidence.Tools, tools...)
		evidence.BrowsedSegments = append(evidence.BrowsedSegments, browsed...)
		evidence.SearchHits = append(evidence.SearchHits, hits...)
		evidence.ReadBackMessages = append(evidence.ReadBackMessages, readback...)
		for _, source := range ReadCardArmRecall(asked.Steps) {
			if _, exists := seen[source]; exists {
				continue
			}
			seen[source] = struct{}{}
			evidence.RecallSources = append(evidence.RecallSources, source)
		}
	}
	if len(evidence.RecallSources) == 0 || sandbox == nil {
		return nil
	}
	kinds, err := ClassifyRecallSources(context.Background(), sandbox.db, userID, evidence.RecallSources)
	if err != nil {
		return err
	}
	evidence.RecallKinds = kinds
	return nil
}
