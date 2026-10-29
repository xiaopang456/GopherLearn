// Package contextwindow decides how much a single model request may carry.
//
// It exists because the pieces of a prompt are produced by different owners - the
// service trims history, the summarizer writes a summary, a tool returns whatever
// its search found - and none of them can see the others' totals. A package that
// only the assembly point reads would therefore be a number nobody could verify,
// so every entry here is a budget rule plus the measurement it applies to.
//
// Two units live side by side on purpose. Runes are what this project could always
// count, and they still bound the chat and knowledge strategies. Estimated tokens
// are what a token budget is expressed in, and they bound a learning-agent turn.
// The two are not interchangeable: a thousand Chinese runes estimate as a thousand
// tokens while a thousand English runes estimate as roughly two hundred and fifty,
// so a figure is only comparable with others measured the same way.
package contextwindow

import (
	"unicode"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// MessageEnvelopeTokens is the framing cost every message carries on top of its
// content: a role, a separator, and whatever the provider's own serialization adds.
// It is an estimate of overhead, not a measured vendor figure, but omitting it makes
// a long conversation look cheaper than it is, so every message is charged.
const MessageEnvelopeTokens = 4

// ToolEnvelopeTokens frames one tool definition. A tool call's own framing is charged
// with the message that carries it, in RequestTokens.
const ToolEnvelopeTokens = 8

// Tokens is a deterministic, model-independent budget estimate: CJK characters and
// punctuation count one each, an ASCII word counts one per four characters rounded
// up, a newline counts one, and a run of other whitespace counts one.
//
// It is not a model tokenizer, and it is not the provider's count. Local estimates
// decide what this process is willing to send; the provider's usage report says what
// actually arrived, and the two are recorded separately.
func Tokens(text string) int {
	runes := []rune(text)
	tokens := 0
	for index := 0; index < len(runes); {
		current := runes[index]
		switch {
		case current == '\n':
			tokens++
			index++
		case unicode.IsSpace(current):
			tokens++
			for index < len(runes) && runes[index] != '\n' && unicode.IsSpace(runes[index]) {
				index++
			}
		case current <= unicode.MaxASCII && (unicode.IsLetter(current) || unicode.IsDigit(current) || current == '_'):
			start := index
			for index < len(runes) {
				next := runes[index]
				if next > unicode.MaxASCII || (!unicode.IsLetter(next) && !unicode.IsDigit(next) && next != '_') {
					break
				}
				index++
			}
			tokens += (index - start + 3) / 4
		default:
			tokens++
			index++
		}
	}
	return tokens
}

// MessageTokens prices one message the way a request will carry it.
func MessageTokens(content string) int {
	return Tokens(content) + MessageEnvelopeTokens
}

// ToolDefinitionTokens prices the tool offers sent with every call of a turn. They are
// server-authored and constant across a turn, which is exactly why they were easy to
// leave out of a budget: the omission repeats itself on every single call.
func ToolDefinitionTokens(definitions []model.ToolDefinition) int {
	total := 0
	for _, definition := range definitions {
		total += Tokens(definition.Name) + Tokens(definition.Description) +
			Tokens(string(definition.Parameters)) + ToolEnvelopeTokens
	}
	return total
}

// RequestTokens estimates the whole outgoing request: every message, the tool calls
// an assistant message carries, and the tool definitions offered with it.
//
// Everything is measured, not sampled. A turn that has already run tools holds their
// observations in the message list, so a check that only looked at the newest message
// would keep passing while the request grew past the window.
func RequestTokens(request model.Request) int {
	total := 0
	for _, message := range request.Messages {
		total += MessageTokens(message.Content)
		for _, call := range message.ToolCalls {
			// The call's name and id travel back to the provider too, and the
			// arguments are model text whose length nobody bounded.
			total += Tokens(call.Name) + Tokens(call.ID) + Tokens(call.Arguments) +
				MessageEnvelopeTokens
		}
	}
	total += ToolDefinitionTokens(request.Tools)
	return total
}

// PrefixWithinTokens keeps the longest rune prefix whose estimate fits maxTokens,
// plus the ellipsis that says something was removed. It returns the original text
// when nothing needs to go.
//
// Only derived material is ever cut with this. Stored conversation is not: a
// half-message in a window reads as a broken exchange, which is why the window keeps
// whole user to assistant pairs instead.
func PrefixWithinTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	runes := []rune(text)
	if Tokens(text) <= maxTokens {
		return text
	}
	// The ellipsis is part of what gets sent, so it is paid for out of the same
	// budget rather than added on top of it.
	available := maxTokens - 1
	if available < 0 {
		return ""
	}
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if Tokens(string(runes[:middle])) <= available {
			low = middle
			continue
		}
		high = middle - 1
	}
	return string(runes[:low]) + "…"
}
