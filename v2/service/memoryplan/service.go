package memoryplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"GopherAI/common/observability"
	"GopherAI/model"

	modelcomponent "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// Config is everything the mode needs. There are no defaults for the identity
// fields: a model name or an endpoint that came from a Default*Config() is a
// production default no matter what the comment above it says, which is the
// finding BUG-AGENT-140 recorded.
type Config struct {
	Model             string
	BaseURL           string
	PlannerTimeout    time.Duration
	VerifierTimeout   time.Duration
	MaxAttempts       int
	MaxConcurrency    int
	MinConfidence     float64
	CandidateDepth    int
	MaxContextEpisode int
}

// DefaultConfig carries only the bounds that are not an identity. Model, base
// URL and key are absent on purpose.
//
// The stage timeouts are 30s, and that number is deliberately far larger than
// any latency this path should have.
//
// It got there in two steps. The design fixed 2500ms against a 612ms
// measurement; the endpoint now answers a planner call in about 2.3s, so the
// first development run timed out 31 planner and 22 verifier stages and
// measured nothing. Raising it to 8s fixed the planner but not the verifier: the
// verifier writes one verdict per candidate, so its output length - and its
// generation time - scale with the pool, and every case with a pool above about
// five candidates still timed out. Pool size where it finished: median 2. Pool
// size where it timed out: median 12.
//
// This is a run timeout, not an acceptance standard. The effect gates still
// require the model stage at P95 within 4s and no single case beyond 6s,
// measured on the run. A generous timeout cannot make a slow method pass; it
// only lets the gate see a real number instead of a truncation, so that being
// too slow ends in "the method failed", which is a conclusion, rather than in
// "the run is void", which is not.
func DefaultConfig() Config {
	return Config{
		PlannerTimeout: 30000 * time.Millisecond, VerifierTimeout: 30000 * time.Millisecond,
		MaxAttempts: 2, MaxConcurrency: 8, MinConfidence: 0.6, CandidateDepth: 20,
		MaxContextEpisode: 3,
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Model) == "" {
		return errors.New("memory planner model is required")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		return errors.New("memory planner base URL is required")
	}
	if c.PlannerTimeout < 200*time.Millisecond || c.PlannerTimeout > 30*time.Second {
		return errors.New("memory planner timeout must be between 200ms and 30s")
	}
	if c.VerifierTimeout < 200*time.Millisecond || c.VerifierTimeout > 30*time.Second {
		return errors.New("memory verifier timeout must be between 200ms and 30s")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 2 {
		return errors.New("memory planner attempts must be 1 or 2")
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 64 {
		return errors.New("memory planner concurrency must be between 1 and 64")
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		return errors.New("memory planner minimum confidence must be between 0 and 1")
	}
	if c.CandidateDepth < 1 || c.CandidateDepth > 100 {
		return errors.New("memory planner candidate depth must be between 1 and 100")
	}
	return nil
}

// Request is one turn.
type Request struct {
	UserID    int64
	SessionID string
	Question  string
	Now       time.Time
}

// InjectedMemory is one record that reached the context, with the relation that
// let it in.
type InjectedMemory struct {
	ID       string
	Kind     CandidateKind
	Relation Relation
	Line     string
}

// Result is what one turn produced.
//
// Plan, Pool and Verdicts are the intermediate state, exposed rather than
// discarded. Production ignores them; an evaluation needs them, and needs them
// from the same call that produced the injection. The alternative - running the
// planner once to measure it and again to use it - would make "both arms
// consumed the same plan" a claim rather than a fact, and STABILIZE-031 is a
// record of what unattributable arms cost.
type Result struct {
	Topic       []string
	TopicIDs    []string
	Episodes    []model.EpisodeMemory
	Injected    []InjectedMemory
	Diagnostics Diagnostics

	Plan     MemoryReadPlan
	Pool     []Candidate
	Verdicts []CandidateVerdict
}

// PlannerCaller is the small-model client. It is an interface so the
// deterministic tests can drive every protocol branch without a network.
type PlannerCaller interface {
	Generate(ctx context.Context, input []*schema.Message, opts ...modelcomponent.Option) (*schema.Message, error)
	WithTools(tools []*schema.ToolInfo) (modelcomponent.ToolCallingChatModel, error)
}

// Service is the whole mode.
type Service struct {
	chat      PlannerCaller
	catalogue *CatalogueBuilder
	builder   *CandidateBuilder
	config    Config

	semaphore chan struct{}

	// observer is called with each turn's diagnostics. It is how an evaluation
	// harness reads what happened without the service knowing an evaluation
	// exists.
	observerMu sync.RWMutex
	observer   func(Diagnostics)
}

func NewService(chat PlannerCaller, catalogue *CatalogueBuilder, builder *CandidateBuilder, config Config) (*Service, error) {
	if chat == nil {
		return nil, errors.New("model-planned recall needs a chat model")
	}
	if catalogue == nil || builder == nil {
		return nil, errors.New("model-planned recall needs a catalogue and a candidate builder")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Service{
		chat: chat, catalogue: catalogue, builder: builder, config: config,
		semaphore: make(chan struct{}, config.MaxConcurrency),
	}, nil
}

// UseObserver attaches a diagnostics sink.
func (s *Service) UseObserver(observer func(Diagnostics)) {
	s.observerMu.Lock()
	defer s.observerMu.Unlock()
	s.observer = observer
}

func (s *Service) publish(diagnostics Diagnostics) {
	s.observerMu.RLock()
	observer := s.observer
	s.observerMu.RUnlock()
	if observer != nil {
		observer(diagnostics)
	}
}

// Recall runs the whole two-stage read.
//
// Every failure below is closed rather than open: a stage that fails injects
// nothing from topic memory and nothing from episodes, and it never falls back
// to another mode or to "whatever was most recent". The turn still has the
// current conversation, the session summary, mastery, global memory and the
// knowledge base, so a memory outage costs personalisation rather than the turn.
func (s *Service) Recall(ctx context.Context, request Request) (Result, error) {
	started := time.Now()
	diagnostics := Diagnostics{Stage: StageCatalogue, Reasons: []string{}}
	if request.UserID <= 0 || strings.TrimSpace(request.SessionID) == "" || strings.TrimSpace(request.Question) == "" {
		return Result{}, fmt.Errorf("model-planned recall needs an owner, a session and a question")
	}
	now := request.Now
	if now.IsZero() {
		now = time.Now()
	}

	snapshot, err := s.catalogue.Build(ctx, request.UserID, request.SessionID, now)
	if err != nil {
		diagnostics.finish(started, StageCatalogue, "catalogue_unavailable")
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics}, nil
	}
	diagnostics.CatalogueTopics = len(snapshot.Topics)
	diagnostics.CatalogueEpisodes = len(snapshot.Episodes)
	diagnostics.CatalogueTokens = snapshot.Tokens()
	diagnostics.CatalogueVersion = snapshot.CatalogueVersion
	diagnostics.CatalogueOverflow = !snapshot.TopicComplete || !snapshot.EpisodeComplete
	if len(snapshot.Topics) == 0 && len(snapshot.Episodes) == 0 {
		diagnostics.finish(started, StageCatalogue, "catalogue_empty")
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics}, nil
	}

	plan, plannerCalls, err := s.plan(ctx, request, snapshot)
	diagnostics.PlannerCalls = plannerCalls
	if err != nil {
		diagnostics.finish(started, StagePlanner, failureCode("planner", err))
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics}, nil
	}
	diagnostics.PlanHash = plan.Hash()
	diagnostics.Decision = string(plan.Decision)
	diagnostics.Purpose = string(plan.Purpose)
	diagnostics.PlanTopicKeys = len(plan.TopicKeys)
	diagnostics.PlanEpisodeIDs = len(plan.EpisodeIDs)
	diagnostics.PlanHasQuery = strings.TrimSpace(plan.EpisodeQuery) != ""
	if plan.Decision == DecisionNone {
		diagnostics.finish(started, StageComplete, "plan_none")
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics, Plan: plan}, nil
	}

	candidates, err := s.builder.Build(ctx, CandidateRequest{
		UserID: request.UserID, SessionID: request.SessionID, Question: request.Question,
		Now: now, Plan: plan, Catalogue: snapshot, CandidateDeep: s.config.CandidateDepth,
	})
	diagnostics.Reasons = append(diagnostics.Reasons, candidates.Reasons...)
	diagnostics.CandidateExplicit = candidates.ExplicitHits
	diagnostics.CandidateExact = candidates.ExactHits
	diagnostics.CandidateQuery = candidates.QueryHits
	diagnostics.CandidateLatest = candidates.LatestHits
	diagnostics.ReloadRejections = candidates.ReloadRejections
	if err != nil {
		diagnostics.finish(started, StageCandidates, "candidate_build_failed")
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics}, nil
	}
	pool := append(append([]Candidate{}, candidates.Semantic...), candidates.Episodes...)
	diagnostics.CandidateSemantic = len(candidates.Semantic)
	diagnostics.CandidateEpisodes = len(candidates.Episodes)
	if len(pool) == 0 {
		diagnostics.finish(started, StageCandidates, "no_candidate_survived_reload")
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics, Plan: plan}, nil
	}

	verdicts, verifierCalls, err := s.verify(ctx, request, plan, pool)
	diagnostics.VerifierCalls = verifierCalls
	if err != nil {
		diagnostics.finish(started, StageVerifier, failureCode("verifier", err))
		s.publish(diagnostics)
		return Result{Diagnostics: diagnostics, Plan: plan, Pool: pool}, nil
	}
	relations := map[string]Relation{}
	for _, verdict := range verdicts {
		relations[verdict.CandidateID] = verdict.Relation
		diagnostics.countRelation(verdict.Relation)
	}

	result := Result{Diagnostics: diagnostics, Plan: plan, Pool: pool, Verdicts: verdicts}
	for _, candidate := range SelectInjectable(verdicts, pool) {
		relation := relations[candidate.ID]
		switch candidate.Kind {
		case CandidateSemantic:
			line := renderTopicMemory(candidate.Memory, relation)
			result.Topic = append(result.Topic, line)
			result.TopicIDs = append(result.TopicIDs, candidate.ID)
			result.Injected = append(result.Injected, InjectedMemory{
				ID: candidate.ID, Kind: CandidateSemantic, Relation: relation, Line: line,
			})
		case CandidateEpisode:
			// The context builder applies the same three-episode ceiling it
			// always has. Cutting here as well would be a second, invisible
			// budget; the count that matters is the one the builder reports.
			result.Episodes = append(result.Episodes, candidate.Episode)
			result.Injected = append(result.Injected, InjectedMemory{
				ID: candidate.ID, Kind: CandidateEpisode, Relation: relation,
			})
		}
	}
	result.Diagnostics.InjectedSemantic = len(result.Topic)
	result.Diagnostics.InjectedEpisodes = len(result.Episodes)
	result.Diagnostics.finish(started, StageComplete, "")
	s.publish(result.Diagnostics)
	return result, nil
}

// renderTopicMemory keeps the legacy line shape and adds the relation tag.
//
// The tag is the difference between "the learner believes this" and "this is
// how the answer should be pitched". Flattening them is how a personalisation
// note gets read back to the learner as a technical fact.
func renderTopicMemory(memory model.LongTermMemory, relation Relation) string {
	label := RelationLabel(relation)
	return fmt.Sprintf("[%s; tags=%s; %s] %s: %s", memory.Scope, strings.Join(memory.Tags, ","),
		label, strings.TrimSpace(memory.Subject), strings.TrimSpace(memory.Content))
}

func (s *Service) plan(ctx context.Context, request Request, snapshot MemoryCatalogueSnapshot) (MemoryReadPlan, int, error) {
	rendered, err := snapshot.Render()
	if err != nil {
		return MemoryReadPlan{}, 0, err
	}
	payload := fmt.Sprintf("CURRENT TIME\n%s\n\nMEMORY CATALOGUE\n%s\n\nQUESTION\n%s",
		request.Now.Format(time.RFC3339), rendered, strings.TrimSpace(request.Question))
	index := snapshot.Index()
	calls := 0
	var lastErr error
	for attempt := 1; attempt <= s.config.MaxAttempts; attempt++ {
		message, err := s.call(ctx, s.config.PlannerTimeout, PlannerPrompt, payload, PlanTool(), 700)
		calls++
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return MemoryReadPlan{}, calls, ctx.Err()
			}
			continue
		}
		plan, decodeErr := DecodePlan(message)
		if decodeErr != nil {
			lastErr = decodeErr
			continue
		}
		if validateErr := plan.ValidateAgainst(index); validateErr != nil {
			lastErr = validateErr
			continue
		}
		return plan, calls, nil
	}
	return MemoryReadPlan{}, calls, fmt.Errorf("planner failed after %d attempts: %w", calls, lastErr)
}

func (s *Service) verify(ctx context.Context, request Request, plan MemoryReadPlan, pool []Candidate) ([]CandidateVerdict, int, error) {
	rendered, err := RenderCandidates(pool)
	if err != nil {
		return nil, 0, err
	}
	payload := fmt.Sprintf("QUESTION\n%s\n\nWHAT THE READ WAS FOR\ndecision=%s purpose=%s\n\nCANDIDATES\n%s",
		strings.TrimSpace(request.Question), plan.Decision, plan.Purpose, rendered)
	// The verdict schema asks for one entry per candidate, so the output length
	// scales with the pool. A fixed ceiling is therefore a self-inflicted
	// protocol failure on exactly the largest pools: the v1.0 development run
	// lost thirteen of sixty cases that way, all of them at pool sizes eleven to
	// fourteen, while smaller pools passed. The budget is derived from the pool
	// instead, with a floor for the small ones.
	maxTokens := verdictTokenBudget(len(pool))
	calls := 0
	var lastErr error
	for attempt := 1; attempt <= s.config.MaxAttempts; attempt++ {
		message, err := s.call(ctx, s.config.VerifierTimeout, VerifierPrompt, payload, VerdictTool(), maxTokens)
		calls++
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, calls, ctx.Err()
			}
			continue
		}
		set, decodeErr := DecodeVerdicts(message)
		if decodeErr != nil {
			lastErr = decodeErr
			continue
		}
		verdicts, validateErr := ValidateVerdicts(set, pool)
		if validateErr != nil {
			lastErr = validateErr
			continue
		}
		return verdicts, calls, nil
	}
	return nil, calls, fmt.Errorf("verifier failed after %d attempts: %w", calls, lastErr)
}

// call is the one place a request leaves the process.
//
// The stage timeout is applied on top of the agent's own context rather than
// replacing it, so a cancelled turn cancels the stage; and the concurrency
// permit is taken under that same deadline, so a saturated process fails the
// stage instead of queueing behind it and blowing the turn's budget.
func (s *Service) call(ctx context.Context, timeout time.Duration, prompt, payload string,
	tool *schema.ToolInfo, maxTokens int) (*schema.Message, error) {
	staged, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	case <-staged.Done():
		return nil, fmt.Errorf("model-planned recall concurrency limit: %w", staged.Err())
	}
	bound, err := s.chat.WithTools([]*schema.ToolInfo{tool})
	if err != nil {
		return nil, err
	}
	return bound.Generate(staged, []*schema.Message{
		schema.SystemMessage(prompt), schema.UserMessage(payload),
	}, modelcomponent.WithToolChoice(schema.ToolChoiceForced), modelcomponent.WithMaxTokens(maxTokens))
}

// verdictTokenBudget sizes the verifier's output allowance to the pool.
//
// One verdict is a candidate id, a relation and a short field reference. The
// ids are UUIDs, which are the expensive part, so the per-candidate figure is
// generous rather than tight: running out of output tokens costs the whole
// stage, while over-allowing costs nothing, because the model stops when it has
// answered.
func verdictTokenBudget(candidates int) int {
	const perCandidate = 90
	const floor = 600
	budget := floor + candidates*perCandidate
	if budget < floor {
		return floor
	}
	return budget
}

// failureCode separates the three failures that look alike in a log and mean
// different things: the network did not answer, the model answered something
// this system cannot read, and the turn itself was cancelled.
func failureCode(stage string, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return stage + "_cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return stage + "_timeout"
	case errors.Is(err, ErrProtocolFailure):
		return stage + "_protocol_failure"
	default:
		return stage + "_transport_failure"
	}
}

// ObserveDegradation reports a fail-closed read to the shared memory metrics, so
// the mode's outages show up next to every other provider's.
func ObserveDegradation(stage Stage) {
	observability.Memory.ProviderDegraded("model_planned_" + string(stage))
}
