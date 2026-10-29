package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	serviceagent "GopherAI/service/agent"

	"github.com/cloudwego/eino/components/tool"
)

// preinjectedSemantic builds the descriptor the ContextBuilder would hand over
// for a record it already showed the model. The version is derived the same way
// the candidate layer derives it, which is the whole reason both go through
// model.SemanticMemoryVersion.
func preinjectedSemantic(id string, updatedAt time.Time) serviceagent.PreinjectedMemoryRef {
	return serviceagent.PreinjectedMemoryRef{
		Kind: serviceagent.PreinjectedMemoryKindSemantic, ID: id,
		Version: model.SemanticMemoryVersion(model.LongTermMemory{UpdatedAt: updatedAt}), Ref: "S1",
	}
}

// buildWithPreinjected builds the run's tool the way the runner does: the window
// decides what was already shown, and the tool obeys.
func (h *toolHarness) buildWithPreinjected(t *testing.T,
	preinjected []serviceagent.PreinjectedMemoryRef) (tool.InvokableTool, *serviceagent.MemoryEvidenceRegistry) {
	t.Helper()
	evidence := serviceagent.NewMemoryEvidenceRegistry()
	tools, err := h.registry.Tools(context.Background(), serviceagent.RunContext{
		RunID: "run-1", UserID: 1, UserName: "learner", SessionID: "current", Question: "q",
		MemoryEvidence: evidence, PreinjectedMemory: preinjected,
	})
	if err != nil {
		t.Fatal(err)
	}
	invokable, ok := tools[0].(tool.InvokableTool)
	if !ok {
		t.Fatalf("memory_search is not invokable")
	}
	return invokable, evidence
}

// A record the model has already read does not come back under a second label.
// The other record on the same topic still does: suppression is per record, not
// per topic, or a learner with six notes on one subject would only ever reach
// the five the window happened to fit.
func TestPreinjectedRecordsAreNotReturnedTwiceButNeighboursStillAre(t *testing.T) {
	h := newToolHarness(t, nil)
	semanticFixture(h, "m-1", "m-2")
	invokable, evidence := h.buildWithPreinjected(t,
		[]serviceagent.PreinjectedMemoryRef{preinjectedSemantic("m-1", toolClock.Add(-time.Hour))})

	output := call(t, invokable, `{"query":"这位学习者对该主题的理解","kinds":["semantic"]}`)
	if output.Status != statusOK {
		t.Fatalf("status = %s reason = %s", output.Status, output.Reason)
	}
	if len(output.Results) != 1 || !strings.Contains(output.Results[0].Content, "m-2") {
		t.Fatalf("results = %#v", output.Results)
	}
	for _, entry := range evidence.Entries() {
		if entry.InternalID == "m-1" {
			t.Fatal("a record already in the window was registered a second time")
		}
	}
}

// The honest-report case. Every hit was already shown, so there are no cards -
// and the observation must not say the learner has no such memory, because the
// window is carrying exactly that memory as this call returns.
func TestFullySuppressedSearchReportsAlreadyInContextRatherThanNoMemory(t *testing.T) {
	h := newToolHarness(t, nil)
	semanticFixture(h, "m-1")
	invokable, _ := h.buildWithPreinjected(t,
		[]serviceagent.PreinjectedMemoryRef{preinjectedSemantic("m-1", toolClock.Add(-time.Hour))})

	output := call(t, invokable, `{"query":"这位学习者对该主题的理解","kinds":["semantic"]}`)
	if output.Status != statusNoEvidence {
		t.Fatalf("status = %s", output.Status)
	}
	if output.Reason != reasonAlreadyProvided {
		t.Fatalf("reason = %s, want %s", output.Reason, reasonAlreadyProvided)
	}
	if strings.Contains(output.Summary, "没有与该检索相关的记录") {
		t.Fatalf("a suppressed search claimed the learner has no such memory: %q", output.Summary)
	}
	if !strings.Contains(output.Summary, "已经在初始上下文中提供") {
		t.Fatalf("summary = %q", output.Summary)
	}
}

// A genuinely empty memory still reports a genuinely empty memory. The new
// reason must not swallow the old one.
func TestAnEmptySearchStillReportsNoCandidate(t *testing.T) {
	h := newToolHarness(t, nil)
	invokable, _ := h.buildWithPreinjected(t,
		[]serviceagent.PreinjectedMemoryRef{preinjectedSemantic("m-1", toolClock.Add(-time.Hour))})

	output := call(t, invokable, `{"query":"从未记录过的事","kinds":["semantic"]}`)
	if output.Status != statusNoEvidence || output.Reason == reasonAlreadyProvided {
		t.Fatalf("status = %s reason = %s", output.Status, output.Reason)
	}
}

// With no preinjected set - which is every turn under v1, and every v2 turn
// with no active quiz - the search is untouched.
func TestWithoutPreinjectionSemanticSearchIsUnchanged(t *testing.T) {
	h := newToolHarness(t, nil)
	semanticFixture(h, "m-1", "m-2")
	withNone, _ := h.buildWithPreinjected(t, nil)
	baseline := call(t, withNone, `{"query":"这位学习者对该主题的理解","kinds":["semantic"]}`)

	plain := newToolHarness(t, nil)
	semanticFixture(plain, "m-1", "m-2")
	invokable, _ := plain.build(t)
	original := call(t, invokable, `{"query":"这位学习者对该主题的理解","kinds":["semantic"]}`)

	got, _ := json.Marshal(baseline.Results)
	want, _ := json.Marshal(original.Results)
	if string(got) != string(want) {
		t.Fatalf("an empty preinjection changed the window:\n%s\n%s", got, want)
	}
}

// Episodes are never preinjected, and a suppression set built from semantic
// records must not reach them even if an id collided.
func TestEpisodeSearchIsNeverSuppressed(t *testing.T) {
	h := newToolHarness(t, nil)
	episodeFixture(h, "shared-id", nil)
	// Same id, semantic kind: the dedup key carries the kind, so this must not
	// match the episode.
	invokable, _ := h.buildWithPreinjected(t, []serviceagent.PreinjectedMemoryRef{{
		Kind: serviceagent.PreinjectedMemoryKindSemantic, ID: "shared-id", Version: 2, Ref: "S1",
	}})

	output := call(t, invokable, `{"query":"上次会话发生了什么","kinds":["episodic"]}`)
	if output.Status != statusOK || len(output.Results) != 1 {
		t.Fatalf("an episode was suppressed by a semantic key: %#v", output)
	}
}

// An edited note is genuinely something the model has not read. The version is
// part of the identity so an update is not silently hidden.
func TestAnEditedRecordIsNotSuppressed(t *testing.T) {
	h := newToolHarness(t, nil)
	semanticFixture(h, "m-1")
	invokable, _ := h.buildWithPreinjected(t,
		// The window carried an older revision of the same row.
		[]serviceagent.PreinjectedMemoryRef{preinjectedSemantic("m-1", toolClock.Add(-48*time.Hour))})

	output := call(t, invokable, `{"query":"这位学习者对该主题的理解","kinds":["semantic"]}`)
	if output.Status != statusOK || len(output.Results) != 1 {
		t.Fatalf("an updated record was suppressed: %#v", output)
	}
}
