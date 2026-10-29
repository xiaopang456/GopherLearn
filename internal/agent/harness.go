package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/contextwindow"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// Budget defaults and ceilings. The ceilings exist so that a mistyped environment
// value cannot turn one user message into an unbounded number of paid calls: a
// normalize step that only ever raises a zero to its default still lets a hand-set 1e6
// through.
const (
	DefaultMaxModelCalls = 4
	DefaultMaxToolCalls  = 3
	DefaultMaxSteps      = 10
	DefaultDeadline      = 45 * time.Second

	MaxModelCallsCeiling = 12
	MaxToolCallsCeiling  = 12
	MaxStepsCeiling      = 24
	DeadlineCeiling      = 5 * time.Minute
)

// Budget bounds one agent turn.
type Budget struct {
	// MaxModelCalls is how many times the loop may ask the model anything, including
	// the call that finally produces the answer.
	MaxModelCalls int
	// MaxToolCalls is how many tools may be executed in total, across all rounds.
	MaxToolCalls int
	// MaxSteps counts rounds through the loop, so a model that alternates between two
	// useless calls still stops.
	MaxSteps int
	// Deadline bounds the whole turn, model latency and tool time together.
	Deadline time.Duration
}

// Normalize fills zero values with defaults and clamps absurd ones. A negative value is
// a configuration fault and becomes the default rather than a disabled budget: "no
// limit" is not an option this loop offers.
//
// The model-call and step limits are independent, and the tighter one governs: a turn
// configured for 20 calls but 3 steps stops after 3 steps. Raising one to match the
// other would silently discard the stricter cap, which is the only setting that stops a
// model that keeps asking for tools.
func (b Budget) Normalize() Budget {
	if b.MaxModelCalls <= 0 {
		b.MaxModelCalls = DefaultMaxModelCalls
	}
	if b.MaxModelCalls > MaxModelCallsCeiling {
		b.MaxModelCalls = MaxModelCallsCeiling
	}
	if b.MaxToolCalls <= 0 {
		b.MaxToolCalls = DefaultMaxToolCalls
	}
	if b.MaxToolCalls > MaxToolCallsCeiling {
		b.MaxToolCalls = MaxToolCallsCeiling
	}
	if b.MaxSteps <= 0 {
		b.MaxSteps = DefaultMaxSteps
	}
	if b.MaxSteps > MaxStepsCeiling {
		b.MaxSteps = MaxStepsCeiling
	}
	if b.Deadline <= 0 {
		b.Deadline = DefaultDeadline
	}
	if b.Deadline > DeadlineCeiling {
		b.Deadline = DeadlineCeiling
	}
	return b
}

// Usage reports what one turn actually spent. It is returned to the caller rather than
// kept on the harness, because a shared harness serving concurrent turns cannot own
// per-turn counters.
type Usage struct {
	ModelCalls int
	ToolCalls  int
	Steps      int
	// ToolRounds counts rounds in which the model asked for at least one tool.
	ToolRounds int
	// Context reports what the window budget decided and spent.
	Context ContextUsage
}

// ContextUsage is the context side of one turn's receipt.
//
// The opening figures are what the turn started with, not what any later call held:
// the estimate of the request actually sent is reported per call by the check that
// gates it, so a comparison of two windows can read what each one put in front of the
// model rather than inferring it from a message count.
type ContextUsage struct {
	// AssembledTokens estimates the opening request: system instruction, summary
	// block, kept exchanges, the question and the tool offers.
	AssembledTokens int
	// RequiredTokens is the part of that which no budget may shorten.
	RequiredTokens int
	// EvidenceUsed is what this turn spent on evidence bodies across every tool call,
	// out of the whole-turn allowance.
	EvidenceUsed int
	// HistoryKept and HistoryDropped count whole exchanges.
	HistoryKept    int
	HistoryDropped int
	// SummaryTruncated and SummaryDropped say the stored summary did not reach the
	// model as stored. They are separate because "shortened" and "absent" are
	// different answers to "what did this turn know".
	SummaryTruncated bool
	SummaryDropped   bool
}

// ErrContextBudget wraps the reason a turn stopped for want of context rather than
// for want of calls. It is a distinct sentinel because the two need different answers
// from an operator: an exhausted call budget is a model that kept asking, while a
// context refusal means the configured window does not hold what this turn carries.
//
// The wrapped contextwindow error says which half refused - the required parts, the
// assembled request, or the declared window - so the classification is available to a
// log or a test with errors.Is without adding a public termination reason that no
// client has agreed to read.
var ErrContextBudget = errors.New("agent: this turn exceeded its context budget")

// Harness runs the loop: ask, validate what came back, execute what the server vouches
// for, feed bounded results back, stop when the model answers or a budget says enough.
//
// It is a responder.Strategy, so it is installed the same way the two chat strategies
// are and the service layer keeps owning persistence and the session lock.
type Harness struct {
	chat         model.ToolChatModel
	tools        *Registry
	budget       Budget
	systemPrompt string
	// window is the context budget this loop holds itself to. It is never nil-able:
	// a harness with no configured window runs the defaults, because "no check" is
	// not an option a loop that bills the user by the token should offer.
	window contextwindow.Config
	// providerMaxOutputTokens is the limit the client will apply to a request that
	// names none. The window check needs it: an input target says how much a reply
	// leaves room for, and only the effective output limit says how much a reply may
	// take.
	providerMaxOutputTokens int
}

// Option configures a harness beyond its call and time budgets.
type Option func(*Harness) error

// WithContextWindow installs the window budget and the provider's own output limit.
// A zero Config resolves to the defaults; an illegal one is refused at construction,
// which is the last moment it can still stop startup.
func WithContextWindow(config contextwindow.Config, providerMaxOutputTokens int) Option {
	return func(h *Harness) error {
		resolved, err := config.Resolve()
		if err != nil {
			return err
		}
		h.window = resolved
		h.providerMaxOutputTokens = providerMaxOutputTokens
		return nil
	}
}

// New builds a harness. An empty registry is refused: a loop with no tools can only
// ever be a plain completion, and pretending otherwise would cost the extra budget
// without the capability.
func New(chat model.ToolChatModel, tools *Registry, budget Budget, opts ...Option) (*Harness, error) {
	if chat == nil {
		return nil, errors.New("agent: a harness needs a tool-capable chat model")
	}
	if tools == nil || len(tools.ordered) == 0 {
		return nil, errors.New("agent: a harness needs at least one registered tool")
	}
	harness := &Harness{
		chat:         chat,
		tools:        tools,
		budget:       budget.Normalize(),
		systemPrompt: SystemPrompt,
		window:       contextwindow.DefaultConfig(),
	}
	for _, opt := range opts {
		if err := opt(harness); err != nil {
			return nil, err
		}
	}
	return harness, nil
}

// Strategy implements responder.Strategy.
func (h *Harness) Strategy() domain.AnswerStrategy { return domain.AnswerStrategyAgent }

// ToolNames reports the capabilities this harness may use, for the startup log.
func (h *Harness) ToolNames() []string { return h.tools.Names() }

// Budget reports the effective limits.
func (h *Harness) Budget() Budget { return h.budget }

// Window reports the effective context budget, so a startup line can say what the
// loop will actually enforce rather than what was asked for.
func (h *Harness) Window() contextwindow.Config { return h.window }

var _ responder.Strategy = (*Harness)(nil)

// Run executes one agent turn and reports the answer plus what it cost.
func (h *Harness) Run(ctx context.Context, request responder.Request) (responder.Answer, Usage, error) {
	return h.run(ctx, request, nil, nil)
}

// run is the loop. A nil onStep is the ordinary turn: nothing is reported, and nothing about
// the answer changes. A nil out is a turn nobody reads while it runs - the model is asked in
// one completion, and the finished answer is chunked afterwards if the caller wants deltas.
// With an out, each round's public text is handed to it as the provider produces it, and the
// publisher decides whether that text can be shown immediately or only once the round has
// proved it was an answer; see stream.go.
func (h *Harness) run(ctx context.Context, request responder.Request, onStep responder.StepSink,
	out *publisher,
) (answer responder.Answer, spent Usage, turnErr error) {
	if strings.TrimSpace(request.UserID) == "" {
		return responder.Answer{}, spent, ErrNoPrincipal
	}
	if strings.TrimSpace(request.Content) == "" {
		return responder.Answer{}, spent, fmt.Errorf("%w: an agent turn needs a question", model.ErrInvalidRequest)
	}

	// The caller's context is kept apart from the turn deadline so that "the operator's
	// budget ran out" and "the client hung up" cannot be reported as the same event.
	parent := ctx
	if h.budget.Deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, h.budget.Deadline)
		defer cancel()
	}

	principal := Principal{UserID: request.UserID, SessionID: request.SessionID, UserMessageID: request.UserMessageID}
	// Document knowledge is reachable only when the question is not about this
	// conversation itself. It is one predicate behind two decisions that must
	// agree: the window reserves its evidence tokens only when it returns true,
	// and the tool offers exclude the document tools when it returns false. If
	// the two ever disagreed in the direction of reserving nothing while the
	// tools stayed registered, the assembled window plus evidence could exceed
	// the input target - so both call this and not the underlying classifier.
	documentReachable := documentKnowledgeReachable(request.Content)
	definitions := h.tools.Definitions()
	if !documentReachable {
		definitions = withoutDocumentKnowledgeTools(definitions)
	}
	// One turn's evidence allowance and its citation authority, both on the
	// run's own stack. The token allowance lives in contextwindow and is what
	// the tools charge; the registry beside it owns citations, memory
	// references and read-back eligibility. The instances are process-wide but
	// the state is per-turn, for the same reason the per-tool use counts live
	// on the stack: a budget or a citation set shared across turns would let
	// one learner's turn shape another's.
	evidence := contextwindow.NewEvidence(h.window.EvidenceMaxTokens)
	ctx = contextwindow.WithEvidence(ctx, evidence)
	turnEvidence := NewTurnEvidence(h.window.EvidenceMaxTokens)
	turnEvidence.AttachBudget(evidence)
	ctx = WithTurnEvidence(ctx, turnEvidence)
	// Reported on the way out, including on a turn that stopped early: what a turn
	// spent before it stopped is the number an operator needs, and a receipt that
	// only exists for successful turns hides every refusal.
	defer func() { spent.Context.EvidenceUsed = evidence.Used() }()
	messages, window, err := contextwindow.Build(contextwindow.Parts{
		SystemPrompt:               h.systemPrompt,
		TaskState:                  request.TaskState,
		Mastery:                    firstNonEmpty(request.Mastery, request.TopicMastery),
		GlobalMemory:               request.GlobalMemory,
		TopicMemory:                request.TopicMemory,
		EpisodeMemory:              request.EpisodeMemory,
		Summary:                    request.Summary,
		History:                    request.History,
		Question:                   request.Content,
		Tools:                      definitions,
		DocumentKnowledgeReachable: documentReachable,
	}, h.window)
	spent.Context.AssembledTokens = window.AssembledTokens
	spent.Context.RequiredTokens = window.RequiredTokens
	spent.Context.HistoryKept = window.HistoryKept
	spent.Context.HistoryDropped = window.HistoryDropped
	spent.Context.SummaryTruncated = window.SummaryTruncated
	spent.Context.SummaryDropped = window.SummaryDropped
	if err != nil {
		return responder.Answer{}, spent, fmt.Errorf("%w: %w", ErrContextBudget, err)
	}
	var collected []domain.Source
	// Uses of a write-capable tool, counted for this run only. The map lives on the stack
	// because a run's limits are a run's business: the same tool instance answers for every
	// user for the whole process lifetime.
	uses := map[string]int{}
	// The consecutive-call fingerprint and the no-progress ledger, both per-turn
	// for the same reason the use counts are: a repeat that one turn answered
	// honestly is still a repeat when another turn asks it, and a ledger shared
	// across turns would let one learner's fruitless searches mute another's.
	lastFingerprint := ""
	progress := newToolProgressLedger()
	// Sources this turn has already shown, keyed the way dedupeSources keys
	// them. The ledger reads the delta across one tool call from it.
	seenSources := map[string]bool{}
	// The tools this turn may actually run. A question about the conversation
	// itself has no document tool in its offers, and a model that asks for one
	// anyway is answered by the same unknown-tool error any unregistered name
	// gets - the offers and the executor cannot disagree about the set.
	toolScope := h.tools
	if !documentReachable {
		toolScope = h.tools.restrictedTo(func(name string) bool { return !isDocumentKnowledgeTool(name) })
	}
	steps := &stepReporter{sink: onStep}

	for {
		if err := h.checkTurn(ctx, parent, spent); err != nil {
			return responder.Answer{}, spent, err
		}
		// Every budget is checked before the counter moves, so a reported number is
		// always work that actually happened rather than an attempt that was refused.
		if spent.Steps >= h.budget.MaxSteps {
			return responder.Answer{}, spent, fmt.Errorf("%w: %d steps", ErrStepBudget, h.budget.MaxSteps)
		}
		spent.Steps++
		if spent.ModelCalls >= h.budget.MaxModelCalls {
			return responder.Answer{}, spent, fmt.Errorf("%w: %d calls", ErrModelCallBudget, h.budget.MaxModelCalls)
		}
		// Measured on the request as it will actually go out, on every round and not
		// only the first: the observations this loop has already fed back stay in the
		// message list, so a turn that fitted its opening prompt can still grow past
		// the window. Checked before the counter moves, like every other budget here,
		// and refusing spends nothing - no call is made to find out.
		pending := model.Request{Messages: messages, Tools: definitions}
		// The tool ceiling bounds executions, not the ability to synthesize a
		// final answer from the evidence already collected. Once it is spent,
		// the next call closes the turn out: the model is told it may not ask
		// for tools again, and whatever it produces is the answer. The call
		// still counts against the model ceiling, and a provider that violates
		// tool_choice=none is still stopped by the hard check inside the tool
		// loop below - what it may not do is buy another round of executions.
		if spent.ToolCalls >= h.budget.MaxToolCalls {
			pending.ToolChoice = model.ToolChoiceNone
		}
		if err := contextwindow.CheckRequest(pending, h.window, h.providerMaxOutputTokens); err != nil {
			return responder.Answer{}, spent, fmt.Errorf("%w: %w", ErrContextBudget, err)
		}
		spent.ModelCalls++
		// The step is announced before the call is made, so a client that has gone away
		// stops the spend instead of being reported to afterwards.
		if err := steps.modelCall(spent.ModelCalls); err != nil {
			return responder.Answer{}, spent, err
		}
		completion, err := h.decide(ctx, pending, spent.ModelCalls, out)
		if err != nil {
			if deadline := h.deadlineError(ctx, parent); deadline != nil {
				return responder.Answer{}, spent, deadline
			}
			return responder.Answer{}, spent, err
		}
		if len(completion.ToolCalls) == 0 {
			// Plain prose is the final answer. Note what it excludes: a model that
			// wrote {"name":"knowledge_search",...} into its text asked for nothing,
			// because only the provider's structured field is a request. Reading prose
			// as a call is how a document that contains a JSON blob gets executed.
			if strings.TrimSpace(completion.Content) == "" {
				return responder.Answer{}, spent, model.ErrEmptyCompletion
			}
			answer = responder.Answer{
				Content: completion.Content,
				Sources: dedupeSources(collected),
				// A client that watched this answer arrive did so one fragment at a
				// time into one named block; the receipt says which one, so the
				// text it confirmed is the text it read rather than a similar copy.
				FinalBlockID: out.answerBlock(),
			}
			return answer, spent, nil
		}

		// The text beside a tool request is kept in the transcript, because that is what
		// the provider sent, but it is never the turn's answer.
		messages = append(messages, model.Message{
			Role:      model.RoleAssistant,
			Content:   completion.Content,
			ToolCalls: completion.ToolCalls,
		})
		spent.ToolRounds++

		for _, call := range completion.ToolCalls {
			// The reference project's budget order, restored: the whole-run
			// ceiling first, then the per-capability one, then the consecutive
			// fingerprint. Every refusal below is a stop, not an observation -
			// a model that keeps asking after its budget is spent is looping,
			// and answering it would spend the user's model calls on a
			// foregone conclusion. The one exception is the fruitless
			// read-only memory call, which the ledger answers before any of
			// this: it cannot achieve anything new, so it is neither executed
			// nor charged.
			if spent.ToolCalls >= h.budget.MaxToolCalls {
				return responder.Answer{}, spent, fmt.Errorf("%w: %d calls", ErrToolCallBudget, h.budget.MaxToolCalls)
			}
			name := strings.TrimSpace(call.Name)
			if readOnlyMemoryTool(call.Name) {
				if decision, previous := progress.Decide(call.Name, call.Arguments); decision != ToolDecisionRun {
					if decision == ToolDecisionExhausted {
						previous = progress.lastOutcomeFor(call.Name, call.Arguments)
					}
					suppressed := Observation{Text: noProgressObservation(previous, decision == ToolDecisionExhausted),
						Summary: "重复的长期记忆检索已停止，本轮请基于已有信息作答"}
					sequence, err := steps.toolCall(spent.ToolCalls+1, call.Name)
					if err != nil {
						return responder.Answer{}, spent, err
					}
					if err := steps.toolResult(sequence, call.Name, suppressed, 0); err != nil {
						return responder.Answer{}, spent, err
					}
					messages = append(messages, model.Message{
						Role:       model.RoleTool,
						ToolCallID: call.ID,
						Content:    suppressed.Text,
					})
					continue
				}
			}
			fingerprint := toolFingerprint(call.Name, call.Arguments)
			if fingerprint == lastFingerprint {
				return responder.Answer{}, spent, fmt.Errorf("%w: %s", ErrRepeatedToolCall, call.Name)
			}
			limit := toolScope.usesPerTurn(call.Name)
			if limit > 0 && uses[name] >= limit {
				// The per-capability ceiling is a stop, not an observation: the
				// reference project measured its budgets under this direction,
				// and a model that has spent a capability's allowance and asks
				// again has no further claim on the turn's remaining model
				// calls.
				return responder.Answer{}, spent, fmt.Errorf("%w: %s", ErrToolCallLimit, call.Name)
			}
			spent.ToolCalls++
			// The arguments are never part of a step, and neither is any label derived
			// from them: they are model text, and model text is not something this server
			// repeats back to a browser as a fact about what it is doing.
			sequence, err := steps.toolCall(spent.ToolCalls, call.Name)
			if err != nil {
				return responder.Answer{}, spent, err
			}
			// A cancellation between two tool calls of one round stops the remaining
			// calls: the model already answered, but nobody said the side effects were
			// wanted.
			if err := ctx.Err(); err != nil {
				if deadline := h.deadlineError(ctx, parent); deadline != nil {
					return responder.Answer{}, spent, deadline
				}
				return responder.Answer{}, spent, err
			}
			started := time.Now()
			observation, err := toolScope.run(ctx, principal, call)
			if err != nil {
				if deadline := h.deadlineError(ctx, parent); deadline != nil {
					return responder.Answer{}, spent, deadline
				}
				return responder.Answer{}, spent, err
			}
			if limit > 0 {
				// Only a call that actually ran is charged against the limit, so a failure
				// cannot silently consume the one write this turn is allowed.
				uses[name]++
			}
			lastFingerprint = fingerprint
			// The ledger watches the read-only memory tools: what a call reached
			// and what it newly showed decides whether the next equivalent call
			// is executed, answered from the ledger, or answered as exhausted.
			if readOnlyMemoryTool(call.Name) {
				newEvidence := countNewSources(observation.Sources, seenSources)
				outcome := ToolOutcome{Retrieved: !observation.Refused, NewEvidence: newEvidence, Reason: observation.Reason}
				progress.Observe(call.Name, call.Arguments, outcome)
				if observation.Refused {
					// A refused call never reached storage, so refund its
					// reservation and clear the exact-call fingerprint.
					if spent.ToolCalls > 0 {
						spent.ToolCalls--
					}
					if limit > 0 && uses[name] > 0 {
						uses[name]--
					}
					lastFingerprint = ""
				}
			}
			// Reported after the call returned, and never for a call that failed: a failed
			// tool produced no result, and a progress channel that says otherwise is the
			// same lie as a fabricated citation.
			if err := steps.toolResult(sequence, call.Name, observation, time.Since(started)); err != nil {
				return responder.Answer{}, spent, err
			}
			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				Content:    observation.Text,
				ToolCallID: call.ID,
			})
			collected = append(collected, observation.Sources...)
		}
	}
}

// countNewSources reports how many of the sources this call produced were not
// already shown earlier in the turn. It is the ledger's new-evidence signal:
// a card the model has already been given is not progress, whatever the tool
// call cost.
func countNewSources(sources []domain.Source, seen map[string]bool) int {
	if len(sources) == 0 {
		return 0
	}
	fresh := 0
	for _, source := range sources {
		key := source.SourceID
		if key == "" {
			key = source.CollectionID + "/" + source.DocumentID + "/" + source.ChunkID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		fresh++
	}
	return fresh
}

// Respond implements responder.Strategy.
func (h *Harness) Respond(ctx context.Context, request responder.Request) (responder.Answer, error) {
	answer, _, err := h.Run(ctx, request)
	return answer, err
}

// Stream implements responder.Strategy.
//
// It runs the same loop Run runs, and the answer it produces is the same one. What leaves as
// a delta is each finished model call's text: a round that turned out to ask for a tool
// contributes nothing, and the round that answers contributes its whole text at the end of
// that call. So an agent answer is not fragment-by-fragment live - a tool loop cannot know
// which kind of call it is reading until the call has ended - and it is not the finished
// answer sliced afterwards either, because the deltas are the provider's own chunks.
// Progress events are separate from both and go out as they happen; see StreamSteps.
func (h *Harness) Stream(ctx context.Context, request responder.Request, emit func(string) error) (responder.Answer, error) {
	if emit == nil {
		return responder.Answer{}, errors.New("agent: a streamed turn needs a delta handler")
	}
	out := h.publisher(emit)
	answer, _, err := h.run(ctx, request, nil, out)
	if err != nil {
		return responder.Answer{}, err
	}
	if out == nil {
		// No streaming transport: the honest fallback, and a different claim from the one
		// above. Nothing here asks the model a second time to make an answer look
		// incremental - that would bill the user twice for an appearance.
		if err := emitChunks(ctx, answer.Content, emit); err != nil {
			return responder.Answer{}, err
		}
	}
	return answer, nil
}

// publisher returns the answer channel for this harness, or nil when the installed model
// cannot stream a tool turn at all. The distinction is reported rather than hidden: a
// deployment whose provider offers no streaming tool calls still answers correctly, it just
// does so in one piece, and startup says which case it is in.
func (h *Harness) publisher(emit func(string) error) *publisher {
	if emit == nil {
		return nil
	}
	if _, ok := h.chat.(model.ToolStreamChatModel); !ok {
		return nil
	}
	return newPublisher(emit)
}

// StreamSteps implements responder.StepStreamer: the same loop, the same answer, the same
// budgets and the same cancellation rules, with progress reported while the loop runs.
//
// On this path the answer's text is a block rather than a message.delta stream: each model
// call's public text goes out as it is produced, and the block's ending frame says whether
// that text was a decision note or the answer the turn intends to store. emit is therefore
// unused - the service would otherwise send the same characters twice under two names - and
// it is kept in the signature because the responder face is one face for every strategy.
//
// What it deliberately does not do is answer any differently. The steps go out before
// the answer exists, which is exactly why no step may claim a fact about the answer -
// and why a step that cannot be delivered ends the turn instead of being logged and
// ignored.
func (h *Harness) StreamSteps(ctx context.Context, request responder.Request, emit func(string) error,
	onStep responder.StepSink,
) (responder.Answer, error) {
	if emit == nil {
		return responder.Answer{}, errors.New("agent: a streamed turn needs a delta handler")
	}
	if onStep == nil {
		// A turn that asked for progress and is given no way to receive it would otherwise
		// silently keep its text back: the answer would exist and never be shown.
		return responder.Answer{}, errors.New("agent: a turn that reports steps needs somewhere to report them")
	}
	answer, _, err := h.run(ctx, request, onStep, newBlockPublisher(onStep))
	if err != nil {
		return responder.Answer{}, terminate(err)
	}
	return answer, nil
}

var _ responder.StepStreamer = (*Harness)(nil)

// checkTurn notices a cancellation or expiry between steps, so a stopped client cannot
// be charged for another call.
func (h *Harness) checkTurn(ctx, parent context.Context, spent Usage) error {
	if err := ctx.Err(); err != nil {
		if deadline := h.deadlineError(ctx, parent); deadline != nil {
			return deadline
		}
		return err
	}
	return nil
}

// deadlineError reports ErrDeadline only when this harness's own budget is what
// expired. If the caller's context is also done, the caller ended the turn and that is
// what gets reported.
func (h *Harness) deadlineError(ctx, parent context.Context) error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	if parent != ctx && parent.Err() != nil {
		return nil
	}
	return fmt.Errorf("%w after %s", ErrDeadline, h.budget.Deadline)
}

// learningContextNote appends the mastery note to the agent's own system prompt.
//
// It belongs here rather than in the shared prompt layout because only this strategy
// plans work from it. A chat or knowledge turn that received the same paragraph would
// be told how well its user is doing without ever having been given a reason to know,
// and the byte-for-byte comparison those strategies rely on would no longer hold.
func learningContextNote(note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return ""
	}
	// The marker and the blank line are what tell a model where server-written
	// commentary starts, so the note cannot be read as part of the rules above it.
	return "\n\n[学习状态] 服务端对学习者最近练习主题的内部判断：" + note
}

// dedupeSources keeps one citation per source, in the order retrieval produced them.
// Repeats are normal - two rounds can search the same collection - and a client that
// saw the same citation twice would read it as two independent pieces of evidence.
func dedupeSources(sources []domain.Source) []domain.Source {
	if len(sources) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(sources))
	unique := make([]domain.Source, 0, len(sources))
	for _, source := range sources {
		key := source.SourceID
		if key == "" {
			key = source.CollectionID + "/" + source.DocumentID + "/" + source.ChunkID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, source)
	}
	return unique
}

func emitChunks(ctx context.Context, content string, emit func(string) error) error {
	if content == "" {
		return nil
	}
	runes := []rune(content)
	for offset := 0; offset < len(runes); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := offset + 16
		if end > len(runes) {
			end = len(runes)
		}
		if err := emit(string(runes[offset:end])); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
