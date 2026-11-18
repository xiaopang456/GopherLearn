package conversation

import (
	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ProductScope = "完整产品服务；后台任务在被测回答之前不执行；之后产生的后台调用单独记账"

// ProductResult is offline wiring evidence, never a real-model quality result.
type ProductResult struct {
	Scope           string               `json:"scope"`
	Environment     string               `json:"env"`
	Answer          string               `json:"answer"`
	Response        json.RawMessage      `json:"response"`
	ResponseText    string               `json:"response_text,omitempty"`
	BeforeCounts    map[string]JobCounts `json:"before_counts"`
	AfterCounts     map[string]JobCounts `json:"after_counts"`
	StoppedCounts   map[string]JobCounts `json:"stopped_counts"`
	Events          []JobEvent           `json:"job_events"`
	Calls           []ModelCall          `json:"model_calls"`
	UsageByRole     map[string]RoleUsage `json:"usage_by_role"`
	Error           string               `json:"error,omitempty"`
	Entry           string               `json:"entry"`
	Provenance      Provenance           `json:"provenance"`
	ContextMessages []*schema.Message    `json:"context_messages"`
	Audit           BackgroundAudit      `json:"background_evidence"`
	// MemoryIndexEvidence is read from the shared derived memory indexes: once
	// before the answer is asked and once after the background window closed.
	// A real attempt without both readings is not decidable, because the second
	// one is the only evidence that the answer did not leave state behind for
	// the next repeat.
	MemoryIndexEvidence MemoryIndexEvidence `json:"memory_index_evidence"`
	BackgroundAudit     struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason,omitempty"`
	} `json:"backgroundAudit"`
	QuietWindowSeconds   int   `json:"quiet_window_seconds"`
	RestoreMilliseconds  int64 `json:"restore_ms"`
	ShutdownComplete     bool  `json:"shutdown_complete"`
	QuestionMilliseconds int64 `json:"question_ms"`
	// Questions holds one entry per question the attempt asked, in the order
	// they were asked. It is written only when the attempt asked more than
	// one: the single-question attempt keeps its own fields, and a reader of
	// an old attempt finds the same shape it always did.
	Questions []QuestionResult `json:"questions,omitempty"`
	// SeededMemories says what the seeded profile memories became, and
	// SeededMemoryRows is what was read back from the database afterwards.
	// The write is checked by the read: a seed that cannot be read back is
	// not a seed.
	SeededMemories   SeedMemoryReport       `json:"seeded_memories"`
	SeededMemoryRows []model.LongTermMemory `json:"seeded_memory_rows,omitempty"`
	// SessionCard is what the card arm observed: the card that was written,
	// whether it was indexed before the question was asked, the conversation
	// the question was asked in and what the answer looked at.
	SessionCard *CardArmEvidence `json:"session_card,omitempty"`
}

// QuestionResult is one question of an attempt and what came back.
//
// The answer, the raw response and the steps are kept per question, so that
// a scenario with several questions has the evidence of each question next
// to its own answer and not in one pile.
type QuestionResult struct {
	Index int `json:"index"`
	// Turn is the round of the conversation the question was asked at, when
	// the attempt says so. The scorer reads the attempt by it.
	Turn                 int               `json:"turn,omitempty"`
	Question             string            `json:"question"`
	Answer               string            `json:"answer"`
	RunID                string            `json:"run_id,omitempty"`
	Response             json.RawMessage   `json:"response,omitempty"`
	Steps                []model.AgentStep `json:"steps,omitempty"`
	ContextMessages      []*schema.Message `json:"context_messages,omitempty"`
	StartedAt            time.Time         `json:"started_at"`
	FinishedAt           time.Time         `json:"finished_at"`
	QuestionMilliseconds int64             `json:"question_ms"`
	Error                string            `json:"error,omitempty"`
}

// AttemptInput is what one attempt adds to the standard wiring.
//
// The memories are the profile memories the learner confirmed before this
// attempt. They are written into the evaluation database before the
// conversation is prepared and again before the question is asked, because
// the answer is asked in a fresh database.
//
// The questions are asked in order, in the same session. A scenario with one
// question has one entry; a scenario that asks a follow-up has one entry per
// question, and the earlier question and its answer stay in the session, so
// that the later question is answered in the context of the earlier one.
type AttemptInput struct {
	Memories  []SeedMemory
	Questions []Question
	// WaitForSummarySegment makes the run wait until the session has a
	// summary segment before the first question is asked.
	//
	// The layered summary's writer keeps its own schedule, and a run that
	// asked its question before the segment existed would measure a window
	// without one. The wait is what turns "the segment was written" into
	// "the segment was in the window when the model was asked".
	WaitForSummarySegment bool
	// SessionCard says which arm this attempt measures. It is nil for the
	// old attempt, and it names the card path when the attempt measures it.
	SessionCard *CardArm
}

// Question is one question of an attempt.
//
// The Turn is the round of the conversation the question was asked at. It is
// zero when the attempt does not say: a question that names no turn is asked
// at the end of the conversation, and the scorer reads it by its position.
type Question struct {
	Text string
	Turn int
}

// ProductSummaryConfig mirrors the production wiring, including its context
// summary output limit rather than the standalone summarizer test defaults.
func ProductSummaryConfig(c *config.Config) agent.SummaryConfig {
	a := c.AgentConfig
	return agent.SummaryConfig{TriggerMessages: a.SummaryTriggerMessages, TriggerTokens: a.SummaryTriggerTokens, RetainMessages: a.SummaryRetainMessages, MaxInputTokens: a.SummaryMaxInputTokens, MaxOutputTokens: a.ContextMaxSummaryTokens, FetchLimit: a.SummaryFetchLimit, Timeout: time.Duration(a.SummaryTimeoutSeconds) * time.Second, StageBudget: time.Duration(a.SummaryStageBudgetSeconds) * time.Second, MaxDeadlineShare: agent.DefaultSummaryConfig().MaxDeadlineShare, FailureCooldown: time.Duration(a.SummaryFailureCooldownSeconds) * time.Second, Budget: agent.SummaryBudgetOf(a.SummaryGroupItemLimit, a.SummaryOpenLoopsItemLimit, a.SummaryEvidenceIDLimit)}
}

func offlineSettings(repository, storage string) (*entry.Settings, error) {
	data, err := os.ReadFile(filepath.Join(repository, "tutorial", "v2-entry", "isolated.example.toml"))
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), `key = ""`, `key = "synthetic-offline-key-with-at-least-32-characters"`)
	text = strings.ReplaceAll(text, `chatKey = ""`, `chatKey = "synthetic-never-sent"`)
	text = strings.ReplaceAll(text, `embeddingApiKey = ""`, `embeddingApiKey = "synthetic-never-sent"`)
	dir, err := os.MkdirTemp("", "gl-offline-config-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "offline.toml")
	if err = os.WriteFile(path, []byte(text), 0600); err != nil {
		return nil, err
	}
	settings, err := entry.Load(path)
	if err != nil {
		return nil, err
	}
	applySummaryAssistantContentEnv(settings)
	settings.Config.KnowledgeConfig.StorageDir = storage
	settings.Config.AgentConfig.EpisodeQuietWindowSeconds = 86400
	settings.Config.AgentThinkingEnabled = true
	settings.Config.RagChatThinkingDialect = config.ChatThinkingDialectDeepSeek
	settings.Config.RagRerankEnabled = true
	settings.Config.RagRerankProvider = config.RerankProviderDashScope
	settings.Config.RagRerankEndpoint = "http://127.0.0.1:1/v1/rerank"
	settings.Config.RagRerankModel = "synthetic-rerank"
	settings.Config.RagRerankToken = "synthetic-rerank-token-at-least-32-characters"
	return settings, settings.Validate()
}

// summaryAssistantContentEnv is the switch that says whether the layered
// summary records what the assistant produced as well as what the learner
// said.
const summaryAssistantContentEnv = "AGENT_SUMMARY_ASSISTANT_CONTENT"

// applySummaryAssistantContentEnv carries the switch from the environment
// into the settings the product is run with.
//
// The product's own configuration loader reads this variable, but the
// isolated entry the harness runs never consults the process environment: it
// is handed one explicit file and reads nothing else, so a switch said in the
// environment never reached it. A measurement of the mode that records the
// assistant's products therefore had no way to turn that mode on.
//
// The value is not checked here. The configuration's own validation is the
// check: a value that is neither "off" nor "on" stops the run instead of
// quietly measuring a mode nobody meant.
func applySummaryAssistantContentEnv(settings *entry.Settings) {
	if settings == nil {
		return
	}
	if value, ok := os.LookupEnv(summaryAssistantContentEnv); ok {
		settings.Config.AgentConfig.SummaryAssistantContent = strings.TrimSpace(value)
	}
}

func closeSandbox(s *Sandbox) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.Close(ctx)
}

// PrepareOfflineProduct uses a deterministic fake model behind the real summary
// generator and rolling provider. There is no online/provider credential path.
type WarmupReport struct {
	Snapshot      Snapshot             `json:"-"`
	Calls         []ModelCall          `json:"model_calls"`
	Turns         []PreparedTurn       `json:"turns"`
	DegradedTurns int                  `json:"degraded_turns"`
	UsageByRole   map[string]RoleUsage `json:"usage_by_role"`
	// FailureCooldownSeconds is the product's own failure cooldown, and
	// CooldownWaits is how many times the replay waited it out before writing
	// the next turn. A replay that does not wait would spend the cooldown in
	// one breath and never compact again.
	FailureCooldownSeconds int `json:"failure_cooldown_seconds"`
	CooldownWaits          int `json:"cooldown_waits"`
	// SummaryFailureReasons counts the degraded turns by the step that
	// failed. The cooldown is one of them: it is counted on its own so that
	// the share of degraded turns still reads as the share of real failures.
	SummaryFailureReasons map[string]int `json:"summary_failure_reasons,omitempty"`
	// CooldownTurns is how many of the degraded turns were held back by the
	// cooldown rather than failed by themselves.
	CooldownTurns int `json:"cooldown_turns"`
	// SummaryTruncations is how many times the compression budget trimmed a
	// summary, and SummaryDroppedItems is how many items it cut.
	SummaryTruncations  int `json:"summary_truncations"`
	SummaryDroppedItems int `json:"summary_dropped_items"`
	// SeededMemories says what the seeded profile memories became. The
	// memories are written before the conversation is prepared, so that the
	// product sees them while the conversation is built.
	SeededMemories SeedMemoryReport `json:"seeded_memories"`
	// Projection is what the preparation projected: the conversations it
	// published, the kind of document it published, and how long it waited
	// for the memories to be indexed.
	Projection *ArmProjection `json:"projection,omitempty"`
	// LayeredSummary is what the preparation wrote for the layered summary:
	// the summary segments and the whiteboard of every conversation.
	//
	// It is here because the card path reads THAT state, and a report that
	// said the card path was measured without saying whether the compressed
	// state existed would be describing a card that was written from the raw
	// messages alone.
	//
	// A conversation whose entry carries an Error got no segment and no
	// board, and its card was written from the raw messages. The count of
	// those is here so that a reader does not have to count them.
	LayeredSummary []LayeredSummaryRecord `json:"layered_summary,omitempty"`
	// LayeredSummaryFailures is how many conversations got no layered
	// summary.
	//
	// It is a number of its own and not something to count out of the list,
	// because the number is the finding: a card arm where a quarter of the
	// cards were written from raw messages is a different measurement from
	// one where they all were, and the reader has to be able to see it.
	LayeredSummaryFailures int `json:"layered_summary_failures"`
}

func PrepareOfflineProduct(ctx context.Context, repository string, histories []History, input AttemptInput) (Snapshot, []ModelCall, error) {
	report, err := PrepareOfflineProductReport(ctx, repository, histories, input)
	return report.Snapshot, report.Calls, err
}
func PrepareOfflineProductReport(ctx context.Context, repository string, histories []History, input AttemptInput) (report WarmupReport, resultErr error) {
	report.Snapshot, report.Calls, report.SeededMemories, resultErr = prepareOfflineProduct(ctx, repository, histories, input, func(prepared []PreparedTurn) {
		report.Turns = prepared
		SummarizeTurns(prepared, &report)
	}, func(layered []LayeredSummaryRecord) {
		report.LayeredSummary = layered
		report.LayeredSummaryFailures = countLayeredSummaryFailures(layered)
	})
	var usageErr error
	report.UsageByRole, usageErr = SummarizeCalls(report.Calls)
	return report, errors.Join(resultErr, usageErr)
}
func prepareOfflineProduct(ctx context.Context, repository string, histories []History, input AttemptInput, observe func([]PreparedTurn), observeLayered func([]LayeredSummaryRecord)) (snapshot Snapshot, calls []ModelCall, memories SeedMemoryReport, resultErr error) {
	if err := ctx.Err(); err != nil {
		return snapshot, nil, memories, err
	}
	storage, err := os.MkdirTemp("", "gl-offline-storage-")
	if err != nil {
		return snapshot, nil, memories, err
	}
	defer os.RemoveAll(storage)
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return snapshot, nil, memories, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return snapshot, nil, memories, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return snapshot, nil, memories, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "synthetic-learner"}, nil, storage); err != nil {
		return snapshot, nil, memories, err
	}
	// The seeded profile memories are written before the conversation is
	// prepared, so that the product sees them while the conversation is
	// built and not only when the question is asked.
	memories, _, err = seedAttemptMemories(ctx, s, 7, input.Memories)
	if err != nil {
		return snapshot, nil, memories, err
	}
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		for _, history := range histories {
			session := model.Session{ID: history.SessionID, UserID: 7, UserName: "synthetic-learner",
				Mode: model.SessionModeAgent, Title: "Synthetic evaluation"}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return snapshot, nil, memories, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("warmup model telemetry incomplete"))
		}
	}()
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.AgentConfig.MemoryReadMode))
	if err != nil {
		return snapshot, nil, memories, err
	}
	chat, err := WrapModel(&offlineChat{}, ledger, roles)
	if err != nil {
		return snapshot, nil, memories, err
	}
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(chat, policy.MaxInputTokens, policy.MaxOutputTokens, policy.Budget)
	if err != nil {
		return snapshot, nil, memories, err
	}
	prepared, err := s.PrepareMany(ctx, 7, "synthetic-learner", histories, generator, policy)
	observe(prepared)
	if err != nil {
		return snapshot, ledger.Calls(), memories, err
	}
	sessionIDs := make([]string, 0, len(histories))
	for _, history := range histories {
		sessionIDs = append(sessionIDs, history.SessionID)
	}
	// The layered summary is written here, with the product's own producer,
	// for the same reason the real path writes it: the card path reads the
	// summary segments and the whiteboard, and a preparation that stopped at
	// the rolling summary would produce a card written from the raw messages
	// alone.
	layered, layeredErr := WriteLayeredSummaries(ctx, s, 7, sessionIDs, chat,
		LayeredSummarySettings(settings.Config))
	if observeLayered != nil {
		observeLayered(layered)
	}
	if layeredErr != nil {
		return snapshot, ledger.Calls(), memories, layeredErr
	}
	if err := (guardedStore{sandbox: s}).with(ctx, func(tx *gorm.DB) error {
		for _, record := range layered {
			if record.Segments == 0 && record.Error == "" {
				return fmt.Errorf("the conversation %s got no summary segment", record.SessionID)
			}
		}
		return nil
	}); err != nil {
		return snapshot, ledger.Calls(), memories, err
	}
	snapshot, err = s.CaptureMany(ctx, 7, sessionIDs)
	return snapshot, ledger.Calls(), memories, err
}

// RunOfflineProduct invokes the unchanged standard product entry. Its generated
// configuration points exclusively at freshly owned databases and a loopback
// fake model endpoint. There are no replacement product dependencies.
func RunOfflineProduct(ctx context.Context, repository string, snapshot Snapshot, input AttemptInput) (ProductResult, error) {
	return runOfflineEntry(ctx, repository, snapshot, input)
}

type offlineChat struct {
	mu sync.Mutex
	// assistantContent says the fake model answers the summary segment call
	// the way the mode that records the assistant's products asks for it.
	//
	// The two modes send different prompts and expect different documents:
	// the mode that does not record the assistant's products asks for the
	// learner's points only, and a document that carried a product of the
	// assistant's would be a document nobody asked for.
	assistantContent bool
	// answers holds the prompt of every answer the fake model gave, in the
	// order it gave them. An attempt that asks several questions keeps one
	// entry per question, so that the context of the later question can be
	// read and compared with the earlier one.
	answers [][]*schema.Message
	// segmentCalls is how many summary segments the fake model was asked for.
	segmentCalls int
}

func (m *offlineChat) WithTools([]*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error) {
	return m, nil
}
func (m *offlineChat) Generate(ctx context.Context, input []*schema.Message, _ ...modelcomponent.Option) (*schema.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(input) > 0 && strings.HasPrefix(input[0].Content, segmentWriterPromptPrefix) {
		m.mu.Lock()
		m.segmentCalls++
		m.mu.Unlock()
		return schema.AssistantMessage(m.segmentDocument(input), nil), nil
	}
	if len(input) > 0 && strings.HasPrefix(input[0].Content, cardWriterPromptPrefix) {
		// The card is read as a tool call, because that is how the product
		// reads it: the arguments are the card document, and the program
		// checks every id in it against the evidence it showed the model. A
		// fake that answered with prose would leave the product with nothing
		// to store, and the arm would measure a window without a card.
		arguments, err := m.cardDocument(input)
		if err != nil {
			return nil, err
		}
		return schema.AssistantMessage("", []schema.ToolCall{{
			ID: "offline-card", Type: "function",
			Function: schema.FunctionCall{Name: cardToolName, Arguments: arguments},
		}}), nil
	}
	if len(input) > 0 && strings.HasPrefix(input[0].Content, "You compress an older portion") {
		return schema.AssistantMessage(`{"schema_version":2,"current_goal":"synthetic offline summary"}`, nil), nil
	}
	if len(input) > 0 && strings.HasPrefix(input[0].Content, "You are GopherAI") {
		m.mu.Lock()
		m.answers = append(m.answers, append([]*schema.Message(nil), input...))
		m.mu.Unlock()
		return schema.AssistantMessage("synthetic offline answer", nil), nil
	}
	return nil, fmt.Errorf("offline model has no response for this product role")
}
func (m *offlineChat) Stream(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.StreamReader[*schema.Message], error) {
	out, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	out.ResponseMeta = &schema.ResponseMeta{FinishReason: "stop"}
	return schema.StreamReaderFromArray([]*schema.Message{out}), nil
}
func (m *offlineChat) answerInput() []*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.answers) == 0 {
		return nil
	}
	return append([]*schema.Message(nil), m.answers[len(m.answers)-1]...)
}

// answerInputs returns the prompt of every answer, in the order the answers
// were given.
func (m *offlineChat) answerInputs() [][]*schema.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]*schema.Message, 0, len(m.answers))
	for _, answer := range m.answers {
		out = append(out, append([]*schema.Message(nil), answer...))
	}
	return out
}

// cardWriterPromptPrefix is how the session card's writer prompt starts.
//
// The prompt is the product's own, and the fake model answers the call it
// recognizes. A call the fake model does not recognize is refused, and a
// refused card call is a projection that produced nothing: the window then
// carries no card at all, and an attempt that only checked that the
// projection ran would call that a success.
const cardWriterPromptPrefix = "You write one card for one finished chat session"

// cardToolName is the tool the product asks the card to be written
// into. It is the product's name, and the product refuses a card that came
// back under another one.
//
// The same name is the harness's marker for the card generation: the call
// that binds this tool is the one whose raw exchange is kept.
const cardToolName = "publish_session_card"

// cardDocument is what the fake model answers when the product asks for a
// card.
//
// The card cites the messages it was shown, because the program refuses a
// citation it never showed the model: a card that cites nothing is a claim
// with nothing behind it, and the whole card is refused rather than the one
// entry.
func (m *offlineChat) cardDocument(input []*schema.Message) (string, error) {
	payload := ""
	if len(input) > 1 && input[1] != nil {
		payload = input[1].Content
	}
	var request struct {
		SessionTitle string `json:"session_title"`
		Messages     []struct {
			ID      uint   `json:"id"`
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Segments []struct {
			Seq    int      `json:"seq"`
			Points []string `json:"points"`
		} `json:"segments"`
	}
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return "", fmt.Errorf("decode the session card payload: %w", err)
	}
	title := strings.TrimSpace(request.SessionTitle)
	if title == "" {
		title = "synthetic offline session"
	}
	document := model.SessionCardDocument{
		Version:  model.SessionCardContentVersion,
		Title:    title,
		Synopsis: fmt.Sprintf("synthetic offline card for %d messages", len(request.Messages)),
		Topics:   []model.EpisodeTopic{{Key: "synthetic-offline", Label: "synthetic offline"}},
	}
	for _, message := range request.Messages {
		if message.ID == 0 {
			continue
		}
		entry := model.SessionCardEntry{
			Text:       offlineCardText(message.Content),
			MessageIDs: []uint{message.ID},
		}
		if message.Role == "user" {
			entry.Status = model.SessionCardStatusStated
			document.Arrangements = append(document.Arrangements, entry)
			continue
		}
		entry.Status = model.SessionCardStatusAssistantProposed
		document.AssistantProducts = append(document.AssistantProducts, entry)
	}
	// The conversation was compressed before the card was written, so the raw
	// messages are gone and what the fake was shown is the summary. A real
	// model writes the card from what it was shown, and so does the fake: the
	// card cites the segments it read, and the words it puts there are the
	// conversation's own words.
	//
	// Without this the card would carry nothing of the conversation, and a card
	// that says nothing about what was said is a card the retrieval cannot
	// find: the arm would measure a card path that found no card.
	if len(document.Arrangements) == 0 && len(document.AssistantProducts) == 0 {
		for _, segment := range request.Segments {
			for position, point := range segment.Points {
				text := strings.TrimSpace(point)
				if text == "" {
					continue
				}
				// The card payload carries the points as plain texts, so the
				// only attribution left is the order: the fake's summary
				// writes the learner's point first and the assistant's
				// second. Writing the assistant's product into the
				// learner's "arrangements" would say the learner had settled
				// something the assistant only proposed, and a card that
				// misattributes is worse than a card that says less.
				entry := model.SessionCardEntry{Text: offlineCardText(text), SegmentSeqs: []int{segment.Seq}}
				switch position {
				case 0:
					entry.Status = model.SessionCardStatusStated
					document.Arrangements = append(document.Arrangements, entry)
				case 1:
					entry.Status = model.SessionCardStatusAssistantProposed
					document.AssistantProducts = append(document.AssistantProducts, entry)
				default:
					// The fake writes one point per role, so a third point is
					// something the fake did not write and is left out.
				}
			}
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode the offline session card: %w", err)
	}
	return string(encoded), nil
}

// offlineCardText is the line a card entry carries.
//
// It is short on purpose: the entry is a pointer back to the message, and
// the whole wording is one read-back away.
func offlineCardText(content string) string {
	text := strings.TrimSpace(content)
	runes := []rune(text)
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	if text == "" {
		return "synthetic offline line"
	}
	return text
}

// segmentWriterPromptPrefix is how the layered summary's writer prompt starts.
//
// The prompt is the product's own, and the fake model answers the call it
// recognizes. A call the fake model does not recognize is refused, and a
// refused segment call is a batch that produced nothing: the window then
// carries no segment at all, and a measurement of the mode that records the
// assistant's products would find nothing to compare.
const segmentWriterPromptPrefix = "You maintain two kinds of compressed state"

// The markers the fake model's segment carries.
//
// The topic is what a test looks for in the window the answer was built from:
// a segment that was written but did not reach the model leaves no trace
// there, and a run that only checked that the segment was written would call
// that a success.
const (
	syntheticSegmentTopic       = "synthetic offline segment"
	syntheticSegmentLearnerText = "synthetic offline learner point"
	syntheticSegmentProductText = "synthetic offline assistant product"
)

// segmentPayloadMessage is one message of the batch, as the writer prompt
// names it.
var segmentPayloadMessage = regexp.MustCompile(`\[message_id=(\d+) role=(\w+)\]`)

// segmentDocument is what the fake model answers when the product asks for a
// summary segment.
//
// The document has to be a real one. The program checks every point against
// the messages of the batch, and a point that names a message the batch never
// contained is dropped on its own; a segment with no points is not a segment.
// So the fake model reads the ids out of the payload it was given and cites
// them, and the segment it produces is stored and read back like any other.
//
// The mode decides what the segment carries. The mode that does not record the
// assistant's products gets the learner's points only. The mode that does gets
// one of the assistant's products as well, and that product is what the two
// arms are compared on.
func (m *offlineChat) segmentDocument(input []*schema.Message) string {
	payload := ""
	if len(input) > 1 && input[1] != nil {
		payload = input[1].Content
	}
	learner, product, learnerText, productText := segmentPayloadMessages(payload)
	points := []map[string]any{}
	if learner > 0 {
		points = append(points, map[string]any{
			"text": summaryPointText(learnerText, syntheticSegmentLearnerText), "evidence_message_ids": []uint{learner}, "source": "user",
		})
	}
	if m.assistantContent && product > 0 {
		points = append(points, map[string]any{
			"text": summaryPointText(productText, syntheticSegmentProductText), "evidence_message_ids": []uint{product}, "source": "assistant",
		})
	}
	document := map[string]any{
		"schema_version": 1,
		"segment": map[string]any{
			"topic": syntheticSegmentTopic, "points": points, "names": []string{}, "numbers": []string{},
		},
	}
	// The writer asks for the whole board or for the changes only, and the
	// two documents have different shapes. Which one it asked for is in the
	// prompt it answered under.
	delta := len(input) > 0 && input[0] != nil && strings.Contains(input[0].Content, "whiteboard_changes")
	if delta {
		document["whiteboard_changes"] = map[string]any{
			"goal": nil, "constraints": []any{}, "plan": []any{}, "open_items": []any{},
		}
	} else {
		document["whiteboard"] = map[string]any{
			"schema_version": 1, "goal": nil, "constraints": []any{}, "plan": []any{}, "open_items": []any{},
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// segmentPayloadMessages reads the batch's first learner message and of its
// first assistant message out of the payload the writer was given.
//
// The payload names every message with its id and its role and shows its
// text, so the fake model cites messages the batch really contained and
// writes down the subject the batch really talked about.
//
// The text matters as much as the ids. A summary that kept no words of its own
// would leave every card written from it empty of the conversation, and a card
// nobody can find is a card the arm cannot measure: the retrieval would look
// for what the learner said and find nothing, and the attempt would say the
// card path found nothing while the card was sitting in the index.
func segmentPayloadMessages(payload string) (learner, product uint, learnerText, productText string) {
	matches := segmentPayloadMessage.FindAllStringSubmatchIndex(payload, -1)
	for index, match := range matches {
		id, err := strconv.ParseUint(payload[match[2]:match[3]], 10, 64)
		if err != nil {
			continue
		}
		role := payload[match[4]:match[5]]
		// The text is what follows the marker, up to the next marker. A
		// message may span several lines, so the line is not the end of it.
		end := len(payload)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		text := strings.TrimSpace(payload[match[1]:end])
		switch role {
		case "user":
			if learner == 0 {
				learner, learnerText = uint(id), text
			}
		case "assistant":
			if product == 0 {
				product, productText = uint(id), text
			}
		}
	}
	return learner, product, learnerText, productText
}

// summaryPointText is what one summary point says.
//
// The message's own words are kept, because a summary is a compression of what
// was said and not a replacement for it: the words the learner used are the
// words a later question will use. The fixed text is the fallback for a
// payload that carries no readable text.
func summaryPointText(text, fallback string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return fallback
	}
	return offlineCardText(trimmed)
}
