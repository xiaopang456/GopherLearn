package conversation

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// The tool table the answer's model was offered, and the memory block it was
// shown.
//
// Two facts a comparison needs and that nothing else in the attempt records:
// the tools the two arms were given differ, and the memory block that was
// injected differs. Both are read out of the request that was actually sent,
// because that request is the record of what the model was shown.

// toolNamePattern reads a tool name out of an OpenAI-shaped tool entry.
var toolNamePattern = regexp.MustCompile(`"name"\s*:\s*"([^"]+)"`)

// ToolNamesFromBody reads the tool table out of a chat request body.
//
// The body is the record: the harness forwards it to the provider unchanged,
// so what it says the model was offered is what the model was offered. The
// names are read out of the raw body rather than out of a parsed struct
// because the shape of the body belongs to the provider, not to the harness.
func ToolNamesFromBody(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var request struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			// Some providers put the name at the top level of the entry.
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	var names []string
	for _, tool := range request.Tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			name = strings.TrimSpace(tool.Name)
		}
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolNamesFromInfos reads the tool table out of the tool list the product
// bound to the model.
//
// This is the in-process path: the product hands the model its tools before
// the call, so the names are known here even when the request body is not
// available.
func ToolNamesFromInfos(tools []*schema.ToolInfo) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AnswerToolTable reads the tool table the answer's model was offered.
//
// Only the answer's own calls count: the tools of the calls that wrote a
// memory are the tools of that path, not of the answer. The table of the last
// answer call is the one that was in front of the model when it answered.
func AnswerToolTable(calls []ModelCall) []string {
	var table []string
	for _, call := range calls {
		if call.Role != "answer" {
			continue
		}
		if len(call.ToolTable) == 0 {
			continue
		}
		table = call.ToolTable
	}
	return table
}

// The states a tool can be in.
//
// They are kept apart because they are three different findings: a tool the
// product never registered cannot be called at all, a tool the product
// registered but did not offer in this request cannot be called in this
// request, and a tool that was offered and not called is a fact about the
// answer, not about the wiring. A reader who collapses them reads "the model
// did not use the tool" into a sentence that is about the product.
const (
	// ToolStateOfferedCalled says the request carried the tool and the answer
	// called it.
	ToolStateOfferedCalled = "offered_called"
	// ToolStateOfferedNotCalled says the request carried the tool and the
	// answer did not call it.
	ToolStateOfferedNotCalled = "offered_not_called"
	// ToolStateRegisteredNotOffered says the product registers the tool for
	// this arm and this request did not carry it.
	ToolStateRegisteredNotOffered = "registered_not_offered"
	// ToolStateNotRegistered says the product does not register the tool for
	// this arm, so it could not have been called.
	ToolStateNotRegistered = "not_registered"
	// ToolStateCalledWithoutOffering says the answer called a tool that the
	// recorded request did not carry. The harness's view of the request was
	// incomplete, and the evidence says so instead of guessing.
	ToolStateCalledWithoutOffering = "called_without_offering"
)

// ToolUsageEntry says what happened to one tool.
type ToolUsageEntry struct {
	// Name is the tool.
	Name string `json:"name"`
	// State is one of the five states above. It is the row's conclusion.
	State string `json:"state"`
	// Registered says the product registers the tool for this arm.
	//
	// It is established two ways, and the harness says which one it used by
	// the state it reports: a tool the request carried is registered, because
	// the product cannot offer a tool it does not have; the tools the product
	// registers but did not offer are established by the product's own
	// registration rule, which the harness mirrors for the one tool whose
	// registration differs between the two arms.
	Registered bool `json:"registered"`
	// Offered says the request carried the tool's schema.
	Offered bool `json:"offered"`
	// Called says the answer actually called it.
	Called bool `json:"called"`
	// Calls is how many times it was called.
	Calls int `json:"calls"`
}

// ToolUsageInput is what the harness knows about the tools of a request.
type ToolUsageInput struct {
	// Offered names the tools the request carried.
	Offered []string
	// Registered names the tools the product registers for this arm and this
	// request did not carry.
	Registered []string
	// Compared names the tools the comparison asks about, whether or not this
	// arm touched them. It is how "the old path does not register this tool"
	// becomes a row of the evidence instead of an absence nobody can see.
	Compared []string
	// Steps are the answer's tool rounds.
	Steps []model.AgentStep
}

// ToolUsage reads what happened to every tool the comparison asks about.
//
// The three states are told apart as far as the harness can tell them. The
// product's registry is assembled inside its own internal wiring, which a
// harness in another module cannot import, so the registration of a tool that
// was not offered is established by the mirrored registration rule. That rule
// covers the one tool whose registration differs between the two arms; a tool
// that neither arm offered is simply not a row, because nothing the harness
// saw says anything about it.
func ToolUsage(input ToolUsageInput) []ToolUsageEntry {
	calls := map[string]int{}
	for _, step := range input.Steps {
		if step.Type != model.AgentStepTool {
			continue
		}
		calls[step.ToolName]++
	}
	offered := map[string]bool{}
	for _, name := range input.Offered {
		if name = strings.TrimSpace(name); name != "" {
			offered[name] = true
		}
	}
	registered := map[string]bool{}
	for _, name := range input.Registered {
		if name = strings.TrimSpace(name); name != "" {
			registered[name] = true
		}
	}
	seen := map[string]struct{}{}
	var out []ToolUsageEntry
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, exists := seen[name]; exists {
			return
		}
		seen[name] = struct{}{}
		entry := ToolUsageEntry{
			Name: name,
			// A tool the request carried is registered: the product cannot
			// offer a tool it does not have.
			Registered: registered[name] || offered[name],
			Offered:    offered[name],
			Called:     calls[name] > 0,
			Calls:      calls[name],
		}
		switch {
		case entry.Offered && entry.Called:
			entry.State = ToolStateOfferedCalled
		case entry.Offered:
			entry.State = ToolStateOfferedNotCalled
		case entry.Called:
			entry.State = ToolStateCalledWithoutOffering
		case entry.Registered:
			entry.State = ToolStateRegisteredNotOffered
		default:
			entry.State = ToolStateNotRegistered
		}
		out = append(out, entry)
	}
	for _, name := range input.Offered {
		add(name)
	}
	for _, name := range input.Compared {
		add(name)
	}
	for name := range calls {
		add(name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AnswerShownMemory reads the cross-session memory the answer was given.
//
// The last answer request is the one that was in front of the model when it
// answered, the same rule the tool table follows.
func AnswerShownMemory(calls []ModelCall) ShownMemory {
	shown := ShownMemory{}
	for _, call := range calls {
		if call.Role != "answer" {
			continue
		}
		if call.ShownMemoryKind == "" {
			continue
		}
		shown = ShownMemory{
			Kind:       call.ShownMemoryKind,
			Sessions:   call.CardSessions,
			MessageIDs: call.InjectedMessageIDs,
			Text:       call.InjectedText,
		}
	}
	return shown
}

// The memory block the product puts into the window.
//
// The card block names the conversation it was written from, and the Episode
// block does not: it carries the time, the title, the topics and the
// assertions, with the message ids of the source. So the two blocks are told
// apart by their own headers, and the sessions of an Episode block are read
// from the rows the answer was given rather than from the text.
var (
	cardBlockHeader    = regexp.MustCompile(`(?i)session cards?`)
	episodeBlockHeader = regexp.MustCompile(`(?i)cross-session episodes`)
)

// ShownMemory is what the prompt carried of the cross-session memory.
type ShownMemory struct {
	// Kind is "card", "episode" or "none".
	Kind string `json:"kind"`
	// Sessions are the conversations the card block named. An Episode block
	// names none, and the rows of the answer say which conversations it was
	// about instead.
	Sessions []string `json:"sessions,omitempty"`
	// MessageIDs are the message ids the Episode block cited.
	MessageIDs []uint `json:"message_ids,omitempty"`
	// Text is the injected block itself, truncated.
	Text string `json:"text,omitempty"`
}

// ReadShownMemory reads the cross-session memory out of the prompt the model
// was given.
//
// The prompt is the record: the product keeps the ids of the cards it showed
// in the window, but it does not persist them, so there is no other place to
// read this from. A prompt with no memory block is a prompt that carried no
// memory - which is a fact worth having, because an arm that was shown no
// memory measured nothing.
//
// The text of the block is kept as well, truncated: what the answer was given
// is the difference between "the memory was there" and "the memory was
// there and said this".
//
// Only the system messages are scanned, and only one of them carries the
// block. The product sends the block as its own system message
// (v2/service/agent/context_builder.go:1927-1930), so a question that happens
// to quote the header cannot be mistaken for the memory, and the text that
// follows the block in the prompt - the learner's own question - cannot be
// read as part of it.
func ReadShownMemory(prompt []*schema.Message) ShownMemory {
	for _, message := range prompt {
		if message == nil || message.Role != schema.System {
			continue
		}
		text := message.Content
		switch {
		case episodeBlockHeader.MatchString(text):
			return ShownMemory{
				Kind:       "episode",
				MessageIDs: episodeBlockMessageIDs(text),
				Text:       truncateMemoryText(extractMemoryBlock(text)),
			}
		case cardBlockHeader.MatchString(text):
			_, sessions := ReadShownCards([]*schema.Message{message})
			return ShownMemory{
				Kind: "card",
				// The card names the conversation it came from, and it cites
				// the messages it was written from. Both are kept: the name is
				// the card's own claim and the ids are what the harness can
				// check against the database.
				Sessions:   sessions,
				MessageIDs: episodeBlockMessageIDs(text),
				Text:       truncateMemoryText(extractMemoryBlock(text)),
			}
		}
	}
	return ShownMemory{Kind: "none"}
}

// memoryBlockLimit is how much of the injected block the evidence keeps.
const memoryBlockLimit = 1200

func truncateMemoryText(text string) string {
	runes := []rune(text)
	if len(runes) <= memoryBlockLimit {
		return text
	}
	return string(runes[:memoryBlockLimit]) + "…"
}

// extractMemoryBlock returns the part of the prompt that carries the
// cross-session memory.
//
// The markers are the headers the product itself writes, so the block is cut
// where the product put it rather than at a phrase that might occur in the
// conversation.
func extractMemoryBlock(text string) string {
	markers := []string{
		"Relevant cross-session episodes",
		"Relevant session cards",
	}
	start := -1
	for _, marker := range markers {
		if at := strings.Index(text, marker); at >= 0 && (start < 0 || at < start) {
			start = at
		}
	}
	if start < 0 {
		return ""
	}
	return text[start:]
}

// episodeBlockMessageIDs reads the message ids an Episode block cited.
//
// The block cites them as "[message_ids=12,13]", and they are the ids of the
// source conversation's messages: they say which conversation the memory was
// written from, once the rows are read back.
func episodeBlockMessageIDs(text string) []uint {
	seen := map[uint]struct{}{}
	var ids []uint
	for _, match := range regexp.MustCompile(`message_ids=([0-9,]+)`).FindAllStringSubmatch(text, -1) {
		if len(match) != 2 {
			continue
		}
		for _, field := range strings.Split(match[1], ",") {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			value := 0
			for _, digit := range field {
				if digit < '0' || digit > '9' {
					value = 0
					break
				}
				value = value*10 + int(digit-'0')
			}
			if value == 0 {
				continue
			}
			if _, exists := seen[uint(value)]; exists {
				continue
			}
			seen[uint(value)] = struct{}{}
			ids = append(ids, uint(value))
		}
	}
	return ids
}
