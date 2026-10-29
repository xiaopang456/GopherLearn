package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

var (
	// ErrUnknownTool reports a model that asked for a capability this server does
	// not offer. It ends the turn instead of answering the request: a tool the
	// registry does not know has no implementation, and "guess what was meant" is
	// how an agent ends up running something nobody reviewed.
	ErrUnknownTool = errors.New("agent: the model asked for a tool this server does not offer")
	// ErrInvalidArguments reports arguments that do not fit the tool's own schema.
	ErrInvalidArguments = errors.New("agent: the model produced arguments this tool cannot accept")
	// The refusals below each wrap ErrInvalidArguments as well, so an existing caller
	// that only asks "was this a bad-argument refusal?" keeps working, while a caller
	// that needs to say *which* rule fired can ask with errors.Is instead of matching
	// error text. Which rule fired is a fact about this server's own parser, so unlike
	// a provider's message it is safe to record.
	//
	// ErrArgumentsNotObject reports an argument envelope that is empty or is not a
	// single JSON object - the shape a double-encoded payload arrives in.
	ErrArgumentsNotObject = errors.New("agent: tool arguments are not a single JSON object")
	// ErrArgumentsMalformed reports text that is not parseable JSON at all.
	ErrArgumentsMalformed = errors.New("agent: tool arguments are not parseable JSON")
	// ErrArgumentsTooLarge reports an argument blob past the size this server parses.
	ErrArgumentsTooLarge = errors.New("agent: tool arguments are larger than this server parses")
	// ErrUnknownArgumentField reports a key the tool never declared. Inventing a
	// privilege key is exactly what a parser must not absorb quietly.
	ErrUnknownArgumentField = errors.New("agent: tool arguments use a field this tool never declared")
	// ErrToolExecution reports a tool that failed while running. It is distinct from
	// a tool that succeeded and found nothing, which is an honest observation rather
	// than a failure.
	ErrToolExecution = errors.New("agent: tool execution failed")
	// ErrModelCallBudget reports the loop reaching its ceiling on provider calls.
	ErrModelCallBudget = errors.New("agent: the model-call budget for this turn is exhausted")
	// ErrToolCallBudget reports the loop reaching its ceiling on tool executions.
	ErrToolCallBudget = errors.New("agent: the tool-call budget for this turn is exhausted")
	// ErrToolCallLimit reports the model exceeding one capability's per-turn
	// ceiling. It is distinct from the whole-run ceiling so a report can say
	// which budget refused.
	ErrToolCallLimit = errors.New("agent: the per-tool budget for this turn is exhausted")
	// ErrRepeatedToolCall reports the model asking for exactly the call it just
	// made: same tool, same canonical arguments. A repeated exact call cannot
	// produce anything the previous one did not.
	ErrRepeatedToolCall = errors.New("agent: the model repeated the same tool call")
	// ErrStepBudget reports the loop reaching its ceiling on total steps, which is
	// what a model that keeps asking for tools forever runs into.
	ErrStepBudget = errors.New("agent: the step budget for this turn is exhausted")
	// ErrDeadline reports the whole turn exceeding its time budget. It is distinct
	// from a caller cancellation so an operator can tell a stuck loop apart from a
	// client that hung up.
	ErrDeadline = errors.New("agent: the turn exceeded its time budget")
	// ErrNoPrincipal reports a turn with no authenticated identity. A tool cannot
	// scope itself without one, and defaulting to "everything" would be a leak.
	ErrNoPrincipal = errors.New("agent: a tool run needs an authenticated principal")
)

// maxArgumentsBytes bounds what the model may hand a tool. The provider response is
// already capped, but an argument blob is model-authored text, and a tool should never
// be handed an unbounded one to parse.
const maxArgumentsBytes = 4 << 10

// Principal is who a tool runs as. It arrives from the authenticated request and is
// never assembled from model output: a tool that took a user id out of its own
// arguments would let a model read another user's documents by naming them.
//
// UserMessageID is the stored id of this turn's question, and it is here for the same
// reason: a capability that records something about "what just happened" needs evidence
// the server has already written, not a message id the model guessed.
type Principal struct {
	UserID        string
	SessionID     string
	UserMessageID string
}

// Observation is one tool's answer to the model.
type Observation struct {
	// Text is what the model reads back. A tool bounds it itself, because this is
	// the one string in the loop whose size is decided by whatever the search found.
	Text string
	// Sources are the citations this server earned by actually retrieving material.
	// They attach to the turn's final answer; a model cannot add to them, rename
	// them, or point at them from its prose.
	Sources []domain.Source
	// Found reports whether the tool produced evidence at all. "Searched, nothing
	// relevant" is a successful call with Found false, which is what keeps an honest
	// empty result from being reported as a failure - and keeps a fabricated citation
	// from having anything real behind it.
	Found bool
	// Refused reports that the call never reached storage: a permission or
	// protocol refusal the tool decided before asking it. It is the seam the
	// no-progress ledger uses to tell "I searched and found nothing" - which
	// cost a retrieval - from "I refused to search" - which cost nothing and is
	// refunded. A tool that does not set it reports every executed call as a
	// retrieval, which is the pre-existing behaviour.
	//
	// Reason is the tool's own closed-vocabulary outcome code, carried for the
	// same ledger: "you are not allowed to search this" and "there is nothing
	// recorded" are different facts about the learner, and an exhausted turn
	// that can still say which one it hit gives the model something specific.
	// It is an enumeration, never the tool's observation text.
	Refused bool
	Reason  string
	// Summary is one sentence about what the call did, written by the tool for a human
	// watching the turn rather than for the model. Text is model input and stays on the
	// server side; a step may only carry what this server composed itself, which is why
	// the tools write a count rather than an excerpt.
	//
	// It must never contain recalled content, an identifier, or anything derived from the
	// arguments the model supplied.
	Summary string
	// Draft is the one memory draft this call created, projected down to what the learner
	// needs in order to decide on it. Nil means this call stored nothing - which is also
	// the answer for a duplicate claim: the draft it matched is waiting on the card of the
	// turn that wrote it, and a second card pointing at it would let one click decide
	// something this conversation did not create.
	//
	// Owner, session, source messages, confidence and timestamps are absent by
	// construction: a draft's provenance belongs to the memory page, and a progress frame
	// is a request for a decision, not a place to expose the stored row.
	Draft *responder.MemoryDraft
}

// Tool is one capability the server implements and vouches for.
type Tool interface {
	Name() string
	Description() string
	// Parameters is the JSON Schema offered to the model. It is server-authored text;
	// a model never supplies it.
	Parameters() json.RawMessage
	// Execute runs the tool as the given principal. It must validate its own
	// arguments rather than trust the schema was honored, and it must not read any
	// identity out of those arguments.
	Execute(ctx context.Context, principal Principal, arguments json.RawMessage) (Observation, error)
}

// Registry holds the installed tools a turn may use. Keeping the set in one server-owned
// place is what makes "unknown tool" a checkable fact instead of a judgement call.
type Registry struct {
	byName  map[string]Tool
	ordered []Tool
}

// perTurnUseLimits is the server's own list of capabilities that may run at most a fixed
// number of times in one agent turn, keyed by the tool name the server registered.
//
// It is a table here rather than a method on the Tool interface on purpose: a caller may
// wrap a tool to observe it (the one-shot smoke check counts executions), and a wrapper that
// does not forward an extra method would silently loosen the limit to unlimited. Read
// anything off the instance and the guard depends on how it was decorated; look the name up
// and it does not.
var perTurnUseLimits = map[string]int{
	// A write is a decision, and a turn may make one of these per capability. Repeating it
	// would either duplicate the draft or let a persistent model keep spending the user's
	// budget on a claim that is already waiting.
	ProposeMemoryName: 1,
	// The episodic reads are bounded at two calls per turn: a search and a refined
	// search, or two read-backs, is what answering a history question honestly
	// takes. Beyond that the model is looping, and each call re-pays the same
	// boundary checks on content it has already been shown.
	EpisodeSearchName:      2,
	ReadHistoryMessageName: 2,
}

// usesPerTurn reports how many times one turn may run this tool, or 0 when the tool is not
// limited beyond the turn's budgets.
func (r *Registry) usesPerTurn(name string) int {
	if _, exists := r.byName[strings.TrimSpace(name)]; !exists {
		// An unknown name is ErrUnknownTool's business, not a limit's.
		return 0
	}
	return perTurnUseLimits[strings.TrimSpace(name)]
}

// NewRegistry installs tools and refuses duplicates: two implementations answering to
// one name would mean the model, not the server, chose which ran.
func NewRegistry(tools ...Tool) (*Registry, error) {
	registry := &Registry{byName: make(map[string]Tool, len(tools))}
	for _, tool := range tools {
		if tool == nil {
			return nil, errors.New("agent: nil tool")
		}
		name := strings.TrimSpace(tool.Name())
		if name == "" {
			return nil, errors.New("agent: a tool needs a name")
		}
		if !json.Valid(tool.Parameters()) || len(tool.Parameters()) == 0 {
			return nil, fmt.Errorf("agent: tool %q has no valid parameter schema", name)
		}
		if _, exists := registry.byName[name]; exists {
			return nil, fmt.Errorf("agent: duplicate tool %q", name)
		}
		registry.byName[name] = tool
		registry.ordered = append(registry.ordered, tool)
	}
	return registry, nil
}

// restrictedTo returns a view of the registry that answers only the tools
// whose names satisfy allowed. The tool instances are shared, not copied: a
// restriction is a per-turn decision about what this turn may run, and the
// tools themselves hold no per-turn state - the turn's evidence and citations
// travel in the context. An unknown name behaves exactly as it does on the
// full registry: ErrUnknownTool at execution time.
func (r *Registry) restrictedTo(allowed func(name string) bool) *Registry {
	view := &Registry{byName: make(map[string]Tool, len(r.ordered))}
	for _, tool := range r.ordered {
		name := strings.TrimSpace(tool.Name())
		if !allowed(name) {
			continue
		}
		view.byName[name] = tool
		view.ordered = append(view.ordered, tool)
	}
	return view
}

// Names lists the installed tools in registration order, for startup logs and tests.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.ordered))
	for _, tool := range r.ordered {
		names = append(names, tool.Name())
	}
	return names
}

// Definitions renders the registry as the tool offers sent to the model.
func (r *Registry) Definitions() []model.ToolDefinition {
	definitions := make([]model.ToolDefinition, 0, len(r.ordered))
	for _, tool := range r.ordered {
		definitions = append(definitions, model.ToolDefinition{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.Parameters(),
		})
	}
	return definitions
}

// run looks the tool up by the exact name the model used and executes it. A lookup
// miss is ErrUnknownTool, and a tool failure is ErrToolExecution wrapping whatever the
// tool reported, so a tool's own refusal - ErrInvalidArguments, a foreign collection -
// stays testable by the caller.
func (r *Registry) run(ctx context.Context, principal Principal, call model.ToolCall) (Observation, error) {
	name := strings.TrimSpace(call.Name)
	tool, exists := r.byName[name]
	if !exists {
		return Observation{}, fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	if len(call.Arguments) > maxArgumentsBytes {
		return Observation{}, fmt.Errorf("%w: %q exceeds %d bytes of arguments: %w",
			ErrInvalidArguments, name, maxArgumentsBytes, ErrArgumentsTooLarge)
	}
	if err := requireJSONObject(call.Arguments); err != nil {
		return Observation{}, fmt.Errorf("%w of %q: %w", ErrInvalidArguments, name, err)
	}
	observation, err := tool.Execute(ctx, principal, json.RawMessage(call.Arguments))
	if err != nil {
		return Observation{}, fmt.Errorf("%w: %q: %w", ErrToolExecution, name, err)
	}
	return observation, nil
}

// requireJSONObject rejects an argument envelope that is not one JSON object before
// any tool sees it. Arrays, scalars and trailing text are all ways for a model to look
// like it complied while handing a tool something the tool never declared. The two
// rejections carry different sentinels because they describe different vendor failures: a
// payload that parses to something other than an object is usually a double-encoded
// string, and one that does not parse at all is truncated or mis-escaped. This server
// repairs neither, but it can say which it was without quoting the payload.
func requireJSONObject(raw string) error {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("%w: arguments must be a single JSON object", ErrArgumentsNotObject)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return fmt.Errorf("%w: %v", ErrArgumentsMalformed, err)
	}
	return nil
}

// decodeArguments parses a tool's arguments strictly into its own struct. Unknown keys
// are refused rather than ignored, because the keys a model invents - "user_id",
// "all_collections", "admin" - are exactly the ones that must not be quietly dropped. The
// declared set is read back out of the struct, so a tool cannot advertise one shape to the
// model and parse another.
func decodeArguments(raw json.RawMessage, target any) error {
	// A tool validates its own envelope rather than trusting that the registry looked at it
	// first, and the shape verdict comes from the same classifier the registry uses, so the
	// two paths cannot disagree about which rule fired.
	if err := requireJSONObject(string(raw)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	if undeclared := countUndeclared(envelope, declaredArgumentFields(target)); undeclared > 0 {
		return fmt.Errorf("%w: %d field(s) this tool never declared: %w",
			ErrInvalidArguments, undeclared, ErrUnknownArgumentField)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	if decoder.More() {
		return fmt.Errorf("%w: trailing data after the argument object", ErrInvalidArguments)
	}
	return nil
}

// declaredArgumentFields lists the JSON keys one tool's argument struct accepts, so the
// parser and the advertised schema cannot drift apart silently.
func declaredArgumentFields(target any) map[string]bool {
	structType := reflect.TypeOf(target)
	if structType == nil {
		return nil
	}
	for structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return nil
	}
	declared := make(map[string]bool, structType.NumField())
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if !field.IsExported() {
			continue
		}
		name := strings.TrimSpace(strings.Split(field.Tag.Get("json"), ",")[0])
		if name == "" {
			name = field.Name
		}
		if name != "-" {
			declared[name] = true
		}
	}
	return declared
}

// countUndeclared counts the keys a model sent that no field asked for. The count is a
// fact about the payload's shape and may be recorded; the names are model text and may not.
func countUndeclared(envelope map[string]json.RawMessage, declared map[string]bool) int {
	undeclared := 0
	for key := range envelope {
		if !declared[key] {
			undeclared++
		}
	}
	return undeclared
}
