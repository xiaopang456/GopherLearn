package hybrid

import (
	"encoding/json"
	"strings"
	"testing"
)

// A conversation turn now runs on the shared path, and that path puts this turn's
// identity in every frame: `run_id` on each event, `original_v2.runId` inside the
// completed frame, and the persisted row id beside them. Those are identities, not
// contract, so a byte-for-byte golden would be decided by a random uuid.
//
// The normalizer replaces exactly those fields and nothing else. It is deliberately
// not a uuid sweep: an id-shaped string inside an answer, a title or a citation is
// content, and replacing it would let a change to the content pass unnoticed. The
// tests below pin that distinction, because a normalizer that over-replaces turns a
// golden into a shape check that cannot fail.

func normalizeSSE(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		lines := strings.Split(frame, "\n")
		out.WriteString(lines[0] + "\n")
		for _, line := range lines[1:] {
			if !strings.HasPrefix(line, "data: ") {
				out.WriteString(line + "\n")
				continue
			}
			var value any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
				out.WriteString(line + "\n")
				continue
			}
			// Without this the placeholders come back as \u003crun_id\u003e, which is
			// what encoding/json does to angle brackets by default.
			var encoded strings.Builder
			encoder := json.NewEncoder(&encoded)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(normalizeFrameIdentities(value)); err != nil {
				t.Fatalf("normalize sse frame: %v", err)
			}
			out.WriteString("data: " + strings.TrimRight(encoded.String(), "\n") + "\n")
		}
		out.WriteString("\n")
	}
	return out.String()
}

// normalizeFrameIdentities replaces this turn's identities with placeholders and
// reduces original_v2 to its structure.
//
// original_v2 carries the whole RunResult, contextBudget included. Its numbers are
// what the budget tests are for, and the assembly those numbers come from is still
// being changed by other work, so the golden compares the field names and the nesting
// and leaves the values to the tests that can say what a value should be.
func normalizeFrameIdentities(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			switch key {
			case "run_id", "runId":
				normalized[key] = "<run_id>"
			case "message_id", "messageId":
				normalized[key] = "<message_id>"
			case "created_at", "createdAt", "updated_at", "updatedAt":
				normalized[key] = "<timestamp>"
			case "original_v2":
				normalized[key] = structureOf(key, item)
			default:
				normalized[key] = normalizeFrameIdentities(item)
			}
		}
		return normalized
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, normalizeFrameIdentities(item))
		}
		return items
	default:
		return value
	}
}

// structureOf keeps the names and the nesting of a value and drops what it holds. An
// empty array and a non-empty one stay different, because "this turn cited nothing"
// is a fact about the turn and not about a number.
//
// An identity inside the structure stays a named placeholder rather than becoming
// "<string>": the golden has to be able to say that original_v2 names the same run as
// the frame around it, which is one of the checks this normalizer exists for.
func structureOf(key string, value any) any {
	switch key {
	case "run_id", "runId":
		return "<run_id>"
	case "message_id", "messageId":
		return "<message_id>"
	case "created_at", "createdAt", "updated_at", "updatedAt":
		return "<timestamp>"
	}
	switch typed := value.(type) {
	case map[string]any:
		structure := make(map[string]any, len(typed))
		for inner, item := range typed {
			structure[inner] = structureOf(inner, item)
		}
		return structure
	case []any:
		if len(typed) == 0 {
			return []any{}
		}
		return []any{"<array>"}
	case float64:
		return "<number>"
	case string:
		return "<string>"
	case bool:
		return "<bool>"
	default:
		return value
	}
}

func TestNormalizerReplacesIdentitiesAndNothingElse(t *testing.T) {
	frame := `event: message.completed
data: {"run_id":"1ee0e562-0b9c-4d83-b50e-647a2db0857f","message_id":"2","content":"回答里也有一个 1ee0e562-0b9c-4d83-b50e-647a2db0857f","publication":"validated_snapshot","original_v2":{"runId":"1ee0e562-0b9c-4d83-b50e-647a2db0857f","answer":"a","modelCalls":1,"sources":[],"contextBudget":{"system":325,"assembled":331},"usedTopicKeys":null}}

`
	got := normalizeSSE(t, frame)
	for _, want := range []string{
		`"run_id":"<run_id>"`,
		`"message_id":"<message_id>"`,
		// The id inside the answer text is content, and must survive.
		`"content":"回答里也有一个 1ee0e562-0b9c-4d83-b50e-647a2db0857f"`,
		`"publication":"validated_snapshot"`,
		// original_v2 is reduced to names and nesting, so its numbers are gone...
		`"modelCalls":"<number>"`,
		`"contextBudget":{"assembled":"<number>","system":"<number>"}`,
		`"sources":[]`,
		// ...while its own run id is named rather than compared.
		`"runId":"<run_id>"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized frame lost %s\ngot: %s", want, got)
		}
	}
	for _, unwanted := range []string{`"modelCalls":1`, `"system":325`, `"assembled":331`} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("a number inside original_v2 is still being compared: %s", unwanted)
		}
	}
	if strings.Count(got, "<run_id>") != 2 {
		t.Fatalf("expected the frame's run_id and original_v2's runId, got %s", got)
	}
}

func TestNormalizerKeepsEmptyAndNonEmptyCollectionsApart(t *testing.T) {
	empty := normalizeSSE(t, "event: message.completed\ndata: {\"original_v2\":{\"sources\":[]}}\n\n")
	filled := normalizeSSE(t, "event: message.completed\ndata: {\"original_v2\":{\"sources\":[{\"citation\":1}]}}\n\n")
	if !strings.Contains(empty, `"sources":[]`) {
		t.Fatalf("an empty collection was flattened: %s", empty)
	}
	if !strings.Contains(filled, `"sources":["<array>"]`) {
		t.Fatalf("a non-empty collection was not marked: %s", filled)
	}
	if empty == filled {
		t.Fatal("a turn that cited nothing and a turn that cited something normalize to the same bytes")
	}
}

func TestNormalizerLeavesFramesItCannotParseAlone(t *testing.T) {
	body := "event: message.delta\ndata: not-json\n\n"
	if got := normalizeSSE(t, body); got != body {
		t.Fatalf("a frame the normalizer could not read was rewritten: %q", got)
	}
}

// sseRunIdentity reads back the one identity a stream has to be consistent about.
//
// Every frame of one turn carries the run it belongs to, and the completed frame
// carries the same id inside the result it publishes. A stream that disagreed would
// let a client attribute one turn's answer to another turn's record, and normalizing
// the id away before checking it would make that invisible - so this runs first.
func sseRunIdentity(body string) (runID string, problem string) {
	seen := map[string]bool{}
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				continue
			}
			if value, ok := payload["run_id"].(string); ok && value != "" {
				seen[value] = true
				runID = value
			} else if _, present := payload["run_id"]; present {
				return "", "a frame carried an empty run_id"
			}
			if result, ok := payload["original_v2"].(map[string]any); ok {
				inner, _ := result["runId"].(string)
				if inner == "" {
					return "", "the published result names no run"
				}
				seen[inner] = true
			}
		}
	}
	if len(seen) > 1 {
		return "", "the frames of one turn disagree about which run they belong to"
	}
	return runID, ""
}

func TestRunIdentityRejectsAStreamThatDisagreesWithItself(t *testing.T) {
	if _, problem := sseRunIdentity("event: message.started\ndata: {\"run_id\":\"run-a\"}\n\nevent: message.delta\ndata: {\"run_id\":\"run-b\"}\n\n"); problem == "" {
		t.Fatal("two run ids in one stream were accepted")
	}
	if _, problem := sseRunIdentity("event: message.completed\ndata: {\"run_id\":\"run-a\",\"original_v2\":{\"runId\":\"run-b\"}}\n\n"); problem == "" {
		t.Fatal("a published result naming another run was accepted")
	}
	if _, problem := sseRunIdentity("event: message.started\ndata: {\"run_id\":\"\"}\n\n"); problem == "" {
		t.Fatal("an empty run id was accepted")
	}
	runID, problem := sseRunIdentity("event: message.started\ndata: {\"run_id\":\"run-a\"}\n\nevent: message.completed\ndata: {\"run_id\":\"run-a\",\"original_v2\":{\"runId\":\"run-a\"}}\n\n")
	if problem != "" || runID != "run-a" {
		t.Fatalf("a consistent stream was rejected: %s %q", problem, runID)
	}
}
