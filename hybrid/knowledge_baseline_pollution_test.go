package hybrid

import (
	"os"
	"strings"
	"testing"
)

// Negative controls for the retrieval verifier.
//
// The previous revision accepted "some source came back" as proof that the uploaded document
// was retrievable. An independent review replaced every source's document id with a wrong one
// and all three cases still passed, so that acceptance was worth nothing. This file makes the
// opposite true: the rules that decide a result is allowed are exercised against deliberately
// polluted results, and every pollution type must be reported by name.
//
// Two levels are covered, because they fail in different ways:
//
//  1. The verifier itself, over synthetic results. These always run, and they are the check
//     that a rule still rejects what it is supposed to reject.
//  2. The live chain, over a real Agent turn whose response is corrupted in flight through
//     the observer seam. This is the end-to-end demonstration that the rules are applied to
//     what the running application returned rather than only to hand-written fixtures. It is
//     a gated ordinary negative control: a healthy chain and an explicitly rejected
//     corruption PASS; a broken/bypassed verifier FAILs. The runner checks its terminal.

// baselinePollution is one way a result can be wrong, with the phrase the verifier must use
// when it reports it.
type baselinePollution struct {
	Name     string
	Mutate   func(sources []any) []any
	WantTerm string
}

// baselinePollutionCases is the list both levels use. One list means the live-chain
// demonstration cannot quietly test a different thing from the unit-level one.
func baselinePollutionCases(targetDocID, otherUserDocID, otherOwnedDocID string) []baselinePollution {
	return []baselinePollution{
		{
			Name: "every source carries a wrong document id",
			Mutate: func(sources []any) []any {
				for _, item := range sources {
					if source, ok := item.(map[string]any); ok {
						source["doc_id"] = "codex-wrong-document"
					}
				}
				return sources
			},
			WantTerm: "not an active document for this user",
		},
		{
			Name: "the first source is right and a later one carries a superseded version",
			Mutate: func(sources []any) []any {
				if len(sources) == 0 {
					return sources
				}
				first, ok := sources[0].(map[string]any)
				if !ok {
					return sources
				}
				trailing := make(map[string]any, len(first))
				for key, value := range first {
					trailing[key] = value
				}
				trailing["doc_version"] = float64(99)
				return append(sources, trailing)
			},
			WantTerm: "want active version",
		},
		{
			Name: "a source names another user's document",
			Mutate: func(sources []any) []any {
				if len(sources) == 0 {
					return sources
				}
				if first, ok := sources[0].(map[string]any); ok {
					first["doc_id"] = otherUserDocID
				}
				return sources
			},
			WantTerm: "is forbidden",
		},
		{
			Name: "no source comes from the document under test",
			Mutate: func(sources []any) []any {
				// A structurally valid source that belongs to another document the same user
				// owns: the allow-list accepts it, and only the target requirement catches
				// it. That is the shape a multi-document query can really produce.
				return []any{map[string]any{
					"doc_id": otherOwnedDocID, "doc_version": float64(1),
					"chunk_index": float64(0), "start_line": float64(1), "end_line": float64(2),
					"title_path": []any{},
				}}
			},
			WantTerm: "no source came from the target document",
		},
		{
			Name:     "the result is emptied",
			Mutate:   func(sources []any) []any { return []any{} },
			WantTerm: "returned no source",
		},
		{
			Name:     "a source has no chunk index",
			Mutate:   func(sources []any) []any { delete(sources[0].(map[string]any), "chunk_index"); return sources },
			WantTerm: "missing or invalid chunk_index",
		},
		{
			Name:     "a source has a fractional chunk index",
			Mutate:   func(sources []any) []any { sources[0].(map[string]any)["chunk_index"] = 0.5; return sources },
			WantTerm: "missing or invalid chunk_index",
		},
		{
			Name:     "a later source is null",
			Mutate:   func(sources []any) []any { return append(sources, nil) },
			WantTerm: "is not an object",
		},
		{
			Name:     "a later source is a string",
			Mutate:   func(sources []any) []any { return append(sources, "malformed source") },
			WantTerm: "is not an object",
		},
	}
}

// baselineHealthySource is one structurally valid source for the given document.
func baselineHealthySource(docID string, version int, chunkIndex, startLine, endLine int, titlePath []string) map[string]any {
	path := make([]any, 0, len(titlePath))
	for _, item := range titlePath {
		path = append(path, item)
	}
	return map[string]any{
		"doc_id": docID, "doc_version": float64(version), "chunk_index": float64(chunkIndex),
		"start_line": float64(startLine), "end_line": float64(endLine), "title_path": path,
	}
}

// TestKnowledgeBaselineVerifierRejectsPollutedResults is the unit-level negative control. It
// builds a healthy result, corrupts one thing at a time, and requires the verifier to report
// that specific thing.
func TestKnowledgeBaselineVerifierRejectsPollutedResults(t *testing.T) {
	const (
		target     = "target-document"
		otherOwned = "another-owned-document"
		otherUser  = "another-users-document"
	)
	expectation := baselineRetrievalExpectation{
		Allowed: []baselineExpectedDocument{
			{DocID: target, Version: 1, Label: "the document under test"},
			{DocID: otherOwned, Version: 1, Label: "another document the user owns"},
		},
		MustInclude:     []string{target},
		ForbiddenDocIDs: []string{otherUser},
	}
	healthy := []map[string]any{baselineHealthySource(target, 1, 0, 1, 3, []string{"标题"})}
	if verdict := baselineVerifySources(expectation, healthy); verdict.Failed() {
		t.Fatalf("the reference result this list is built from is itself rejected: %v", verdict.Problems)
	}

	for _, pollution := range baselinePollutionCases(target, otherUser, otherOwned) {
		t.Run(pollution.Name, func(t *testing.T) {
			// A fresh copy per subtest: the mutations edit the maps in place, so sharing one
			// slice would let an earlier pollution decide a later subtest's result.
			mutated := pollution.Mutate(baselineSourcesAsAny(baselineCloneSources(healthy)))
			sources, problems := baselineSourcesOf(map[string]any{"sources": mutated})
			verdict := baselineVerifySources(expectation, sources)
			verdict.Problems = append(verdict.Problems, problems...)
			if !verdict.Failed() {
				t.Fatalf("%s was accepted; the verifier reported nothing", pollution.Name)
			}
			if !baselineContainsPhrase(verdict.Problems, pollution.WantTerm) {
				t.Fatalf("%s was rejected, but not for the checked reason: %v", pollution.Name, verdict.Problems)
			}
		})
	}

	// The review's own reproduction, kept as a named case: a result whose every id is wrong.
	reproduced := baselineVerifySources(expectation, []map[string]any{baselineHealthySource("codex-wrong-document", 1, 0, 1, 3, nil)})
	if !reproduced.Failed() {
		t.Fatal("the review's reproduction is still accepted, so the verifier has not closed the gap it found")
	}

	// An empty result is a failure only where a source is required. The stage where a user
	// owns nothing states the opposite, and must still pass.
	if verdict := baselineVerifySources(baselineRetrievalExpectation{ExpectNoSources: true}, nil); verdict.Failed() {
		t.Fatalf("an empty result was rejected where emptiness is the requirement: %v", verdict.Problems)
	}
	if verdict := baselineVerifySources(baselineRetrievalExpectation{ExpectNoSources: true}, healthy); !verdict.Failed() {
		t.Fatal("a non-empty result was accepted where emptiness is the requirement")
	}
}

func baselineSourcesAsAny(sources []map[string]any) []any {
	out := make([]any, 0, len(sources))
	for _, source := range sources {
		out = append(out, source)
	}
	return out
}

// baselineCloneSources copies a whole result, so one mutation cannot reach another case.
func baselineCloneSources(sources []map[string]any) []map[string]any {
	cloned := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		cloned = append(cloned, baselineCloneSource(source))
	}
	return cloned
}

func baselineContainsPhrase(problems []string, phrase string) bool {
	for _, problem := range problems {
		if strings.Contains(problem, phrase) {
			return true
		}
	}
	return false
}

// TestKnowledgeBaselineVerifierRejectsFabricatedLocations covers the second half of R1: a
// source can name the right document and the right version and still cite a location no
// stored chunk has. Only the cross-check against the store sees that.
func TestKnowledgeBaselineVerifierRejectsFabricatedLocations(t *testing.T) {
	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")
	status, body := world.upload(token, "locations.md", baselineSyntheticMarkdown("定位", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)

	stored := world.store.versionChunks(1, id, 1)
	if len(stored) == 0 {
		t.Fatal("the store received nothing, so no location could be cross-checked")
	}
	first := stored[0].Chunk
	healthy := []map[string]any{baselineHealthySource(id, 1, first.Index, first.StartLine, first.EndLine, first.TitlePath)}
	if problems := baselineLocationProblems(world.store, 1, healthy); len(problems) > 0 {
		t.Fatalf("a citation copied from the stored chunk was rejected: %v", problems)
	}

	cases := []struct {
		name     string
		mutate   func(map[string]any)
		wantTerm string
	}{
		{"an index this version never stored", func(source map[string]any) { source["chunk_index"] = float64(9999) }, "un-stored document/version/chunk"},
		{"missing index", func(source map[string]any) { delete(source, "chunk_index") }, "missing or invalid chunk_index"},
		{"fractional index", func(source map[string]any) { source["chunk_index"] = 0.5 }, "missing or invalid chunk_index"},
		{"fractional version", func(source map[string]any) { source["doc_version"] = 1.5 }, "invalid doc_version"},
		{"fractional start line", func(source map[string]any) { source["start_line"] = float64(first.StartLine) + 0.5 }, "start_line does not match"},
		{"fractional end line", func(source map[string]any) { source["end_line"] = float64(first.EndLine) + 0.5 }, "end_line does not match"},
		{"a start line the chunk does not have", func(source map[string]any) { source["start_line"] = float64(9999) }, "start_line does not match"},
		{"an end line the chunk does not have", func(source map[string]any) { source["end_line"] = float64(9999) }, "end_line does not match"},
		{"a heading path the chunk does not have", func(source map[string]any) {
			source["title_path"] = []any{"编造的标题"}
		}, "title_path does not match"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := []map[string]any{baselineCloneSource(healthy[0])}
			testCase.mutate(mutated[0])
			problems := baselineLocationProblems(world.store, 1, mutated)
			if !baselineContainsPhrase(problems, testCase.wantTerm) {
				t.Fatalf("%s was not reported (%v)", testCase.name, problems)
			}
		})
	}
}

func baselineCloneSource(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// TestKnowledgeBaselineLiveChainPollutionEvidenceIsRecorded runs the same corruptions over a
// real indexed document and records the verifier's verdict for each into the run's event file.
// It does not fail: it is the evidence that every corruption is caught on the live chain,
// without making the ordinary suite red.
func TestKnowledgeBaselineLiveChainPollutionEvidenceIsRecorded(t *testing.T) {
	recorder := newBaselineRecorder(t, "live-chain-pollution")
	defer recorder.flush(t)
	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")
	status, body := world.upload(token, "pollution-evidence.md", baselineSyntheticMarkdown("污染取证", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)

	// A second document the same user owns, so the "no target hit" pollution is judged by the
	// target requirement rather than by the allow-list rule.
	status, secondBody := world.upload(token, "pollution-second.md", baselineSyntheticMarkdown("污染取证二", "丁"))
	if status != 201 {
		t.Fatalf("second upload: %d %v", status, secondBody)
	}
	secondID := docView(t, secondBody)["id"].(string)
	world.requireDocumentState(token, secondID, "ready", baselineFlowTimeout)

	expectation := baselineRetrievalExpectation{
		Allowed: []baselineExpectedDocument{
			{DocID: id, Version: 1, Label: "the document under test"},
			{DocID: secondID, Version: 1, Label: "another document the user owns"},
		},
		MustInclude: []string{id},
		// The other user's document is named as an outright refusal, so the pollution that
		// swaps it in is reported as a leak rather than merely as an unexpected id.
		ForbiddenDocIDs: []string{"another-users-document"},
	}

	// The control: the uncorrupted chain must pass every rule.
	healthy := world.retrieveChecked(token, "污染取证", expectation)
	recorder.event("pollution-control", map[string]any{
		"document_id": id, "sources": len(healthy.Sources),
		"identity_rules_passed": true, "location_rules_passed": true,
	})

	results := make([]map[string]any, 0)
	for _, pollution := range baselinePollutionCases(id, "another-users-document", secondID) {
		baselineSourceObserver = pollution.Mutate
		attempt := world.retrieveRaw(token, "污染取证")
		baselineSourceObserver = nil
		verdict := baselineVerifySources(expectation, attempt.Sources)
		verdict.Problems = append(verdict.Problems, attempt.Problems...)
		verdict.Problems = append(verdict.Problems, baselineLocationProblems(world.store, 1, attempt.Sources)...)
		results = append(results, map[string]any{
			"pollution": pollution.Name, "rejected": verdict.Failed(),
			"problem_count":           len(verdict.Problems),
			"matched_expected_reason": baselineContainsPhrase(verdict.Problems, pollution.WantTerm),
		})
		if !verdict.Failed() {
			t.Errorf("pollution %q was not rejected on the live chain", pollution.Name)
		} else if !baselineContainsPhrase(verdict.Problems, pollution.WantTerm) {
			t.Errorf("pollution %q was rejected, but not for the checked reason: %v", pollution.Name, verdict.Problems)
		}
	}
	recorder.event("live-chain-pollution", map[string]any{
		"document_id": id, "pollutions": results,
		"note": "each corruption was applied to the response of a real Agent turn through the test-only observer seam; the application itself ran unchanged",
	})
}

// This gated control now PASSes only after the real retrieveChecked path rejects
// the specified pollution. Setup failure or bypassing that path must FAIL.
func TestKnowledgeBaselineLiveChainPollutionFailsTheCheck(t *testing.T) {
	if strings.TrimSpace(os.Getenv("GOPHERLEARN_DOC_B01_POLLUTION")) == "" {
		t.Skip("set GOPHERLEARN_DOC_B01_POLLUTION=1 to run the live-chain rejection control")
	}
	recorder := newBaselineRecorder(t, "gated-pollution-control")
	defer recorder.flush(t)
	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")
	status, body := world.upload(token, "pollution-fails.md", baselineSyntheticMarkdown("污染失败演示", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)

	expectation := baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the document under test"}},
		MustInclude: []string{id},
	}
	// The uncorrupted query first, so the demonstration can state that the same query passes
	// before anything is corrupted. Without it, the failure below could be read as a
	// pre-existing problem with the chain.
	healthy := world.retrieveChecked(token, "污染失败演示", expectation)
	t.Logf("the uncorrupted query passed verification with %d source(s)", len(healthy.Sources))
	recorder.event("healthy_control_passed", map[string]any{"sources": len(healthy.Sources)})

	baselineSourceObserver = baselinePollutionCases(id, "another-users-document", id)[0].Mutate
	t.Cleanup(func() { baselineSourceObserver = nil })

	var rejected []string
	world.retrievalRejected = func(problems []string) { rejected = problems }
	attempt := world.retrieveChecked(token, "污染失败演示", expectation)
	if !baselineContainsPhrase(rejected, "not an active document for this user") || len(attempt.Problems) == 0 {
		t.Fatal("unexpected_acceptance: actual retrieveChecked did not reject the wrong document identity")
	}
	recorder.event("expected_rejection", map[string]any{
		"healthy_control_passed": true, "rule": "not an active document for this user", "problem_count": len(rejected),
	})
	t.Log("expected_rejection: healthy control passed; actual retrieveChecked rejected wrong document identity")
}
