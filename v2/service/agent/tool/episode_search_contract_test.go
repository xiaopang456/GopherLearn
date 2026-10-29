package agenttool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
)

// episodicHarness is a harness whose registry serves the Episode-only contract.
func episodicHarness(t *testing.T, mutate func(*toolHarness)) *toolHarness {
	t.Helper()
	h := newToolHarness(t, mutate)
	h.registry.UseEpisodicOnly(true)
	return h
}

// toolParameters renders what the model actually receives: the description and
// the parameter schema. The schema has to be pulled out of ParamsOneOf, because
// marshalling the ToolInfo drops it (BUG-EVAL-034).
func toolParameters(t *testing.T, invokable tool.InvokableTool) (string, string) {
	t.Helper()
	info, err := invokable.Info(context.Background())
	if err != nil {
		t.Fatalf("read tool info: %v", err)
	}
	jsonSchema, err := info.ToJSONSchema()
	if err != nil {
		t.Fatalf("read the parameter schema: %v", err)
	}
	encoded, err := json.Marshal(jsonSchema)
	if err != nil {
		t.Fatalf("encode the parameter schema: %v", err)
	}
	return info.Desc, string(encoded)
}

// 1. The Episode-only contract does not offer kinds, and does not mention
// semantic memory as something this tool can reach.
//
// This is the whole fix. v1.8.1 lost seven of fourteen questions to a field the
// mode was never going to serve, so the field is gone rather than better
// documented.
func TestEpisodeContractOffersNoKinds(t *testing.T) {
	h := episodicHarness(t, nil)
	invokable, _ := h.build(t)
	description, blob := toolParameters(t, invokable)
	if strings.Contains(blob, "kinds") || strings.Contains(blob, "semantic") {
		t.Fatalf("the episode contract still mentions kinds: %s", blob)
	}
	for _, field := range []string{"query", "temporal", "top_k", "include_source_quotes"} {
		if !strings.Contains(blob, field) {
			t.Fatalf("the episode contract lost %s: %s", field, blob)
		}
	}
	if !strings.Contains(description, "topic memory") || !strings.Contains(description, "learning state") {
		t.Fatalf("the description must say what it does not reach: %s", description)
	}
}

// 2. Every other mode keeps the contract its published measurements describe.
//
// The digest is pinned rather than compared to itself: a test that recomputed
// both sides would pass through any edit. This number is the one the v1.8.1
// freeze bound, and it is what makes "the old modes are byte-identical" a fact
// rather than an intention.
func TestGeneralContractIsUnchanged(t *testing.T) {
	const frozenBySTABILIZE058v181 = "790f2ba6e0bf9b9f517d72437c01636f6324ad6c59b1652109fed93e1a8c6490"
	digest, err := MemorySearchSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != frozenBySTABILIZE058v181 {
		t.Fatalf("the general memory_search schema changed: %s", digest)
	}
	h := newToolHarness(t, nil)
	invokable, _ := h.build(t)
	_, blob := toolParameters(t, invokable)
	if !strings.Contains(blob, "kinds") {
		t.Fatalf("the general contract lost kinds: %s", blob)
	}
	// The parameter-covering digests differ too, which the name-and-description
	// digest alone could not have told us.
	generalContract, err := MemorySearchContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	episodeContract, err := EpisodeSearchContractDigest()
	if err != nil {
		t.Fatal(err)
	}
	if generalContract == episodeContract {
		t.Fatal("the two parameter tables produced the same contract digest")
	}
	episode, err := EpisodeSearchSchemaDigest()
	if err != nil {
		t.Fatal(err)
	}
	if episode == digest {
		t.Fatal("the two contracts must not have the same digest")
	}
}

// 3. With kinds gone, an ordinary call still runs an episodic search, and the
// semantic index is never asked.
func TestEpisodeContractSearchesEpisodicWithoutBeingTold(t *testing.T) {
	h := episodicHarness(t, func(h *toolHarness) {})
	semanticFixture(h, "m-1")
	episodeFixture(h, "e-1", nil)
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"上次聊到哪"}`)
	if output.Status != statusOK {
		t.Fatalf("status = %s reason = %s", output.Status, output.Reason)
	}
	if len(output.Results) != 1 || output.Results[0].Kind != "episodic" {
		t.Fatalf("results = %#v, want one episodic card", output.Results)
	}
	if queries := h.memoryIndex.queries.Load(); queries != 0 {
		t.Fatalf("the semantic index was queried %d times", queries)
	}
}

// 4. A call that did not come through this contract fails closed.
//
// The dangerous case is kinds=["episodic"]: a lenient decoder would drop it and
// run the very search the caller asked for, so nothing would look wrong and the
// contract would quietly stop being the contract. It is refused like the rest.
func TestEpisodeContractRefusesFieldsItDoesNotOffer(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		arguments string
	}{
		{"an explicit semantic kind", `{"query":"我对 JWT 的理解","kinds":["semantic"]}`},
		{"mixed kinds", `{"query":"上次聊到哪","kinds":["episodic","semantic"]}`},
		{"the kind this mode does serve", `{"query":"上次聊到哪","kinds":["episodic"]}`},
		{"an invented field", `{"query":"上次聊到哪","scope":"global"}`},
		{"a second object", `{"query":"上次聊到哪"}{"query":"再来一次"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := episodicHarness(t, nil)
			episodeFixture(h, "e-1", nil)
			invokable, evidence := h.build(t)
			output := call(t, invokable, testCase.arguments)
			if output.Status != statusInvalid || output.Reason != reasonUnsupportedField {
				t.Fatalf("status = %s reason = %s, want %s/%s",
					output.Status, output.Reason, statusInvalid, reasonUnsupportedField)
			}
			if output.Retryable {
				t.Fatal("a field this contract does not offer is not fixable by rewording")
			}
			if len(output.Results) != 0 {
				t.Fatalf("a refused call returned %d cards", len(output.Results))
			}
			// Refused before the search: nothing was retrieved, and the run's
			// two searches are both still available.
			if output.RemainingCalls != h.registry.config.MaxCalls {
				t.Fatalf("remaining = %d, want %d", output.RemainingCalls, h.registry.config.MaxCalls)
			}
			calls := evidence.Calls()
			if len(calls) != 1 || calls[0].Reason != reasonUnsupportedField {
				t.Fatalf("recorded calls = %#v", calls)
			}
			// And the refusal never quotes the rejected arguments back.
			if strings.Contains(output.Summary+output.NextStep, "semantic") &&
				!strings.Contains(output.NextStep, "不搜索") {
				t.Fatalf("the refusal echoed the request: %s / %s", output.Summary, output.NextStep)
			}
		})
	}
}

// 5. Topic memory, global profile and learning state cannot come out of this
// tool under any input it accepts.
func TestEpisodeContractCannotReturnSemanticRecords(t *testing.T) {
	h := episodicHarness(t, nil)
	semanticFixture(h, "m-1", "m-2")
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"我对 JWT 的理解","top_k":6}`)
	if output.Status != statusNoEvidence {
		t.Fatalf("status = %s reason = %s, want %s", output.Status, output.Reason, statusNoEvidence)
	}
	if len(output.Results) != 0 {
		t.Fatalf("semantic records reached the model: %#v", output.Results)
	}
	if queries := h.memoryIndex.queries.Load(); queries != 0 {
		t.Fatalf("the semantic index was queried %d times", queries)
	}
}

// 6. The filters that were already protecting episodes still protect them. This
// contract narrows what the model may ask for; it must not widen what the
// server will hand back.
func TestEpisodeContractKeepsTheEpisodeFilters(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		break_ func(*model.EpisodeMemory)
	}{
		{"another learner's episode", func(e *model.EpisodeMemory) { e.UserID = 2 }},
		{"an archived episode", func(e *model.EpisodeMemory) { e.Status = model.EpisodeStatusArchived }},
		{"an episode from the current session", func(e *model.EpisodeMemory) { e.SessionID = "current" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := episodicHarness(t, nil)
			episodeFixture(h, "e-1", nil)
			testCase.break_(&h.episodeStore.rows[0])
			invokable, _ := h.build(t)
			output := call(t, invokable, `{"query":"上次聊到哪"}`)
			for _, card := range output.Results {
				if card.Kind == "episodic" {
					t.Fatalf("%s was returned: %#v", testCase.name, card)
				}
			}
		})
	}
}

// 7. A failure below this tool is reported as unavailable. It is never reported
// as an empty memory, and it never falls back to semantic records - a mode that
// answered a broken episode search with topic notes would be the unconfirmed
// door this whole mode exists to close.
func TestEpisodeContractReportsUnavailableWithoutFallingBack(t *testing.T) {
	h := episodicHarness(t, func(h *toolHarness) {
		h.embedder.err = errors.New("the embedding service is down")
	})
	semanticFixture(h, "m-1")
	invokable, _ := h.build(t)
	output := call(t, invokable, `{"query":"上次聊到哪"}`)
	if output.Status != statusUnavailable {
		t.Fatalf("status = %s reason = %s, want %s", output.Status, output.Reason, statusUnavailable)
	}
	if len(output.Results) != 0 {
		t.Fatalf("a failed search returned %d cards", len(output.Results))
	}
	if strings.Contains(output.Summary, "redis") {
		t.Fatalf("the internal error reached the model: %s", output.Summary)
	}
}

// 8. A legal query still runs the whole production chain: the episode index is
// asked for candidates and the store is asked to reload them, in that order.
func TestEpisodeContractStillUsesTheCandidateChain(t *testing.T) {
	h := episodicHarness(t, func(h *toolHarness) {
		episodeFixture(h, "e-1", []uint{7})
		h.messages.rows = map[uint]model.Message{
			7: {ID: 7, IsUser: true, Content: "我当时说的原话", CreatedAt: toolClock},
		}
	})
	invokable, evidence := h.build(t)
	output := call(t, invokable, `{"query":"上次聊到哪","include_source_quotes":true,"top_k":3}`)
	if output.Status != statusOK || len(output.Results) != 1 {
		t.Fatalf("status = %s results = %d", output.Status, len(output.Results))
	}
	card := output.Results[0]
	if card.Ref == "" {
		t.Fatal("a returned card must carry a reference the answer can cite")
	}
	if !evidence.IssuedRecord(serviceagent.MemoryEvidenceEpisodic, "e-1", 2) {
		t.Fatal("the run's evidence registry did not record the episode it issued")
	}
	// The card came back through the candidate chain: the episode index ranked
	// it and the store reloaded the row the card was rendered from.
	if !strings.Contains(card.Content, "synopsis e-1") {
		t.Fatalf("the card was not rendered from the reloaded row: %#v", card)
	}
	if h.embedder.calls.Load() == 0 {
		t.Fatal("the query was never embedded, so no vector candidate chain ran")
	}
	_ = episodedao.CurrentEpisodeEmbeddingVersion
}
