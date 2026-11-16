package conversation

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

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
}

// ReadShownMemory reads the cross-session memory out of the prompt the model
// was given.
//
// The prompt is the record: the product keeps the ids of the cards it showed
// in the window, but it does not persist them, so there is no other place to
// read this from. A prompt with no memory block is a prompt that carried no
// memory - which is a fact worth having, because an arm that was shown no
// memory measured nothing.
func ReadShownMemory(prompt []*schema.Message) ShownMemory {
	var joined strings.Builder
	for _, message := range prompt {
		if message == nil {
			continue
		}
		joined.WriteString(message.Content)
		joined.WriteString("\n")
	}
	text := joined.String()
	switch {
	case episodeBlockHeader.MatchString(text):
		return ShownMemory{Kind: "episode", MessageIDs: episodeBlockMessageIDs(text)}
	case cardBlockHeader.MatchString(text):
		_, sessions := ReadShownCards(prompt)
		return ShownMemory{Kind: "card", Sessions: sessions}
	default:
		return ShownMemory{Kind: "none"}
	}
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
