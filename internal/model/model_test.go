package model

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeFinishReasonCoversProviderSpellings(t *testing.T) {
	cases := map[string]FinishReason{
		"stop":           FinishStop,
		"STOP":           FinishStop,
		"end_turn":       FinishStop,
		"length":         FinishLength,
		"max_tokens":     FinishLength,
		"content_filter": FinishContentFilter,
		"sensitive":      FinishContentFilter,
		// A model that stopped to ask for tools is not "an answer that ended", and
		// the legacy spelling of the same outcome belongs in the same bucket.
		"tool_calls":    FinishToolCalls,
		"function_call": FinishToolCalls,
		"tool_use":      FinishToolCalls,
		"":              FinishOther,
		"  ":            FinishOther,
		"something-new": FinishOther,
	}
	for raw, want := range cases {
		if got := NormalizeFinishReason(raw); got != want {
			t.Fatalf("NormalizeFinishReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRequestValidateKeepsASystemMessageFirstAndAUserTurnLast(t *testing.T) {
	valid := Request{Messages: []Message{
		{Role: RoleSystem, Content: "policy"},
		{Role: RoleUser, Content: "问题"},
	}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a normal prompt was rejected: %v", err)
	}

	cases := map[string]Request{
		"empty":                {},
		"no user turn":         {Messages: []Message{{Role: RoleSystem, Content: "policy"}}},
		"system in the middle": {Messages: []Message{{Role: RoleUser, Content: "q"}, {Role: RoleSystem, Content: "p"}, {Role: RoleUser, Content: "q2"}}},
		"unknown role":         {Messages: []Message{{Role: "wizard", Content: "x"}, {Role: RoleUser, Content: "q"}}},
		"blank role":           {Messages: []Message{{Content: "q"}}},
		"negative temperature": {Messages: []Message{{Role: RoleUser, Content: "q"}}, Temperature: -0.1},
		"negative tokens":      {Messages: []Message{{Role: RoleUser, Content: "q"}}, MaxOutputTokens: -1},
	}
	for name, request := range cases {
		if err := request.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s error = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestLastUserContentPicksTheQuestion(t *testing.T) {
	if _, ok := (Request{}).LastUserContent(); ok {
		t.Fatal("an empty request reported a question")
	}
	request := Request{Messages: []Message{
		{Role: RoleUser, Content: "第一问"},
		{Role: RoleAssistant, Content: "第一答"},
		{Role: RoleUser, Content: "第二问"},
	}}
	content, ok := request.LastUserContent()
	if !ok || content != "第二问" {
		t.Fatalf("LastUserContent() = (%q, %v), want the newest user turn", content, ok)
	}
}

// TestToolRolesAndCallsValidate checks the shapes an agent loop produces. A tool result
// that is not tied to a call cannot be read back correctly by any provider, and a tool
// definition with no name cannot be offered at all.
func TestToolRolesAndCallsValidate(t *testing.T) {
	loop := Request{
		Messages: []Message{
			{Role: RoleSystem, Content: "policy"},
			{Role: RoleUser, Content: "问题"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "knowledge_search", Arguments: `{"query":"a"}`}}},
			{Role: RoleTool, ToolCallID: "call-1", Content: "[工具结果] …"},
		},
		Tools: []ToolDefinition{{Name: "knowledge_search", Parameters: []byte(`{"type":"object"}`)}},
	}
	if err := loop.Validate(); err != nil {
		t.Fatalf("a complete agent transcript was rejected: %v", err)
	}

	orphaned := loop
	orphaned.Messages[3] = Message{Role: RoleTool, Content: "没有对应的调用"}
	if err := orphaned.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unattributed tool result error = %v, want ErrInvalidRequest", err)
	}

	nameless := Request{
		Messages: []Message{{Role: RoleUser, Content: "q"}},
		Tools:    []ToolDefinition{{Name: "  "}},
	}
	if err := nameless.Validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nameless tool error = %v, want ErrInvalidRequest", err)
	}
}

// noToolModel is an implementation that predates tool calling, which is exactly what
// the optional interface has to tolerate.
type noToolModel struct{}

func (noToolModel) Complete(context.Context, Request) (Completion, error) { return Completion{}, nil }

func (noToolModel) Stream(context.Context, Request, DeltaHandler) (Completion, error) {
	return Completion{}, nil
}

// TestToolSupportIsAnOptionalFaceOnChatModel keeps the extension honest: adding tools
// must not force every existing ChatModel - a test double, a wrapper, a future
// provider without tool support - to implement a method it cannot honour.
func TestToolSupportIsAnOptionalFaceOnChatModel(t *testing.T) {
	var plain ChatModel = noToolModel{}
	if _, ok := plain.(ToolChatModel); ok {
		t.Fatal("ChatModel now demands tool support, which breaks every existing implementation")
	}
	var offered ToolChatModel = toolCapableModel{}
	if _, ok := interface{}(offered).(ChatModel); !ok {
		t.Fatal("a tool model is no longer usable where a ChatModel is expected")
	}
}

type toolCapableModel struct{ noToolModel }

func (toolCapableModel) CompleteWithTools(context.Context, Request) (Completion, error) {
	return Completion{}, nil
}
