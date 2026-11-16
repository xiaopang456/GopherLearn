package conversation

// Real-mode evaluation. The measured product keeps its own behaviour: no
// request body is rewritten, no output limit is added, and no product
// dependency is replaced. The only difference from a local run is the address
// of the model endpoints, which point at the operator's counting proxy. That
// proxy injects the provider credentials, enforces the per-lane call caps,
// records the provider usage, and writes the stop file when a chat call is
// refused or the cost watchdog trips. The evaluation therefore never sees a
// provider key and never changes what the product asks for.
//
// The knowledge index is shared by every run of one evaluation and is read
// only during a run: the product's index worker never sees a pending
// document, so no chunk is written while an answer is being produced.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/aihelper"
	"GopherAI/common/memoryrecall"
	"GopherAI/config"
	"GopherAI/entry"
	"GopherAI/model"
	agent "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"gorm.io/gorm"
)

// RealEnvironment describes where a paid evaluation run finds its model
// endpoints, its shared knowledge index, and its evaluation identity.
type RealEnvironment struct {
	// ProxyBaseURL is the operator's counting proxy, for example
	// http://127.0.0.1:18140. The proxy holds the credentials.
	ProxyBaseURL string
	// RedisAddress is the shared evaluation index, host:port.
	RedisAddress string
	// RedisPassword opens the shared evaluation index. It never appears in
	// an output file, a manifest, or an error message.
	RedisPassword string
	// StorageDir holds the seeded documents of the shared index.
	StorageDir string
	// ChatModel, EmbeddingModel and RerankModel are the provider model
	// names the product is measured with.
	ChatModel      string
	EmbeddingModel string
	RerankModel    string
	// UserID, UserName and SessionID identify the fictional evaluation
	// learner and the conversation under test.
	UserID    int64
	UserName  string
	SessionID string
	// SummarySegmentMode is which arm the run measures: "off" is the old
	// rolling summary, "shadow" writes the segments and the board without
	// the window reading them, and "on" reads the new state back.
	//
	// The field is here rather than in a flag of its own because the mode is
	// a property of the configuration the run boots with: the product reads
	// it once, at startup, and the run has to be able to say which arm it
	// was.
	SummarySegmentMode string
	// SessionCardMode selects the cross-session memory path: "disabled" is
	// the old Episode, "enabled" is the session card.
	//
	// The two paths are the two arms of the comparison, and they are kept
	// apart by two levers that both have to be used: the card index has its
	// own namespace per arm, and the evaluation identity carries the arm.
	// The shared evaluation index is the reason the namespace matters: two
	// arms that wrote into one index would read each other's rows.
	SessionCardMode string
	// SessionCardIndexName and SessionCardIndexKeyPrefix are that namespace.
	SessionCardIndexName      string
	SessionCardIndexKeyPrefix string
	// MemoryReadMode is the product's read path. It is written into the
	// product's configuration rather than left to the product's default, and
	// it is exported with the attempt: an arm that does not say which read
	// path it ran has not said what it measured.
	MemoryReadMode string
	// EpisodeRecallMode is the product's Episode recall switch. The old arm
	// needs "inject_enabled" or nothing is injected; the card arm needs
	// "disabled", because the card path replaces the Episode path.
	EpisodeRecallMode string
	// QuestionSession is the conversation the question is asked in.
	//
	// It is named by the run, because two questions of the same preparation
	// must not be asked in the same conversation: the second one would find
	// the first one's answer in the history.
	QuestionSession string
}

// The read path and the recall switch a run may ask for.
//
// The list is the product's own: a run that asks for a mode the product does
// not know must fail before it spends anything.
var (
	// MemoryReadModes are the product's read paths.
	MemoryReadModes = []string{
		"legacy_v2", "episode_v3", "episode_v3_lexical", "episode_v3_semantic",
		"model_planned_v1", "agent_memory_tool_v1", "agent_memory_tool_v2",
		"agent_memory_tool_v3", "agent_memory_tool_v4", "agent_memory_user_confirmed_v1",
		"episode_assertion_governed_v1_1", "agent_memory_explicit_actions_v1",
		"agent_memory_explicit_sources_v1", "agent_memory_source_priority_v1",
		"agent_memory_source_priority_v2", "agent_memory_source_priority_v3",
	}
	// EpisodeRecallModes are the product's Episode recall switches.
	EpisodeRecallModes = []string{"disabled", "shadow_only", "inject_enabled"}
)

// MemoryReadModeKnown reports whether the read path is one the product
// understands.
func MemoryReadModeKnown(mode string) bool {
	for _, candidate := range MemoryReadModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// EpisodeRecallModeKnown reports whether the recall switch is one the product
// understands.
func EpisodeRecallModeKnown(mode string) bool {
	for _, candidate := range EpisodeRecallModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// SummarySegmentModes are the arms a paid run may measure.
var SummarySegmentModes = []string{"off", "shadow", "on"}

// SessionCardModes are the cross-session memory paths a paid run may measure.
var SessionCardModes = []string{CardArmModeOff, CardArmModeOn}

// SessionCardModeKnown reports whether the mode is one the product
// understands.
func SessionCardModeKnown(mode string) bool {
	for _, candidate := range SessionCardModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// CardArmForEnvironment reads the card arm out of the paid run's wiring.
//
// The arm is a property of the configuration the run boots with, exactly like
// the summary segment mode: the product reads it once, at startup, and a run
// whose label does not match the mode it booted with cannot be compared with
// anything.
func CardArmForEnvironment(env RealEnvironment) *CardArm {
	arm := &CardArm{
		Mode:              CardArmModeOff,
		MemoryReadMode:    strings.TrimSpace(env.MemoryReadMode),
		EpisodeRecallMode: strings.TrimSpace(env.EpisodeRecallMode),
	}
	if env.SessionCardMode == CardArmModeOn {
		arm.Mode = CardArmModeOn
		arm.IndexName = env.SessionCardIndexName
		arm.KeyPrefix = env.SessionCardIndexKeyPrefix
	}
	return arm
}

// SummarySegmentModeKnown reports whether the mode is one the product
// understands.
func SummarySegmentModeKnown(mode string) bool {
	for _, candidate := range SummarySegmentModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// Validate reports an incomplete paid-run wiring before any resource opens.
func (e RealEnvironment) Validate() error {
	if err := loopbackAddress(e.ProxyBaseURL); err != nil {
		return fmt.Errorf("counting proxy: %w", err)
	}
	if err := loopbackAddress("http://" + e.RedisAddress); err != nil {
		return fmt.Errorf("evaluation index: %w", err)
	}
	if strings.TrimSpace(e.StorageDir) == "" {
		return fmt.Errorf("evaluation storage directory is required")
	}
	if strings.TrimSpace(e.ChatModel) == "" || strings.TrimSpace(e.EmbeddingModel) == "" || strings.TrimSpace(e.RerankModel) == "" {
		return fmt.Errorf("model names are required")
	}
	if e.UserID <= 0 || strings.TrimSpace(e.UserName) == "" || strings.TrimSpace(e.SessionID) == "" {
		return fmt.Errorf("evaluation identity is required")
	}
	if !SummarySegmentModeKnown(e.SummarySegmentMode) {
		return fmt.Errorf("the summary segment mode must be off, shadow or on")
	}
	if !SessionCardModeKnown(e.SessionCardMode) {
		return fmt.Errorf("the session card mode must be disabled or enabled")
	}
	if mode := strings.TrimSpace(e.MemoryReadMode); mode != "" && !MemoryReadModeKnown(mode) {
		return fmt.Errorf("the memory read mode %q is not one the product knows", mode)
	}
	if mode := strings.TrimSpace(e.EpisodeRecallMode); mode != "" && !EpisodeRecallModeKnown(mode) {
		return fmt.Errorf("the episode recall mode %q is not one the product knows", mode)
	}
	if e.SessionCardMode == CardArmModeOn {
		if strings.TrimSpace(e.SessionCardIndexName) == "" || strings.TrimSpace(e.SessionCardIndexKeyPrefix) == "" {
			return fmt.Errorf("the session card arm needs its own index namespace")
		}
		if !strings.HasPrefix(e.SessionCardIndexName, "gopherai_session_card_") {
			return fmt.Errorf("the session card index name is outside the declared namespace")
		}
	}
	return nil
}

// loopbackAddress keeps a paid run away from any host the operator uses.
func loopbackAddress(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "http" {
		return fmt.Errorf("address must be an http URL")
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host = parsed.Host
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return fmt.Errorf("address must be a loopback address")
	}
	return nil
}

// proxyLane binds one product model path to the counting proxy lane that
// carries it and to the upstream path that lane forwards to. The upstream
// paths are the provider paths the product itself uses; nothing is added to
// the request body.
type proxyLane struct {
	lane string
	path string
}

var proxyLanes = map[string]proxyLane{
	"/v1/chat/completions": {lane: "chat", path: "/v1/chat/completions"},
	"/v1/embeddings":       {lane: "embed", path: "/embeddings"},
	"/v1/rerank":           {lane: "rerank", path: "/api/v1/services/rerank/text-rerank/text-rerank"},
}

// proxyForwarder relays a model request to the counting proxy unchanged.
func proxyForwarder(base string) http.Handler {
	client := &http.Client{Timeout: 10 * time.Minute}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spec, ok := proxyLanes[r.URL.Path]
		if !ok {
			http.Error(w, "unknown model path", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20+1))
		r.Body.Close()
		if err != nil || len(body) > 8<<20 {
			http.Error(w, "model request body is invalid", http.StatusBadRequest)
			return
		}
		target := strings.TrimSuffix(base, "/") + "/lane/" + spec.lane + spec.path
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "model request could not be built", http.StatusBadGateway)
			return
		}
		request.Header.Set("Content-Type", "application/json")
		if accept := r.Header.Get("Accept"); accept != "" {
			request.Header.Set("Accept", accept)
		}
		response, err := client.Do(request)
		if err != nil {
			http.Error(w, "the counting proxy is unreachable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			switch strings.ToLower(key) {
			case "content-length", "content-encoding", "transfer-encoding", "connection":
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 32<<10)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				if _, writeErr := w.Write(buffer[:read]); writeErr != nil {
					return
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if readErr != nil {
				return
			}
		}
	})
}

// realWire serves the product's model endpoints on loopback. It records every
// request at the HTTP boundary and forwards it to the counting proxy.
func realWire(ledger *ModelLedger, roles RolePrompts, proxyBase string) *httptest.Server {
	return httptest.NewServer(modelHTTPHandler(ledger, roles, proxyForwarder(proxyBase)))
}

// realSettings points the standard entry at the counting proxy and at the
// shared evaluation index. The product's own defaults are kept.
func realSettings(repository, storage string, env RealEnvironment) (*entry.Settings, error) {
	settings, err := offlineSettings(repository, storage)
	if err != nil {
		return nil, err
	}
	settings.Entry.ChatModel = env.ChatModel
	settings.Entry.ChatKey = "placeholder-proxy-injected"
	settings.Config.RagModelConfig.RagEmbeddingAPIKey = "placeholder-proxy-injected"
	settings.Config.RagModelConfig.RagEmbeddingModel = env.EmbeddingModel
	settings.Config.RagModelConfig.RagRerankModel = env.RerankModel
	settings.Config.RagModelConfig.RagChatModelName = env.ChatModel
	settings.Config.RagModelConfig.RagChatThinkingDialect = config.ChatThinkingDialectDeepSeek
	settings.Config.AgentThinkingEnabled = true
	settings.Config.AgentConfig.EpisodeQuietWindowSeconds = 86400
	settings.Config.RedisPassword = env.RedisPassword
	// The arm is read by the product at startup, so it is set before the
	// configuration is validated: a run that asked for a mode the product
	// does not know has to fail before it spends anything.
	settings.Config.AgentConfig.SummarySegmentMode = env.SummarySegmentMode
	// The card path is the other arm of the comparison, and it is read at
	// startup for the same reason.
	settings.Config.AgentConfig.SessionCardMode = env.SessionCardMode
	if env.SessionCardMode == CardArmModeOn {
		settings.Config.AgentConfig.SessionCardIndexName = env.SessionCardIndexName
		settings.Config.AgentConfig.SessionCardIndexKeyPrefix = env.SessionCardIndexKeyPrefix
	}
	// The read path and the recall switch are written into the product's own
	// configuration rather than left to its defaults, for the same reason the
	// arm is: a run that does not say which path it ran has not said what it
	// measured. The old arm needs the Episode recall switch on, or nothing is
	// injected into the answer at all.
	if mode := strings.TrimSpace(env.MemoryReadMode); mode != "" {
		settings.Config.AgentConfig.MemoryReadMode = mode
	}
	if mode := strings.TrimSpace(env.EpisodeRecallMode); mode != "" {
		settings.Config.AgentConfig.EpisodeRecallMode = mode
	}
	return settings, settings.Validate()
}

// PrepareRealProductReport warms the attempt's conversations with the
// product's rolling summarizer. The summary model is the provider model,
// reached through the counting proxy, so the summaries a run restores are the
// summaries the product would have produced.
//
// The attempt may carry more than one conversation: a comparison of two
// cross-session memory paths needs the retrieval to have several
// conversations to choose between, and an attempt whose database holds only
// the conversation the question is about measures nothing about retrieval.
func PrepareRealProductReport(ctx context.Context, repository string, histories []History, env RealEnvironment, input AttemptInput) (report WarmupReport, resultErr error) {
	if err := env.Validate(); err != nil {
		return report, err
	}
	if len(histories) == 0 {
		return report, fmt.Errorf("turns are required")
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	storage, err := os.MkdirTemp("", "gl-real-storage-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(storage)
	settings, err := realSettings(repository, storage, env)
	if err != nil {
		return report, err
	}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return report, err
	}
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			resultErr = errors.Join(resultErr, fmt.Errorf("warmup model telemetry incomplete"))
		}
	}()
	wire := realWire(ledger, roles, env.ProxyBaseURL)
	defer wire.Close()
	dialect, err := config.NormalizeChatThinkingDialect(settings.Config.RagModelConfig.RagChatThinkingDialect)
	if err != nil {
		return report, err
	}
	chat, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:     wire.URL + "/v1",
		Model:       env.ChatModel,
		APIKey:      "placeholder-proxy-injected",
		ExtraFields: aihelper.ThinkingExtraFields(dialect, false),
	})
	if err != nil {
		return report, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return report, err
	}
	password, err := randomHex(24)
	if err != nil {
		return report, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return report, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: env.UserID, Username: env.UserName, Password: hash}, nil, env.StorageDir); err != nil {
		return report, err
	}
	// The seeded profile memories are written before the conversation is
	// prepared, so that the product sees them while the conversation is
	// built. They are written again in the database the answer is asked in,
	// because that database is a fresh one.
	report.SeededMemories, _, err = seedAttemptMemories(ctx, s, env.UserID, input.Memories)
	if err != nil {
		return report, err
	}
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		for _, history := range histories {
			session := model.Session{ID: history.SessionID, UserID: env.UserID, UserName: env.UserName,
				Mode: model.SessionModeAgent, Title: "Evaluation conversation"}
			if err := tx.Create(&session).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return report, err
	}
	policy := ProductSummaryConfig(settings.Config)
	generator, err := agent.NewModelSummaryGenerator(chat, policy.MaxInputTokens, policy.MaxOutputTokens, policy.Budget)
	if err != nil {
		return report, err
	}
	prepared, err := s.PrepareMany(ctx, env.UserID, env.UserName, histories, generator, policy)
	SummarizeTurns(prepared, &report)
	report.FailureCooldownSeconds = int(policy.FailureCooldown / time.Second)
	report.Turns = prepared
	report.Calls = ledger.Calls()
	if err != nil {
		return report, err
	}
	sessionIDs := make([]string, 0, len(histories))
	for _, history := range histories {
		sessionIDs = append(sessionIDs, history.SessionID)
	}
	// The preparation publishes the projection and waits for the memories to
	// be indexed, so that the archive carries a memory for every conversation
	// and the queries can reuse it.
	//
	// This is where the product has to run: the worker that writes the memory
	// is the product's own worker, and the index it writes into is the shared
	// one the queries will search.
	arm := CardArmForEnvironment(env)
	// The preparation is the FIRST use of this learner's partition, so the
	// isolation gate applies here: the index must be empty, or the projection
	// would land on top of somebody else's rows.
	if _, err = requireEmptyUserIndexes(ctx, env); err != nil {
		return report, err
	}
	projection, projectionCalls, err := projectArmMemories(ctx, repository, env.StorageDir, env, s, sessionIDs, arm)
	if err != nil {
		return report, err
	}
	report.Projection = projection
	// The projection's calls belong to this preparation: they are the
	// generation of the memory, and a preparation whose ledger does not carry
	// them is a preparation whose cost nobody can see.
	report.Calls = append(report.Calls, projectionCalls...)
	report.Snapshot, err = s.CaptureMany(ctx, env.UserID, sessionIDs)
	if err != nil {
		return report, err
	}
	report.Snapshot.IndexEntries = projection.IndexEntries
	report.UsageByRole, err = SummarizeCalls(report.Calls)
	return report, errors.Join(err, nil)
}

// projectArmMemories publishes the projection of every conversation of the
// attempt and waits for the memory to be indexed.
//
// It boots the product, because the worker that writes the memory and the
// indexer that indexes it are the product's own. The product is stopped
// again before the archive is written: the archive is what the queries
// reuse, and the queries must not depend on a process that is still running.
func projectArmMemories(ctx context.Context, repository, storage string, env RealEnvironment,
	s *Sandbox, sessionIDs []string, arm *CardArm) (*ArmProjection, []ModelCall, error) {
	out := &ArmProjection{Mode: arm.Mode, MemoryKind: arm.MemoryKind(), Sessions: sessionIDs}
	if err := arm.Validate(); err != nil {
		return out, nil, err
	}
	password, err := randomHex(24)
	if err != nil {
		return out, nil, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return out, nil, err
	}
	// The product logs in as the evaluation user, and the user has to be
	// there with the hash the harness knows.
	if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
		return tx.Model(&model.User{}).Where("id = ?", env.UserID).
			Update("password", hash).Error
	}); err != nil {
		return out, nil, err
	}
	dir, err := os.MkdirTemp("", "gl-prepare-product-")
	if err != nil {
		return out, nil, err
	}
	defer os.RemoveAll(dir)
	ledger := &ModelLedger{}
	defer func() {
		if !ledger.Complete() {
			err = errors.Join(err, fmt.Errorf("preparation model telemetry incomplete"))
		}
	}()
	runtime, err := bootProduct(ctx, repository, storage, env, s, dir, ledger, password)
	if err != nil {
		return out, nil, err
	}
	defer runtime.stop()
	for _, sessionID := range sessionIDs {
		published, due, waited, waitErr := s.PublishAndAwaitMemory(ctx, env.UserID, sessionID, arm)
		if published {
			out.Published++
		}
		if due {
			out.Due++
		}
		out.WaitMilliseconds += waited
		if waitErr != nil {
			out.Error = waitErr.Error()
			return out, ledger.Calls(), waitErr
		}
	}
	// The memory row is written by the projector and the index entry by the
	// indexer, and they are two steps. The row can say "indexed" before the
	// index entry exists, so the archive records what the index really holds:
	// an archive whose index does not agree with it would make every query
	// that reuses it fail its own gate, and the reason has to be visible.
	state, err := readMemoryIndexState(ctx, redisKeyLister{client: redisClientFor(env)}, env.UserID, env.cardIndexPrefix())
	if err != nil {
		out.Error = err.Error()
		return out, ledger.Calls(), err
	}
	out.IndexEntries = state.EpisodeEntries + state.MemoryEntries + state.CardEntries
	out.IndexReadable = true
	if out.IndexEntries < len(sessionIDs) {
		// The archive is written anyway, and it says what the index held.
		//
		// The gate of a query compares the index with what the preparation
		// recorded, not with the number of memories: the two are different
		// facts, and a query that refuses to run because the preparation
		// under-indexed would hide the very thing it is meant to show.
		out.Error = fmt.Sprintf("the shared index holds %d entr(ies) and the preparation produced %d", out.IndexEntries, len(sessionIDs))
	}
	return out, ledger.Calls(), nil
}

// memoryRowsOf reads the memory rows the archive carries into the evidence
// the attempt keeps.
func memoryRowsOf(snapshot Snapshot) []*CardRow {
	out := make([]*CardRow, 0, len(snapshot.Memories))
	for _, memory := range snapshot.Memories {
		out = append(out, MemoryRowOf(&memory))
	}
	return out
}

// ArmProjection is what the preparation projected.
type ArmProjection struct {
	// Mode is the arm: "disabled" is the old Episode, "enabled" the card.
	Mode string `json:"mode"`
	// MemoryKind is the kind of document the arm projected.
	MemoryKind string `json:"memory_kind"`
	// Sessions are the conversations that were projected.
	Sessions []string `json:"sessions,omitempty"`
	// Published counts the conversations whose projection was published, and
	// Due the ones whose job was moved into the present.
	Published int `json:"published"`
	Due       int `json:"due"`
	// WaitMilliseconds is how long the preparation waited for the memories.
	WaitMilliseconds int64 `json:"wait_ms"`
	// IndexEntries is how many entries the shared index held for this learner
	// when the archive was written. It is read from the index, not counted
	// from the archive, so a disagreement between the two is visible.
	IndexEntries int `json:"index_entries"`
	// IndexReadable says the index could be read at all.
	IndexReadable bool `json:"index_readable"`
	// Error says why the preparation could not project, when it could not.
	Error string `json:"error,omitempty"`
}

// RunRealProduct restores one prepared conversation into a fresh evaluation
// database and asks the unchanged standard product entry.
func RunRealProduct(ctx context.Context, repository string, snapshot Snapshot, env RealEnvironment, seed *KnowledgeSeed, input AttemptInput) (ProductResult, error) {
	return runRealEntry(ctx, repository, snapshot, env, seed, input)
}

func runRealEntry(ctx context.Context, repository string, snapshot Snapshot, env RealEnvironment, seed *KnowledgeSeed, input AttemptInput) (result ProductResult, resultErr error) {
	result.Scope = ProductScope
	result.Environment = "real"
	result.Entry = "gopherlearn-integrated:hybrid.Run"
	result.QuietWindowSeconds = 86400
	defer func() {
		if len(result.Response) > 0 && !json.Valid(result.Response) {
			result.ResponseText = string(result.Response)
			result.Response = nil
		}
		if resultErr != nil {
			result.BackgroundAudit.Valid = false
			result.BackgroundAudit.Reason = resultErr.Error()
			result.Error = resultErr.Error()
		}
	}()
	if err := env.Validate(); err != nil {
		return result, err
	}
	// The query reuses the preparation, so the index is NOT empty: it holds
	// the memories the preparation projected. What has to hold is that the index
	// agrees with the archive.
	before, err := checkSnapshotIndexes(ctx, env, snapshot)
	result.MemoryIndexEvidence.BeforeAnswer = before
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	questions := input.Questions
	if len(questions) == 0 {
		return result, fmt.Errorf("question required")
	}
	for _, question := range questions {
		if strings.TrimSpace(question.Text) == "" {
			return result, fmt.Errorf("question required")
		}
	}
	if err := snapshot.Validate(); err != nil {
		return result, err
	}
	// The archive has to belong to this attempt: the same learner, and the
	// attempt's own conversation among the ones it carries.
	primary, _ := snapshot.Primary()
	if primary.Session.UserID != env.UserID || primary.Session.UserName != env.UserName {
		return result, fmt.Errorf("the snapshot belongs to another evaluation identity")
	}
	if !snapshotHoldsSession(snapshot, env.SessionID) {
		return result, fmt.Errorf("the snapshot does not carry the attempt's conversation")
	}
	dir, err := os.MkdirTemp("", "gl-real-entry-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	settings, err := realSettings(repository, env.StorageDir, env)
	if err != nil {
		return result, err
	}
	result.Provenance, err = collectProvenance(ctx, repository, settings, snapshot)
	if err != nil {
		return result, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return result, err
	}
	password, err := randomHex(24)
	if err != nil {
		return result, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return result, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: env.UserID, Username: env.UserName, Password: hash}, seed.documents(), env.StorageDir); err != nil {
		return result, err
	}
	// The seeded profile memories are written into the database the answer is
	// asked in, because that database is a fresh one and the restored
	// conversation does not carry the learner's profile with it.
	result.SeededMemories, result.SeededMemoryRows, err = seedAttemptMemories(ctx, s, env.UserID, input.Memories)
	if err != nil {
		return result, err
	}
	restoreStart := time.Now()
	if err = s.Restore(ctx, snapshot); err != nil {
		return result, err
	}
	result.RestoreMilliseconds = time.Since(restoreStart).Milliseconds()
	journal, err := s.StartJobJournal(ctx)
	if err != nil {
		return result, err
	}
	ledger := &ModelLedger{}
	dir2 := dir
	// The observed interval starts BEFORE the product is started. The product
	// does work when it starts - the startup index audit reconciles the
	// derived indexes with the database - and a call the harness cannot
	// account for is a call the harness cannot claim to have measured.
	result.Audit.Started = time.Now().UTC()
	runtime, err := bootProduct(ctx, repository, env.StorageDir, env, s, dir2, ledger, password)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, runtime.stop()) }()
	client := runtime.client
	base := runtime.base
	token := runtime.token
	settings = runtime.settings
	result.Audit.Before, result.BeforeCounts, err = s.ObserveJobs(ctx)
	if err != nil {
		return result, err
	}
	// The query phase. The memories were projected by the preparation and
	// travel with the archive, so the query does NOT project them again: it
	// restores them and asks.
	//
	// What the query still owns is the conversation it asks in. It opens one
	// of its own, and it is not one of the archive's conversations: a question
	// asked in the conversation the memory came from would find the raw
	// messages anyway, and the test conversations must not become candidates
	// for the retrieval.
	arm := CardArmForEnvironment(env)
	armEvidence := &CardArmEvidence{
		Mode: arm.Mode, MemoryKind: arm.MemoryKind(),
		MemoryReadMode:    strings.TrimSpace(arm.MemoryReadMode),
		EpisodeRecallMode: strings.TrimSpace(arm.EpisodeRecallMode),
		SourceSessions:    snapshot.SessionIDs(),
		Memories:          memoryRowsOf(snapshot),
		Ready:             len(snapshot.Memories) > 0,
	}
	result.SessionCard = armEvidence
	questionSession := strings.TrimSpace(env.QuestionSession)
	if questionSession == "" {
		questionSession = QuestionSessionID(env.SessionID)
	}
	if err = s.CreateQuestionSession(ctx, env.UserID, env.UserName, questionSession); err != nil {
		return result, err
	}
	armEvidence.QuestionSessionID = questionSession
	// The preparation phase ends here: the source data and the memory version
	// are fixed, and nothing may be generated from here to the answer.
	result.Audit.Prepared = time.Now().UTC()
	if err = journal.PrepareBarrier(ctx); err != nil {
		return result, err
	}
	asked, requestErr := askProductQuestions(ctx, client, base, token, questionSession, questions, "evaluation-final-question")
	result.Answer = asked[0].Answer
	result.Response = asked[0].Response
	result.Questions = asked
	if err := s.readAgentSteps(ctx, env.UserID, asked); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	result.Audit.Answered = time.Now().UTC()
	result.QuestionMilliseconds = asked[len(asked)-1].FinishedAt.Sub(asked[0].StartedAt).Milliseconds()
	collect, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	barrierErr := journal.AnswerBarrier(collect)
	result.Audit.After, result.AfterCounts, err = s.ObserveJobs(collect)
	stopErr := runtime.stop()
	result.ShutdownComplete = stopErr == nil
	var stoppedErr error
	_, result.StoppedCounts, stoppedErr = s.ObserveJobs(collect)
	result.Events, result.Audit.ExecutedInQueryPhase, resultErr = journal.Finish(collect)
	// The product has stopped and the background window has closed, so this is
	// the state the next repeat of this scenario would inherit. It reads on its
	// own deadline: a journal window that ran out must not erase the evidence,
	// and a failed read is recorded and reported, never left blank.
	evidence, evidenceDone := context.WithTimeout(context.Background(), 20*time.Second)
	after, indexErr := observeMemoryIndexes(evidence, env)
	evidenceDone()
	if indexErr != nil {
		after.Error = indexErr.Error()
	}
	result.MemoryIndexEvidence.AfterAnswer = after
	result.Calls = BindRuns(AttributeCallPhases(ledger.Calls(), result.Audit.Prepared, result.Audit.Answered), primary.Session.ID, asked)
	// The request body is the record of what the model was shown, and the
	// ledger keeps the cards it carried with each call. So the card evidence
	// is read out of the recorded calls rather than out of the arm's own
	// bookkeeping.
	if err := recordCardArmEvidence(result.SessionCard, s, env.UserID, questionSession, ObserveCardCalls(result.Calls), asked); err != nil {
		resultErr = errors.Join(resultErr, err)
	}
	// The tool table is the same record: the two arms are offered different
	// tools, and an attempt that does not say which table it was given has
	// not said what it measured.
	if result.SessionCard != nil {
		result.SessionCard.ToolTable = AnswerToolTable(result.Calls)
	}
	var usageErr error
	result.UsageByRole, usageErr = SummarizeCalls(result.Calls)
	for _, c := range result.Calls {
		result.Audit.Calls = append(result.Audit.Calls, BackgroundCall{Role: c.Role, Started: c.Started})
	}
	result.Audit.Complete = resultErr == nil && barrierErr == nil && stopErr == nil && stoppedErr == nil && err == nil && indexErr == nil && usageErr == nil && ledger.Complete()
	resultErr = errors.Join(resultErr, requestErr, barrierErr, err, indexErr, stopErr, stoppedErr, usageErr, result.Audit.ValidateBackground())
	result.BackgroundAudit.Valid = resultErr == nil
	return result, resultErr
}

func splitRedisAddress(address string) (string, int, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", 0, err
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, fmt.Errorf("evaluation index port is invalid")
	}
	return host, number, nil
}
