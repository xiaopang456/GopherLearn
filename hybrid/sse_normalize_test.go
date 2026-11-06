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
// The normalizer replaces exactly the fields named below and nothing else. It is
// deliberately not a uuid sweep, and not a "reduce the result to its shape" sweep
// either: an answer, an answer basis or a termination reason is what the completed
// frame is for, and erasing it would turn the golden into a shape check that cannot
// fail. The tests below pin both distinctions.

// dynamicWireFields are the values a stream carries that are decided when this turn
// runs rather than by the contract: the run's own identity, the persisted row id, the
// block and draft ids derived from them, and wall-clock times. Both spellings are
// listed because the wire uses snake_case while the published V2 result uses the Go
// field's camelCase, and a normalizer that only knew one would leak the other into the
// golden as a random uuid.
var dynamicWireFields = map[string]string{
	"run_id":         "<run_id>",
	"runId":          "<run_id>",
	"message_id":     "<message_id>",
	"messageId":      "<message_id>",
	"block_id":       "<block_id>",
	"blockId":        "<block_id>",
	"final_block_id": "<block_id>",
	"finalBlockId":   "<block_id>",
	"draft_id":       "<draft_id>",
	"draftId":        "<draft_id>",
	"created_at":     "<timestamp>",
	"createdAt":      "<timestamp>",
	"updated_at":     "<timestamp>",
	"updatedAt":      "<timestamp>",
	"valid_until":    "<timestamp>",
	"validUntil":     "<timestamp>",
}

// structuralFields hold values no transport contract fixes. contextBudget is this run's
// own arithmetic, and the assembly producing it is still being changed by other work,
// so the golden compares its field names and nesting and leaves the numbers to the
// budget tests, which can say what a number should be. It is the only such field, and
// it is named here rather than inferred from "this value is a number".
var structuralFields = map[string]bool{
	"contextBudget":  true,
	"context_budget": true,
}

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

func normalizeFrameIdentities(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			switch {
			case dynamicWireFields[key] != "":
				normalized[key] = dynamicWireFields[key]
			case structuralFields[key]:
				normalized[key] = structureOf(item)
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

// structureOf keeps the names and the nesting of a value and drops what it holds. It
// recurses into array elements rather than collapsing them, because "this turn cited
// two sources, each with these fields" is part of what the budget's shape says; only the
// values are left to the tests that can judge them.
func structureOf(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		structure := make(map[string]any, len(typed))
		for key, item := range typed {
			structure[key] = structureOf(item)
		}
		return structure
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, structureOf(item))
		}
		return items
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
data: {"run_id":"1ee0e562-0b9c-4d83-b50e-647a2db0857f","message_id":"2","content":"回答里也有一个 1ee0e562-0b9c-4d83-b50e-647a2db0857f","publication":"validated_snapshot","original_v2":{"runId":"1ee0e562-0b9c-4d83-b50e-647a2db0857f","answer":"a","modelCalls":1,"sources":[],"contextBudget":{"system":325,"assembled":331}}}

`
	got := normalizeSSE(t, frame)
	for _, want := range []string{
		`"run_id":"<run_id>"`,
		`"message_id":"<message_id>"`,
		// The id inside the answer text is content, and must survive.
		`"content":"回答里也有一个 1ee0e562-0b9c-4d83-b50e-647a2db0857f"`,
		`"publication":"validated_snapshot"`,
		// The budget is the one field reduced to names and nesting...
		`"contextBudget":{"assembled":"<number>","system":"<number>"}`,
		// ...while everything else the result publishes is compared as its value.
		`"answer":"a"`,
		`"modelCalls":1`,
		`"sources":[]`,
		// The result's own run id is named rather than compared, so the golden can
		// still say that it names the same run as the frame around it.
		`"runId":"<run_id>"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized frame lost %s\ngot: %s", want, got)
		}
	}
	for _, unwanted := range []string{`"system":325`, `"assembled":331`} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("a budget number the golden should not decide is still being compared: %s", unwanted)
		}
	}
	// The uuid survives once, in the answer text where it is content. Had runId not been
	// normalized it would survive twice, and the golden would be decided by a random id.
	if count := strings.Count(got, "1ee0e562-0b9c-4d83-b50e-647a2db0857f"); count != 1 {
		t.Fatalf("the run id appears %d times in the normalized frame, want 1: %s", count, got)
	}
	if strings.Count(got, "<run_id>") != 2 {
		t.Fatalf("expected the frame's run_id and original_v2's runId, got %s", got)
	}
}

// The completed frame is the transport contract for a finished turn: which basis the
// answer was given on, how the run ended, whether it was a replay, what it cost in
// calls. None of that is decided per run, so none of it may be normalized away - a
// golden that could not fail on a changed termination reason would not be pinning the
// contract it exists to pin.
func TestNormalizerKeepsTheResultFieldsThatAreContract(t *testing.T) {
	frame := `event: message.completed
data: {"run_id":"run-a","original_v2":{"runId":"run-a","answer":"答案","answerBasis":"knowledge_fallback","knowledgeStatus":"not_required","knowledgeReason":"","terminationReason":"answered","replayed":true,"modelCalls":2,"toolCalls":1,"usedTopicKeys":["go"]}}

`
	got := normalizeSSE(t, frame)
	for _, want := range []string{
		`"answer":"答案"`,
		`"answerBasis":"knowledge_fallback"`,
		`"knowledgeStatus":"not_required"`,
		`"terminationReason":"answered"`,
		`"replayed":true`,
		`"modelCalls":2`,
		`"toolCalls":1`,
		`"usedTopicKeys":["go"]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("a contract value was normalized away, lost %s\ngot: %s", want, got)
		}
	}
	// The point of keeping them: two turns that differ only in how they ended must not
	// normalize to the same bytes.
	other := strings.Replace(frame, `"terminationReason":"answered"`, `"terminationReason":"model_budget_closed"`, 1)
	if normalizeSSE(t, other) == got {
		t.Fatal("a changed termination reason normalizes to the same bytes")
	}
	if normalizeSSE(t, strings.Replace(frame, `"replayed":true`, `"replayed":false`, 1)) == got {
		t.Fatal("a changed replay flag normalizes to the same bytes")
	}
}

func TestNormalizerKeepsEmptyAndNonEmptyCollectionsApart(t *testing.T) {
	empty := normalizeSSE(t, "event: message.completed\ndata: {\"original_v2\":{\"sources\":[]}}\n\n")
	filled := normalizeSSE(t, "event: message.completed\ndata: {\"original_v2\":{\"sources\":[{\"citation\":1,\"docId\":\"原文.md\"}]}}\n\n")
	if !strings.Contains(empty, `"sources":[]`) {
		t.Fatalf("an empty collection was flattened: %s", empty)
	}
	// A cited source keeps its fields and their values: which document a turn cited is
	// contract, and only the budget's numbers are left to their own tests.
	if !strings.Contains(filled, `"sources":[{"citation":1,"docId":"原文.md"}]`) {
		t.Fatalf("a non-empty collection lost its element structure: %s", filled)
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
