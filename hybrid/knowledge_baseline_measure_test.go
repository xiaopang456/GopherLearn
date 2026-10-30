package hybrid

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file is the measurement tool's smoke path. It is deliberately separate from the
// correctness cases: those assert behaviour, this one records numbers.
//
// What it produces: one JSONL record per event in the run's event directory, and a summary
// CSV written by the report script under the task directory. Every record states its own
// unit, its denominator and what it is not.
//
// What it does NOT produce, and must never be read as producing:
//   - performance results. The timings below were taken on a development machine while a
//     second development window was building and testing. They are smoke values.
//   - token counts or cost. The embedder's byte counter is bytes of UTF-8 input.
//   - retrieval quality. The fake embedder is a lexical hash, not a semantic model.
//   - production resource use. A test process holding a fake store in memory is not a
//     backend process holding Redis and MySQL.
//
// Resource sampling is not done here at all. The previous revision kept a second, in-process
// sampler beside the script's; the two disagreed about what they measured and the in-process
// one had no bound on its own cleanup, so it was removed. `measure-baseline.ps1` samples the
// whole `go test` process tree, records the interval it aimed for, the timestamps it actually
// observed and how many samples were usable, and writes null with a reason for anything the
// platform cannot report. One sampling path, one meaning.

// baselineStageTiming records the stages this round can actually separate. The chain exposes
// an upload response, a ready transition and a retrieval turn; it does not expose parse,
// chunk, embed and index separately, so those are reported as "not decomposed" rather than
// divided up by guesswork.
type baselineStageTiming struct {
	// EndToEndMS is measured from immediately before the upload request to the moment the
	// retrieval's sources have been verified to include the target document. It is the
	// headline number, and it is a wall-clock observation of one query on this machine, not
	// the product's internal publish instant.
	EndToEndMS int64 `json:"end_to_end_ms"`
	// AcceptMS, ReadyMS and RetrievalMS are the sub-intervals the chain does expose. They are
	// supplementary: their sum is not the end-to-end value, because the end-to-end window also
	// covers the polling between ready and retrieval and whatever the harness does between
	// steps.
	AcceptMS    int64 `json:"accept_to_response_ms"`
	ReadyMS     int64 `json:"ready_wait_ms"`
	RetrievalMS int64 `json:"retrieval_ms"`

	EndToEndMeaning string `json:"end_to_end_meaning"`
	Decomposed      bool   `json:"stage_decomposition_available"`
	Undecomposed    string `json:"undecomposed_stages"`
	TimingContext   string `json:"timing_context"`
}

// TestKnowledgeBaselineMeasurementSmoke is the tool smoke the task asks for: one end-to-end
// submission that becomes retrievable, with the embedding call and item counts, the chunk and
// reuse statistics, the task outcome and the observable resources recorded.
//
// The end-to-end clock starts before the upload and stops as soon as the target source has
// been verified, which is before any record is written or any counter is read. That ordering
// is the point: a duration taken after the bookkeeping would include the bookkeeping.
//
// Metrics this round cannot observe are written as null or as an explicit "not measured",
// never as zero: a zero would be indistinguishable from a real measurement of zero.
func TestKnowledgeBaselineMeasurementSmoke(t *testing.T) {
	recorder := newBaselineRecorder(t, "measurement-smoke")
	defer recorder.flush(t)

	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")

	// The query session is created before the clock starts and the fact is recorded, so the
	// number below is not silently paying for session creation. It is stated as a
	// precondition rather than left for a reader to guess.
	sessionID := world.agentSession(token)

	content := baselineSyntheticMarkdown("测量冒烟", "丙")

	// ---- the measured window opens here ----
	started := time.Now()
	status, body := world.upload(token, "measure.md", content)
	if status != 201 {
		t.Fatalf("upload: %d", status)
	}
	acceptMS := time.Since(started).Milliseconds()
	id := docView(t, body)["id"].(string)
	ready := world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
	attempt := world.retrieveChecked(token, "测量冒烟", baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the document under test"}},
		MustInclude: []string{id},
	})
	endToEnd := time.Since(started)
	// ---- the measured window closes here; everything below is bookkeeping ----

	chunks := world.store.versionChunks(1, id, 1)
	counts := world.embedder.counts()
	storeCalls, vectorCalls, textCalls, deleted, loadCalls, ensureCalls := world.store.counts()

	timing := baselineStageTiming{
		EndToEndMS:  endToEnd.Milliseconds(),
		AcceptMS:    acceptMS,
		ReadyMS:     ready.Elapsed.Milliseconds(),
		RetrievalMS: attempt.Elapsed.Milliseconds(),
		EndToEndMeaning: "upload request start to verified target source, measured on the first successful query of this " +
			"run; it is a wall-clock observation of one query on this machine, not the product's internal publish instant",
		Decomposed: false,
		Undecomposed: "the local chain exposes no separate timing for parse, chunk, embed and index; they are reported " +
			"together inside ready_wait_ms, and the sub-intervals are not a partition of the end-to-end value",
		TimingContext: "parallel development machine smoke; not a performance result. Another development " +
			"window was building and testing on the same machine during this run.",
	}
	recorder.event("stage-timing", map[string]any{
		"stages": timing,
		"preconditions": map[string]any{
			"agent_session_created_before_the_clock_started": true,
			"session_id_used": sessionID != "",
			"measured_window": "from immediately before the upload request to the moment the target source was verified",
		},
		"note": "the end-to-end value is the headline; the sub-intervals are supplementary and do not sum to it",
	})
	recorder.event("task-outcome", map[string]any{
		"document_id": id, "index_status": ready.View["index_status"],
		"version": ready.View["version"], "active_version": ready.View["active_version"],
		"chunk_count": ready.View["chunk_count"], "chunks_stored": len(chunks),
		"chunks_returned_to_retrieval": len(attempt.Sources),
		"target_document_in_sources":   true,
		"answer_basis":                 attempt.Data["answerBasis"],
		"tool_calls":                   attempt.Data["toolCalls"], "model_calls": attempt.Data["modelCalls"],
		"chunk_order": "ascending index, as produced by the original chunker",
	})
	recorder.event("embedding-and-reuse", map[string]any{
		"embedding_calls": counts.Calls, "embedding_ingest_calls": counts.IngestCalls, "embedding_query_calls": counts.QueryCalls,
		"embedding_texts": counts.Texts, "embedding_bytes": counts.Bytes,
		"bytes_note":   "UTF-8 bytes of embedder input; not tokens and not a price",
		"chunks_total": len(chunks), "chunks_reused": 0, "chunks_embedded": len(chunks),
		"reuse_note": "a first version has no active version to reuse from, so reuse is not_applicable by contract",
	})
	// The fields this round cannot observe are null with a reason. Zero would read as "the
	// measurement was taken and came out zero", which is a different claim.
	recorder.event("unmeasured-signals", map[string]any{
		"goroutines":  nil,
		"connections": nil,
		"queue_depth": nil,
		"peak_memory": nil,
		"cpu_time":    nil,
		"reason": "these are not observable from inside the test process. Resource sampling is done " +
			"from outside, over the whole go test process tree; the values it cannot read are " +
			"recorded there as null with the platform reason, and a sampled peak is not an absolute peak.",
	})
	recorder.event("fake-store-counters", map[string]any{
		"store_calls": storeCalls, "vector_search_calls": vectorCalls, "text_search_calls": textCalls,
		"deleted_versions": deleted, "load_version_calls": loadCalls, "ensure_index_calls": ensureCalls,
		"chunks_held": world.store.totalChunks(),
	})
}

// TestKnowledgeBaselineMeasurementRepeatsTheSameScenario is the repeatability check the tool
// needs before anyone runs an experiment with it: the same synthetic input must produce the
// same chunk count and the same chunk digest on independent runs. If it does not, no number
// this tool prints is comparable between two runs.
func TestKnowledgeBaselineMeasurementRepeatsTheSameScenario(t *testing.T) {
	recorder := newBaselineRecorder(t, "measurement-repeatability")
	defer recorder.flush(t)

	content := baselineSyntheticMarkdown("可重复性", "丙")
	digests := make([][]baselineChunkDigest, 0, 2)
	for run := 1; run <= 2; run++ {
		world := newBaselineWorld(t, baselineOptions{})
		token := world.login("learner@invalid.example")
		status, body := world.upload(token, "repeat.md", content)
		if status != 201 {
			t.Fatalf("run %d upload: %d", run, status)
		}
		id := docView(t, body)["id"].(string)
		world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
		digests = append(digests, baselineDigestChunks(world.store.versionChunks(1, id, 1)))
	}
	identical := baselineDigestEqual(digests[0], digests[1])
	recorder.event("repeatability", map[string]any{
		"runs": 2, "chunk_count": len(digests[0]),
		"digests_identical": identical,
		"run_1_digest":      digests[0], "run_2_digest": digests[1],
	})
	if !identical {
		t.Fatal("the same input produced different chunk digests on two runs, so no timing from this tool is comparable")
	}
}

// TestKnowledgeBaselineEndToEndTimingNeedsAVerifiedTarget states the rule the headline number
// depends on: a duration is only recorded when the target document was actually verified in
// the result. A run that times out waiting for the document, or whose query returns nothing
// usable, must not produce a successful end-to-end value.
func TestKnowledgeBaselineEndToEndTimingNeedsAVerifiedTarget(t *testing.T) {
	recorder := newBaselineRecorder(t, "measurement-timing-guard")
	defer recorder.flush(t)

	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")
	status, body := world.upload(token, "timing-guard.md", baselineSyntheticMarkdown("计时前提", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d", status)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)

	// A result that names no allowed document cannot yield a duration. The verifier is asked
	// for its verdict rather than being allowed to fail the test, so the guard itself is what
	// is being checked here.
	baselineSourceObserver = func(sources []any) []any { return []any{} }
	t.Cleanup(func() { baselineSourceObserver = nil })
	attempt := world.retrieveRaw(token, "计时前提")
	baselineSourceObserver = nil
	verdict := baselineVerifySources(baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the document under test"}},
		MustInclude: []string{id},
	}, attempt.Sources)
	if !verdict.Failed() {
		t.Fatal("a result with no target source was accepted, so an end-to-end duration could be recorded without one")
	}
	recorder.event("timing-guard", map[string]any{
		"document_id":                      id,
		"result_without_target_rejected":   true,
		"end_to_end_recorded_for_this_run": false,
		"note":                             "the guard is a precondition of the headline duration: no verified target, no successful end-to-end value",
	})
}

// TestKnowledgeBaselineTwoRunsDoNotShareEventFiles is the separation check the tool needs
// before any two runs can be compared. It runs the smoke scenario as a real subprocess three
// times: twice into two different run directories with two different run ids, and once more
// into the first run's directory under a third run id, which must be refused.
//
// The subprocess is this test binary re-entered with -test.run, so the check exercises the same
// recorder and the same environment plumbing a scripted run uses, rather than a reimplementation
// of them.
func TestKnowledgeBaselineTwoRunsDoNotShareEventFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("re-entering the test binary is a subprocess check; it is skipped in short mode")
	}
	recorder := newBaselineRecorder(t, "measurement-run-separation")
	defer recorder.flush(t)

	root := t.TempDir()
	firstDir := filepath.Join(root, "run-a", "events")
	secondDir := filepath.Join(root, "run-b", "events")

	first := baselineRunScenarioAsSubprocess(t, firstDir, "run-a")
	second := baselineRunScenarioAsSubprocess(t, secondDir, "run-b")
	if first.ExitCode != 0 {
		t.Fatalf("the first run exited %d:\n%s", first.ExitCode, first.Output)
	}
	if second.ExitCode != 0 {
		t.Fatalf("the second run exited %d:\n%s", second.ExitCode, second.Output)
	}

	firstRecords := baselineReadEvents(t, filepath.Join(firstDir, "measurement-smoke.jsonl"))
	secondRecords := baselineReadEvents(t, filepath.Join(secondDir, "measurement-smoke.jsonl"))
	if len(firstRecords) == 0 || len(secondRecords) == 0 {
		t.Fatalf("a run produced no events: %d and %d", len(firstRecords), len(secondRecords))
	}
	for _, record := range firstRecords {
		if record["run_id"] != "run-a" {
			t.Fatalf("the first run's file holds a record from run %v", record["run_id"])
		}
	}
	for _, record := range secondRecords {
		if record["run_id"] != "run-b" {
			t.Fatalf("the second run's file holds a record from run %v", record["run_id"])
		}
	}
	if len(firstRecords) != len(secondRecords) {
		t.Fatalf("the two runs recorded different numbers of events: %d and %d", len(firstRecords), len(secondRecords))
	}
	firstBefore := baselineFileDigest(t, filepath.Join(firstDir, "measurement-smoke.jsonl"))

	// The third invocation names a run of its own but writes into the first run's directory.
	// It must fail, and it must leave the first run's file byte-identical: a refusal that had
	// already appended before noticing would be worse than no check at all.
	third := baselineRunScenarioAsSubprocess(t, firstDir, "run-c")
	if third.ExitCode == 0 {
		t.Fatalf("a third run was allowed to write into the first run's event directory:\n%s", third.Output)
	}
	if !strings.Contains(third.Output, "already holds records from run") {
		t.Fatalf("the third run failed, but not for the separation rule:\n%s", third.Output)
	}
	firstAfter := baselineFileDigest(t, filepath.Join(firstDir, "measurement-smoke.jsonl"))
	if firstBefore != firstAfter {
		t.Fatal("the refused run changed the first run's event file")
	}

	recorder.event("run-separation", map[string]any{
		"first_run_id": "run-a", "second_run_id": "run-b",
		"first_events": len(firstRecords), "second_events": len(secondRecords),
		"event_counts_equal":                      len(firstRecords) == len(secondRecords),
		"records_carry_their_run":                 true,
		"third_run_into_first_directory_refused":  true,
		"third_run_exit_code":                     third.ExitCode,
		"first_run_file_unchanged_by_the_refusal": firstBefore == firstAfter,
		"note": "the runs are the same scenario on purpose: the check is that identical work in two run " +
			"directories stays two bodies of evidence, and that a third run cannot join either",
	})
}

// TestKnowledgeBaselineEventFileOwnershipRule exercises the separation rule directly, including
// the two shapes the subprocess check cannot easily produce: a file whose records carry no run
// id, and a file that is not JSON at all.
func TestKnowledgeBaselineEventFileOwnershipRule(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scenario.jsonl")

	if err := baselineCheckEventFileOwnership(path, "run-a"); err != nil {
		t.Fatalf("a file that does not exist yet was refused: %v", err)
	}
	write := func(content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := func(runID string) string {
		if runID == "" {
			return `{"event":"x","at":"2026-01-01T00:00:00Z"}` + "\n"
		}
		return `{"event":"x","at":"2026-01-01T00:00:00Z","run_id":"` + runID + `"}` + "\n"
	}

	write(record("run-a"))
	if err := baselineCheckEventFileOwnership(path, "run-a"); err != nil {
		t.Fatalf("the same run was refused its own file: %v", err)
	}
	if err := baselineCheckEventFileOwnership(path, "run-b"); err == nil {
		t.Fatal("a different run was allowed to append to run-a's records")
	}
	if err := baselineCheckEventFileOwnership(path, ""); err == nil {
		t.Fatal("a run that never named itself was allowed to append to records it cannot be checked against")
	}
	write(record("run-a") + record("run-b"))
	if err := baselineCheckEventFileOwnership(path, "run-a"); err == nil {
		t.Fatal("a file mixing two runs was accepted, so a merged file would go unnoticed")
	}
	write("{not json}\n")
	if err := baselineCheckEventFileOwnership(path, "run-a"); err == nil {
		t.Fatal("an unreadable event file was accepted")
	}
	write("")
	if err := baselineCheckEventFileOwnership(path, "run-a"); err != nil {
		t.Fatalf("an empty file belongs to nobody and must be writable: %v", err)
	}
}

// baselineSubprocessRun is one re-entered test-binary invocation.
type baselineSubprocessRun struct {
	ExitCode int
	Output   string
}

// baselineRunScenarioAsSubprocess re-enters this test binary to run the smoke scenario with the
// given event directory and run id. The dataset variables are cleared: this check must not
// depend on the frozen notes being present.
func baselineRunScenarioAsSubprocess(t *testing.T, eventsDir, runID string) baselineSubprocessRun {
	t.Helper()
	command := exec.Command(os.Args[0],
		"-test.run", "^TestKnowledgeBaselineMeasurementSmoke$",
		"-test.count", "1", "-test.timeout", "300s")
	command.Env = append(baselineEnvWithout("GOPHERLEARN_DOC_DATASET_ROOT", "GOPHERLEARN_DOC_DATASET_SPLIT"),
		"GOPHERLEARN_DOC_B01_OUT="+eventsDir,
		"GOPHERLEARN_DOC_B01_RUN_ID="+runID,
		"GOPHERLEARN_DOC_B01_POLLUTION=",
	)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			t.Fatalf("could not re-enter the test binary: %v", err)
		}
	}
	return baselineSubprocessRun{ExitCode: code, Output: string(output)}
}

func baselineEnvWithout(names ...string) []string {
	dropped := make(map[string]bool, len(names))
	for _, name := range names {
		dropped[name] = true
	}
	kept := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if !dropped[key] {
			kept = append(kept, entry)
		}
	}
	return kept
}

func baselineReadEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	records := make([]map[string]any, 0)
	for index, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("%s line %d: %v", path, index+1, err)
		}
		records = append(records, record)
	}
	return records
}

func baselineFileDigest(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// baselineWriteSummaryCSV renders the events a run produced into its summary CSV. It is
// called by the report script, not by a test, so that a test run never rewrites a delivered
// artefact.
func baselineWriteSummaryCSV(outDir, target string) error {
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return err
	}
	rows := [][]string{{"scenario", "event", "key", "value"}}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(outDir, entry.Name()))
		if err != nil {
			return err
		}
		scenario := strings.TrimSuffix(entry.Name(), ".jsonl")
		for _, line := range strings.Split(string(content), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				return fmt.Errorf("%s: %w", entry.Name(), err)
			}
			event := fmt.Sprint(record["event"])
			keys := make([]string, 0, len(record))
			for key := range record {
				if key == "event" || key == "at" || key == "run_id" {
					continue
				}
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				encoded, err := json.Marshal(record[key])
				if err != nil {
					return err
				}
				rows = append(rows, []string{scenario, event, key, string(encoded)})
			}
		}
	}
	var builder strings.Builder
	for _, row := range rows {
		for index, cell := range row {
			if index > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(baselineCSVCell(cell))
		}
		builder.WriteByte('\n')
	}
	return os.WriteFile(target, []byte(builder.String()), 0o600)
}

func baselineCSVCell(value string) string {
	if strings.ContainsAny(value, ",\"\n") {
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	return value
}
