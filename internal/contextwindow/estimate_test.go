package contextwindow

import (
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// The estimator is the root of every number this package reports, so its edges are
// pinned with hand-counted cases rather than a reference implementation: a rule that
// is only checked against itself proves nothing.

func TestTokensCountsEachScriptAtItsOwnRate(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"cjk one per character", "虚拟内存", 4},
		{"cjk punctuation counts too", "虚拟内存。", 5},
		{"ascii word four characters per token", "abcd", 1},
		{"ascii word rounds up", "abcde", 2},
		{"two ascii words plus their space", "hello world", 5},
		{"newline is its own token", "a\nb", 3},
		{"a run of spaces collapses to one", "a   b", 3},
		{"code pays for its operators", "func main() {}", 8},
		{"json braces count individually", `{"query":"记忆"}`, 11},
		{"digits group with letters", "a1b2c3d4", 2},
		{"underscore is part of a word", "max_tokens", 3},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Tokens(testCase.text); got != testCase.want {
				t.Fatalf("Tokens(%q) = %d, want %d", testCase.text, got, testCase.want)
			}
		})
	}
}

// A long JSON observation is the shape that used to be measured in runes and silently
// under-counted for Chinese. The scaling here is what makes that visible: the same
// rune length in two scripts is not the same budget.
func TestTokensSeparatesRunesFromEstimatedTokens(t *testing.T) {
	chinese := strings.Repeat("记", 400)
	english := strings.Repeat("word ", 80)
	if len([]rune(chinese)) != len([]rune(english)) {
		t.Fatalf("the two samples must be the same length in runes to compare")
	}
	if Tokens(chinese) <= Tokens(english) {
		t.Fatalf("a CJK run must estimate higher than the same rune count of english: %d vs %d",
			Tokens(chinese), Tokens(english))
	}
	if Tokens(chinese) != 400 {
		t.Fatalf("CJK estimate = %d, want one token per character", Tokens(chinese))
	}
}

func TestMessageTokensAlwaysPaysTheEnvelope(t *testing.T) {
	if got := MessageTokens(""); got != MessageEnvelopeTokens {
		t.Fatalf("an empty message cost %d, want the envelope alone at %d", got, MessageEnvelopeTokens)
	}
	if got := MessageTokens("你好"); got != 2+MessageEnvelopeTokens {
		t.Fatalf("cost = %d, want content plus envelope", got)
	}
}

func TestRequestTokensCoversToolsAndCalls(t *testing.T) {
	request := model.Request{
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "规则"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{
				{ID: "call-1", Name: "knowledge_search", Arguments: `{"query":"持久化"}`},
			}},
			{Role: model.RoleTool, ToolCallID: "call-1", Content: "资料正文"},
		},
		Tools: []model.ToolDefinition{
			{Name: "knowledge_search", Description: "检索资料", Parameters: []byte(`{"type":"object"}`)},
		},
	}
	got := RequestTokens(request)
	messagesOnly := 0
	for _, message := range request.Messages {
		messagesOnly += MessageTokens(message.Content)
	}
	if got <= messagesOnly {
		t.Fatalf("the request cost %d is no more than its message bodies %d: tools and calls are not being measured",
			got, messagesOnly)
	}
	if got < ToolDefinitionTokens(request.Tools) {
		t.Fatalf("the request cost %d is below the tool offers alone %d", got, ToolDefinitionTokens(request.Tools))
	}
}

func TestPrefixWithinTokensPaysForItsOwnEllipsis(t *testing.T) {
	text := strings.Repeat("记", 100)
	cut := PrefixWithinTokens(text, 10)
	if Tokens(cut) > 10 {
		t.Fatalf("the cut block itself exceeds the budget: %d tokens in %q", Tokens(cut), cut)
	}
	if !strings.HasSuffix(cut, "…") {
		t.Fatalf("a shortened block does not say it was shortened: %q", cut)
	}
	if PrefixWithinTokens(text, 200) != text {
		t.Fatal("a block that already fits was rewritten")
	}
	if got := PrefixWithinTokens(text, 0); got != "" {
		t.Fatalf("a zero budget returned %q, want nothing", got)
	}
	// One token of budget is entirely consumed by the ellipsis; the function must not
	// return a marker that overstates what it carries.
	if got := PrefixWithinTokens(text, 1); Tokens(got) > 1 {
		t.Fatalf("budget of one token produced %d: %q", Tokens(got), got)
	}
}
