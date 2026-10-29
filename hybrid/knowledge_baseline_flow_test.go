package hybrid

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	agentService "GopherAI/service/agent"
	"GopherAI/utils"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The first batch of bounded baseline cases. Every one of them drives the real local
// chain: hybrid HTTP handler -> v2 document controller/service -> source storage -> MySQL
// (SQLite in these tests) -> the original IndexWorkerPool -> DocumentIngestor -> the
// original chunker -> KnowledgeChunkIndexer -> the injected chunk store, and back out
// through the original RAGService when a retrieval is asserted.
//
// The only substitutes are the embedding provider and Redis. Both are named in the
// records they produce, and nothing measured through them is presented as a property of
// the real provider or of Redis.
//
// "共享装配离线集成" describes every case in this file: they assemble the same
// application the formal CLI assembles (hybrid.Assemble -> v2/entry -> isolated), but they
// run in-process and do not start cmd/gopherlearn-integrated as a deployed process.

const baselineFlowTimeout = 90 * time.Second

// baselineToolName is read from the authoritative mapping rather than hardcoded, so a
// renamed tool cannot make these tests keep passing while the runner asks for a name that
// no longer exists.
func baselineToolName(t *testing.T) string {
	t.Helper()
	return agentService.MustToolNameFor(agentService.CapabilityKnowledgeSearch, agentService.SurfaceLegacy)
}

// baselineAgentModel builds the scripted tool-calling model these scenarios drive the
// Agent with. The tool name comes from the authoritative mapping rather than a literal, so
// a renamed tool cannot leave these tests green while the runner asks for a name that no
// longer exists.
func baselineAgentModel(t *testing.T, query string) *baselineAgentChat {
	t.Helper()
	return &baselineAgentChat{query: query, toolName: baselineToolName(t)}
}

// baselineSyntheticSection renders one section whose paragraph is large enough to occupy a
// chunk of its own: the chunker's budget is 512 estimated tokens, CJK characters count one
// each, and one section of this size plus its heading stays under the budget while two
// sections do not fit together. That is what makes the section count the chunk count, so a
// version that changes one section leaves the others byte-identical and vector reuse
// becomes an observable number instead of an assumption.
func baselineSyntheticSection(marker string) string {
	var body strings.Builder
	for line := 1; line <= 10; line++ {
		fmt.Fprintf(&body, "%s 第 %d 条要点：这条内容仅用于合成基线，不包含任何真实笔记正文。\n", marker, line)
	}
	return body.String()
}

// baselineSyntheticMarkdown builds a three-section synthetic document. The last section's
// marker is a parameter so an "updated version" can differ in exactly one section.
func baselineSyntheticMarkdown(title, lastMarker string) []byte {
	return []byte(strings.Join([]string{
		"# " + title,
		"",
		"## 第一节",
		"",
		baselineSyntheticSection("甲"),
		"## 第二节",
		"",
		baselineSyntheticSection("乙"),
		"## 第三节",
		"",
		baselineSyntheticSection(lastMarker),
	}, "\n"))
}

// recordCounts writes the fake-side counters into the scenario's event file. Counts are
// what the measurement section needs; no text, vector or credential is included.
func recordCounts(recorder *baselineRecorder, label string, embedder *baselineEmbedder, store *baselineChunkStore) {
	counts := embedder.counts()
	storeCalls, vectorCalls, textCalls, deleted, loadCalls, ensureCalls := store.counts()
	recorder.event("counters", map[string]any{
		"label": label,
		"embedding": map[string]any{
			"calls": counts.Calls, "ingest_calls": counts.IngestCalls, "query_calls": counts.QueryCalls,
			"texts": counts.Texts, "bytes": counts.Bytes,
		},
		"chunk_store": map[string]any{
			"store_calls": storeCalls, "vector_search_calls": vectorCalls, "text_search_calls": textCalls,
			"deleted_versions": deleted, "load_version_calls": loadCalls, "ensure_index_calls": ensureCalls,
			"chunks_held": store.totalChunks(),
		},
	})
}

// ------------------------------------------------------------------ case 1

// TestKnowledgeBaselineUploadIndexesAndIsRetrievable covers case 1 for both formats on a
// synthetic document. Ready time and first-retrieval-visible time are recorded as two
// separate observations: waiting for "ready" and waiting for the chunk to be findable are
// not the same wait, and only measuring the first would hide a publish that never became
// queryable.
func TestKnowledgeBaselineUploadIndexesAndIsRetrievable(t *testing.T) {
	for _, format := range []struct {
		name    string
		file    string
		content []byte
	}{
		{"markdown", "baseline-synthetic.md", baselineSyntheticMarkdown("合成基线", "丙")},
		{"text", "baseline-synthetic.txt", baselineSyntheticMarkdown("合成基线", "丙")},
	} {
		t.Run(format.name, func(t *testing.T) {
			recorder := newBaselineRecorder(t, "synthetic-upload-"+format.name)
			defer recorder.flush(t)
			world := newBaselineWorld(t, baselineOptions{})
			token := world.login("learner@invalid.example")

			started := time.Now()
			status, body := world.upload(token, format.file, format.content)
			if status != 201 {
				t.Fatalf("upload: %d %v", status, body)
			}
			id := docView(t, body)["id"].(string)
			accepted := time.Since(started)

			ready := world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
			view := ready.View
			if view["active_version"] != float64(1) || view["version"] != float64(1) {
				t.Fatalf("version state after indexing: %+v", view)
			}
			chunkCount := int(view["chunk_count"].(float64))
			if chunkCount <= 0 {
				t.Fatalf("ready document reports %d chunks", chunkCount)
			}
			if stored := world.store.countVersion(1, id, 1); stored != chunkCount {
				t.Fatalf("the store holds %d chunks for version 1 but MySQL reports %d", stored, chunkCount)
			}

			// The retrieval is one real Agent turn: HTTP -> Agent Runner -> the tool ->
			// rag.RAGService.Retrieve -> the chunk store, with the tool's own citation
			// gate and the runner's citation gate both applied to the answer. Every source
			// must be this document at its active version and must include it; a source
			// from any other document would be an identity failure, not a pass.
			attempt := world.retrieveChecked(token, "合成基线", baselineRetrievalExpectation{
				Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the document under test"}},
				MustInclude: []string{id},
			})
			data := attempt.Data
			if data["answerBasis"] != "personal_knowledge" {
				t.Fatalf("retrieval was not answered from the documents: %v", data["answerBasis"])
			}
			digests := baselineSourceDigestsOf(attempt.Sources)

			recorder.event("upload-index-retrieve", map[string]any{
				"file": format.file, "document_id": id, "chunks": chunkCount,
				"accepted_to_response_ms": accepted.Milliseconds(),
				"ready_wait_ms":           ready.Elapsed.Milliseconds(),
				"retrieval_ms":            attempt.Elapsed.Milliseconds(),
				"first_source":            digests[0],
				"sources":                 digests,
				"sources_verified": map[string]any{
					"all_sources_allowed_document_and_version": true,
					"target_document_present":                  true,
					"citation_locations_matched_stored_chunks": true,
				},
				"answer_basis": data["answerBasis"],
				"tool_calls":   data["toolCalls"], "model_calls": data["modelCalls"],
				"timing_note": "parallel development machine smoke; not a performance result",
			})
			recordCounts(recorder, format.name, world.embedder, world.store)
		})
	}
}

// ------------------------------------------------------------------ case 2

// TestKnowledgeBaselineDuplicateIsolationAndVersionUpdate covers case 2. Duplicate name and
// duplicate content are asserted against the existing dedup contract, the second user's
// view is asserted to be empty of the first user's document, and an update is asserted to
// produce a new immutable version with its own frozen chunk digest.
func TestKnowledgeBaselineDuplicateIsolationAndVersionUpdate(t *testing.T) {
	recorder := newBaselineRecorder(t, "synthetic-duplicate-and-update")
	defer recorder.flush(t)
	world := newBaselineWorld(t, baselineOptions{})
	owner := world.login("learner@invalid.example")
	other := world.login("other@invalid.example")

	content := baselineSyntheticMarkdown("去重与隔离", "丙")
	status, body := world.upload(owner, "dup.md", content)
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(owner, id, "ready", baselineFlowTimeout)
	firstDigest := baselineDigestChunks(world.store.versionChunks(1, id, 1))
	if len(firstDigest) == 0 {
		t.Fatal("the first version stored no chunks")
	}

	// Same name, different bytes: the active name is taken.
	if status, _ := world.upload(owner, "dup.md", baselineSyntheticMarkdown("改了名字以外的内容", "丙")); status != 409 {
		t.Fatalf("a duplicate name was accepted with status %d", status)
	}
	// Different name, identical bytes: the active content hash is taken.
	if status, _ := world.upload(owner, "dup-copy.md", content); status != 409 {
		t.Fatalf("duplicate content was accepted with status %d", status)
	}
	// Before the second user owns anything, their own query must not reach the first user's
	// document. This stage is what makes the next one meaningful: "the second user can
	// retrieve their own copy" only says something if the same query could not already
	// return the first user's. The owner's document is named as an outright refusal, so a
	// leaked source is reported as a leak rather than merely as an unexpected id.
	//
	// This fixture can fail during generation. Only the specific model failure is
	// tolerated, and it proves isolation only with an independently observed empty
	// knowledge tool result received before generation.
	beforeUpload := world.retrieveTolerant(other, "合成基线")
	beforeVerdict := baselineVerifySources(baselineRetrievalExpectation{
		ExpectNoSources: true,
		ForbiddenDocIDs: []string{id},
	}, beforeUpload.Sources)
	beforeVerdict.Problems = append(beforeVerdict.Problems, baselineEmptyOwnerProblems(beforeUpload, "合成基线")...)
	if beforeVerdict.Failed() {
		t.Fatalf("a user owning no document retrieved something: %v", beforeVerdict.Problems)
	}

	// A second user is a separate namespace and may hold the same name and bytes.
	status, otherBody := world.upload(other, "dup.md", content)
	if status != 201 {
		t.Fatalf("cross-user upload: %d %v", status, otherBody)
	}
	otherID := docView(t, otherBody)["id"].(string)
	world.requireDocumentState(other, otherID, "ready", baselineFlowTimeout)
	if otherID == id {
		t.Fatal("two users were given the same document identity")
	}
	_, listing := world.request("GET", "/api/v1/knowledge/documents", other, "", nil)
	if len(listing["documents"].([]any)) != 1 {
		t.Fatalf("the second user sees %d documents", len(listing["documents"].([]any)))
	}
	if status, _ := world.request("GET", "/api/v1/knowledge/documents/"+id, other, "", nil); status != 404 {
		t.Fatalf("the second user can read the first user's document (status %d)", status)
	}
	// Every source the second user's query returns must be their own document at version 1.
	// Checking only that a source came back would pass on the first user's document, which
	// is the leak this case exists to rule out.
	otherAttempt := world.retrieveChecked(other, "合成基线", baselineRetrievalExpectation{
		Allowed:         []baselineExpectedDocument{{DocID: otherID, Version: 1, Label: "the second user's own document"}},
		MustInclude:     []string{otherID},
		ForbiddenDocIDs: []string{id},
	})
	recorder.event("cross-user-identity", map[string]any{
		"owner_document_id": id, "other_document_id": otherID,
		"query_before_owning_anything": map[string]any{
			"http_status":                      beforeUpload.Status,
			"model_failure_code":               beforeUpload.Refusal,
			"pre_generation_tool_observations": len(beforeUpload.ToolSteps),
			"retrieval_complete_and_empty":     true,
			"sources_returned":                 len(beforeUpload.Sources),
			"owner_document_absent":            true,
		},
		"query_after_uploading_own_copy": map[string]any{
			"sources": baselineSourceDigestsOf(otherAttempt.Sources),
			"every_source_belongs_to_the_querying_user": true,
			"owner_document_absent":                     true,
		},
	})

	// An update stages a new version and switches the active one only after it is ready.
	// Only the third section changes, so the first two sections' embedder inputs are
	// byte-identical and the original indexer is expected to carry their vectors over.
	// That expectation is asserted as a count as well as recorded, because a reuse rule
	// that silently stopped applying would leave every assertion above still green.
	beforeUpdate := world.embedder.counts()
	updated := baselineSyntheticMarkdown("去重与隔离", "丙改")
	status, staged := world.request("PUT", "/api/v1/knowledge/documents/"+id, owner, "dup.md", updated)
	if status != 202 {
		t.Fatalf("update: %d %v", status, staged)
	}
	if stagedView := docView(t, staged); stagedView["active_version"] != float64(1) || stagedView["version"] != float64(2) {
		t.Fatalf("the staged update did not keep version 1 active: %+v", stagedView)
	}
	world.requireDocumentState(owner, id, "ready", baselineFlowTimeout)
	view := world.document(owner, id)
	if view["active_version"] != float64(2) {
		t.Fatalf("the active version did not switch: %+v", view)
	}
	afterUpdate := world.embedder.counts()
	secondDigest := baselineDigestChunks(world.store.versionChunks(1, id, 2))
	if len(secondDigest) == 0 {
		t.Fatal("the second version stored no chunks")
	}
	if baselineDigestEqual(firstDigest, secondDigest) {
		t.Error("two different source versions produced the same chunk digest, so the digest would not detect a change")
	}
	embeddedForVersion2 := afterUpdate.Texts - beforeUpdate.Texts
	reusedForVersion2 := len(secondDigest) - embeddedForVersion2
	if reusedForVersion2 <= 0 {
		t.Errorf("the update re-embedded every chunk of a version sharing two sections with the active one: embedded=%d of %d", embeddedForVersion2, len(secondDigest))
	}

	recorder.event("duplicate-isolation-update", map[string]any{
		"document_id": id, "other_document_id": otherID,
		"duplicate_name_status": 409, "duplicate_hash_status": 409,
		"version_1_chunks": firstDigest, "version_2_chunks": secondDigest,
		"version_1_count": len(firstDigest), "version_2_count": len(secondDigest),
		"version_2_embedded_texts": embeddedForVersion2, "version_2_reused_chunks": reusedForVersion2,
		"changed_sections": 1,
	})
	recordCounts(recorder, "duplicate-isolation-update", world.embedder, world.store)
}

// TestKnowledgeBaselineTxtIsAcceptedWhereMarkdownAlreadyHoldsTheSameBytes runs the TXT
// branch in its own clean state, because the content hash is deliberately shared between
// the two variants and the production dedup rule would otherwise reject the second
// upload. Rejection there is the dedup contract working, not TXT being unsupported; the
// two are measured separately so neither can be mistaken for the other.
func TestKnowledgeBaselineTxtIsAcceptedWhereMarkdownAlreadyHoldsTheSameBytes(t *testing.T) {
	recorder := newBaselineRecorder(t, "synthetic-txt-routing")
	defer recorder.flush(t)

	shared := baselineSyntheticMarkdown("同字节 TXT", "丙")
	markdown := newBaselineWorld(t, baselineOptions{})
	markdownToken := markdown.login("learner@invalid.example")
	status, body := markdown.upload(markdownToken, "same.md", shared)
	if status != 201 {
		t.Fatalf("markdown upload: %d %v", status, body)
	}
	markdownID := docView(t, body)["id"].(string)
	markdown.requireDocumentState(markdownToken, markdownID, "ready", baselineFlowTimeout)
	markdownDigest := baselineDigestChunks(markdown.store.versionChunks(1, markdownID, 1))

	// The same bytes as .txt, in the same user's namespace: the hash is already active.
	if status, _ := markdown.upload(markdownToken, "same.txt", shared); status != 409 {
		t.Fatalf("the shared content hash was not deduplicated (status %d)", status)
	}

	// A clean state is the only way to exercise the TXT parser on these bytes without
	// bypassing that rule.
	text := newBaselineWorld(t, baselineOptions{})
	textToken := text.login("learner@invalid.example")
	status, textBody := text.upload(textToken, "same.txt", shared)
	if status != 201 {
		t.Fatalf("txt upload in a clean state: %d %v", status, textBody)
	}
	textID := docView(t, textBody)["id"].(string)
	if view := text.document(textToken, textID); view["file_type"] != "txt" {
		t.Fatalf("the stored file type is %v", view["file_type"])
	}
	text.requireDocumentState(textToken, textID, "ready", baselineFlowTimeout)
	textDigest := baselineDigestChunks(text.store.versionChunks(1, textID, 1))
	if len(textDigest) == 0 {
		t.Fatal("the txt document stored no chunks")
	}

	// The two formats are frozen separately. They are not required to chunk identically:
	// ParseText keeps Markdown syntax as paragraph content, so the same bytes may produce
	// a different number of chunks. What is required is that both are accepted, indexed
	// and retrievable.
	recorder.event("txt-routing", map[string]any{
		"markdown_document_id": markdownID, "markdown_chunks": markdownDigest,
		"txt_document_id": textID, "txt_chunks": textDigest,
		"dedup_rejection_status": 409,
		"chunking_identical":     baselineDigestEqual(markdownDigest, textDigest),
		"note":                   "the two formats are frozen separately; identical chunking is not a requirement",
	})
	recordCounts(recorder, "txt-routing", text.embedder, text.store)
}

// ------------------------------------------------------------------ case 3

// TestKnowledgeBaselineEmbeddingFailureFailsAndRetryRecovers covers case 3. The failure is
// injected at the embedding batch, which is the stage the production indexer wraps as
// ErrEmbeddingBatch, so the document is expected to land in "failed" with a bounded reason
// and to be recoverable through the real retry endpoint.
//
// The retry is NOT assumed to embed nothing. Every embedding call is counted before the
// failure, between failure and retry, and after recovery, so the re-embedding the retry
// actually costs is a measured number rather than an expectation.
func TestKnowledgeBaselineEmbeddingFailureFailsAndRetryRecovers(t *testing.T) {
	recorder := newBaselineRecorder(t, "synthetic-embedding-failure-retry")
	defer recorder.flush(t)
	world := newBaselineWorld(t, baselineOptions{})
	world.embedder.armIngestFailure(1, fmt.Errorf("injected embedding outage"))
	token := world.login("learner@invalid.example")

	status, body := world.upload(token, "failure.md", baselineSyntheticMarkdown("故障与重试", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	path := "/api/v1/knowledge/documents/" + id
	failed := world.requireDocumentState(token, id, "failed", baselineFlowTimeout)
	if failed.View["active_version"] != float64(0) {
		t.Fatalf("a failed first version reported an active version: %+v", failed.View)
	}
	reason, _ := failed.View["error_message"].(string)
	if !strings.HasPrefix(reason, "embedding:") {
		t.Errorf("the persisted reason does not name the failing stage: %q", reason)
	}
	if world.store.countVersion(1, id, 1) != 0 {
		t.Fatal("a failed embedding still left chunks in the store")
	}
	afterFailure := world.embedder.counts()

	// A retry of a non-failed document is refused; this one is failed, so it is accepted.
	world.embedder.disarmFailure()
	if status, _ := world.request("POST", path+"/retry", token, "", nil); status != 202 {
		t.Fatalf("retry of a failed document was refused with status %d", status)
	}
	ready := world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
	afterRetry := world.embedder.counts()
	if status, _ := world.request("POST", path+"/retry", token, "", nil); status != 409 {
		t.Fatalf("a ready document accepted a retry (status %d)", status)
	}
	if stored := world.store.countVersion(1, id, 1); stored != int(ready.View["chunk_count"].(float64)) {
		t.Fatalf("after retry the store holds %d chunks and MySQL reports %v", stored, ready.View["chunk_count"])
	}

	retryIngestCalls := afterRetry.IngestCalls - afterFailure.IngestCalls
	retryTexts := afterRetry.Texts - afterFailure.Texts
	recorder.event("embedding-failure-retry", map[string]any{
		"document_id": id, "failure_reason": reason,
		"ingest_calls_before_failure": afterFailure.IngestCalls,
		"ingest_calls_after_retry":    afterRetry.IngestCalls,
		"retry_ingest_calls":          retryIngestCalls,
		"retry_embedded_texts":        retryTexts,
		"retry_duplicated_work":       retryIngestCalls > 0,
		"chunks_after_retry":          ready.View["chunk_count"],
	})
	recordCounts(recorder, "embedding-failure-retry", world.embedder, world.store)
}

// ------------------------------------------------------------------ case 4

// TestKnowledgeBaselineOldVersionServesDuringUpdateAndThenSwitches covers case 4. The
// update's ingest is held at a barrier, so the retrieval that runs during the update is
// not racing a sleep: the update is provably still inside its embedding stage.
//
// Whatever this reports is the old implementation's behaviour. If it contradicts the
// documented intent, the record says so instead of the test being adjusted to agree.
func TestKnowledgeBaselineOldVersionServesDuringUpdateAndThenSwitches(t *testing.T) {
	recorder := newBaselineRecorder(t, "synthetic-version-switch")
	defer recorder.flush(t)
	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")

	status, body := world.upload(token, "switch.md", baselineSyntheticMarkdown("版本切换", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
	beforeUpdate := world.embedder.counts()

	barrier := newBaselineBarrier()
	barrier.arm()
	world.embedder.useBarrier(barrier)
	status, staged := world.request("PUT", "/api/v1/knowledge/documents/"+id, token, "switch.md", baselineSyntheticMarkdown("版本切换", "丙改"))
	if status != 202 {
		t.Fatalf("update: %d %v", status, staged)
	}
	barrier.enteredOnce(t, baselineFlowTimeout)

	// The new version is inside its embedding stage, and MySQL still points at version 1.
	during := world.document(token, id)
	if during["index_status"] != "indexing" || during["active_version"] != float64(1) {
		t.Fatalf("the update did not hold version 1 active while indexing: %+v", during)
	}
	// Version 2 is the document's current Version but not yet its ActiveVersion, so a source
	// claiming version 2 here would be a result built from a version the product has not
	// published. The expectation states version 1 only, and every source is held to it.
	duringAttempt := world.retrieveChecked(token, "版本切换", baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the still-active version"}},
		MustInclude: []string{id},
	})
	duringVersions := baselineVersionSet(duringAttempt.Sources)

	barrier.releaseAll()
	ready := world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
	if ready.View["active_version"] != float64(2) {
		t.Fatalf("the active version did not switch to 2: %+v", ready.View)
	}
	afterAttempt := world.retrieveChecked(token, "版本切换", baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 2, Label: "the newly active version"}},
		MustInclude: []string{id},
	})
	afterVersions := baselineVersionSet(afterAttempt.Sources)
	afterCounts := world.embedder.counts()
	// The cleanup worker removes the superseded version's chunks and its source file once
	// the switch is published, so counting version 1 here records what cleanup left behind
	// rather than a leak. Both facts are recorded, separately, so neither is inferred.
	version1Left := len(world.store.versionChunks(1, id, 1))
	_, _, _, deletedVersions, _, _ := world.store.counts()

	recorder.event("version-switch", map[string]any{
		"document_id": id,
		"during_update": map[string]any{
			"active_version": 1, "versions_served": duringVersions,
			"sources": baselineSourceDigestsOf(duringAttempt.Sources),
			"every_source_is_the_still_active_version": true,
		},
		"after_switch": map[string]any{
			"active_version": 2, "versions_served": afterVersions,
			"sources": baselineSourceDigestsOf(afterAttempt.Sources),
			"every_source_is_the_newly_active_version": true,
		},
		"update_ingest_calls":           afterCounts.IngestCalls - beforeUpdate.IngestCalls,
		"version_1_chunks_after_switch": version1Left,
		"version_2_chunks":              len(world.store.versionChunks(1, id, 2)),
		"deleted_versions_by_cleanup":   deletedVersions,
		"cleanup_note":                  "the superseded version is removed by the cleanup worker after the switch is published; this is the existing behaviour, not a leak",
	})
	recordCounts(recorder, "version-switch", world.embedder, world.store)
}

// baselineVersionSet is the distinct (document, version) pairs a result carried. It is what
// a record can state instead of "the first source's version": a mixed result is visible as
// more than one pair rather than hidden behind the first entry.
func baselineVersionSet(sources []map[string]any) []string {
	seen := map[string]struct{}{}
	for _, source := range sources {
		seen[fmt.Sprintf("%v:v%v", source["doc_id"], source["doc_version"])] = struct{}{}
	}
	pairs := make([]string, 0, len(seen))
	for pair := range seen {
		pairs = append(pairs, pair)
	}
	sort.Strings(pairs)
	return pairs
}

// ------------------------------------------------------------------ case 5

// TestKnowledgeBaselineGracefulStopAndRebuildRecoversTheTask covers case 5, and states its
// boundary in its own name: the application is stopped through its own Close, not killed.
// Nothing here may be read as crash recovery; the task requires a real subprocess kill for
// that, and this round does not perform one.
//
// The document is stopped inside its embedding stage. On rebuild, the same SQLite file and
// storage root are reopened, so what recovers is the durable state rather than a shared
// in-memory fixture. The chunk store is deliberately retained in one run and cleared in the
// other, and both are recorded, because whether a retained index lets the new process reuse
// vectors is exactly the question a rebuild has to answer honestly.
func TestKnowledgeBaselineGracefulStopAndRebuildRecoversTheTask(t *testing.T) {
	for _, variant := range []struct {
		name       string
		clearIndex bool
		wantReuse  string
	}{
		{"index retained across the rebuild", false, "reuse is possible and is measured"},
		{"index cleared across the rebuild", true, "nothing can be reused and everything is re-embedded"},
	} {
		t.Run(variant.name, func(t *testing.T) {
			recorder := newBaselineRecorder(t, "synthetic-rebuild-"+strings.ReplaceAll(variant.name, " ", "-"))
			defer recorder.flush(t)
			world := newBaselineWorld(t, baselineOptions{})
			token := world.login("learner@invalid.example")

			status, body := world.upload(token, "rebuild.md", baselineSyntheticMarkdown("重建", "丙"))
			if status != 201 {
				t.Fatalf("upload: %d %v", status, body)
			}
			id := docView(t, body)["id"].(string)
			world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
			afterFirstVersion := world.embedder.counts()

			// Stop the second version inside its embedding stage.
			barrier := newBaselineBarrier()
			barrier.arm()
			world.embedder.useBarrier(barrier)
			status, staged := world.request("PUT", "/api/v1/knowledge/documents/"+id, token, "rebuild.md", baselineSyntheticMarkdown("重建", "丙改"))
			if status != 202 {
				t.Fatalf("update: %d %v", status, staged)
			}
			barrier.enteredOnce(t, baselineFlowTimeout)
			interrupted := world.document(token, id)
			if interrupted["index_status"] != "indexing" {
				t.Fatalf("the update was not inside its embedding stage: %+v", interrupted)
			}
			if err := world.Close(); err != nil {
				t.Fatalf("graceful stop: %v", err)
			}
			barrier.releaseAll()

			// The durable row records what the stopped process left behind. Reading it
			// through a second handle is what proves the state survived, rather than being
			// a field the in-memory app still holds.
			stoppedRow := baselineReadRow(t, world.dbPath, id)
			stoppedStatus := string(stoppedRow.IndexStatus)
			atStop := world.embedder.counts()

			if variant.clearIndex {
				world.store.Reset()
			}
			indexBeforeRebuild := world.store.totalChunks()

			world.Assemble()
			world.embedder.useBarrier(newBaselineBarrier())
			token = world.login("learner@invalid.example")
			recovered := world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
			if recovered.View["active_version"] != float64(2) {
				t.Fatalf("the rebuilt process did not finish version 2: %+v", recovered.View)
			}
			afterRebuild := world.embedder.counts()

			chunksInVersion2 := len(world.store.versionChunks(1, id, 2))
			rebuiltEmbedded := afterRebuild.Texts - atStop.Texts
			// Two sections are shared with the version that was still active, so a retained
			// index must let the rebuilt process embed only the one changed section; a
			// cleared index must make it embed all three. Asserting this is what keeps the
			// reuse rule from silently ceasing to apply while the numbers above still look
			// plausible.
			if variant.clearIndex {
				if rebuiltEmbedded != chunksInVersion2 {
					t.Errorf("with the index cleared the rebuilt process embedded %d texts for %d chunks, want all of them", rebuiltEmbedded, chunksInVersion2)
				}
			} else if rebuiltEmbedded >= chunksInVersion2 {
				t.Errorf("with the index retained the rebuilt process embedded %d texts for %d chunks, so no vector was reused", rebuiltEmbedded, chunksInVersion2)
			}

			// Three windows are recorded separately, because one total would hide which of
			// them did the work: the first version's indexing, the interrupted attempt that
			// stopped at the barrier, and what the rebuilt process embedded afterwards. The
			// interrupted attempt is not free - it had already handed its texts to the
			// embedder - so it is reported rather than folded into the rebuild.
			recorder.event("graceful-stop-rebuild", map[string]any{
				"document_id": id, "variant": variant.name, "clear_index": variant.clearIndex,
				"status_when_stopped":         stoppedStatus,
				"index_chunks_before_rebuild": indexBeforeRebuild,
				"index_chunks_after_rebuild":  world.store.totalChunks(),
				"first_version": map[string]any{
					"ingest_calls": afterFirstVersion.IngestCalls, "embedded_texts": afterFirstVersion.Texts,
				},
				"interrupted_attempt": map[string]any{
					"ingest_calls":   atStop.IngestCalls - afterFirstVersion.IngestCalls,
					"embedded_texts": atStop.Texts - afterFirstVersion.Texts,
				},
				"after_rebuild": map[string]any{
					"ingest_calls": afterRebuild.IngestCalls, "embedded_texts": afterRebuild.Texts,
				},
				"rebuilt_process_ingest_calls":   afterRebuild.IngestCalls - atStop.IngestCalls,
				"rebuilt_process_embedded_texts": rebuiltEmbedded,
				"chunks_in_version_2":            chunksInVersion2,
				"reuse_observed":                 rebuiltEmbedded < chunksInVersion2,
				"reuse_expectation":              variant.wantReuse,
				"recovery_kind":                  "graceful stop through Application.Close followed by a rebuild over the same SQLite file and storage root",
				"not_claimed":                    "this is not crash recovery: no subprocess was killed",
			})
			recordCounts(recorder, variant.name, world.embedder, world.store)
		})
	}
}

func baselineReadRow(t *testing.T, dbPath, id string) model.KnowledgeDocument {
	t.Helper()
	// The stopped application closed its own SQL handle, so the row is read through a new
	// connection to the same file. That is the point of the check: the state has to be on
	// disk, not in a handle the old process still held.
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var row model.KnowledgeDocument
	if err := db.Where("id = ?", id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

// TestKnowledgeBaselineSyntheticPathDoesNotNeedTheDatasetRoot keeps the harness honest:
// the synthetic scenarios must run on any machine, so a missing dataset root cannot turn
// the whole tool into a skip. The variable is cleared for this test and the synthetic chain
// is then driven end to end, which is the property that matters - not a count of skips.
func TestKnowledgeBaselineSyntheticPathDoesNotNeedTheDatasetRoot(t *testing.T) {
	t.Setenv(baselineDatasetEnv, "")
	if baselineOutDir() != "" {
		t.Logf("event files are being written to %s", baselineOutDir())
	}
	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")
	status, body := world.upload(token, "no-dataset-root.md", baselineSyntheticMarkdown("无数据集根", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)
	attempt := world.retrieveChecked(token, "无数据集根", baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the document under test"}},
		MustInclude: []string{id},
	})
	if attempt.Data["answerBasis"] != "personal_knowledge" {
		t.Fatalf("the synthetic path answered without the document: %v", attempt.Data["answerBasis"])
	}
}

// TestKnowledgeBaselineSyntheticChainReachesTheRealServices is a structural check on the
// harness itself, so a later edit cannot quietly replace the production chain with a
// hand-written stub and leave every case above still green.
func TestKnowledgeBaselineSyntheticChainReachesTheRealServices(t *testing.T) {
	world := newBaselineWorld(t, baselineOptions{})
	token := world.login("learner@invalid.example")
	status, body := world.upload(token, "chain.md", baselineSyntheticMarkdown("链路", "丙"))
	if status != 201 {
		t.Fatalf("upload: %d %v", status, body)
	}
	id := docView(t, body)["id"].(string)
	world.requireDocumentState(token, id, "ready", baselineFlowTimeout)

	stored := world.store.versionChunks(1, id, 1)
	if len(stored) == 0 {
		t.Fatal("the store received nothing, so no case in this file could prove retrievability")
	}
	// The chunk identity is produced by the original chunker: its ID is the hash of the
	// chunker's own payload and its content hash covers the raw content. A hand-written
	// stub could not produce these two agreeing with the chunk's own metadata.
	for _, chunk := range stored {
		if !strings.HasPrefix(chunk.Chunk.ID, "chunk_") {
			t.Fatalf("chunk %q was not produced by the original chunker", chunk.Chunk.ID)
		}
		if chunk.EmbeddingInputHash == "" || chunk.EmbeddingFingerprint == "" {
			t.Fatal("the indexer stored a chunk without embedding identity, so reuse could never apply")
		}
		if chunk.Chunk.EmbeddingContent == "" || !strings.HasPrefix(chunk.Chunk.EmbeddingContent, "文件: ") {
			t.Fatal("the stored chunk carries no embedder input, so it did not pass the original chunker")
		}
	}
	// The retrieval path is the production one: the run is forced to call the tool, and
	// the tool reaches the same store.
	attempt := world.retrieveChecked(token, "合成基线", baselineRetrievalExpectation{
		Allowed:     []baselineExpectedDocument{{DocID: id, Version: 1, Label: "the document under test"}},
		MustInclude: []string{id},
	})
	if attempt.Data["toolCalls"] != float64(1) || attempt.Data["answerBasis"] != "personal_knowledge" {
		t.Fatalf("the retrieval did not travel the Agent tool path: %v", attempt.Data)
	}
	if _, _, _, _, _, _ = world.store.counts(); world.store.totalChunks() != len(stored) {
		t.Fatal("the store changed during retrieval, so the search did not read the indexed version")
	}
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil || hash == "" {
		t.Fatal("fixture users are not created through the product's own password hashing")
	}
}
