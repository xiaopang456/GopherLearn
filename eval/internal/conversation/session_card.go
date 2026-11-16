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

	"GopherAI/common/memoryrecall"
	episodeDao "GopherAI/dao/episode"
	"GopherAI/entry"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	episodeService "GopherAI/service/episode"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// CardArm says how one attempt measures the cross-session memory path.
//
// The name is historical: the arm was first written for the card path alone.
// It now covers both paths, because a comparison that measures the card path
// properly and the Episode path carelessly is not a comparison. The two arms
// differ in WHICH document carries the memory, and in nothing else that the
// harness controls:
//
//   - the card arm projects a session card and reads it back;
//   - the Episode arm projects an Episode and injects it.
//
// Both publish the projection, wait for it to be indexed, open a second
// conversation and ask the question there. The read mode and the recall mode
// are written into the product's own configuration and exported with the
// attempt, so a reader can see what each arm actually ran.
type CardArm struct {
	// Mode is the product's own switch.
	Mode string `json:"mode"`
	// IndexName and KeyPrefix are the card index's namespace. They differ per
	// arm: one arm's cards must not be visible to the other arm's searches.
	IndexName string `json:"index_name,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	// MemoryReadMode is the product's read path. It is the same for both arms
	// (legacy_v2 reads Episodes and is the only mode that injects them), and
	// it is written into the product's configuration rather than left to the
	// product's default: a run that does not say which read path it measured
	// has not said what it measured.
	MemoryReadMode string `json:"memory_read_mode,omitempty"`
	// EpisodeRecallMode is the product's Episode recall switch. The Episode
	// arm needs "inject_enabled" or nothing is injected; the card arm needs
	// "disabled", because the card path replaces the Episode path and the
	// two would otherwise both answer the same question about the same
	// conversation.
	EpisodeRecallMode string `json:"episode_recall_mode,omitempty"`
	// WaitTimeout is how long the arm waits for the memory to be indexed.
	WaitTimeout time.Duration `json:"-"`
}

// Enabled reports whether this attempt runs the card path.
func (a *CardArm) Enabled() bool {
	return a != nil && strings.TrimSpace(a.Mode) == CardArmModeOn
}

// MemoryKind is the content kind this arm's memory is.
//
// The arm waits for a row of this kind and no other, so a card arm cannot
// report an Episode as its memory and an Episode arm cannot report a card.
func (a *CardArm) MemoryKind() string {
	if a.Enabled() {
		return string(model.EpisodeContentSessionCard)
	}
	return string(model.EpisodeContentEpisode)
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
	if mode := strings.TrimSpace(a.MemoryReadMode); mode != "" {
		if _, err := memoryrecall.ParseReadMode(mode); err != nil {
			return fmt.Errorf("memory read mode %q is not one the product knows", mode)
		}
	}
	if mode := strings.TrimSpace(a.EpisodeRecallMode); mode != "" &&
		mode != string(agentService.EpisodeRecallDisabled) &&
		mode != string(agentService.EpisodeRecallShadow) &&
		mode != string(agentService.EpisodeRecallInject) {
		return fmt.Errorf("episode recall mode %q is not one the product knows", mode)
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
	// ContentKind says whether this row is an Episode or a session card.
	ContentKind string `json:"content_kind,omitempty"`
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
	// SourceSessions are the conversations the attempt carries. They are all
	// of them, not only the one the question is about: the retrieval chooses
	// between them, and an attempt that says which ones were there is an
	// attempt whose retrieval can be read.
	SourceSessions []string `json:"source_sessions,omitempty"`
	// Memories are the rows the arm waited for, one per conversation.
	Memories []*CardRow `json:"memories,omitempty"`
	// WaitMilliseconds is how long the arm waited for the card to be indexed.
	WaitMilliseconds int64 `json:"wait_ms"`
	// Ready says the card was written and indexed before the question was
	// asked. An attempt that measured a window without a card is not a
	// measurement of the card path.
	Ready bool `json:"ready"`
	// Card is the row that was found, when there was one.
	Card *CardRow `json:"card,omitempty"`
	// Memory is the row the arm waited for, whatever kind it was. It is the
	// same row as Card for the card arm, and the Episode for the old arm.
	Memory *CardRow `json:"memory,omitempty"`
	// MemoryKind says which kind of document this arm's memory is
	// ("episode" or "session_card"). A reader that does not know which
	// document answered the question has not said what it measured.
	MemoryKind string `json:"memory_kind,omitempty"`
	// MemoryReadMode, EpisodeRecallMode and SessionCardMode are the switches
	// the product actually ran with. They are exported so that a reader can
	// tell the two arms apart by what they ran, not by what they are called.
	MemoryReadMode    string `json:"memory_read_mode,omitempty"`
	EpisodeRecallMode string `json:"episode_recall_mode,omitempty"`
	SessionCardMode   string `json:"session_card_mode,omitempty"`
	// ToolTable is the set of tools the answer's model was offered, read out
	// of the request that was sent. The two arms do not have the same table:
	// the card path registers a cross-session search tool the Episode path
	// does not, and a comparison that does not say so would be comparing
	// something the reader cannot see.
	ToolTable []string `json:"tool_table,omitempty"`
	// ToolUsage says what happened to every tool the comparison asks about,
	// one row per tool, with the three states kept apart: not registered,
	// registered but not offered in this request, and offered but not called.
	// The three are different findings, and a reader who collapses them reads
	// "the model did not use the tool" into a sentence about the product.
	ToolUsage []ToolUsageEntry `json:"tool_usage,omitempty"`
	// SearchTrace is the retrieval trace of the answer: the intent the model
	// expressed, the tool's own verdict and the records it brought back. It
	// is read out of the tool rounds the product persisted, so "the search
	// ran and found nothing" and "the search never ran" are told apart.
	SearchTrace []SearchTrace `json:"search_trace,omitempty"`
	// InjectedText is the cross-session memory the answer was actually given,
	// as it was sent to the model. It is what separates "the memory was
	// there" from "the memory was there and said this".
	InjectedText string `json:"injected_text,omitempty"`
	// InjectedMessageIDs are the messages the injected block cited, and
	// InjectedSessions are the conversations those messages were in. The
	// citations are the block's own, so the conversations are read out of
	// the messages rather than out of the arm's own bookkeeping.
	InjectedMessageIDs []uint   `json:"injected_message_ids,omitempty"`
	InjectedSessions   []string `json:"injected_sessions,omitempty"`
	// InjectedDeclaredSessions are the conversations the block named itself.
	// A card names the conversation it was written from; an Episode block
	// does not, and its source has to be looked up from the rows. The field
	// is always there so that "the block named none" is a fact on the record
	// rather than an absent key.
	InjectedDeclaredSessions []string `json:"injected_declared_sessions"`
	// InjectedSourceCheck says whether the injected memory came from the
	// arm's own conversations: "within", "outside" or "unknown".
	//
	// It is three states and not a boolean because "the learner was shown
	// something they were not granted" and "nothing was shown" are different
	// findings, and a boolean says both. The check does not trust the
	// block's own claim: the cited messages are looked up in the database
	// and the conversations they are in are the ones that are checked.
	InjectedSourceCheck string `json:"injected_source_check,omitempty"`
	// ShownCard says the answer was given the card block, and
	// ShownCardSessions are the source conversations that block named.
	//
	// It is read out of the prompt that was actually sent to the model,
	// because that prompt is the record of what the model was shown. The
	// product keeps the ids of the cards it showed in the window, but it
	// does not persist them, so there is no other place to read this from.
	ShownCard         bool     `json:"shown_card"`
	ShownCardSessions []string `json:"shown_card_sessions,omitempty"`
	// ShownCardObserved says the attempt could read that prompt. A card arm
	// that could not look at the prompt has not shown a card and has not
	// shown the absence of one either, and the two are different facts.
	ShownCardObserved bool `json:"shown_card_observed"`
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
	// EpisodeSources are the Episodes the answer's message carried as its
	// provenance. The old path fills them; the card path does not, because a
	// card has no Episode ancestors.
	EpisodeSources []string `json:"episode_sources,omitempty"`
	// RecallSources are the memory rows the answer was given, and RecallKinds
	// says what kind of row each of them was. A card and an Episode are
	// different documents, and an attempt that does not say which one it
	// read has not said what it measured.
	RecallSources []string          `json:"recall_sources,omitempty"`
	RecallKinds   map[string]string `json:"recall_kinds,omitempty"`
	// RecallSessions are the source conversations the recalled memories came
	// from, and RecallTrace says how many memories of each kind were
	// recalled. A recall that found nothing is a fact about the retrieval,
	// and it is only a fact about the RETRIEVAL when the memory itself
	// existed - which the Ready flag and the projection error code say.
	RecallSessions []string      `json:"recall_sessions,omitempty"`
	RecallTrace    []RecallEntry `json:"recall_trace,omitempty"`
	// ShownMemoryKind says which memory block the prompt carried: "card",
	// "episode" or "none". It is read out of the prompt the model was sent.
	ShownMemoryKind string `json:"shown_memory_kind,omitempty"`
	// IndexName and KeyPrefix are the namespace this arm wrote into.
	IndexName string `json:"index_name,omitempty"`
	KeyPrefix string `json:"key_prefix,omitempty"`
	// Tools are the tool rounds of the answer, as the product recorded them.
	Tools []CardToolStep `json:"tools,omitempty"`
	// BrowsedSegments, SearchHits and ReadBackMessages are what the answer
	// looked at. They are counted from the tool rounds above, because a
	// number that was recomputed from the answer text would be a guess.
	//
	// The flags say when a count could not be read in full. The product
	// truncates a tool record to a thousand characters, and a truncated
	// record parses into a short list rather than into an error - so an
	// empty list would read as "the answer browsed nothing", which is a
	// different fact.
	BrowsedSegments      []int  `json:"browsed_segments,omitempty"`
	SearchHits           []uint `json:"search_hits,omitempty"`
	ReadBackMessages     []uint `json:"read_back_messages,omitempty"`
	BrowsedTruncated     bool   `json:"browsed_truncated,omitempty"`
	SearchHitsTruncated  bool   `json:"search_hits_truncated,omitempty"`
	ReadBackTruncated    bool   `json:"read_back_truncated,omitempty"`
	ToolRecordsTruncated bool   `json:"tool_records_truncated,omitempty"`
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

// RecallEntry is one memory the answer was given.
type RecallEntry struct {
	// ID is the memory row.
	ID string `json:"id"`
	// Kind is the content kind of that row: "episode" or "session_card".
	Kind string `json:"kind,omitempty"`
	// SourceSession is the conversation the memory was written from.
	SourceSession string `json:"source_session,omitempty"`
	// Title is the row's own title, so a reader can see what was recalled
	// without opening the database.
	Title string `json:"title,omitempty"`
}

// The tool names the card chain uses. They are the product's own names, and
// an attempt that counts what the answer looked at has to name the same
// tools the product registered.
const (
	cardToolBrowseSummary = "browse_summary_segments"
	cardToolSearchHistory = "search_session_history"
	cardToolReadHistory   = "read_history_message"
)

// cardOnlyTool is the tool the card path registers and the old path does not.
//
// It is the product's own registration rule (v2/internal/isolated/wiring.go):
// the cross-session search tool is registered only under the card path,
// because the old path has no source conversation to search in. The product
// assembles its registry inside its own internal wiring, which a harness in
// another module cannot import, so the rule is mirrored here - the same way
// the layered arm's tool table is mirrored.
//
// It is also the one tool whose registration differs between the two arms, so
// it is the one tool whose absence is a fact about the registration rather
// than about the request. Every other tool is registered for both arms, and
// a tool the other arm offered and this one did not is a tool this request
// did not carry.
const cardOnlyTool = cardToolSearchHistory

// registeredToolsFor returns the tools the product registers for this arm and
// this arm's request did not carry.
//
// Only the card-only tool can be named: it is the one tool whose registration
// differs between the arms, and the difference is known from the arm's own
// mode. The rest of the registry is established by the request that carried
// it, because the product cannot offer a tool it does not have.
func registeredToolsFor(memoryKind string) []string {
	if strings.TrimSpace(memoryKind) == string(model.EpisodeContentSessionCard) {
		return []string{cardOnlyTool}
	}
	return nil
}

// toolRecordLimit is the length the product truncates a tool record to.
//
// A record at the limit may have been cut, so a count read out of it may be
// short. The arm says so rather than reporting a short list as if it were
// the whole list.
const toolRecordLimit = 1000

// PublishSessionCardProjection publishes the projection job for one session.
//
// The product publishes this job when a conversation ends. The harness writes
// the conversation without running an agent, so nothing published it, and a
// projection nobody published is a projection that never runs: the arm would
// measure a window without a card and call it a result.
//
// The quiet window is the arm's own. The product's default waits for the
// conversation to go quiet, and an arm that waits for that would wait for the
// length of the attempt.
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
//
// A job that is already running or already finished is left alone. The arm
// publishes with a zero window, so the product's worker can pick the job up
// before this call arrives, and a job that is already on its way is not a
// failure: it is a job that does not need moving. Only a job that cannot run
// at all is reported, because that is a fact the attempt has to carry.
func MakeProjectionDue(ctx context.Context, db *gorm.DB, userID int64, sessionID string) error {
	if db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("invalid projection identity")
	}
	now := time.Now().UTC()
	result := db.WithContext(ctx).Model(&model.EpisodeProjectionJob{}).
		Where("user_id = ? AND session_id = ? AND status = ?", userID, sessionID, model.EpisodeProjectionPending).
		Update("available_at", now)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var job model.EpisodeProjectionJob
	err := db.WithContext(ctx).Where("user_id = ? AND session_id = ?", userID, sessionID).Order("updated_at DESC").Take(&job).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("the projection job was never published")
		}
		return err
	}
	switch job.Status {
	case model.EpisodeProjectionPending:
		return fmt.Errorf("the projection job is pending but could not be moved")
	case model.EpisodeProjectionProcessing, model.EpisodeProjectionCompleted:
		// The worker already has the job, or already had it. Nothing to move.
		return nil
	default:
		return fmt.Errorf("the projection job is %s and cannot be moved: %s", job.Status, job.ErrorCode)
	}
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
	return WaitForArmMemory(ctx, db, userID, sessionID, string(model.EpisodeContentSessionCard), timeout)
}

// WaitForArmMemory waits until the session has a memory of the named kind
// that is indexed.
//
// The kind is part of the wait, and not a detail: the two arms write different
// documents into the same table, and an arm that waited for either kind could
// report the other path's document as its own. A projection that failed must
// not come back as a timeout with nothing to show, so the job's own error
// code is returned with the failure.
func WaitForArmMemory(ctx context.Context, db *gorm.DB, userID int64, sessionID, contentKind string,
	timeout time.Duration) (*model.EpisodeMemory, string, error) {
	if db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, "", fmt.Errorf("invalid arm memory identity")
	}
	if strings.TrimSpace(contentKind) == "" {
		return nil, "", fmt.Errorf("the arm memory kind is required")
	}
	if timeout <= 0 {
		timeout = cardArmDefaultWait
	}
	deadline := time.Now().Add(timeout)
	for {
		var memory model.EpisodeMemory
		err := db.WithContext(ctx).
			Where("user_id = ? AND session_id = ? AND content_kind = ?",
				userID, sessionID, contentKind).
			Order("version DESC").Take(&memory).Error
		if err == nil && memory.IndexStatus == model.EpisodeIndexIndexed {
			return &memory, "", nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", err
		}
		if code := projectionJobErrorCode(ctx, db, userID, sessionID); code != "" {
			return nil, code, fmt.Errorf("the %s projection failed: %s", contentKind, code)
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("the session has no indexed %s after %s", contentKind, timeout)
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
//
// The write is idempotent: a retry of the same attempt asks the same
// question in the same conversation, and a second insert would fail on the
// primary key and report a duplicate rather than the thing that went wrong.
func CreateQuestionSession(ctx context.Context, db *gorm.DB, userID int64, userName, sessionID string) error {
	if db == nil || userID <= 0 || strings.TrimSpace(userName) == "" || strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("invalid question session identity")
	}
	session := model.Session{ID: sessionID, UserID: userID, UserName: userName,
		Mode: model.SessionModeAgent, Title: "Evaluation question"}
	return db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&session).Error
}

// MemoryRowOf reads the memory row the arm found into the record the attempt
// keeps.
//
// A card and an Episode are different documents, so the row says which kind
// it was and carries the fields that kind has. A reader that does not know
// which document answered the question has not said what it measured.
func MemoryRowOf(memory *model.EpisodeMemory) *CardRow {
	if memory == nil {
		return nil
	}
	row := &CardRow{
		ID: memory.ID, SessionID: memory.SessionID, Version: memory.Version,
		IndexStatus: string(memory.IndexStatus), Title: memory.Title,
		SearchText: memory.SearchText, SourceLastMessageID: memory.SourceLastMessageID,
		SummaryWatermark: memory.SourceSummaryWatermark, SummaryVersion: memory.SourceSummaryVersion,
		ContentKind: string(memory.ContentKind),
	}
	if !memory.Card.IsZero() {
		row.Synopsis = memory.Card.Synopsis
		row.CitedMessageIDs = memory.Card.CitedMessageIDs()
	}
	return row
}

// CardRowOf reads the card row the arm found into the record the attempt
// keeps.
func CardRowOf(card *model.EpisodeMemory) *CardRow {
	return MemoryRowOf(card)
}

// CardToolEvidence is what the answer's tool rounds say.
type CardToolEvidence struct {
	Tools    []CardToolStep
	Browsed  []int
	Hits     []uint
	ReadBack []uint
	// The truncation flags say when a count could not be read in full.
	BrowsedTruncated  bool
	HitsTruncated     bool
	ReadBackTruncated bool
	RecordsTruncated  bool
}

// ReadCardArmSteps reads the tool rounds of the answer and counts what the
// answer looked at.
//
// The counts come from the product's own step record: the tool name is what
// was called, the arguments are what it was called with and the result is
// what it brought back. A count that was recomputed from the answer text
// would be a reading of the text, and the text is not the record.
//
// The product truncates a tool record to a thousand characters. A truncated
// record still parses, into a short list, so each count says whether it may
// be short.
func ReadCardArmSteps(steps []model.AgentStep) CardToolEvidence {
	evidence := CardToolEvidence{}
	for _, step := range steps {
		if step.Type != model.AgentStepTool {
			continue
		}
		evidence.Tools = append(evidence.Tools, CardToolStep{Tool: step.ToolName, Arguments: step.ArgumentsSummary,
			Result: step.ResultSummary, Error: step.ErrorMessage})
		argumentsCut := len(step.ArgumentsSummary) >= toolRecordLimit
		resultCut := len(step.ResultSummary) >= toolRecordLimit
		if argumentsCut || resultCut {
			evidence.RecordsTruncated = true
		}
		switch step.ToolName {
		case cardToolBrowseSummary:
			browsed, ok := browsedSegmentNumbers(step.ArgumentsSummary)
			evidence.Browsed = append(evidence.Browsed, browsed...)
			if argumentsCut || !ok {
				evidence.BrowsedTruncated = true
			}
		case cardToolSearchHistory:
			hits, ok := searchedMessageNumbers(step.ResultSummary)
			evidence.Hits = append(evidence.Hits, hits...)
			if resultCut || !ok {
				evidence.HitsTruncated = true
			}
		case cardToolReadHistory:
			readback, ok := readBackMessageNumbers(step.ArgumentsSummary)
			evidence.ReadBack = append(evidence.ReadBack, readback...)
			if argumentsCut || !ok {
				evidence.ReadBackTruncated = true
			}
		}
	}
	return evidence
}

var (
	segmentNumberPattern = regexp.MustCompile(`"segments"\s*:\s*\[([^\]]*)\]`)
	numberListPattern    = regexp.MustCompile(`\d+`)
	messageIDPattern     = regexp.MustCompile(`"message_id"\s*:\s*(\d+)`)
)

// browsedSegmentNumbers reads the segment numbers a browse call named.
func browsedSegmentNumbers(arguments string) ([]int, bool) {
	match := segmentNumberPattern.FindStringSubmatch(arguments)
	if len(match) != 2 {
		return nil, false
	}
	var out []int
	for _, field := range strings.Split(match[1], ",") {
		value, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || value <= 0 {
			continue
		}
		out = append(out, value)
	}
	return out, true
}

// searchedMessageNumbers reads the message ids a search brought back.
//
// The result is the tool's own observation, truncated by the product, so the
// numbers are read out of it rather than parsed as a whole document.
func searchedMessageNumbers(result string) ([]uint, bool) {
	matches := messageIDPattern.FindAllStringSubmatch(result, -1)
	if len(matches) == 0 {
		return nil, false
	}
	var out []uint
	for _, match := range matches {
		value, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil || value == 0 {
			continue
		}
		out = append(out, uint(value))
	}
	return out, true
}

// readBackMessageNumbers reads the message ids a read-back named.
func readBackMessageNumbers(arguments string) ([]uint, bool) {
	start := strings.Index(arguments, `"message_ids"`)
	if start < 0 {
		return nil, false
	}
	rest := arguments[start:]
	open := strings.Index(rest, "[")
	close := strings.Index(rest, "]")
	if open < 0 || close < 0 || close < open {
		return nil, false
	}
	var out []uint
	for _, field := range numberListPattern.FindAllString(rest[open+1:close], -1) {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil || value == 0 {
			continue
		}
		out = append(out, uint(value))
	}
	return out, true
}

// cardSourceSessionPattern is how a card block names the conversation it was
// written from.
//
// It is the marker the product's own card syntax uses, so a prompt that
// carries a card carries this line. The arm reads the prompt that was
// actually sent to the model, because that prompt is the record of what the
// model was shown.
var cardSourceSessionPattern = regexp.MustCompile(`source_session:\s*(\S+)`)

// ReadShownCards reads the card block out of the prompt the model was given.
//
// The product keeps the ids of the cards it showed in the window, but it does
// not persist them, so the prompt is the only place the attempt can learn
// what the answer was shown. A prompt with no card block is a prompt that
// carried no card - which is a fact worth having, because a card arm that
// showed no card measured nothing.
func ReadShownCards(prompt []*schema.Message) (bool, []string) {
	var joined strings.Builder
	for _, message := range prompt {
		if message == nil {
			continue
		}
		joined.WriteString(message.Content)
		joined.WriteString("\n")
	}
	matches := cardSourceSessionPattern.FindAllStringSubmatch(joined.String(), -1)
	if len(matches) == 0 {
		return false, nil
	}
	seen := map[string]struct{}{}
	var sessions []string
	for _, match := range matches {
		session := strings.TrimSpace(match[1])
		if session == "" {
			continue
		}
		if _, exists := seen[session]; exists {
			continue
		}
		seen[session] = struct{}{}
		sessions = append(sessions, session)
	}
	return true, sessions
}

// CardObservation is what an attempt could learn about the card the answer
// was shown.
//
// It is a separate type from a boolean because "the prompt carried no card"
// and "the attempt never saw the prompt" are different facts, and an attempt
// that reports them as one has measured nothing either way.
type CardObservation struct {
	// Known says the attempt could read the prompt the model was sent.
	Known bool
	// Sessions are the source conversations the card block named.
	Sessions []string
}

// Shown says the answer was given a card block.
//
// A card block always names the conversation it was written from, so a
// prompt with named sessions carried a card and a prompt without them did
// not.
func (o CardObservation) Shown() bool {
	return o.Known && len(o.Sessions) > 0
}

// ObserveCardMessages reads the card observation out of a prompt.
func ObserveCardMessages(messages []*schema.Message) CardObservation {
	_, sessions := ReadShownCards(messages)
	return CardObservation{Known: true, Sessions: sessions}
}

// ObserveCardCalls reads the card observation out of the recorded model calls.
//
// The request body is the record of what the model was shown, and the ledger
// keeps the card sessions of every request it saw. Only the answer's own
// calls count: a card that is being written is not a card that was shown.
func ObserveCardCalls(calls []ModelCall) CardObservation {
	observation := CardObservation{Known: true}
	seen := map[string]struct{}{}
	for _, call := range calls {
		if call.Role != "answer" {
			continue
		}
		for _, session := range call.CardSessions {
			session = strings.TrimSpace(session)
			if session == "" {
				continue
			}
			if _, exists := seen[session]; exists {
				continue
			}
			seen[session] = struct{}{}
			observation.Sessions = append(observation.Sessions, session)
		}
	}
	return observation
}

// ReadMessageEpisodeSources reads the Episode provenance an answer carried.
//
// The product persists the source closure of the recalled Episodes on the
// assistant message, because a later projection must not be able to turn
// recalled text into source-less history. The card path fills no Episode
// ancestors, so an empty list is a fact about the path, not a missing
// record.
func ReadMessageEpisodeSources(ctx context.Context, db *gorm.DB, userID int64, sessionID string) ([]string, error) {
	if db == nil || userID <= 0 || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	// The messages carry the session's user name, not its id, so the ownership
	// is checked on the session row. A question session that does not belong
	// to this learner has no messages to read.
	var session model.Session
	if err := db.WithContext(ctx).Where("id = ? AND user_id = ?", sessionID, userID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var rows []model.Message
	if err := db.WithContext(ctx).
		Where("session_id = ? AND is_user = ?", sessionID, false).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []string
	for _, row := range rows {
		for _, ref := range row.EpisodeSources {
			id := strings.TrimSpace(ref.EpisodeID)
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
	return out, nil
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

// CardsForSessions reads the card rows about the named conversations.
//
// The prompt says which conversations the card block was about, and the card
// row says which document answered for them. Reading the rows back is what
// turns "a card was shown" into "this card was shown": a guess from the
// arm's own bookkeeping would be the arm agreeing with itself.
func CardsForSessions(ctx context.Context, db *gorm.DB, userID int64, sessions []string) ([]model.EpisodeMemory, error) {
	if db == nil || userID <= 0 || len(sessions) == 0 {
		return nil, nil
	}
	var rows []model.EpisodeMemory
	if err := db.WithContext(ctx).
		Where("user_id = ? AND session_id IN ? AND content_kind = ?",
			userID, sessions, model.EpisodeContentSessionCard).
		Order("version DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	// One conversation has one current card. The rows are ordered by version,
	// so the first row of a conversation is the one that was current.
	seen := map[string]struct{}{}
	current := make([]model.EpisodeMemory, 0, len(rows))
	for _, row := range rows {
		if _, exists := seen[row.SessionID]; exists {
			continue
		}
		seen[row.SessionID] = struct{}{}
		current = append(current, row)
	}
	return current, nil
}

// recordCardArmEvidence reads what the answer looked at.
//
// The counts come from the product's own step record, the shown memory comes
// from the prompt the model was sent, the tool table comes from the request
// that was sent, and the kind of every memory row comes from the rows
// themselves. A number that was recomputed from the answer text would be a
// reading of the text, and the text is not the record.
//
// It works for both arms. The old arm's answer carries its Episodes as
// provenance, and the card arm's answer carries the card rows it was shown;
// the two are read back from the rows rather than from the arm's own name.
func recordCardArmEvidence(evidence *CardArmEvidence, sandbox *Sandbox, userID int64,
	questionSessionID string, observation CardObservation, questions []QuestionResult, calls []ModelCall) error {
	if evidence == nil {
		return nil
	}
	var allSteps []model.AgentStep
	for _, asked := range questions {
		steps := ReadCardArmSteps(asked.Steps)
		evidence.Tools = append(evidence.Tools, steps.Tools...)
		evidence.BrowsedSegments = append(evidence.BrowsedSegments, steps.Browsed...)
		evidence.SearchHits = append(evidence.SearchHits, steps.Hits...)
		evidence.ReadBackMessages = append(evidence.ReadBackMessages, steps.ReadBack...)
		evidence.BrowsedTruncated = evidence.BrowsedTruncated || steps.BrowsedTruncated
		evidence.SearchHitsTruncated = evidence.SearchHitsTruncated || steps.HitsTruncated
		evidence.ReadBackTruncated = evidence.ReadBackTruncated || steps.ReadBackTruncated
		evidence.ToolRecordsTruncated = evidence.ToolRecordsTruncated || steps.RecordsTruncated
		allSteps = append(allSteps, asked.Steps...)
	}
	// The tool table is the same record the request was: the two arms are
	// offered different tools, and an attempt that does not say which table
	// it was given has not said what it measured.
	offered := AnswerToolTable(calls)
	evidence.ToolTable = offered
	evidence.ToolUsage = ToolUsage(ToolUsageInput{
		Offered:    offered,
		Registered: registeredToolsFor(evidence.MemoryKind),
		Compared:   []string{cardOnlyTool},
		Steps:      allSteps,
	})
	// The retrieval trace comes from the product's own tool rounds, so the
	// intent and the verdict beside it are the product's, not a reading of
	// the answer text.
	evidence.SearchTrace = ReadSearchTrace(allSteps)
	// The memory the answer was given comes from the request that was sent.
	shown := AnswerShownMemory(calls)
	evidence.ShownMemoryKind = shown.Kind
	evidence.InjectedText = shown.Text
	evidence.InjectedMessageIDs = shown.MessageIDs
	// The declared sessions are always a list, even an empty one: "the block
	// named no conversation" is a fact about the block, and a null on the
	// record would read as "the harness did not look".
	evidence.InjectedDeclaredSessions = shown.Sessions
	if evidence.InjectedDeclaredSessions == nil {
		evidence.InjectedDeclaredSessions = []string{}
	}
	evidence.InjectedSourceCheck = InjectedSourceUnknown
	evidence.ShownCardObserved = observation.Known
	evidence.ShownCard = observation.Shown()
	evidence.ShownCardSessions = observation.Sessions
	if sandbox == nil {
		return nil
	}
	ctx := context.Background()
	// The conversations the injected memory came from are read out of the
	// messages it cited. The block's own citations are the record, and the
	// messages say which conversation they were in - so a block that cited
	// something outside the arm's own conversations is caught here rather
	// than assumed away.
	//
	// The card's own claim (the conversation it names) is kept beside this
	// one, and the check is made against the conversations the citations
	// actually resolve to. A block that names one conversation and cites
	// another is then visible as the disagreement it is.
	if len(evidence.InjectedMessageIDs) > 0 {
		sessions, err := SessionsForMessageIDs(ctx, sandbox.db, userID, evidence.InjectedMessageIDs)
		if err != nil {
			return err
		}
		evidence.InjectedSessions = sessions
		if len(sessions) > 0 {
			if WithinSessions(sessions, evidence.SourceSessions) {
				evidence.InjectedSourceCheck = InjectedSourceWithin
			} else {
				evidence.InjectedSourceCheck = InjectedSourceOutside
			}
		}
	}
	sources, err := ReadMessageEpisodeSources(ctx, sandbox.db, userID, questionSessionID)
	if err != nil {
		return err
	}
	evidence.EpisodeSources = sources
	recall := map[string]struct{}{}
	for _, source := range sources {
		if _, exists := recall[source]; exists {
			continue
		}
		recall[source] = struct{}{}
		evidence.RecallSources = append(evidence.RecallSources, source)
	}
	// The card that was shown is a memory row the answer was given too. It is
	// read back from the database rather than taken from the arm's own
	// bookkeeping: the prompt said which conversations it carried, and the
	// card rows say which documents answered for them.
	cards, err := CardsForSessions(ctx, sandbox.db, userID, observation.Sessions)
	if err != nil {
		return err
	}
	for _, card := range cards {
		if _, exists := recall[card.ID]; exists {
			continue
		}
		recall[card.ID] = struct{}{}
		evidence.RecallSources = append(evidence.RecallSources, card.ID)
	}
	if len(evidence.RecallSources) == 0 {
		return nil
	}
	kinds, err := ClassifyRecallSources(ctx, sandbox.db, userID, evidence.RecallSources)
	if err != nil {
		return err
	}
	evidence.RecallKinds = kinds
	// The trace says which conversation each recalled memory came from. A
	// recall that found nothing is a fact about the retrieval - and it is
	// only a fact about the RETRIEVAL when the memory itself existed, which
	// the Ready flag and the projection error code say.
	entries, sessions, err := RecallTrace(ctx, sandbox.db, userID, evidence.RecallSources)
	if err != nil {
		return err
	}
	evidence.RecallTrace = entries
	evidence.RecallSessions = sessions
	return nil
}

// RecallTrace says what every recalled memory row was and where it came from.
//
// It is read out of the rows themselves: the row knows its kind and the
// conversation it was written from.
func RecallTrace(ctx context.Context, db *gorm.DB, userID int64, ids []string) ([]RecallEntry, []string, error) {
	if db == nil || userID <= 0 || len(ids) == 0 {
		return nil, nil, nil
	}
	var rows []model.EpisodeMemory
	if err := db.WithContext(ctx).Where("user_id = ? AND id IN ?", userID, ids).Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	byID := map[string]model.EpisodeMemory{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	var entries []RecallEntry
	seenSession := map[string]struct{}{}
	var sessions []string
	for _, id := range ids {
		row, exists := byID[id]
		if !exists {
			continue
		}
		entries = append(entries, RecallEntry{
			ID: row.ID, Kind: string(row.ContentKind), SourceSession: row.SessionID, Title: row.Title,
		})
		if _, exists := seenSession[row.SessionID]; exists {
			continue
		}
		seenSession[row.SessionID] = struct{}{}
		sessions = append(sessions, row.SessionID)
	}
	return entries, sessions, nil
}

// ApplyCardArm puts the arm's switch into the product's own configuration.
//
// The product reads the switch once, at startup, and the arm has to be
// visible to it before the configuration is validated: a run that asked for
// a mode the product does not know has to fail before it spends anything.
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
	// The read path and the recall switch travel with the arm, so that the
	// configuration the product boots with is the configuration the attempt
	// reports.
	if mode := strings.TrimSpace(arm.MemoryReadMode); mode != "" {
		settings.Config.AgentConfig.MemoryReadMode = mode
	}
	if mode := strings.TrimSpace(arm.EpisodeRecallMode); mode != "" {
		settings.Config.AgentConfig.EpisodeRecallMode = mode
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

// PublishAndAwaitMemory publishes the projection of one conversation and
// waits for its memory to be indexed.
//
// It is the preparation's half of the arm: the preparation publishes it, the
// queries read what it produced.
func (s *Sandbox) PublishAndAwaitMemory(ctx context.Context, userID int64, sessionID string,
	arm *CardArm) (published bool, due bool, waited int64, err error) {
	if err = s.verify(ctx); err != nil {
		return false, false, 0, err
	}
	if s.db == nil {
		return false, false, 0, fmt.Errorf("sandbox database unavailable")
	}
	// The target is the conversation's last message.
	var last struct {
		ID uint
	}
	if err = s.db.WithContext(ctx).Model(&model.Message{}).
		Where("session_id = ?", sessionID).Order("id DESC").Take(&last).Error; err != nil {
		return false, false, 0, err
	}
	if last.ID == 0 {
		return false, false, 0, fmt.Errorf("the conversation %s has no message", sessionID)
	}
	if err = PublishSessionCardProjection(ctx, s.db, userID, sessionID, last.ID, 0); err != nil {
		return false, false, 0, err
	}
	published = true
	if err = MakeProjectionDue(ctx, s.db, userID, sessionID); err != nil {
		return published, false, 0, err
	}
	due = true
	started := time.Now()
	if _, code, waitErr := WaitForArmMemory(ctx, s.db, userID, sessionID, arm.MemoryKind(), arm.Wait()); waitErr != nil {
		return published, due, time.Since(started).Milliseconds(), fmt.Errorf("%w (%s)", waitErr, code)
	}
	return published, due, time.Since(started).Milliseconds(), nil
}

// DriveSessionCard publishes the projection, waits for the card and opens the
// conversation the question is asked in.
//
// The card path calls it; the Episode path calls DriveArm. It is kept as a
// name because the card path is what it was written for.
func DriveSessionCard(ctx context.Context, sandbox *Sandbox, snapshot Snapshot, arm *CardArm) (*CardArmEvidence, error) {
	return DriveArm(ctx, sandbox, snapshot, arm)
}

// DriveArm publishes the projection, waits for the memory and opens the
// conversation the question is asked in.
//
// The order is the point. The projection is published before the question
// session exists, so the memory cannot be written from the question's own
// conversation. The memory is waited for before the question is asked, so the
// question is asked against a memory that exists rather than against a window
// that would have had one. The question session is opened last, so it holds
// nothing but the question.
//
// Both arms go through here. An arm that did not publish, wait and open a
// second conversation would measure an empty window, and an empty window is
// not a measurement of the memory path - it is a measurement of the wiring.
//
// EVERY conversation of the attempt is projected. The retrieval has to choose
// between them, and a database that holds only the conversation the question
// is about would measure nothing about the choosing.
func DriveArm(ctx context.Context, sandbox *Sandbox, snapshot Snapshot, arm *CardArm) (*CardArmEvidence, error) {
	if err := arm.Validate(); err != nil {
		return nil, err
	}
	evidence := &CardArmEvidence{
		Mode: arm.Mode, SourceSessionID: primaryOf(snapshot).Session.ID,
		IndexName: arm.IndexName, KeyPrefix: arm.KeyPrefix,
		MemoryReadMode:    strings.TrimSpace(arm.MemoryReadMode),
		EpisodeRecallMode: strings.TrimSpace(arm.EpisodeRecallMode),
		MemoryKind:        arm.MemoryKind(),
	}
	evidence.SourceSessions = snapshot.SessionIDs()
	for _, conversation := range snapshot.Sessions {
		var target uint
		for _, message := range conversation.Messages {
			evidence.SourceMessages++
			evidence.SourceCharacters += len(message.Content)
			if message.ID > target {
				target = message.ID
			}
		}
		if target == 0 {
			evidence.Error = "a source conversation has no messages"
			return evidence, fmt.Errorf("%s", evidence.Error)
		}
		if err := sandbox.PublishSessionCardProjection(ctx, conversation.Session.UserID, conversation.Session.ID, target); err != nil {
			evidence.Error = err.Error()
			return evidence, err
		}
		evidence.Published = true
		if err := sandbox.MakeProjectionDue(ctx, conversation.Session.UserID, conversation.Session.ID); err != nil {
			evidence.Error = err.Error()
			return evidence, err
		}
		evidence.Due = true
		started := time.Now()
		memory, code, err := sandbox.WaitForArmMemory(ctx, conversation.Session.UserID, conversation.Session.ID, arm.MemoryKind(), arm.Wait())
		if err != nil {
			evidence.JobErrorCode = code
			evidence.Error = err.Error()
			return evidence, err
		}
		evidence.Ready = true
		evidence.WaitMilliseconds += time.Since(started).Milliseconds()
		row := MemoryRowOf(memory)
		evidence.Memories = append(evidence.Memories, row)
		// The row of the conversation the question is about is the card the
		// arm will read. It is recorded here rather than left empty: an
		// attempt whose evidence says "no card" while a card was indexed is
		// an attempt that cannot be read.
		if conversation.Session.ID == evidence.SourceSessionID {
			evidence.Card = row
			evidence.Memory = row
			// The target belongs to the conversation the question is about.
			// An attempt with several conversations would otherwise report the
			// last one's target beside the first one's card, and the two would
			// describe different conversations.
			evidence.PublishedTargetMessageID = target
		}
	}
	// The question is asked in a conversation of its own, derived from the
	// attempt's own conversation so that two attempts of the same run cannot
	// ask their question in the same place.
	questionSession := QuestionSessionID(primaryOf(snapshot).Session.ID)
	if err := sandbox.CreateQuestionSession(ctx, primaryOf(snapshot).Session.UserID, primaryOf(snapshot).Session.UserName, questionSession); err != nil {
		evidence.Error = err.Error()
		return evidence, err
	}
	evidence.QuestionSessionID = questionSession
	return evidence, nil
}

// snapshotHoldsSession says whether the attempt's conversation is among the
// ones the archive carries.
func snapshotHoldsSession(snapshot Snapshot, sessionID string) bool {
	for _, conversation := range snapshot.Sessions {
		if conversation.Session.ID == sessionID {
			return true
		}
	}
	return false
}
