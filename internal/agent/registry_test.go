package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

func TestRegistryRejectsASetItCannotNameUnambiguously(t *testing.T) {
	first := &recordingTool{name: "knowledge_search"}
	if _, err := NewRegistry(first, &recordingTool{name: "knowledge_search"}); err == nil {
		t.Fatal("two tools answering to one name were accepted")
	}
	if _, err := NewRegistry(nil); err == nil {
		t.Fatal("a nil tool was accepted")
	}
	if _, err := NewRegistry(&recordingTool{name: "   "}); err == nil {
		t.Fatal("a tool with no name was accepted")
	}
	if _, err := NewRegistry(&brokenSchemaTool{}); err == nil {
		t.Fatal("a tool with an unparsable parameter schema was accepted")
	}
	registry, err := NewRegistry(first)
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Names(); len(got) != 1 || got[0] != "knowledge_search" {
		t.Fatalf("Names = %v", got)
	}
	definitions := registry.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "knowledge_search" || !json.Valid(definitions[0].Parameters) {
		t.Fatalf("definitions = %+v", definitions)
	}
}

type brokenSchemaTool struct{}

func (brokenSchemaTool) Name() string        { return "broken" }
func (brokenSchemaTool) Description() string { return "no valid schema" }
func (brokenSchemaTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object",`)
}
func (brokenSchemaTool) Execute(context.Context, Principal, json.RawMessage) (Observation, error) {
	return Observation{}, errors.New("never reached")
}

func TestRegistryRunRefusesNamesAndEnvelopes(t *testing.T) {
	tool := &recordingTool{name: "knowledge_search", observations: []Observation{{Text: "资料", Found: true}}}
	registry, err := NewRegistry(tool)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{UserID: "user-owner"}

	if _, err := registry.run(context.Background(), principal, model.ToolCall{ID: "a", Name: "delete_all"}); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("unknown tool error = %v", err)
	}
	// A name that only differs by surrounding space is the same tool: providers pad
	// ids and names, and refusing on whitespace would be a mystery failure.
	if _, err := registry.run(context.Background(), principal,
		model.ToolCall{ID: "a", Name: " knowledge_search ", Arguments: `{"query":"x"}`}); err != nil {
		t.Fatalf("padded name refused: %v", err)
	}
	for _, raw := range []string{``, `null`, `[1]`, `"text"`, `{`, `{"a":1} trailing`} {
		if _, err := registry.run(context.Background(), principal,
			model.ToolCall{ID: "a", Name: "knowledge_search", Arguments: raw}); !errors.Is(err, ErrInvalidArguments) {
			t.Fatalf("envelope %q produced %v, want a refusal", raw, err)
		}
	}
	// Oversized arguments are refused before any tool parses them.
	huge := `{"query":"` + strings.Repeat("字", maxArgumentsBytes) + `"}`
	if _, err := registry.run(context.Background(), principal,
		model.ToolCall{ID: "a", Name: "knowledge_search", Arguments: huge}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("oversized arguments produced %v", err)
	}
	// A tool that fails is reported as a tool failure that keeps the tool's own reason.
	failing := &recordingTool{name: "failing", failWith: ErrNoPrincipal}
	failingRegistry, err := NewRegistry(failing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := failingRegistry.run(context.Background(), principal,
		model.ToolCall{ID: "a", Name: "failing", Arguments: `{}`}); !errors.Is(err, ErrToolExecution) || !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("error = %v, want a tool failure that still names the cause", err)
	}
}

func TestRequireJSONObjectShapes(t *testing.T) {
	if err := requireJSONObject(`{}`); err != nil {
		t.Fatalf("an empty object is a well-formed envelope: %v", err)
	}
	if err := requireJSONObject(`{"a":1}`); err != nil {
		t.Fatalf("a normal object refused: %v", err)
	}
}

func TestDecodeArgumentsIsStrict(t *testing.T) {
	type target struct {
		Query string `json:"query"`
	}
	var parsed target
	if err := decodeArguments(json.RawMessage(`{"query":"aof"}`), &parsed); err != nil || parsed.Query != "aof" {
		t.Fatalf("decode = %+v err %v", parsed, err)
	}
	if err := decodeArguments(json.RawMessage(`{"query":"aof","extra":1}`), &target{}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("unknown key error = %v, want a refusal", err)
	}
	if err := decodeArguments(json.RawMessage(`{"query":"aof"} {"query":"second"}`), &target{}); !errors.Is(err, ErrInvalidArguments) {
		t.Fatalf("trailing object error = %v, want a refusal", err)
	}
}
