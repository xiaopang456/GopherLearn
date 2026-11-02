package aihelper

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// The fuzz target is the patch itself, with the request bodies the other
// tests in this package use as its seeds.
//
// Three things have to hold for every input:
//
//   - a body that is valid JSON stays a valid JSON body;
//   - a body that needs no field comes out byte for byte;
//   - a body that needed a field comes out with nothing changed but the
//     added fields, each of them an empty reasoning_content.
//
// The fuzzer runs the seeds in the ordinary test run, so the ordinary run
// covers the same ground without needing the fuzzing engine.
func FuzzPatchMissingReasoningContent(f *testing.F) {
	for _, seed := range []string{
		`{"messages":[{"role":"assistant","content":"hi","tool_calls":[{"type":"function"}]}],"model":"m"}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":"I looked it up"}],"model":"m"}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":null}],"model":"m"}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"type":"function"}],"reasoning_content":""}],"model":"m"}`,
		`{"messages":[{"role":"assistant","content":"hi"}],"model":"m"}`,
		`{"messages":[{"role":"tool","tool_call_id":"call_1","content":"result"}],"model":"m"}`,
		`{"messages":[{"role":"user","content":"q"},{"role":"assistant","tool_calls":[{"type":"function"}]},{"role":"tool","tool_call_id":"call_1","content":"r"}],"model":"m"}`,
		`{"messages":[{"role":"assistant","tool_calls":null}],"model":"m"}`,
		`{"model":"m","max_tokens":1.50,"messages":[{"role":"user","content":"a \"quoted\" brace } and a backslash \\"}],"stream":false}`,
		`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"u"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"knowledge_search","arguments":"{\"query\":\"go\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"r"}],"model":"deepseek-flash","stream":true,"thinking":{"type":"enabled"},"tool_choice":"auto","tools":[{"type":"function","function":{"name":"knowledge_search","parameters":{"type":"object"}}}]}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		patched, err := patchMissingReasoningContent(input)
		if err != nil {
			// The patch gave up. The bytes that come back are the original
			// ones, because that is what may be sent.
			if !bytes.Equal(patched, input) {
				t.Fatalf("a failed patch must hand back the original body\n got: %s\nwant: %s", patched, input)
			}
			return
		}
		if !json.Valid(input) {
			// The patch reads bytes rather than a document, so it can work on
			// a body that is not JSON. There is nothing to compare it with.
			return
		}
		if !json.Valid(patched) {
			t.Fatalf("the patched body is not valid json:\n%s", patched)
		}

		var before, after any
		if err := json.Unmarshal(input, &before); err != nil {
			t.Fatalf("read the original body: %v", err)
		}
		if err := json.Unmarshal(patched, &after); err != nil {
			t.Fatalf("read the patched body: %v", err)
		}
		stripped, added := stripAddedReasoningContent(t, after, before)
		if added == 0 {
			if !bytes.Equal(patched, input) {
				t.Fatalf("no turn needed the field, but the body changed\n got: %s\nwant: %s", patched, input)
			}
			return
		}
		if !reflect.DeepEqual(stripped, before) {
			t.Fatalf("the patch changed more than the added fields\n got: %s\nwant: %s", patched, input)
		}
	})
}

// stripAddedReasoningContent takes the patched body and the original one,
// removes the fields the patch added, and reports how many there were.
//
// Everything else has to be the same in both, which is what the caller
// checks.
func stripAddedReasoningContent(t *testing.T, after, before any) (any, int) {
	t.Helper()
	afterMap, ok := after.(map[string]any)
	if !ok {
		return after, 0
	}
	beforeMap, ok := before.(map[string]any)
	if !ok {
		return after, 0
	}
	afterMessages, ok := afterMap["messages"].([]any)
	if !ok {
		return after, 0
	}
	beforeMessages, _ := beforeMap["messages"].([]any)
	if len(beforeMessages) != len(afterMessages) {
		t.Fatalf("the patch changed the number of turns: %d became %d", len(beforeMessages), len(afterMessages))
	}

	stripped := make(map[string]any, len(afterMap))
	for key, value := range afterMap {
		stripped[key] = value
	}
	turns := make([]any, len(afterMessages))
	added := 0
	for i, turn := range afterMessages {
		message, ok := turn.(map[string]any)
		if !ok {
			turns[i] = turn
			continue
		}
		copyMessage := make(map[string]any, len(message))
		for key, value := range message {
			copyMessage[key] = value
		}
		value, has := copyMessage["reasoning_content"]
		hadBefore := false
		if i < len(beforeMessages) {
			if beforeMessage, ok := beforeMessages[i].(map[string]any); ok {
				_, hadBefore = beforeMessage["reasoning_content"]
			}
		}
		if has && !hadBefore {
			text, ok := value.(string)
			if !ok || text != "" {
				t.Fatalf("the added field is %v, want an empty string", value)
			}
			delete(copyMessage, "reasoning_content")
			added++
		}
		turns[i] = copyMessage
	}
	stripped["messages"] = turns
	return stripped, added
}
