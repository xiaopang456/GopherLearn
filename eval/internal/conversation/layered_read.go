package conversation

// The layered summary's read side, measured end to end.
//
// The writer's run in layered_run.go stops where the segments and the board
// are produced. This one goes on: the segments and the board are stored, the
// product's own context builder reads them back, and the product's own model
// answers the question from the window that came out. What a comparison needs
// to know is not "were they written" but "did they reach the model, did the
// right segment reach it, and did the answer use the value the board says is
// current".
//
// The window is built by the product's builder rather than by a harness copy
// of it, and the answer is produced by the product's model with the product's
// prompt. The one thing this path does not do is go through the HTTP entry:
// the product's own protocol runs no background work before the measured
// answer, so under the layered mode the read side would find nothing to read
// on the first turn. The measurement therefore writes the state the
// background would have written and then reads it, which is the same state
// the second turn of a real session would see.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/common/observability"
	"GopherAI/config"
	sessiondao "GopherAI/dao/session"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino-ext/components/model/openai"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// LayeredToolCheck is one authorization the read side had to honour.
type LayeredToolCheck struct {
	Tool string `json:"tool"`
	// Source is what drew the authorization: the board, or a segment the
	// window carried.
	Source     string `json:"source,omitempty"`
	MessageID  uint   `json:"message_id,omitempty"`
	SegmentSeq int    `json:"segment_seq,omitempty"`
	// Granted says the window authorised this id. An id the window did not
	// authorise is one the model never saw, and asking for it must fail.
	Granted bool `json:"granted"`
	// Authorised is what the product's own grant set answers, not what this
	// harness computed: the same HistoryGrantSet the read-back tool consults
	// is asked whether the id is readable. It is still not the same thing as
	// asking the tool, and Exercised says which question was answered.
	Authorised bool `json:"authorised"`
	// Pending says no product-side decision exists for this row at all, so it
	// must not be counted with the ones that do. The browse tool has no grant
	// set of its own: whether a segment is reachable is decided inside the
	// tool, and a row that filled in true here would be this harness's
	// opinion wearing the product's name.
	Pending bool `json:"pending"`
	// Exercised says the tool itself ran. It is false everywhere in this
	// driver: the read-back and browse tools are built inside the product's
	// internal wiring, which a harness in another module cannot import. A
	// check that only reaches the grant set must not be reported as a tool
	// call that succeeded or was refused.
	Exercised bool   `json:"exercised"`
	Detail    string `json:"detail,omitempty"`
}

// LayeredRead is what one read-side run found.
// LayeredBudget is what one window spent, block by block, in the product's own
// estimator.
//
// A scaled run that reported only the provider's total could not say which
// block took the room, and the acceptance checks are per block: the old summary
// must be zero under the layered mode, the recent messages must keep the room
// that was left for them, and the board must not silently take everything.
type LayeredBudget struct {
	InputTarget       int `json:"input_target"`
	EvidenceReserved  int `json:"evidence_reserved"`
	System            int `json:"system"`
	TaskState         int `json:"task_state"`
	CurrentQuestion   int `json:"current_question"`
	Summary           int `json:"summary"`
	Whiteboard        int `json:"whiteboard"`
	ResidentSegments  int `json:"resident_segments"`
	RetrievedSegments int `json:"retrieved_segments"`
	RecentMessages    int `json:"recent_messages"`
	PreRetrieval      int `json:"pre_retrieval"`
	Assembled         int `json:"assembled"`
}

func budgetFrom(budget agent.BudgetBreakdown) LayeredBudget {
	return LayeredBudget{
		InputTarget:       budget.InputTarget,
		EvidenceReserved:  budget.EvidenceReserved,
		System:            budget.System,
		TaskState:         budget.TaskState,
		CurrentQuestion:   budget.CurrentQuestion,
		Summary:           budget.Summary,
		Whiteboard:        budget.Whiteboard,
		ResidentSegments:  budget.ResidentSegments,
		RetrievedSegments: budget.RetrievedSegments,
		RecentMessages:    budget.RecentMessages,
		PreRetrieval:      budget.PreRetrievalEvidence,
		Assembled:         budget.Assembled,
	}
}

type LayeredRead struct {
	Scenario string `json:"scenario"`
	Question string `json:"question"`
	Mode     string `json:"mode"`

	// What reached the window.
	WhiteboardInWindow bool     `json:"whiteboard_in_window"`
	WhiteboardText     string   `json:"whiteboard_text"`
	ResidentSegments   []int    `json:"resident_segments"`
	RetrievedSegments  []int    `json:"retrieved_segments"`
	SegmentsInWindow   []int    `json:"segments_in_window"`
	SupersessionMarks  []string `json:"supersession_marks"`
	// Tools is the tool table this turn was rendered against, and
	// PromptRendered says the product's renderer changed the prompt because of
	// it. A run that claims to measure the read side has to say which turn it
	// simulated: a window that advertises a search tool the model cannot call
	// measures neither the contract nor the board.
	Tools          []string `json:"capabilities"`
	PromptRendered bool     `json:"prompt_rendered"`
	WindowMessages int      `json:"window_messages"`
	// Budget is what the window actually spent, block by block, in the
	// product's own estimator. A scaled run that reported only a total could
	// not say which block took the room, and the acceptance check on the old
	// summary (zero under the layered mode) needs the block's own number.
	Budget LayeredBudget `json:"budget"`

	// Where the answer lives and whether the window carried it.
	EvidenceTurns  []int  `json:"evidence_turns"`
	EvidenceSeqs   []int  `json:"evidence_seqs"`
	EvidenceInView bool   `json:"evidence_in_view"`
	EvidenceNote   string `json:"evidence_note,omitempty"`

	// The authorization chain.
	Grants     int                `json:"grants"`
	GrantedIDs []uint             `json:"granted_ids"`
	ToolChecks []LayeredToolCheck `json:"tool_checks"`

	// The answer.
	Answer       string   `json:"answer"`
	Accepted     []string `json:"accepted"`
	Forbidden    []string `json:"forbidden"`
	AcceptedSeen bool     `json:"accepted_seen"`
	// ForbiddenSeen is the raw reading: the answer names a value the key
	// forbids. ForbiddenUnframed is the one the heuristic verdict uses. Both
	// are kept because they answer different questions, and a reader who
	// distrusts the heuristic needs the first to re-decide for themselves.
	ForbiddenSeen     bool `json:"forbidden_seen"`
	ForbiddenUnframed bool `json:"forbidden_unframed"`
	// ForbiddenFramed names the replaced values the answer mentions only as
	// something that was changed. They do not count against the answer.
	ForbiddenFramed []string `json:"forbidden_framed"`
	// QuestionTense is what the question asked for: the value that holds now,
	// the value it replaced, or neither. It decides which phrasing excuses a
	// replaced figure.
	QuestionTense string `json:"question_tense"`
	// Scorer names the rule the verdict came from. This driver's verdict is a
	// smoke reading, not a scorer: the formal comparison is judged by the
	// product's own score.mjs, where a value that appears on both sides goes
	// to needs_judge and is judged twice. The name is here so a verdict
	// copied out of this file cannot be mistaken for that one.
	Scorer           string `json:"scorer"`
	Verdict          string `json:"verdict"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	// SeededSegments is what the writer produced for this conversation. It is
	// reported because a read that found nothing is only readable next to the
	// state it was reading.
	SeededSegments int `json:"seeded_segments"`
	SeededBoard    int `json:"seeded_board_entries"`
	// Batches is what the writer did: how many batches, how many attempts,
	// how many retries, and how many batches still failed. The last number is
	// the one the product's own failure rate is measured on.
	Batches batchOutcome `json:"batches"`
	Errors  []string     `json:"errors,omitempty"`
}

// RunLayeredRead writes one conversation's layered state and then measures
// what the read side makes of it.
func RunLayeredRead(ctx context.Context, repository, scenario, question string,
	turns []Turn, evidenceTurns []int, accepted, forbidden []string,
	tools []string, stage, proxyBase, chatModel, scaledConfig string) (read LayeredRead, resultErr error) {
	if len(turns) == 0 {
		return read, fmt.Errorf("turns are required")
	}
	if strings.TrimSpace(question) == "" {
		return read, fmt.Errorf("question is required")
	}
	if err := ctx.Err(); err != nil {
		return read, err
	}
	read = LayeredRead{Scenario: scenario, Question: question, Mode: "on",
		EvidenceTurns: evidenceTurns, Accepted: accepted, Forbidden: forbidden}

	storage, err := os.MkdirTemp("", "gl-layered-read-")
	if err != nil {
		return read, err
	}
	defer os.RemoveAll(storage)
	settings, err := summarySettings(repository, storage, chatModel, scaledConfig)
	if err != nil {
		return read, err
	}
	// This harness builds one arm: the layered state read back into the window.
	// The two baseline arms - the old rolling summary, and a window that simply
	// drops what does not fit - are different assembly paths, and a driver that
	// pasted their budgets onto this one would spend model calls generating
	// segments and then render empty blocks from budgets meant for another arm.
	// Refusing is the honest answer until those paths exist here.
	mode, armErr := resolveArm(settings.Config.AgentConfig.SummarySegmentMode, scaledConfig != "")
	if armErr != nil {
		return read, armErr
	}
	read.Mode = mode
	// The diagnostics carry the mode in their labels, and the recorder's
	// default is off. The product sets this at startup; a harness that did
	// not would label an applied run's counters "off" and a reader would
	// have to guess which arm produced them.
	observability.Memory.SetMode(observability.MemoryModeOn)
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return read, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("layered read model telemetry incomplete"))
		}
	}()
	wire := realWire(ledger, roles, proxyBase)
	defer wire.Close()
	dialect, err := config.NormalizeChatThinkingDialect(settings.Config.RagModelConfig.RagChatThinkingDialect)
	if err != nil {
		return read, err
	}
	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       chatModel,
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return read, err
	}
	wrapped, err := WrapModel(chat, ledger, roles)
	if err != nil {
		return read, err
	}
	// The segment writer is a second model on the same endpoint, asked for a
	// JSON object. The prompt already says "Output JSON only", and saying it
	// is not the same as the provider enforcing it: a parse failure is the
	// one batch failure that throws a whole stretch of turns away. The
	// question is answered in prose, so the two models must stay separate -
	// one response format for both would put every answer in JSON.
	segmentChat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:        wire.URL + "/v1",
		Model:          chatModel,
		APIKey:         "placeholder-proxy-injected",
		ExtraFields:    aihelper.ThinkingExtraFields(dialect, false),
		ResponseFormat: jsonObjectResponseFormat(),
	})
	if err != nil {
		return read, err
	}
	segmentModel, err := WrapModel(segmentChat, ledger, roles)
	if err != nil {
		return read, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return read, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return read, err
	}
	password, err := randomHex(24)
	if err != nil {
		return read, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return read, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7001, Username: "l4-eval-learner", Password: hash}, nil, ""); err != nil {
		return read, err
	}
	sessionID := "l4-read-" + scenario
	if err = (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Create(&model.Session{ID: sessionID, UserID: 7001, UserName: "l4-eval-learner",
			Mode: model.SessionModeAgent, Title: "Layered read"}).Error
	}); err != nil {
		return read, err
	}
	guard := guardedStore{sandbox: s}
	messages, err := storeTurns(ctx, guard, sessionID, turns)
	if err != nil {
		return read, err
	}

	// The state the background would have written before this turn.
	seeded, batches, err := seedLayeredState(ctx, s, messages, settings, segmentModel, sessionID,
		settings.Config.AgentConfig.SummarySegmentMaxAttempts)
	read.Batches = batches
	if err != nil {
		return read, err
	}
	read.SeededSegments = len(seeded.segments)
	read.SeededBoard = boardEntryCount(seeded.board)
	// The batch stage stops here. It exists to measure the writer's failure
	// rate on its own: running the read side too would add one call per
	// conversation that says nothing about the writer, and paying for answers
	// to answer a question about parsing is how a measurement gets expensive
	// enough that nobody repeats it.
	if stage == LayeredStageBatches {
		return read, nil
	}

	// The read side, built the way the product builds it.
	window, windowErr := buildLayeredReadWindow(ctx, s, repository, settings, sessionID, question)
	if windowErr != nil {
		read.Errors = append(read.Errors, "build window: "+windowErr.Error())
		return read, windowErr
	}
	// The window's system prompt is not what the builder assembled: the
	// product renders it against this turn's tool table, and a rule that names
	// a tool the model cannot see is removed. Rendering it here is not a
	// nicety - a measurement that skipped it sent the model a knowledge-search
	// contract for a turn with no search tool, and the model obeyed the
	// contract and refused a question the board could answer.
	window, rendered, renderErr := renderWindowPromptFor(window, tools, question)
	if renderErr != nil {
		read.Errors = append(read.Errors, "render prompt: "+renderErr.Error())
		return read, renderErr
	}
	read.PromptRendered = rendered
	read.Tools = tools
	read.WindowMessages = len(window.Messages)
	read.Budget = budgetFrom(window.Budget)
	inspectWindow(&read, window, seeded, messages)

	// The authorization chain: an id the window authorised is readable, an id
	// it never showed the model is not.
	checkToolAuthority(s, sessionID, window, seeded, &read)

	// The answer, produced from the window that was built.
	answer, err := wrapped.Generate(ctx, window.Messages)
	if err != nil {
		read.Errors = append(read.Errors, "answer: "+err.Error())
		return read, err
	}
	if answer != nil {
		read.Answer = strings.TrimSpace(answer.Content)
		if answer.ResponseMeta != nil && answer.ResponseMeta.Usage != nil {
			read.PromptTokens = answer.ResponseMeta.Usage.PromptTokens
			read.CompletionTokens = answer.ResponseMeta.Usage.CompletionTokens
		}
	}
	read.AcceptedSeen = mentionsAny(read.Answer, accepted)
	read.ForbiddenSeen = mentionsAny(read.Answer, forbidList(forbidden, accepted))
	// Naming the value that was replaced is not the same as answering with it.
	//
	// "你最初定的是 15 页，后来改成了 30 页" answers a question about the
	// original value correctly, and a scorer that only asks whether the
	// replaced figure appears marks it wrong. Which phrasing excuses the
	// replaced figure depends on what was asked: a question about the
	// original is answered correctly by naming the change, and a question
	// about the current value is answered correctly by naming what it
	// replaced and saying that it no longer holds.
	read.QuestionTense = tenseOf(read.Question)
	excusing := markersFor(read.QuestionTense)
	for _, value := range forbidList(forbidden, accepted) {
		if mentionsOutsidePhrasing(read.Answer, value, excusing) {
			read.ForbiddenUnframed = true
		} else if mentionsAny(read.Answer, []string{value}) {
			read.ForbiddenFramed = append(read.ForbiddenFramed, value)
		}
	}
	// The framing rule can be talked into excusing the answer itself.
	//
	// "最初 30 页，后来 15 页" asks about the original and answers it with the
	// replaced figure; the figure sits next to 后来, so the rule above calls
	// it a remark. What separates the two is the order: when the replaced
	// value is named first, it is the answer and the accepted one is the
	// remark. That case is reported as ambiguous rather than as a pass,
	// because the driver cannot tell a wrong answer from a correct one
	// phrased backwards.
	inverted := mentionsBefore(read.Answer, forbidList(forbidden, accepted), accepted)
	read.Scorer = layeredReadScorer
	switch {
	case read.Answer == "":
		read.Verdict = "empty"
	case inverted:
		read.Verdict = "ambiguous"
	case read.AcceptedSeen && !read.ForbiddenUnframed:
		read.Verdict = "pass"
	case read.ForbiddenUnframed && !read.AcceptedSeen:
		read.Verdict = "fail"
	default:
		read.Verdict = "ambiguous"
	}
	return read, nil
}

// seededLayered is the state the writer left behind.
type seededLayered struct {
	segments []model.SessionSummarySegment
	board    model.SessionWhiteboardDocument
}

// batchOutcome counts what the writer did, attempt by attempt.
//
// A single failure used to end the conversation: the first batch that came
// back unparseable returned seeded=0 and the whole scenario was lost. The
// product does not behave that way - its worker retries the same batch up to
// summarySegmentMaxAttempts - so a driver that gave up on the first error was
// measuring a policy the product does not have.
type batchOutcome struct {
	Batches  int `json:"batches"`
	Attempts int `json:"attempts"`
	Retries  int `json:"retries"`
	// FirstFailures is what the failure rate would have been without a retry:
	// the batches whose first attempt failed. Without it the retry looks
	// either useless or magical, depending on which number a reader happens to
	// remember.
	FirstFailures int            `json:"first_failures"`
	FinalFailures int            `json:"final_failures"`
	Reasons       map[string]int `json:"reasons,omitempty"`
	// RetryDelaySeconds is the wait the driver used between attempts, taken
	// from the worker's own setting. It is recorded because a failure rate
	// measured with immediate retries is a different number from one measured
	// on the worker's schedule, and a reader cannot tell them apart otherwise.
	RetryDelaySeconds int `json:"retry_delay_seconds"`
	// RawFailures keeps what the writer answered on the batches that never
	// parsed. One batch is enough to tell the failure apart; a list makes the
	// same failure arriving twice visible instead of averaging it away.
	RawFailures []rawFailure `json:"raw_failures,omitempty"`
}

func (o *batchOutcome) failed(err error) {
	o.Attempts++
	if o.Reasons == nil {
		o.Reasons = map[string]int{}
	}
	o.Reasons[segmentFailureLabel(err)]++
}

// segmentFailureLabel names a failure the way the product's counters do.
func segmentFailureLabel(err error) string {
	if err == nil {
		return "none"
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "http") || strings.Contains(text, "status code"):
		return "model_error"
	case strings.Contains(text, "decode") || strings.Contains(text, "invalid character") ||
		strings.Contains(text, "unexpected end of JSON"):
		return "invalid_output"
	default:
		return "other"
	}
}

// waitBeforeRetry is the worker's RetryDelay, honoured by the driver.
//
// It returns the context's error when the run is being torn down, so a
// cancelled run does not spend its remaining budget sleeping.
func waitBeforeRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// rawFailure is what the writer answered on an attempt that never parsed.
//
// The product discards the body with the error, and rightly so: it is the
// learner's own text. A measurement cannot tell a truncated answer from a
// mis-shaped one without it, and the difference decides the fix, so the driver
// keeps a bounded excerpt of every failed attempt. A failure a retry rescued
// matters as much as one it did not - it is the only evidence that the retry
// was spent on the failure it claims to fix. It stays in the private run
// directory and never reaches a commit.
type rawFailure struct {
	Batch   int    `json:"batch"`
	Attempt int    `json:"attempt"`
	Reason  string `json:"reason"`
	Excerpt string `json:"excerpt"`
}

// recordingChatModel keeps the last answer the segment writer produced.
//
// It wraps the model instead of the generator because the generator is the
// product's: the body is decided inside it and thrown away with the error, and
// a driver that reached into it would be testing its own copy.
type recordingChatModel struct {
	inner modelcomponent.ToolCallingChatModel

	mu   sync.Mutex
	last string
}

func (m *recordingChatModel) Generate(ctx context.Context, input []*schema.Message,
	opts ...modelcomponent.Option) (*schema.Message, error) {
	// The body is cleared before the call, not after it. A call that fails
	// without returning a message - a timeout, a refused connection - has no
	// body, and keeping the previous one would file the last batch's answer as
	// this batch's evidence.
	m.mu.Lock()
	m.last = ""
	m.mu.Unlock()
	out, err := m.inner.Generate(ctx, input, opts...)
	if out != nil {
		m.mu.Lock()
		m.last = out.Content
		m.mu.Unlock()
	}
	return out, err
}

func (m *recordingChatModel) Stream(ctx context.Context, input []*schema.Message,
	opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	return m.inner.Stream(ctx, input, opts...)
}

func (m *recordingChatModel) WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	bound, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &recordingChatModel{inner: bound}, nil
}

// excerptForReport bounds what is kept. A body that cannot be parsed is
// usually short; the head is what tells a truncated answer from a wrong shape.
const excerptForReport = 400

func (m *recordingChatModel) excerpt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	runes := []rune(m.last)
	if len(runes) > excerptForReport {
		return string(runes[:excerptForReport]) + " ..."
	}
	return string(runes)
}

// seedLayeredState writes the segments and the board the background would
// have written, retrying a failed batch the way the product's worker does.
//
// The attempt budget is the product's own (summarySegmentMaxAttempts). A batch
// that still fails is skipped and recorded, not turned into an empty run: the
// worker leaves a gap in a session it could not summarize, and a measurement
// that aborted instead would report a coverage of zero where the product
// reports a hole.
func seedLayeredState(ctx context.Context, s *Sandbox, messages []model.Message,
	settings *entry.Settings, chat modelcomponent.ToolCallingChatModel,
	sessionID string, retryBudget int) (seededLayered, batchOutcome, error) {
	outcome := batchOutcome{}
	agentConfig := settings.Config.AgentConfig
	if retryBudget < 1 {
		retryBudget = 1
	}
	// The model is wrapped before the generator sees it: the body is decided
	// inside the product's generator and dropped with the error there.
	recorder := &recordingChatModel{inner: chat}
	generator, err := agent.NewModelSegmentGenerator(recorder, agentConfig.SummarySegmentMaxInputTokens,
		agentConfig.SummarySegmentMaxOutputTokens)
	if err != nil {
		return seededLayered{}, outcome, err
	}
	seeded := seededLayered{}
	whiteboard := ""
	previous := ""
	seq := 0
	// The product's worker does not retry at once: it parks the job until
	// now + RetryDelay and comes back to it. The wait is part of the policy -
	// a provider that timed out or dropped the connection is being given room,
	// not asked the same question twice in a row - so a driver that retried
	// immediately would be measuring a schedule the product never runs.
	retryDelay := time.Duration(agentConfig.SummarySegmentRetryDelaySeconds) * time.Second
	outcome.RetryDelaySeconds = agentConfig.SummarySegmentRetryDelaySeconds
	for start := 0; start < len(messages); {
		end := start + agentConfig.SummarySegmentMaxMessages
		if end > len(messages) {
			end = len(messages)
		}
		if end-start < agentConfig.SummarySegmentTriggerMessages {
			break
		}
		batch := messages[start:end]
		outcome.Batches++
		var result agent.SegmentGenerationResult
		var batchErr error
		for attempt := 0; attempt < retryBudget; attempt++ {
			if attempt > 0 {
				outcome.Retries++
				if err := waitBeforeRetry(ctx, retryDelay); err != nil {
					break
				}
			}
			result, batchErr = generator.GenerateSegment(ctx, agent.SegmentGenerationInput{
				Whiteboard: whiteboard, PreviousSegment: previous, Messages: batch,
				FirstMessageID: batch[0].ID, LastMessageID: batch[len(batch)-1].ID,
			})
			if batchErr == nil {
				outcome.Attempts++
				break
			}
			outcome.failed(batchErr)
			outcome.RawFailures = append(outcome.RawFailures, rawFailure{
				Batch:   outcome.Batches,
				Attempt: attempt + 1,
				Reason:  segmentFailureLabel(batchErr),
				Excerpt: recorder.excerpt(),
			})
			if attempt == 0 {
				outcome.FirstFailures++
			}
			if ctx.Err() != nil {
				break
			}
		}
		if batchErr != nil {
			// The product's worker moves on and leaves the stretch uncovered.
			outcome.FinalFailures++
			start = end
			continue
		}
		seq++
		row := model.SessionSummarySegment{
			UserID: 7001, SessionID: sessionID, Seq: seq,
			StartMessageID: batch[0].ID, EndMessageID: batch[len(batch)-1].ID,
			StartAt: messages[start].CreatedAt, EndAt: batch[len(batch)-1].CreatedAt,
			Topic: result.Segment.Topic, Content: result.SegmentContent,
			NamesAndNumbers:   model.MarshalSessionSummarySegmentTerms(result.Segment.Names, result.Segment.Numbers),
			WhiteboardChanges: mustMarshalChanges(result.Changes),
			TokenEstimate:     agent.RenderedSegmentTokens(model.SessionSummarySegment{Content: result.SegmentContent, EndMessageID: batch[len(batch)-1].ID}),
			IndexStatus:       model.SessionSummarySegmentIndexPending,
		}
		if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error { return tx.Create(&row).Error }); err != nil {
			return seeded, outcome, err
		}
		seeded.segments = append(seeded.segments, row)
		whiteboard = result.WhiteboardContent
		seeded.board = result.Whiteboard
		previous = result.SegmentContent
		start = end
	}
	if whiteboard != "" {
		board := model.SessionWhiteboard{
			UserID: 7001, SessionID: sessionID, Content: whiteboard,
			CoveredMessageID: messages[len(messages)-1].ID,
			TokenEstimate:    agent.RenderedWhiteboardTokens(seeded.board),
		}
		if err := (guardedStore{s}).with(ctx, func(tx *gorm.DB) error { return tx.Create(&board).Error }); err != nil {
			return seeded, outcome, err
		}
	}
	return seeded, outcome, nil
}

// jsonObjectResponseFormat asks the provider for a JSON object.
//
// It is a provider-enforced constraint, not a hint: the prompt has said "Output
// JSON only" for a while, and the parse failures that throw a batch away
// happened anyway. The two are not substitutes - a provider that refuses the
// field returns an error on the first attempt, which is a failure this driver
// would see immediately rather than mistaking for a model that wrote prose.
func jsonObjectResponseFormat() *openai.ChatCompletionResponseFormat {
	return &openai.ChatCompletionResponseFormat{Type: openai.ChatCompletionResponseFormatTypeJSONObject}
}

// renderWindowPromptFor replaces the window's system prompt with the one this
// turn's tool table supports, the way the product's runner does.
//
// The product renders after the tool table is known, because the table decides
// the prompt: a rule that names a tool the model cannot see is a rule it cannot
// obey. A measurement that took the builder's prompt as it stood skipped that
// step and sent a knowledge-search contract to a turn with no search tool, so
// the model followed the contract and refused a question the board answered.
//
// The names are the ones the model would see, spelled out rather than derived
// from a registry, because this driver hands the model no tools at all: the
// honest table is the one the harness actually gives it, and a run that wants
// the knowledge-on case has to say so.
func renderWindowPromptFor(window agent.ContextWindow,
	toolNames []string, question string) (agent.ContextWindow, bool, error) {
	if len(window.Messages) == 0 || window.Messages[0] == nil || window.Messages[0].Role != schema.System {
		return window, false, nil
	}
	caps := agent.CapabilitiesFromToolNames(toolNames)
	caps.MemoryIntent = agent.ExplicitMemoryIntent(question)
	base := window.Messages[0].Content
	rendered := agent.RenderSystemPrompt(base, caps)
	if rendered == base {
		return window, false, nil
	}
	window.Messages[0] = schema.SystemMessage(rendered)
	// The budget describes the prompt that will be sent, not the one the
	// builder assembled. The product recomputes these three fields for the same
	// reason (runner.go, renderWindowPrompt); a report that kept the assembled
	// numbers would publish a system cost nobody paid.
	window.Budget.System = agent.MessageTokenCost(rendered)
	window.Budget.Assembled = window.Budget.Assembled - agent.MessageTokenCost(base) + window.Budget.System
	window.Budget.ProjectedInput = window.Budget.Assembled + window.Budget.EvidenceReserved
	return window, true, nil
}

// requireLayeredArm refuses a configuration this harness cannot run.
//
// The driver builds one arm: the layered state read back into the window. The
// two baseline arms - the old rolling summary, and a window that simply drops
// what does not fit - are different assembly paths. A driver that pasted their
// budgets onto this one would spend model calls generating segments and then
// render empty blocks from budgets meant for another arm, and the result would
// look like a measurement rather than a mistake.
func requireLayeredArm(mode string) error {
	if mode == agent.SummarySegmentModeOn {
		return nil
	}
	return fmt.Errorf("this harness measures the layered read-back arm only; "+
		"the configuration asks for summarySegmentMode=%q, whose assembly path is not built here", mode)
}

// resolveArm is which mode the run will use.
//
// A run with no configuration file is this harness measuring its own arm, so the
// arm's mode is the default. A run that names a file has said what it wants, and
// a mode this harness cannot build is refused rather than overridden: silently
// switching a baseline configuration to the layered mode is what produced model
// calls nobody asked for and blocks rendered from another arm's budgets.
func resolveArm(configuredMode string, hasConfiguration bool) (string, error) {
	if !hasConfiguration {
		return agent.SummarySegmentModeOn, nil
	}
	if err := requireLayeredArm(configuredMode); err != nil {
		return "", err
	}
	return configuredMode, nil
}

// buildLayeredReadWindow assembles the window the way the product does.
func buildLayeredReadWindow(ctx context.Context, s *Sandbox, repository string,
	settings *entry.Settings, sessionID, question string) (agent.ContextWindow, error) {
	config := settings.Config.AgentConfig
	builder, err := agent.NewContextBuilder(agent.ContextConfig{
		ModelWindowTokens:         config.ContextModelWindowTokens,
		InputTargetTokens:         config.ContextInputTargetTokens,
		OutputReserveTokens:       config.ContextOutputReserveTokens,
		EvidenceReserve:           config.KnowledgeMaxRunTokens,
		MaxGlobalUserMemoryTokens: config.ContextMaxGlobalUserMemoryTokens,
		MaxTopicUserMemoryTokens:  config.ContextMaxTopicUserMemoryTokens,
		MaxSummaryTokens:          config.ContextMaxSummaryTokens,
		MaxMasteryTokens:          config.ContextMaxMasteryTokens,
		MaxEpisodeTokens:          config.ContextMaxEpisodeTokens,
		MaxWhiteboardTokens:       config.ContextMaxWhiteboardTokens,
		MaxResidentSegmentTokens:  config.ContextMaxResidentSegmentTokens,
		MaxRetrievedSegmentTokens: config.ContextMaxRetrievedSegmentTokens,
		MaxSegmentTokens:          config.ContextMaxSegmentTokens,
		EpisodeRecallMode:         agent.EpisodeRecallMode(config.EpisodeRecallMode),
		HistoryFetchLimit:         config.ContextHistoryFetchLimit,
		MemoryReadMode:            memoryrecall.ReadMode(settings.Config.MemoryReadMode),
	}, historyAdapter{store: guardedStore{s}, sessionID: sessionID},
		nil, nil, nil, agent.EmptySummaryProvider{})
	if err != nil {
		return agent.ContextWindow{}, err
	}
	preselector, err := agent.NewSummarySegmentPreselector(nil, nil)
	if err != nil {
		return agent.ContextWindow{}, err
	}
	// The preselector runs on the same numbers as the block that renders its
	// answer, exactly as the product entry does. Left at its own defaults it
	// would hand the renderer more segments than fit, and the renderer keeps the
	// newest rather than the highest ranked.
	preselector.UseLimits(agent.SummarySegmentPreselectLimits(
		config.ContextMaxRetrievedSegmentTokens, config.ContextMaxSegmentTokens))
	provider, err := agent.NewDatabaseLayeredSummaryProvider(
		sessiondao.NewRepository(s.db), preselector, agent.DefaultResidentSummarySegments)
	if err != nil {
		return agent.ContextWindow{}, err
	}
	if err := builder.UseLayeredSummaryProvider(provider); err != nil {
		return agent.ContextWindow{}, err
	}
	return builder.Build(ctx, agent.RunContext{UserID: 7001, SessionID: sessionID, Question: question})
}

// historyAdapter lets the builder read the conversation back.
type historyAdapter struct {
	store     guardedStore
	sessionID string
}

func (h historyAdapter) ListRecentAfter(ctx context.Context, sessionID string, afterID uint, limit int) ([]model.Message, error) {
	return h.store.ListAfter(ctx, sessionID, afterID, limit)
}

// inspectWindow records what the builder actually put in front of the model.
//
// The builder sends three blocks and each one is its own system message: the
// board in front of the raw conversation, the resident segments after it, and
// the segments this question selected after those. Keeping the blocks apart is
// what lets a measurement say whether a segment came along every turn or was
// fetched for this question.
func inspectWindow(read *LayeredRead, window agent.ContextWindow, seeded seededLayered,
	messages []model.Message) {
	segmentBlocks := 0
	for _, message := range window.Messages {
		if message == nil || message.Role != schema.System {
			continue
		}
		content := message.Content
		switch {
		case strings.HasPrefix(content, "Current state"):
			read.WhiteboardInWindow = true
			read.WhiteboardText = content
		case strings.HasPrefix(content, "Summary segments"):
			seqs := segmentSeqsIn(content)
			switch segmentBlocks {
			case 0:
				read.ResidentSegments = seqs
			case 1:
				read.RetrievedSegments = seqs
			}
			segmentBlocks++
			for _, seq := range seqs {
				if !containsInt(read.SegmentsInWindow, seq) {
					read.SegmentsInWindow = append(read.SegmentsInWindow, seq)
				}
			}
		}
		if marks := supersessionMarksIn(content); len(marks) > 0 {
			read.SupersessionMarks = append(read.SupersessionMarks, marks...)
		}
	}
	read.EvidenceSeqs = evidenceSeqs(seeded.segments, messages, read.EvidenceTurns)
	for _, seq := range read.EvidenceSeqs {
		if containsInt(read.SegmentsInWindow, seq) {
			read.EvidenceInView = true
		}
	}
	switch {
	case len(read.EvidenceSeqs) == 0:
		read.EvidenceNote = "the turns the answer comes from are not covered by any segment"
	case read.EvidenceInView:
		read.EvidenceNote = "the segment the answer comes from reached the window"
	default:
		read.EvidenceNote = "the segment the answer comes from did not reach the window"
	}
}

// segmentSeqsIn reads the segment numbers out of a rendered block.
//
// The renderer's own header is [第 N 段 · 消息 a-b · time], so the number sits
// between that mark and the word for "segment". A block that no longer has
// that shape yields nothing rather than a guess.
func segmentSeqsIn(block string) []int {
	seqs := make([]int, 0, 8)
	for index := 0; index < len(block); {
		start := strings.Index(block[index:], "[第 ")
		if start < 0 {
			break
		}
		start += index + len("[第 ")
		end := start
		for end < len(block) && block[end] >= '0' && block[end] <= '9' {
			end++
		}
		if end == start || !strings.HasPrefix(block[end:], " 段") {
			index = start
			continue
		}
		seq, err := strconv.Atoi(block[start:end])
		if err == nil && seq > 0 && !containsInt(seqs, seq) {
			seqs = append(seqs, seq)
		}
		index = end
	}
	return seqs
}

// supersessionMarksIn returns every old-value mark a block carries.
//
// The mark is the renderer's own wording: 〔第 N 轮已改为 X，以当前状态为准〕.
// It is written next to the value it replaced, so the model can tell a
// superseded figure from the current one.
func supersessionMarksIn(block string) []string {
	marks := make([]string, 0, 4)
	for index := 0; index < len(block); {
		start := strings.Index(block[index:], "〔")
		if start < 0 {
			break
		}
		start += index
		end := strings.Index(block[start:], "〕")
		if end < 0 {
			break
		}
		mark := block[start : start+end+len("〕")]
		// The block header itself names the pattern as 〔…已改为…〕. It is a
		// description of the mark, not a mark, and counting it would say
		// every window with a board carried a replaced value.
		if strings.Contains(mark, "已改为") && !strings.Contains(mark, "…") {
			marks = append(marks, mark)
		}
		index = start + end + len("〕")
	}
	return marks
}

// evidenceSeqs reports which segments cover the turns the answer comes from.
func evidenceSeqs(segments []model.SessionSummarySegment, messages []model.Message,
	evidenceTurns []int) []int {
	seqs := make([]int, 0, 4)
	for _, turn := range evidenceTurns {
		first := 2*turn - 2
		last := 2*turn - 1
		if first < 0 || last >= len(messages) {
			continue
		}
		from := messages[first].ID
		to := messages[last].ID
		for _, segment := range segments {
			if segment.StartMessageID <= from && to <= segment.EndMessageID {
				if !containsInt(seqs, segment.Seq) {
					seqs = append(seqs, segment.Seq)
				}
			}
		}
	}
	return seqs
}

// checkToolAuthority asks the product's own grant set what the window made
// readable.
//
// The rule the window is built on is that an id is readable because a
// statement that carried it reached the model, and an id from a statement the
// budget cut is not readable because the model never saw it. That decision is
// the product's, so it is asked rather than recomputed here: the same
// HistoryGrantSet the read-back tool consults is built from the window's
// grants, and every id is put to it.
//
// What this does not do is run the tool. The read-back and browse tools are
// built inside the product's internal wiring, which a harness in another
// module cannot import, so the check stops at the authorisation the tool
// would have consulted. Every row says so with Exercised=false, and a report
// that reads these rows must not describe them as tool calls.
func checkToolAuthority(s *Sandbox, sessionID string, window agent.ContextWindow,
	seeded seededLayered, read *LayeredRead) {
	read.Grants = len(window.SummarySegmentHistoryGrants)
	visible := visibleMessageIDs(window)
	run := agent.RunContext{UserID: 7001, SessionID: sessionID, Question: read.Question}
	set := agent.NewHistoryGrantSet(run, window.SummarySegmentHistoryGrants)
	for _, grant := range window.SummarySegmentHistoryGrants {
		if !containsUint(read.GrantedIDs, grant.MessageID) {
			read.GrantedIDs = append(read.GrantedIDs, grant.MessageID)
		}
		// Both answers are kept: what the window printed, and what the
		// product says may be read. They should agree, and a row where they
		// do not is the finding.
		_, shown := visible[grant.MessageID]
		authorised := set.Covers(grant.MessageID, sessionID)
		read.ToolChecks = append(read.ToolChecks, LayeredToolCheck{
			Tool: "read_history_message", MessageID: grant.MessageID, Source: grant.SourceKind,
			Granted: true, Authorised: authorised, Exercised: false,
			Detail: grantNote(shown, authorised),
		})
	}
	// A message the window never showed must not be readable. This is the
	// negative case: the id is real, it is in the session, and the window
	// never put it in front of the model.
	hidden := messageNotShown(s.db, sessionID, visible)
	if hidden != 0 {
		read.ToolChecks = append(read.ToolChecks, LayeredToolCheck{
			Tool: "read_history_message", MessageID: hidden, Granted: false,
			Authorised: set.Covers(hidden, sessionID), Exercised: false,
			Detail: "not granted: the window never showed this message",
		})
	}
	// The browse tool is authorised inside the tool: whether a segment may be
	// opened depends on the segment store, not on a grant set this harness can
	// ask. The row records only what the window carried and says the question
	// is unanswered here.
	for _, segment := range seeded.segments {
		inWindow := containsInt(read.SegmentsInWindow, segment.Seq)
		read.ToolChecks = append(read.ToolChecks, LayeredToolCheck{
			Tool: "browse_summary_segments", SegmentSeq: segment.Seq,
			Granted: inWindow, Authorised: false, Pending: true, Exercised: false,
			Detail: browseNote(inWindow),
		})
	}
}

// visibleMessageIDs collects every message id the window showed.
func visibleMessageIDs(window agent.ContextWindow) map[uint]struct{} {
	visible := map[uint]struct{}{}
	for _, message := range window.Messages {
		if message == nil {
			continue
		}
		for _, id := range evidenceIDsIn(message.Content) {
			visible[id] = struct{}{}
		}
	}
	return visible
}

// evidenceIDsIn reads the ids out of one block's [message_ids=…] markers.
func evidenceIDsIn(block string) []uint {
	const marker = "[message_ids="
	ids := make([]uint, 0, 16)
	for index := 0; index < len(block); {
		start := strings.Index(block[index:], marker)
		if start < 0 {
			break
		}
		start += index + len(marker)
		end := strings.Index(block[start:], "]")
		if end < 0 {
			break
		}
		for _, part := range strings.Split(block[start:start+end], ",") {
			value, err := strconv.Atoi(strings.TrimSpace(part))
			if err == nil && value > 0 {
				ids = append(ids, uint(value))
			}
		}
		index = start + end + 1
	}
	return ids
}

// browseNote says what the browse tool is for on this segment.
func browseNote(inWindow bool) string {
	if inWindow {
		return "already in the window; browsing it again is redundant but allowed"
	}
	return "not in the window; this is what the tool exists to reach"
}

// grantNote says why a grant was or was not honoured.
// grantNote says what the window showed and what the product's grant set
// answered. The two can disagree, and that disagreement is the finding: it
// means the grants and the rendered text no longer describe the same window.
func grantNote(shown, authorised bool) string {
	switch {
	case shown && authorised:
		return "shown in the window and authorised"
	case shown && !authorised:
		return "shown in the window but the grant set refuses it"
	case !shown && authorised:
		return "authorised but the window never showed it"
	default:
		return "neither shown nor authorised"
	}
}

// messageNotShown returns one message the window did not show, or zero when
// every message was shown.
func messageNotShown(db *gorm.DB, sessionID string, visible map[uint]struct{}) uint {
	var messages []model.Message
	if err := db.WithContext(context.Background()).
		Where("session_id = ?", sessionID).Order("id ASC").Find(&messages).Error; err != nil {
		return 0
	}
	for _, message := range messages {
		if _, shown := visible[message.ID]; !shown {
			return message.ID
		}
	}
	return 0
}

// mustMarshalChanges encodes the board changes a segment recorded.
func mustMarshalChanges(changes []model.SessionSummarySegmentChange) string {
	encoded, err := model.MarshalSessionSummarySegmentChanges(changes)
	if err != nil {
		return ""
	}
	return encoded
}

// boardEntryCount counts the entries a board carries.
func boardEntryCount(document model.SessionWhiteboardDocument) int {
	count := 0
	if document.Goal != nil {
		count++
	}
	return count + len(document.Constraints) + len(document.Plan) + len(document.OpenItems)
}

// layeredReadScorer names the rule this driver's verdict comes from.
//
// It is deliberately not a scorer's name. The formal comparison is judged by
// the product's own score.mjs, where an answer that names both the current and
// the replaced value goes to needs_judge and is judged twice. What is written
// here is a smoke reading that says whether the run is worth looking at.
const layeredReadScorer = "tense-heuristic-smoke-only"

// normaliseAnswer folds an answer down to the form the mention checks compare.
//
// Whitespace is removed because a rendered figure comes back as "15 页" from
// one model and "15页" from another, and that difference is formatting, not an
// answer. Case is folded for the same reason.
func normaliseAnswer(answer string) string {
	return strings.ToLower(strings.Join(strings.Fields(answer), ""))
}

// mentionsAny reports whether the answer names one of the values.
func mentionsAny(answer string, values []string) bool {
	normalised := normaliseAnswer(answer)
	for _, value := range values {
		needle := normaliseAnswer(value)
		if needle != "" && strings.Contains(normalised, needle) {
			return true
		}
	}
	return false
}

// mentionsBefore reports whether a forbidden value is named before an accepted
// one.
//
// It separates "最初 15 页，后来 30 页" (the accepted figure is the answer) from
// "最初 30 页，后来 15 页" (the replaced figure is the answer). A phrase rule
// that only looks at what sits next to a figure cannot tell them apart,
// because in both the second figure sits next to a change word.
func mentionsBefore(answer string, forbidden, accepted []string) bool {
	normalised := normaliseAnswer(answer)
	firstForbidden := firstMention(normalised, forbidden)
	if firstForbidden < 0 {
		return false
	}
	firstAccepted := firstMention(normalised, accepted)
	if firstAccepted < 0 {
		return false
	}
	return firstForbidden < firstAccepted
}

// firstMention returns where the earliest of the values first appears, or -1.
func firstMention(normalised string, values []string) int {
	first := -1
	for _, value := range values {
		needle := normaliseAnswer(value)
		if needle == "" {
			continue
		}
		index := strings.Index(normalised, needle)
		if index >= 0 && (first < 0 || index < first) {
			first = index
		}
	}
	return first
}

// forbidList drops the accepted values from the forbidden ones.
//
// An answer key that names the same value right and wrong cannot be scored,
// and the scorer's rule is that the accepted value wins.
func forbidList(forbidden, accepted []string) []string {
	out := make([]string, 0, len(forbidden))
	for _, value := range forbidden {
		if mentionsAny(value, accepted) {
			continue
		}
		out = append(out, value)
	}
	return out
}

// The three things a question can ask for.
const (
	// tenseOriginal asks what the value used to be.
	tenseOriginal = "original"
	// tenseCurrent asks what the value is now.
	tenseCurrent = "current"
	// tenseUnknown asks for a fact that has only one value in the session. A
	// replaced figure named anywhere in such an answer is a wrong figure.
	tenseUnknown = "unknown"
)

// originalMarkers are the words a question uses when it asks for the value
// that was replaced.
var originalMarkers = []string{"最初", "一开始", "起初", "原来", "原先", "最早", "当初"}

// currentMarkers are the words a question uses when it asks for the value
// that holds now.
var currentMarkers = []string{"现在", "目前", "当前", "如今", "最新", "改成多少", "调到"}

// tenseOf reads what a question asks for.
//
// The first marker to appear wins, which is the order a Chinese question
// states its subject in: "现在要求提前几天" asks about now, and "最初每次读多少页"
// asks about the start.
func tenseOf(question string) string {
	text := strings.Join(strings.Fields(question), "")
	first, tense := len(text), tenseUnknown
	for _, marker := range originalMarkers {
		if index := strings.Index(text, marker); index >= 0 && index < first {
			first, tense = index, tenseOriginal
		}
	}
	for _, marker := range currentMarkers {
		if index := strings.Index(text, marker); index >= 0 && index < first {
			first, tense = index, tenseCurrent
		}
	}
	return tense
}

// laterMarkers frame a figure as the one a change produced.
var laterMarkers = []string{
	"改成", "改为", "改到", "换成了", "变成", "更新为", "调整为", "调整到",
	"后来", "之后", "后面",
}

// earlierMarkers frame a figure as the one a change replaced.
var earlierMarkers = []string{
	"之前", "以前", "原来", "原先", "最初", "起初", "最早", "当初",
	"不再", "不作数", "不算数", "已改", "作废", "已经改",
}

// markersFor picks the phrasing that excuses a replaced figure.
//
// A question about the original value is answered by naming the change
// ("后来改成 30 页"), and a question about the current value is answered by
// naming what it replaced ("之前说的 3 天已不作数"). The two are not
// interchangeable: the same words on the other question would excuse the
// wrong answer.
func markersFor(tense string) []string {
	switch tense {
	case tenseOriginal:
		return laterMarkers
	case tenseCurrent:
		return earlierMarkers
	default:
		return nil
	}
}

// mentionsOutsidePhrasing reports whether a value appears anywhere the answer
// is not framing it as the value that was replaced.
//
// The reach is deliberately short. A marker within a dozen characters of the
// figure is the sentence the figure sits in; a marker anywhere in the answer
// would let a single "后来" elsewhere excuse every wrong figure it names.
func mentionsOutsidePhrasing(answer, value string, excusing []string) bool {
	text := []rune(normaliseAnswer(answer))
	needle := []rune(normaliseAnswer(value))
	if len(needle) == 0 {
		return false
	}
	for index := 0; index+len(needle) <= len(text); index++ {
		if string(text[index:index+len(needle)]) != string(needle) {
			continue
		}
		if !framedBy(text, index, len(needle), excusing) {
			return true
		}
	}
	// Every mention was framed as the replaced value, or the answer never
	// named it. Neither is an answer given with the replaced figure.
	return false
}

// framedBy reports whether the figure at this place sits in a sentence the
// excusing words reach.
func framedBy(text []rune, from, width int, excusing []string) bool {
	if len(excusing) == 0 {
		return false
	}
	const reach = 12
	start := from - reach
	if start < 0 {
		start = 0
	}
	end := from + width + reach
	if end > len(text) {
		end = len(text)
	}
	window := string(text[start:end])
	for _, marker := range excusing {
		if strings.Contains(window, marker) {
			return true
		}
	}
	return false
}

func containsInt(values []int, value int) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func containsUint(values []uint, value uint) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// WriteLayeredRead writes one read-side run as JSON.
func WriteLayeredRead(path string, read LayeredRead) error {
	encoded, err := json.MarshalIndent(read, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}

// LayeredReadTimeout is the deadline one read-side run is given.
const LayeredReadTimeout = 30 * time.Minute

// The two things this driver can measure.
const (
	// LayeredStageFull seeds the state, reads it back and answers.
	LayeredStageFull = "full"
	// LayeredStageBatches stops after the writer ran, so the failure rate a
	// batch costs can be measured without paying for an answer per
	// conversation.
	LayeredStageBatches = "batches"
)
