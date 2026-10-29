package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/cloudwego/eino/schema"
)

// Reassembling streamed tool calls before judging them.
//
// STABILIZE-062 v1.1 spent a real run learning why this has to exist. Its
// observer looked at each streamed chunk on its own and asked "is this tool
// name on the table". In a streamed tool call the name arrives once, in one
// chunk, and every following chunk carries an argument fragment and an empty
// name - so the question was asked twenty-three times of something that was
// never a tool name, and the answer was recorded as the model calling a tool
// that does not exist.
//
// The rule that follows is general, and it is the one worth keeping: a stream
// delivers fragments of a semantic object, and a contract may only be checked
// against the assembled object. Validating a fragment is validating something
// the protocol never claimed was complete.
//
// This aggregator is deliberately an observation tool. It does not produce the
// message the Agent acts on - Eino's own concatenation does that, unchanged -
// so nothing here can alter what any mode sends, receives or answers.

// AggregatedToolCall is one tool call, reassembled from its chunks.
type AggregatedToolCall struct {
	// Index and ID are the protocol's own identifiers, carried through so a
	// caller can say which call it is talking about.
	Index *int
	ID    string
	// Name is the first non-empty function name seen for this call. Empty at
	// the end of a stream means the protocol never named it.
	Name string
	// Arguments is every argument fragment in arrival order. It is assembled
	// here because assembling it is how we know the fragments were tracked
	// correctly; it is never written into a record or an artefact.
	Arguments string
	// NameConflict is set when two different non-empty names arrived for the
	// same call. That is a protocol violation, not a routing choice.
	NameConflict bool
	// Chunks counts the fragments that fed this call.
	Chunks int
}

// Named reports whether the stream ever named this call.
func (c AggregatedToolCall) Named() bool { return strings.TrimSpace(c.Name) != "" }

// ToolCallAggregator reassembles tool calls across the chunks of one stream.
//
// One aggregator per stream. It is safe for the reader goroutine to feed it
// while another goroutine asks for the result, because a streamed response is
// read on one goroutine and inspected on another.
type ToolCallAggregator struct {
	mu    sync.Mutex
	order []string
	calls map[string]*AggregatedToolCall
	// anonymous counts chunks that carried neither an index nor an id. The
	// protocol gives nothing to group them by, so each becomes its own call
	// rather than being guessed into a neighbour's.
	anonymous int
}

// NewToolCallAggregator returns an aggregator for one stream.
func NewToolCallAggregator() *ToolCallAggregator {
	return &ToolCallAggregator{calls: map[string]*AggregatedToolCall{}}
}

// Observe feeds one streamed message chunk.
//
// The grouping key follows the protocol and never guesses:
//
//   - Index, when present, is what the protocol says identifies a call across
//     chunks, so it wins.
//   - Otherwise a non-empty ID groups the chunks, which is the only other
//     stable identifier a chunk carries.
//   - A chunk with neither becomes its own call. Eino's own concatenation does
//     the same with an index-less chunk, and the alternative - folding it into
//     whichever call came last - would invent a relationship the stream never
//     stated.
func (a *ToolCallAggregator) Observe(message *schema.Message) {
	if a == nil || message == nil || len(message.ToolCalls) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, chunk := range message.ToolCalls {
		key := ""
		switch {
		case chunk.Index != nil:
			key = fmt.Sprintf("index:%d", *chunk.Index)
		case strings.TrimSpace(chunk.ID) != "":
			key = "id:" + chunk.ID
		default:
			a.anonymous++
			key = fmt.Sprintf("anonymous:%d", a.anonymous)
		}
		call, known := a.calls[key]
		if !known {
			call = &AggregatedToolCall{Index: chunk.Index}
			a.calls[key] = call
			a.order = append(a.order, key)
		}
		call.Chunks++
		if chunk.ID != "" && call.ID == "" {
			call.ID = chunk.ID
		}
		// The first non-empty name fixes the call's identity. A later chunk
		// repeating the same name is ordinary; a later chunk carrying a
		// different one is a violation, and it is recorded rather than
		// silently resolved in either direction.
		if name := chunk.Function.Name; name != "" {
			switch {
			case call.Name == "":
				call.Name = name
			case call.Name != name:
				call.NameConflict = true
			}
		}
		// Arguments accumulate in arrival order. Never replaced, never
		// reordered, and never appended to a different call: the key above is
		// the only thing that decides where a fragment lands.
		if chunk.Function.Arguments != "" {
			call.Arguments += chunk.Function.Arguments
		}
	}
}

// Calls returns the reassembled calls in the order their first chunk arrived.
func (a *ToolCallAggregator) Calls() []AggregatedToolCall {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AggregatedToolCall, 0, len(a.order))
	for _, key := range a.order {
		out = append(out, *a.calls[key])
	}
	return out
}

// StreamToolCallProblem is one enumerated thing wrong with a stream's tool
// calls. It is a code, never a message, and it never carries an argument
// fragment, a question, an answer or anything a provider wrote.
type StreamToolCallProblem string

const (
	// StreamToolCallUnnamed is a call the stream never named.
	StreamToolCallUnnamed StreamToolCallProblem = "tool_call_never_named"
	// StreamToolCallNameConflict is a call given two different names.
	StreamToolCallNameConflict StreamToolCallProblem = "tool_call_name_conflict"
	// StreamToolCallUnregistered is an assembled call naming a tool that was
	// not on this turn's table.
	StreamToolCallUnregistered StreamToolCallProblem = "tool_call_not_registered"
)

// StreamToolCallReport is what a finished stream says about its tool calls.
type StreamToolCallReport struct {
	// Problems is the sorted, de-duplicated set of enumerated faults.
	Problems []StreamToolCallProblem
	// UnregisteredNames are the assembled names that were not on the table.
	// Tool names are method rather than data - this repository defines the
	// legitimate ones - so naming them is safe and is the only way a reader
	// can tell which contract was missed.
	UnregisteredNames []string
	// Calls is how many tool calls the stream assembled to.
	Calls int
}

// Ok reports whether the stream's tool calls were well formed and all on the
// table.
func (r StreamToolCallReport) Ok() bool { return len(r.Problems) == 0 }

// Validate checks the assembled calls against the tools this turn registered.
//
// The check happens here and nowhere earlier. That ordering is the whole point:
// a name is only a name once the stream has finished delivering it, and asking
// "is this registered" of a half-delivered call is asking about something that
// does not exist yet.
//
// An empty table means nothing was bound for this request, so there is nothing
// to be off: the registration check is skipped and only the protocol-level
// faults are reported.
func (a *ToolCallAggregator) Validate(registered []string) StreamToolCallReport {
	report := StreamToolCallReport{}
	table := make(map[string]struct{}, len(registered))
	for _, name := range registered {
		table[name] = struct{}{}
	}
	seen := map[StreamToolCallProblem]struct{}{}
	unregistered := map[string]struct{}{}
	for _, call := range a.Calls() {
		report.Calls++
		if call.NameConflict {
			seen[StreamToolCallNameConflict] = struct{}{}
		}
		if !call.Named() {
			seen[StreamToolCallUnnamed] = struct{}{}
			continue
		}
		if len(table) == 0 {
			continue
		}
		if _, ok := table[call.Name]; !ok {
			seen[StreamToolCallUnregistered] = struct{}{}
			unregistered[call.Name] = struct{}{}
		}
	}
	for problem := range seen {
		report.Problems = append(report.Problems, problem)
	}
	sort.Slice(report.Problems, func(i, j int) bool {
		return report.Problems[i] < report.Problems[j]
	})
	for name := range unregistered {
		report.UnregisteredNames = append(report.UnregisteredNames, name)
	}
	sort.Strings(report.UnregisteredNames)
	return report
}
