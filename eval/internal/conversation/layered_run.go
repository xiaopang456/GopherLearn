package conversation

// The layered summary's own run.
//
// The old path keeps one rolling summary and summarizes on its own schedule.
// The layered path writes a segment and a board per stretch of turns. Both
// are driven from the same turns here, so the two arms of a comparison see
// the same conversation and the difference in their numbers is the difference
// between the two structures.
//
// The mode decides what runs:
//   - "off" keeps the old rolling summary and writes nothing;
//   - "shadow" writes the segments and the board as well, and the window is
//     still served the old summary;
//   - "on" writes the segments and the board, and the old rolling summary is
//     neither read nor written.
//
// The numbers a comparison needs are in the run: how far the summary reached
// (the covered message id), how often a generation failed, how often the turn
// fell back to a synchronous generation, how many values the board marked as
// replaced, whether the segment that carries the answer was the one the
// preselector picked, and what the calls cost.
//
// The counts come from the product's own recorder rather than from a second
// implementation of the same rules. A number that is recomputed here would
// only agree with the product as long as the two copies agree, and a
// measurement that can drift from the thing it measures is not a measurement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gorm.io/gorm"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/common/observability"
	"GopherAI/common/rag"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino-ext/components/model/openai"
	modelcomponent "github.com/cloudwego/eino/components/model"
)

// LayeredSegment is one segment a run produced.
type LayeredSegment struct {
	Seq            int                                 `json:"seq"`
	StartMessageID uint                                `json:"start_message_id"`
	EndMessageID   uint                                `json:"end_message_id"`
	Topic          string                              `json:"topic"`
	Content        string                              `json:"content"`
	Points         int                                 `json:"points"`
	Changes        []model.SessionSummarySegmentChange `json:"changes"`
	Diagnostics    agent.WhiteboardDiagnostics         `json:"diagnostics"`
}

// LayeredCoverage says how far the summary reached.
//
// CoveredMessageID is the high-water mark: the messages at or below it are
// summarized and the ones above it are still raw. Ratio is that mark over the
// number of messages, which is the number a comparison reads.
//
// Series is the coverage after each batch, because a summary that stops
// covering is worse than one that covers less: a ratio that only says "how
// far" cannot tell a summary that is still moving from one that stopped.
type LayeredCoverage struct {
	CoveredMessageID uint    `json:"covered_message_id"`
	TotalMessages    int     `json:"total_messages"`
	Ratio            float64 `json:"ratio"`
	Series           []uint  `json:"series"`
	Monotonic        bool    `json:"monotonic"`
	Stalled          bool    `json:"stalled"`
	Explanation      string  `json:"explanation,omitempty"`
}

// LayeredFailures counts the generations that did not produce a summary.
type LayeredFailures struct {
	// Segments are the segment generations that failed.
	Segments int `json:"segments"`
	// OldSummary are the rolling summary generations that failed.
	OldSummary int `json:"old_summary"`
	// Total is the sum of the two.
	Total int `json:"total"`
}

// LayeredBoard counts what the board did to itself.
//
// Every count here is an intervention the server had to make, so a rising
// number names the part of the generator that is drifting.
type LayeredBoard struct {
	Applied          int `json:"applied"`
	Changed          int `json:"changed"`
	Fabricated       int `json:"fabricated"`
	SuspiciousDelete int `json:"suspicious_delete"`
	MissedUpdate     int `json:"missed_update"`
	Truncated        int `json:"truncated"`
	Dropped          int `json:"dropped"`
	DroppedDone      int `json:"dropped_done"`
	// SupersededMarks are the values the board kept next to the value that
	// replaced them. A board that keeps them can still answer "what did I
	// decide first", and a board that does not cannot.
	SupersededMarks int `json:"superseded_marks"`
}

// LayeredRetrieval says whether the segment that carries the answer is the
// one the preselector picked.
//
// The preselector runs on its lexical channel only: the vector channel costs
// an embedding per question, and the question this run asks is the same one
// every run asks, so the number would not change with the money spent on it.
type LayeredRetrieval struct {
	Question string `json:"question"`
	// Selected are the sequence numbers the preselector returned.
	Selected []int `json:"selected"`
	// Evidence are the sequence numbers of the segments that cover the turns
	// the answer comes from.
	Evidence []int `json:"evidence"`
	// Hit is true when one of the selected segments is one of them.
	Hit bool `json:"hit"`
}

// LayeredCall is one model call the run made.
//
// The same call is observed twice: once by the SDK wrapper and once at the
// HTTP boundary the provider was actually reached at. Only the boundary
// observation is a provider call, so only that one is counted.
type LayeredCall struct {
	Method     string `json:"method"`
	Status     int    `json:"status,omitempty"`
	Prompt     int    `json:"prompt_tokens"`
	Completion int    `json:"completion_tokens"`
	Error      string `json:"error,omitempty"`
}

// LayeredRun is what one conversation produced under one mode.
type LayeredRun struct {
	Scenario   string           `json:"scenario"`
	Condition  string           `json:"condition"`
	Mode       string           `json:"mode"`
	Turns      int              `json:"turns"`
	Messages   int              `json:"messages"`
	Segments   []LayeredSegment `json:"segments"`
	Whiteboard string           `json:"whiteboard"`
	Coverage   LayeredCoverage  `json:"coverage"`
	// OldCoveredMessageID is how far the old rolling summary reached. It is
	// zero in the "on" mode, where the old summary is neither read nor
	// written.
	OldCoveredMessageID uint             `json:"old_covered_message_id"`
	Failures            LayeredFailures  `json:"failures"`
	Board               LayeredBoard     `json:"board"`
	Retrieval           LayeredRetrieval `json:"retrieval"`
	ModelCalls          int              `json:"model_calls"`
	PromptTokens        int              `json:"prompt_tokens"`
	CompletionTokens    int              `json:"completion_tokens"`
	// Calls is the provider boundary of every call the run made.
	Calls []LayeredCall `json:"calls,omitempty"`
	// Holes are the stretches of turns no segment covers, written as the
	// message range each one spans. A hole is the difference between a
	// summary that stopped and a summary that is still moving.
	Holes []string `json:"holes,omitempty"`
	// Counters and Totals are the product's own recorder, read as the
	// difference between the state before the run and the state after it.
	Counters map[string]uint64 `json:"counters,omitempty"`
	Totals   map[string]int64  `json:"totals,omitempty"`
	Errors   []string          `json:"errors,omitempty"`
}

// LayeredModes are the modes a run may use.
var LayeredModes = []string{"off", "shadow", "on"}

// RunLayeredSummary replays one conversation and returns what the summary
// made of it.
//
// The turns are the scenario's own: the same ones the product would have
// seen. The messages are stored first, because a segment cites the messages
// it covers and the ids have to exist before it is written.
func RunLayeredSummary(ctx context.Context, repository, scenario, condition string,
	turns []Turn, question string, evidenceTurns []int,
	proxyBase, chatModel, mode string) (run LayeredRun, resultErr error) {
	if len(turns) == 0 {
		return run, fmt.Errorf("turns are required")
	}
	if !layeredModeKnown(mode) {
		return run, fmt.Errorf("unknown layered mode %q", mode)
	}
	if err := ctx.Err(); err != nil {
		return run, err
	}
	run = LayeredRun{Scenario: scenario, Condition: condition, Mode: mode, Turns: len(turns)}
	storage, err := os.MkdirTemp("", "gl-layered-storage-")
	if err != nil {
		return run, err
	}
	defer os.RemoveAll(storage)
	settings, err := summarySettings(repository, storage, chatModel, "")
	if err != nil {
		return run, err
	}
	settings.Config.AgentConfig.SummarySegmentMode = mode
	// Same reason as the read driver: the recorder defaults to off, and a
	// counter labelled with the wrong arm cannot be compared with one from a
	// run that labelled it correctly.
	observability.Memory.SetMode(mode)
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return run, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("layered run model telemetry incomplete"))
		}
	}()
	wire := realWire(ledger, roles, proxyBase)
	defer wire.Close()
	dialect, err := config.NormalizeChatThinkingDialect(settings.Config.RagModelConfig.RagChatThinkingDialect)
	if err != nil {
		return run, err
	}
	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       chatModel,
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return run, err
	}
	wrapped, err := WrapModel(chat, ledger, roles)
	if err != nil {
		return run, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return run, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return run, err
	}
	password, err := randomHex(24)
	if err != nil {
		return run, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return run, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7001, Username: "l4-eval-learner", Password: hash}, nil, ""); err != nil {
		return run, err
	}
	sessionID := "l4-layered-" + scenario
	if err = (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: sessionID, UserID: 7001, UserName: "l4-eval-learner",
			Mode: model.SessionModeAgent, Title: "Layered run"}).Error
	}); err != nil {
		return run, err
	}
	guard := guardedStore{sandbox: s}
	messages, err := storeTurns(ctx, guard, sessionID, turns)
	if err != nil {
		return run, err
	}
	run.Messages = len(messages)

	// The recorder is global, so the run reads the difference between the
	// state before it and the state after it. A number that was already
	// there before the run started is not this run's number.
	observability.Memory.SetMode(mode)
	before := observability.Memory.Snapshot()

	if mode == "off" || mode == "shadow" {
		covered, failures, err := runOldSummary(ctx, guard, sessionID, turns, settings, wrapped)
		if err != nil {
			return run, err
		}
		run.Failures.OldSummary = failures
		run.OldCoveredMessageID = covered
		if mode == "off" {
			run.Coverage = coverageOf(covered, len(messages), nil)
			run.Coverage.Explanation = "the rolling summary reached this message"
		}
	}
	if mode == "on" || mode == "shadow" {
		if err := runLayeredSegments(ctx, sessionID, messages, settings, wrapped, &run); err != nil {
			return run, err
		}
		run.Coverage = coverageOf(run.Coverage.CoveredMessageID, len(messages), run.Coverage.Series)
	}

	after := observability.Memory.Snapshot()
	run.Counters = counterDelta(before.Counters, after.Counters)
	run.Totals = totalDelta(before.Totals, after.Totals)

	// The same call is in the ledger twice: once as the SDK made it and once
	// at the boundary the provider was reached at. Counting both would
	// double every call and every token, so only the boundary is counted.
	for _, call := range ledger.Calls() {
		if !strings.HasPrefix(call.Method, "http:") {
			continue
		}
		run.ModelCalls++
		entry := LayeredCall{Method: call.Method, Status: call.HTTPStatus, Error: call.Error}
		if call.Usage != nil {
			entry.Prompt = call.Usage.PromptTokens
			entry.Completion = call.Usage.CompletionTokens
			run.PromptTokens += call.Usage.PromptTokens
			run.CompletionTokens += call.Usage.CompletionTokens
		}
		run.Calls = append(run.Calls, entry)
	}
	run.Failures.Total = run.Failures.Segments + run.Failures.OldSummary
	run.Retrieval = runRetrieval(ctx, sessionID, question, evidenceTurns, messages, run.Segments)
	return run, nil
}

// layeredModeKnown reports whether the mode is one of the three the product
// understands.
func layeredModeKnown(mode string) bool {
	for _, candidate := range LayeredModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// storeTurns writes the scenario's turns into the session.
//
// The user turn is written first and the answer after it, because that is
// the order the conversation happened in and the ids have to follow it.
func storeTurns(ctx context.Context, guard guardedStore, sessionID string, turns []Turn) ([]model.Message, error) {
	messages := make([]model.Message, 0, len(turns)*2)
	for _, turn := range turns {
		user, err := guard.Create(ctx, &model.Message{SessionID: sessionID, UserName: "l4-eval-learner",
			Content: turn.User, IsUser: true, CreatedAt: turn.UserAt.UTC(),
			AnswerBasis: rag.AnswerBasisConversation, KnowledgeStatus: rag.KnowledgeStatusNotRequired,
			KnowledgeReason: rag.KnowledgeReasonNone})
		if err != nil {
			return nil, err
		}
		messages = append(messages, *user)
		answer, err := guard.Create(ctx, &model.Message{SessionID: sessionID, UserName: "l4-eval-learner",
			Content: turn.Assistant, CreatedAt: turn.AssistantAt.UTC()})
		if err != nil {
			return nil, err
		}
		messages = append(messages, *answer)
	}
	return messages, nil
}

// runOldSummary keeps the old rolling summary up to date and reports how far
// it reached.
//
// The summary is asked for after every turn, the way the product asks for
// it: the summary provider decides for itself whether this turn is the one
// that triggers a summary.
func runOldSummary(ctx context.Context, guard guardedStore, sessionID string, turns []Turn,
	settings *entry.Settings, chat modelcomponent.ToolCallingChatModel) (uint, int, error) {
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(chat, policy.MaxInputTokens, policy.MaxOutputTokens, policy.Budget)
	if err != nil {
		return 0, 0, err
	}
	provider, err := agent.NewRollingSummaryProvider(guard, guard, generator, policy)
	if err != nil {
		return 0, 0, err
	}
	var covered uint
	failures := 0
	for _, turn := range turns {
		summary, err := provider.SessionSummary(ctx, agent.RunContext{UserID: 7001, UserName: "l4-eval-learner",
			SessionID: sessionID, Question: turn.User})
		if err != nil {
			// A failed compaction does not stop the conversation, and the
			// number of those failures is one of the numbers a comparison
			// reads, so the run keeps going.
			failures++
			continue
		}
		if summary.CoveredMessageID > covered {
			covered = summary.CoveredMessageID
		}
	}
	return covered, failures, nil
}

// runLayeredSegments writes the segments and the board.
//
// The stretches are the ones the background worker would take: a stretch
// starts where the last one ended and is at most MaxMessages long, and a
// stretch is only summarized once there are TriggerMessages of it to
// summarize. The board and the previous segment are what the generator sees
// alongside the turns, exactly as the worker passes them.
//
// The scheduling is the harness's, not the worker's: the worker claims jobs
// from a queue and this run has no queue. What is measured is the generator
// and the board, which are the parts whose behaviour the comparison is about.
func runLayeredSegments(ctx context.Context, sessionID string, messages []model.Message,
	settings *entry.Settings, chat modelcomponent.ToolCallingChatModel, run *LayeredRun) error {
	agentConfig := settings.Config.AgentConfig
	generator, err := agent.NewModelSegmentGenerator(chat, agentConfig.EffectiveSummarySegmentMaxInputTokens(),
		agentConfig.SummarySegmentMaxOutputTokens)
	if err != nil {
		return err
	}
	generator.UseAssistantContent(agentConfig.SummaryAssistantContentEnabled())
	generator.UseSegmentBudget(agentConfig.EffectiveSegmentTokens())
	whiteboard := ""
	previousSegment := ""
	seq := 0
	series := make([]uint, 0, 8)
	for start := 0; start < len(messages); {
		end := start + agentConfig.SummarySegmentMaxMessages
		if end > len(messages) {
			end = len(messages)
		}
		if end-start < agentConfig.SummarySegmentTriggerMessages {
			// Not enough turns yet to be worth a segment. The rest of the
			// conversation stays raw, which is what the product does too.
			break
		}
		batch := messages[start:end]
		result, err := generator.GenerateSegment(ctx, agent.SegmentGenerationInput{
			Whiteboard:      whiteboard,
			PreviousSegment: previousSegment,
			Messages:        batch,
			FirstMessageID:  batch[0].ID,
			LastMessageID:   batch[len(batch)-1].ID,
		})
		if err != nil {
			run.Failures.Segments++
			run.Errors = append(run.Errors, err.Error())
			// The stretch stays uncovered. The product retries the same
			// batch, so the batch after it waits; moving on would leave a
			// hole between two segments and a watermark that runs past
			// the turns no segment covers.
			run.Holes = append(run.Holes, fmt.Sprintf("%d-%d", batch[0].ID, batch[len(batch)-1].ID))
			break
		}
		seq++
		run.Segments = append(run.Segments, LayeredSegment{
			Seq:            seq,
			StartMessageID: batch[0].ID,
			EndMessageID:   batch[len(batch)-1].ID,
			Topic:          result.Segment.Topic,
			Content:        result.SegmentContent,
			Points:         len(result.Segment.Points),
			Changes:        result.Changes,
			Diagnostics:    result.Diagnostics,
		})
		run.SupersededMarksOf(result.Changes)
		run.Board.Applied += result.Diagnostics.AppliedEntries
		run.Board.Changed += result.Diagnostics.ChangedEntries
		run.Board.Fabricated += result.Diagnostics.FabricatedEntries
		run.Board.SuspiciousDelete += result.Diagnostics.SuspiciousDeletes
		run.Board.MissedUpdate += result.Diagnostics.MissedUpdates
		run.Board.Truncated += result.Diagnostics.TruncatedTexts
		run.Board.Dropped += result.Diagnostics.DroppedEntries
		run.Board.DroppedDone += result.Diagnostics.DroppedDonePlan
		whiteboard = result.WhiteboardContent
		previousSegment = result.SegmentContent
		run.Coverage.CoveredMessageID = batch[len(batch)-1].ID
		series = append(series, run.Coverage.CoveredMessageID)
		start = end
	}
	run.Coverage.Series = series
	run.Whiteboard = whiteboard
	return nil
}

// SupersededMarksOf counts the values the board marked as replaced.
//
// A replaced value is kept with a note that says what replaced it, so the
// learner can still be asked about the old one. How often that happens is
// what says whether the board is keeping up with the conversation.
func (r *LayeredRun) SupersededMarksOf(changes []model.SessionSummarySegmentChange) {
	for _, change := range changes {
		if change.OldValue != "" && change.NewValue != "" {
			r.Board.SupersededMarks++
		}
	}
}

// coverageOf reads the coverage of a run.
//
// Monotonic is the property the plan asks for: a summary that stops covering
// is worse than one that covers less, and only a series that never goes back
// can say the summary did not stall.
func coverageOf(covered uint, total int, series []uint) LayeredCoverage {
	coverage := LayeredCoverage{CoveredMessageID: covered, TotalMessages: total, Series: series}
	if total > 0 {
		coverage.Ratio = float64(covered) / float64(total)
	}
	coverage.Monotonic = true
	for index := 1; index < len(series); index++ {
		if series[index] < series[index-1] {
			coverage.Monotonic = false
			break
		}
	}
	coverage.Stalled = len(series) > 1 && !coverage.Monotonic
	switch {
	case len(series) == 0:
		coverage.Explanation = "no segment was written"
	case coverage.Monotonic:
		coverage.Explanation = "the coverage grew with every batch"
	default:
		coverage.Explanation = "the coverage went back after a batch"
	}
	return coverage
}

// runRetrieval asks the product's preselector which segments the question is
// about.
//
// The preselector runs on its lexical channel only. The vector channel costs
// one embedding per question and answers the same question, and a measurement
// whose value depends on how much was spent on it is not comparable between
// two runs that spent different amounts.
func runRetrieval(ctx context.Context, sessionID, question string, evidenceTurns []int,
	messages []model.Message, segments []LayeredSegment) LayeredRetrieval {
	retrieval := LayeredRetrieval{Question: question}
	if strings.TrimSpace(question) == "" || len(segments) == 0 {
		return retrieval
	}
	candidates := make([]model.SessionSummarySegment, 0, len(segments))
	for _, segment := range segments {
		candidates = append(candidates, model.SessionSummarySegment{
			Seq:            segment.Seq,
			StartMessageID: segment.StartMessageID,
			EndMessageID:   segment.EndMessageID,
			Topic:          segment.Topic,
			Content:        segment.Content,
			TokenEstimate:  rag.EstimateTokens(segment.Content),
		})
	}
	preselector, err := agent.NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		return retrieval
	}
	selected, err := preselector.Select(ctx, agent.RunContext{SessionID: sessionID, Question: question}, candidates, nil)
	if err != nil {
		return retrieval
	}
	for _, segment := range selected {
		retrieval.Selected = append(retrieval.Selected, segment.Seq)
	}
	for _, turn := range evidenceTurns {
		// A turn is two messages: the learner's and the answer's. The
		// messages were stored in that order, so the turn's messages are
		// the ones at those two places.
		first := 2*turn - 2
		last := 2*turn - 1
		if first < 0 || last >= len(messages) {
			continue
		}
		from := messages[first].ID
		to := messages[last].ID
		for _, segment := range segments {
			if segment.StartMessageID <= from && to <= segment.EndMessageID {
				retrieval.Evidence = appendUnique(retrieval.Evidence, segment.Seq)
			}
		}
	}
	for _, seq := range retrieval.Selected {
		for _, evidence := range retrieval.Evidence {
			if seq == evidence {
				retrieval.Hit = true
			}
		}
	}
	return retrieval
}

// appendUnique adds a value that is not in the list yet.
func appendUnique(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// counterDelta is the difference between two counter readings.
//
// Only the counters that grew are kept: a counter that did not move is not
// this run's business, and a report that lists every counter of the product
// says nothing about the run.
func counterDelta(before, after map[string]uint64) map[string]uint64 {
	delta := map[string]uint64{}
	for key, value := range after {
		if value > before[key] {
			delta[key] = value - before[key]
		}
	}
	if len(delta) == 0 {
		return nil
	}
	return delta
}

// totalDelta is the difference between two total readings.
func totalDelta(before, after map[string]int64) map[string]int64 {
	delta := map[string]int64{}
	for key, value := range after {
		if value > before[key] {
			delta[key] = value - before[key]
		}
	}
	if len(delta) == 0 {
		return nil
	}
	return delta
}

// WriteLayeredRun writes one run as JSON.
func WriteLayeredRun(path string, run LayeredRun) error {
	encoded, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

// LayeredRunTimeout is the deadline one run is given.
const LayeredRunTimeout = 30 * time.Minute
