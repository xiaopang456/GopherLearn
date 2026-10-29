package episoderelation

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	episodedao "GopherAI/dao/episode"
	episodeassertdao "GopherAI/dao/episodeassert"
	"GopherAI/model"
	"GopherAI/service/episode"
	"GopherAI/service/episodeassert"
	"GopherAI/service/episoderelation"
)

// STABILIZE-065 Phase 1.1.1, blocker 2: the relation pass, driven from the
// worker.
//
// Phase 1.1 wired UseRelationWriter and then tested the generator by calling
// Generate directly. That proves the generator works. It does not prove the
// worker calls it, that a relation failure retries the job instead of
// completing it, or that a retry of the same job does not classify the pair
// twice - and those are properties of the worker, not of the generator.
//
// So everything below enters through worker.Start and the job queue, against a
// real MySQL, with the production projector, the production evidence loader and
// the production job repository. The one scripted part is the document
// generator, which is the step an LLM performs. External calls: none.

// --- the scripted document generator (the LLM step) ---

type chainGenerator struct {
	mu   sync.Mutex
	text map[string]string
}

func (g *chainGenerator) Generate(_ context.Context,
	evidence episode.ProjectionEvidence) (episode.ProjectionOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	text, ok := g.text[evidence.Session.ID]
	if !ok {
		return episode.ProjectionOutcome{}, fmt.Errorf("no script for session %s", evidence.Session.ID)
	}
	ids := make([]uint, 0, len(evidence.Messages))
	for _, message := range evidence.Messages {
		ids = append(ids, message.ID)
	}
	return episode.Generated(model.EpisodeDocument{
		Synopsis:      text,
		SourceHandles: []string{"session:" + evidence.Session.ID},
		Decisions: []model.EpisodeEvidence{{Text: text,
			EvidenceMessageIDs: ids}},
	}), nil
}

// chainAllowAll is the permissive support verifier these tests need after
// STABILIZE-065 Phase 2-C1.1 made one a required constructor argument. Nothing
// here is about semantic support, so the double keeps every assertion; there is
// no such verifier in production code.
type chainAllowAll struct{}

func (chainAllowAll) Verify(context.Context, episode.SupportRequest) (episode.SupportVerdict, error) {
	return episode.SupportEntailed, nil
}

// --- a recording indexer, so the order of the stages is observable ---

// chainIndexer stands in for episode.Indexer. It does the one thing that
// matters downstream: it moves the Episode's index_status, to "indexed" on
// success and "failed" otherwise, exactly as the production indexer does. An
// indexer that only counted calls would leave every fresh projection pending,
// and the governed read path refuses to read a pending Episode - so the fixture
// would be testing a state production never reaches.
type chainIndexer struct {
	db     *gorm.DB
	mu     sync.Mutex
	calls  int
	failed bool
	// failuresLeft fails that many calls and then recovers, so a test can
	// watch a retried job succeed on its second attempt rather than watching
	// it exhaust the attempt budget.
	failuresLeft int
}

func (i *chainIndexer) Index(_ context.Context, episode *model.EpisodeMemory) error {
	i.mu.Lock()
	failed := i.failed
	if i.failuresLeft > 0 {
		failed = true
		i.failuresLeft--
	}
	i.calls++
	i.mu.Unlock()
	status := model.EpisodeIndexIndexed
	if failed {
		status = model.EpisodeIndexFailed
	}
	if err := i.db.Model(&model.EpisodeMemory{}).
		Where("id = ? AND user_id = ?", episode.ID, episode.UserID).
		Update("index_status", status).Error; err != nil {
		return err
	}
	episode.IndexStatus = status
	if failed {
		return fmt.Errorf("index unavailable")
	}
	return nil
}

func (i *chainIndexer) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

// --- a relation store that can be made to fail after the projection ---

type failingProposals struct {
	inner *Repository
	mu    sync.Mutex
	fail  bool
}

func (p *failingProposals) shouldFail() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fail
}

func (p *failingProposals) SaveProposal(ctx context.Context,
	proposal model.EpisodeRelationProposal) error {
	if p.shouldFail() {
		return fmt.Errorf("relation store unavailable")
	}
	return p.inner.SaveProposal(ctx, proposal)
}

func (p *failingProposals) SaveRelation(ctx context.Context,
	relation model.EpisodeAssertionRelation) error {
	if p.shouldFail() {
		return fmt.Errorf("relation store unavailable")
	}
	return p.inner.SaveRelation(ctx, relation)
}

func (p *failingProposals) ProposalByKey(ctx context.Context, userID int64,
	key string) (model.EpisodeRelationProposal, bool, error) {
	if p.shouldFail() {
		return model.EpisodeRelationProposal{}, false,
			fmt.Errorf("relation store unavailable")
	}
	return p.inner.ProposalByKey(ctx, userID, key)
}

func (p *failingProposals) Relations(ctx context.Context, userID int64,
	sourceAssertionID string) ([]model.EpisodeAssertionRelation, error) {
	if p.shouldFail() {
		return nil, fmt.Errorf("relation store unavailable")
	}
	return p.inner.Relations(ctx, userID, sourceAssertionID)
}

func (p *failingProposals) Proposals(ctx context.Context,
	userID int64) ([]model.EpisodeRelationProposal, error) {
	if p.shouldFail() {
		return nil, fmt.Errorf("relation store unavailable")
	}
	return p.inner.Proposals(ctx, userID)
}

func (p *failingProposals) WithinTx(ctx context.Context,
	fn func(episoderelation.ProposalStore, episodeassert.Store) error) error {
	if p.shouldFail() {
		return fmt.Errorf("relation store unavailable")
	}
	return p.inner.WithinTx(ctx, fn)
}

// --- a candidate source that counts how often the worker asked for history ---

type countingSource struct {
	inner *CandidateRepository
	mu    sync.Mutex
	calls int
}

func (s *countingSource) ActiveAssertions(ctx context.Context, userID int64,
	excludeSessionID string) ([]episoderelation.Candidate, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.inner.ActiveAssertions(ctx, userID, excludeSessionID)
}

func (s *countingSource) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// --- the harness ---

const (
	chainOwner   = int64(900000000000000301)
	chainEarlyID = "sess-chain-early"
	chainLateID  = "sess-chain-late"
)

var chainBase = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

type workerChain struct {
	db         *gorm.DB
	jobs       *episodedao.Repository
	worker     *episode.Worker
	indexer    *chainIndexer
	classifier *itClassifier
	proposals  *failingProposals
	source     *countingSource
	relations  *Repository
	states     *episodeassertdao.Repository
	running    bool
}

// newWorkerChain assembles the production chain over the disposable store.
func newWorkerChain(t *testing.T, mode episoderelation.Mode,
	attachWriter bool) *workerChain {
	t.Helper()
	db := openTestDB(t)
	// The production evidence loader reads the session summary table, so the
	// disposable schema needs it too. A projector that could not find it is a
	// fixture defect, not a finding.
	if err := db.AutoMigrate(&model.EpisodeProjectionJob{}, &model.SessionSummary{}); err != nil {
		t.Fatalf("migrate the job table: %v", err)
	}
	if err := db.Exec("DELETE FROM episode_projection_jobs").Error; err != nil {
		t.Fatalf("clear the job table: %v", err)
	}

	seedSession(t, db, chainOwner, chainEarlyID, 9101, chainEarlyText, chainBase)
	lateAt := chainBase.AddDate(0, 0, 30)
	seedSession(t, db, chainOwner, chainLateID, 9102, chainLateText, lateAt)

	episodes := episodedao.NewRepository(db)
	loader, err := episode.NewDatabaseEvidenceLoader(db, 256)
	if err != nil {
		t.Fatal(err)
	}
	generator := &chainGenerator{text: map[string]string{
		chainEarlyID: chainEarlyText, chainLateID: chainLateText}}
	projector, err := episode.NewProjector(loader, generator, episodes, chainAllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := episode.NewReconciler(episodes, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	config := episode.DefaultWorkerConfig()
	config.ScanInterval = 40 * time.Millisecond
	config.ReconcileInterval = time.Hour
	config.RecoveryInterval = time.Hour
	config.RetryDelay = 50 * time.Millisecond
	config.QuietWindow = 0
	worker, err := episode.NewWorker(episodes, projector, reconciler, config)
	if err != nil {
		t.Fatal(err)
	}
	// The earlier session's Episode is the history the relation pass has to
	// find. It is created by the same production projector rather than seeded
	// as a row, because the assertion identities and their times have to come
	// from the real projection chain - a hand-written document carries a
	// legacy identity, and a pair of one legacy and one projected id is not
	// the pair production would ever form.
	if _, err := projector.Project(context.Background(), chainOwner,
		chainEarlyID, 9101); err != nil {
		t.Fatalf("seed the earlier Episode through the projector: %v", err)
	}
	// In production the indexer moves a fresh projection to "indexed". The
	// candidate source only reads indexed Episodes, so the fixture has to
	// reach the same state the real index write would have left behind.
	if err := db.Model(&model.EpisodeMemory{}).
		Where("session_id = ?", chainEarlyID).
		Update("index_status", model.EpisodeIndexIndexed).Error; err != nil {
		t.Fatalf("mark the earlier Episode indexed: %v", err)
	}

	indexer := &chainIndexer{db: db}
	worker.UseIndexer(indexer)

	relations, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	states, err := episodeassertdao.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	governor, err := episodeassert.NewGovernor(states)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := NewCandidateRepository(db, DefaultHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	source := &countingSource{inner: candidates}
	proposals := &failingProposals{inner: relations}
	classifier := &itClassifier{answer: model.RelationReplaces}

	chain := &workerChain{db: db, jobs: episodes, worker: worker, indexer: indexer,
		classifier: classifier, proposals: proposals, source: source,
		relations: relations, states: states}

	if attachWriter {
		generatorService, err := episoderelation.New(episoderelation.Config{
			Source: source, Classifier: classifier, Proposals: proposals,
			Governor: governor, States: states, Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		writer, err := episoderelation.NewRelationWriter(generatorService)
		if err != nil {
			t.Fatal(err)
		}
		worker.UseRelationWriter(writer)
	}
	return chain
}

const (
	chainEarlyText = "接口统一固定超时 30 秒"
	chainLateText  = "取消固定超时值，改为按任务类型动态计算超时"
)

// enqueue creates the projection job the way production does.
func (c *workerChain) enqueue(t *testing.T) string {
	t.Helper()
	job, err := c.jobs.UpsertDesired(context.Background(), chainOwner, chainLateID,
		9102, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("enqueue the projection job: %v", err)
	}
	return job.ID
}

// start brings the worker up for the rest of the test.
//
// It is separate from waiting because a retry has to be observed by the same
// running worker that handled the first attempt. Restarting between attempts
// would be a second worker, and "the second worker did not classify again" is a
// weaker statement than the one this suite needs.
func (c *workerChain) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.worker.Start(ctx); err != nil {
		cancel()
		t.Fatalf("start the worker: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.worker.Stop(stopCtx)
		stopCancel()
		cancel()
	})
}

// runUntil starts the worker if needed and waits for the job to reach a state.
func (c *workerChain) runUntil(t *testing.T, jobID string,
	done func(*model.EpisodeProjectionJob) bool) *model.EpisodeProjectionJob {
	t.Helper()
	c.startOnce(t)
	return c.waitFor(t, jobID, done)
}

func (c *workerChain) startOnce(t *testing.T) {
	t.Helper()
	if c.running {
		return
	}
	c.running = true
	c.start(t)
}

func (c *workerChain) waitFor(t *testing.T, jobID string,
	done func(*model.EpisodeProjectionJob) bool) *model.EpisodeProjectionJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := c.jobs.GetJob(context.Background(), chainOwner, jobID)
		if err == nil && job != nil && done(job) {
			return job
		}
		time.Sleep(40 * time.Millisecond)
	}
	job, _ := c.jobs.GetJob(context.Background(), chainOwner, jobID)
	t.Fatalf("the job never reached the expected state; last = %+v", job)
	return nil
}

func (c *workerChain) counts(t *testing.T) (proposals, relations, states int) {
	t.Helper()
	var p, r, s int64
	c.db.Model(&model.EpisodeRelationProposal{}).Count(&p)
	c.db.Model(&model.EpisodeAssertionRelation{}).Count(&r)
	c.db.Model(&model.EpisodeAssertionState{}).Count(&s)
	return int(p), int(r), int(s)
}

// --- 1. the success chain ---

func TestWorkerChainSuccess(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})

	if job.Attempts > 1 {
		t.Fatalf("the successful chain retried %d times", job.Attempts-1)
	}
	if job.ErrorCode != "" {
		t.Fatalf("a completed job carries error code %q", job.ErrorCode)
	}
	if chain.indexer.Calls() == 0 {
		t.Fatal("the indexer was never offered the card")
	}
	if chain.source.Calls() == 0 {
		t.Fatal("the relation pass never asked the database for history")
	}
	if chain.classifier.calls == 0 {
		t.Fatal("the classifier was never reached from the worker")
	}
	proposals, relations, states := chain.counts(t)
	if proposals != 1 {
		t.Fatalf("the worker chain wrote %d proposals, want 1", proposals)
	}
	if relations != 1 {
		t.Fatalf("the worker chain wrote %d relations, want 1", relations)
	}
	// A replacement is two state rows: the superseded one and its successor.
	if states != 2 {
		t.Fatalf("the worker chain wrote %d assertion states, want 2", states)
	}
	// The relation the worker wrote points from the earlier assertion to the
	// later one, and the earlier one reads as replaced.
	var row model.EpisodeAssertionRelation
	if err := chain.db.First(&row).Error; err != nil {
		t.Fatalf("read the relation row: %v", err)
	}
	if row.Kind != model.RelationReplaces || row.UserID != chainOwner {
		t.Fatalf("the worker wrote %+v", row)
	}
	var superseded model.EpisodeAssertionState
	if err := chain.db.Where("assertion_id = ?", row.SourceAssertionID).
		First(&superseded).Error; err != nil {
		t.Fatalf("read the superseded state: %v", err)
	}
	if superseded.State != model.AssertionReplaced ||
		superseded.ReplacedByAssertionID != row.TargetAssertionID {
		t.Fatalf("the superseded assertion reads as %+v", superseded)
	}
}

// --- 2. a relation failure must retry the job, not complete it ---

func TestWorkerChainRelationFailureRetries(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	chain.proposals.mu.Lock()
	chain.proposals.fail = true
	chain.proposals.mu.Unlock()

	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Attempts >= 1 && j.Status != model.EpisodeProjectionProcessing
	})

	if job.Status == model.EpisodeProjectionCompleted {
		t.Fatal("the job completed although its relation stage failed")
	}
	// The projection itself succeeded, so the failure must not be reported as
	// an invalid projection. A learner's session is not malformed because a
	// relation table was unreachable - and "projection_failed" would send
	// whoever reads the error code looking at the generator's output, which is
	// the one thing that was fine.
	if job.ErrorCode != "relation_failed" {
		t.Fatalf("a relation failure was recorded as %q", job.ErrorCode)
	}
	// The Episode is durable and must survive: deleting it would throw away a
	// projection that worked because a later stage did not.
	var episodes int64
	chain.db.Model(&model.EpisodeMemory{}).
		Where("session_id = ?", chainLateID).Count(&episodes)
	if episodes != 1 {
		t.Fatalf("the persisted Episode count is %d, want 1", episodes)
	}
	proposals, relations, _ := chain.counts(t)
	if proposals != 0 || relations != 0 {
		t.Fatalf("a failed relation stage still wrote %d proposals and %d relations",
			proposals, relations)
	}
}

// --- 3. retrying the same job must not classify the pair twice ---

func TestWorkerChainRetryIsIdempotent(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	jobID := chain.enqueue(t)
	chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})

	callsAfterFirst := chain.classifier.calls
	before := chain.snapshot(t)

	// Re-enqueue the same session at the same watermark: this is what a
	// requeue or a recovered lease produces.
	if err := chain.db.Exec(
		"UPDATE episode_projection_jobs SET status = ?, attempts = 0, available_at = ?, "+
			"lease_token = NULL, lease_ends_at = NULL, completed_at = NULL, error_code = '' "+
			"WHERE id = ?",
		model.EpisodeProjectionPending, time.Now().Add(-time.Minute), jobID).Error; err != nil {
		t.Fatalf("re-enqueue the job: %v", err)
	}
	chain.waitFor(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})

	if chain.classifier.calls != callsAfterFirst {
		t.Fatalf("the retry classified again: %d -> %d calls",
			callsAfterFirst, chain.classifier.calls)
	}
	after := chain.snapshot(t)
	if len(before) != len(after) {
		t.Fatalf("the retry changed the row count: %d -> %d", len(before), len(after))
	}
	for key, was := range before {
		now, ok := after[key]
		if !ok {
			t.Fatalf("the retry dropped %s", key)
		}
		if was != now {
			t.Fatalf("the retry changed %s:\n  before %s\n  after  %s", key, was, now)
		}
	}
}

// snapshot is a field-by-field picture of everything the relation pass owns.
// Comparing lengths would miss exactly the failure this test exists for: an
// UPDATE does not change a row count.
func (c *workerChain) snapshot(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	var proposals []model.EpisodeRelationProposal
	if err := c.db.Order("id asc").Find(&proposals).Error; err != nil {
		t.Fatalf("read proposals: %v", err)
	}
	for _, row := range proposals {
		out[fmt.Sprintf("proposal:%s", row.IdempotencyKey)] = fmt.Sprintf(
			"id=%d kind=%s status=%s reason=%s decided_by=%s created=%s updated=%s",
			row.ID, row.Kind, row.Status, row.RejectedReason, row.DecidedBy,
			row.CreatedAt.UTC().Format(time.RFC3339Nano),
			row.UpdatedAt.UTC().Format(time.RFC3339Nano))
	}
	var relations []model.EpisodeAssertionRelation
	if err := c.db.Order("id asc").Find(&relations).Error; err != nil {
		t.Fatalf("read relations: %v", err)
	}
	for _, row := range relations {
		out[fmt.Sprintf("relation:%s->%s", row.SourceAssertionID, row.TargetAssertionID)] =
			fmt.Sprintf("id=%d kind=%s applied=%t created=%s updated=%s",
				row.ID, row.Kind, row.Applied,
				row.CreatedAt.UTC().Format(time.RFC3339Nano),
				row.UpdatedAt.UTC().Format(time.RFC3339Nano))
	}
	var states []model.EpisodeAssertionState
	if err := c.db.Order("assertion_id asc").Find(&states).Error; err != nil {
		t.Fatalf("read states: %v", err)
	}
	for _, row := range states {
		out[fmt.Sprintf("state:%s", row.AssertionID)] = fmt.Sprintf(
			"state=%s version=%d replaced_by=%s created=%s updated=%s",
			row.State, row.Version, row.ReplacedByAssertionID,
			row.CreatedAt.UTC().Format(time.RFC3339Nano),
			row.UpdatedAt.UTC().Format(time.RFC3339Nano))
	}
	return out
}

// --- 4. off: the shipped default must be inert ---

func TestWorkerChainOffWritesNothing(t *testing.T) {
	for _, shape := range []struct {
		name   string
		attach bool
		mode   episoderelation.Mode
	}{
		{"no writer attached", false, episoderelation.ModeOff},
		{"writer attached but off", true, episoderelation.ModeOff},
	} {
		t.Run(shape.name, func(t *testing.T) {
			chain := newWorkerChain(t, shape.mode, shape.attach)
			jobID := chain.enqueue(t)
			job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
				return j.Status == model.EpisodeProjectionCompleted
			})
			if job.ErrorCode != "" {
				t.Fatalf("the off chain completed with %q", job.ErrorCode)
			}
			if chain.indexer.Calls() == 0 {
				t.Fatal("off changed the indexing behaviour")
			}
			if chain.source.Calls() != 0 {
				t.Fatalf("off still asked for %d candidate reads", chain.source.Calls())
			}
			if chain.classifier.calls != 0 {
				t.Fatalf("off still classified %d pairs", chain.classifier.calls)
			}
			proposals, relations, states := chain.counts(t)
			if proposals != 0 || relations != 0 || states != 0 {
				t.Fatalf("off wrote %d proposals, %d relations, %d states",
					proposals, relations, states)
			}
			// The projection still happened: off is about relations, not
			// about whether the Episode gets written.
			var episodes int64
			chain.db.Model(&model.EpisodeMemory{}).
				Where("session_id = ?", chainLateID).Count(&episodes)
			if episodes != 1 {
				t.Fatalf("off projected %d Episodes for the late session", episodes)
			}
		})
	}
}

// --- 5. the order of the stages, pinned ---

// TestWorkerChainStageOrderAfterFailedIndex pins the repaired behaviour when
// the index write fails and a deciding relation pass is attached.
//
// The first shape of this chain let the job complete over a refused relation:
// the governor cannot read an unindexed Episode, so the pair was recorded as
// uncertain/governor_refused - and a decided pair is never re-decided, so a
// silent completion meant that relation was permanently lost, with no stage
// left that would ever run it again.
//
// The repair reuses the job's own retry: the relation pass is skipped while
// the Episode is unreadable, the job requeues with a relation-stage code, and
// the retry re-runs an idempotent projection (no model calls, no
// classification - the projector returns the existing Episode at the same
// watermark) and re-offers the index. When the index lands, the pass runs
// against an Episode the governor can read, and the relation is applied.
func TestWorkerChainStageOrderAfterFailedIndex(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	chain.indexer.mu.Lock()
	chain.indexer.failuresLeft = 1
	chain.indexer.mu.Unlock()

	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})
	// The index was offered twice: it failed once, and the retry offered it
	// again and succeeded.
	if chain.indexer.Calls() != 2 {
		t.Fatalf("the indexer was called %d times, want 2 (fail, then land)",
			chain.indexer.Calls())
	}
	// The relation pass ran exactly once - on the retry, against an indexed
	// Episode - and applied the pair.
	proposals, relations, _ := chain.counts(t)
	if proposals != 1 || relations != 1 {
		t.Fatalf("after the recovered index: proposals=%d relations=%d, want "+
			"1 applied pair (no refused row may stand in its way)", proposals,
			relations)
	}
	var recorded model.EpisodeRelationProposal
	if err := chain.db.First(&recorded).Error; err != nil {
		t.Fatalf("read the proposal: %v", err)
	}
	if recorded.Status != model.ProposalApplied {
		t.Fatalf("the pair was recorded as status=%q; anything but applied "+
			"means the retry inherited the first run and its refusal",
			recorded.Status)
	}
	// The completed job carries no error: the deferral was resolved inside the
	// attempt budget, not failed out of it.
	if job.ErrorCode != "" {
		t.Fatalf("a recovered index deferral was turned into job error %q",
			job.ErrorCode)
	}
	// And the re-run cost no model work: the projector is idempotent at the
	// same watermark, so the classifier saw the pair exactly once.
	if chain.classifier.calls != 1 {
		t.Fatalf("the classifier ran %d times across the retry, want exactly 1",
			chain.classifier.calls)
	}
}

// TestWorkerChainFailedIndexRetriesWithoutDeciding pins the intermediate
// state: while the index is still failing, the job must neither complete nor
// record a refusal that would stand between the pair and its retry.
func TestWorkerChainFailedIndexRetriesWithoutDeciding(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	chain.indexer.mu.Lock()
	chain.indexer.failed = true
	chain.indexer.mu.Unlock()

	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionFailed
	})
	// The attempt budget was spent retrying the deferral, and the recorded
	// code names the relation stage that is waiting, not the projection that
	// succeeded.
	if job.ErrorCode != "relation_prerequisite_failed" {
		t.Fatalf("an exhausted index deferral was recorded as %q", job.ErrorCode)
	}
	proposals, relations, _ := chain.counts(t)
	if proposals != 0 || relations != 0 {
		t.Fatalf("a retrying deferral recorded proposals=%d relations=%d; a "+
			"refused row here would be reused on every later encounter and "+
			"permanently block the pair", proposals, relations)
	}
	if chain.classifier.calls != 0 {
		t.Fatalf("the classifier ran %d times against an unreadable Episode",
			chain.classifier.calls)
	}
}

// TestWorkerChainFailedIndexWithoutDecidingWriterCompletes pins the untouched
// half: when no deciding relation writer is attached, an index failure is
// still a pure deferral - the Episode is durable, the reindexer will catch
// up, and the job completes exactly as it did before relations existed.
func TestWorkerChainFailedIndexWithoutDecidingWriterCompletes(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeShadow, false)
	chain.indexer.mu.Lock()
	chain.indexer.failed = true
	chain.indexer.mu.Unlock()

	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})
	if job.ErrorCode != "" {
		t.Fatalf("a deferral with no relation pass was turned into job error %q",
			job.ErrorCode)
	}
	if chain.indexer.Calls() != 1 {
		t.Fatalf("the indexer was offered %d times; nothing should be retried "+
			"when no relation pass is waiting", chain.indexer.Calls())
	}
}

// TestWorkerChainStageOrderSuccess pins the happy path's order: the index is
// offered the card before the relation pass runs, and the relation pass runs
// before the job is recorded as complete.
func TestWorkerChainStageOrderSuccess(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	jobID := chain.enqueue(t)
	chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})
	// The relation pass saw an indexed Episode, which is only possible if the
	// indexer ran first.
	var indexStatus string
	if err := chain.db.Model(&model.EpisodeMemory{}).
		Where("session_id = ?", chainLateID).
		Select("index_status").Scan(&indexStatus).Error; err != nil {
		t.Fatalf("read the index status: %v", err)
	}
	if indexStatus != string(model.EpisodeIndexIndexed) {
		t.Fatalf("the Episode reads as %q after a successful chain", indexStatus)
	}
	// The job is complete and the relation rows exist, so the relation pass
	// finished before completion rather than after it.
	proposals, relations, _ := chain.counts(t)
	if proposals != 1 || relations != 1 {
		t.Fatalf("a completed job carries %d proposals and %d relations",
			proposals, relations)
	}
}

// --- Phase 1.1.1-A1: the relation prerequisite ---

// newWorkerChainWithoutIndexer is the A1 fixture: a deciding relation writer
// and NO episode indexer, which is a state production main() can really reach
// - the relation writer is wired before the indexer block, and every indexer
// failure there only logs and degrades.
func newWorkerChainWithoutIndexer(t *testing.T) *workerChain {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	chain.startOnce(t)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = chain.worker.Stop(stopCtx)
	stopCancel()
	db := chain.db
	episodes := episodedao.NewRepository(db)
	loader, err := episode.NewDatabaseEvidenceLoader(db, 256)
	if err != nil {
		t.Fatalf("evidence loader: %v", err)
	}
	chainGen := &chainGenerator{text: map[string]string{
		chainEarlyID: chainEarlyText, chainLateID: chainLateText}}
	projector, err := episode.NewProjector(loader, chainGen, episodes, chainAllowAll{})
	if err != nil {
		t.Fatalf("projector: %v", err)
	}
	reconciler, err := episode.NewReconciler(episodes, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	governor, err := episodeassert.NewGovernor(chain.states)
	if err != nil {
		t.Fatalf("governor: %v", err)
	}
	generator, err := episoderelation.New(episoderelation.Config{
		Source: chain.source, Classifier: chain.classifier,
		Proposals: chain.proposals, Governor: governor, States: chain.states,
		Mode: episoderelation.ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := episoderelation.NewRelationWriter(generator)
	if err != nil {
		t.Fatal(err)
	}
	config := episode.DefaultWorkerConfig()
	config.ScanInterval = 40 * time.Millisecond
	config.ReconcileInterval = time.Hour
	config.RecoveryInterval = time.Hour
	config.RetryDelay = 50 * time.Millisecond
	config.QuietWindow = 0
	worker, err := episode.NewWorker(episodes, projector, reconciler, config)
	if err != nil {
		t.Fatal(err)
	}
	worker.UseRelationWriter(writer)
	chain.worker = worker
	chain.running = false
	return chain
}

// TestRelationPrerequisiteMissingIndexerRetriesFailsClosed is the A1 red
// light: a deciding relation writer with no indexer must not classify, must
// not record a proposal, must not complete the job, and must retry on the
// new closed prerequisite code rather than run the pass over an Episode the
// governor can never read.
func TestRelationPrerequisiteMissingIndexerRetriesFailsClosed(t *testing.T) {
	chain := newWorkerChainWithoutIndexer(t)
	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionFailed
	})
	if job.ErrorCode != "relation_prerequisite_failed" {
		t.Fatalf("RED: a deciding writer with no indexer ended as error %q, "+
			"want relation_prerequisite_failed", job.ErrorCode)
	}
	if chain.classifier.calls != 0 {
		t.Fatalf("RED: the classifier ran %d times with no indexer",
			chain.classifier.calls)
	}
	proposals, relations, _ := chain.counts(t)
	if proposals != 0 || relations != 0 {
		t.Fatalf("RED: %d proposals / %d relations were recorded without an "+
			"indexer; a refused proposal is a decided pair and is never "+
			"re-decided", proposals, relations)
	}
}

// TestRelationPrerequisiteIndexErrorSharesTheCode pins that an indexer that
// exists but fails is the same prerequisite failure - not a relation_failed,
// and not a projection code.
func TestRelationPrerequisiteIndexErrorSharesTheCode(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	chain.indexer.mu.Lock()
	chain.indexer.failed = true
	chain.indexer.mu.Unlock()
	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionFailed
	})
	if job.ErrorCode != "relation_prerequisite_failed" {
		t.Fatalf("RED: a failing indexer ended as error %q, want "+
			"relation_prerequisite_failed", job.ErrorCode)
	}
	if chain.classifier.calls != 0 {
		t.Fatalf("RED: the classifier ran %d times against a failed index",
			chain.classifier.calls)
	}
	proposals, _, _ := chain.counts(t)
	if proposals != 0 {
		t.Fatalf("RED: %d proposals were recorded behind a failed index",
			proposals)
	}
}

// TestRelationPrerequisiteRecoversAfterIndexLands keeps the A behaviour's
// promise under the new code: one failed index offer, a retry that lands,
// the relation applied exactly once, no re-classification, clean completion.
func TestRelationPrerequisiteRecoversAfterIndexLands(t *testing.T) {
	chain := newWorkerChain(t, episoderelation.ModeApply, true)
	chain.indexer.mu.Lock()
	chain.indexer.failuresLeft = 1
	chain.indexer.mu.Unlock()
	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})
	if job.ErrorCode != "" {
		t.Fatalf("a recovered prerequisite ended as error %q", job.ErrorCode)
	}
	if chain.indexer.Calls() != 2 || chain.classifier.calls != 1 {
		t.Fatalf("indexer=%d classifier=%d, want 2/1",
			chain.indexer.Calls(), chain.classifier.calls)
	}
	proposals, relations, _ := chain.counts(t)
	if proposals != 1 || relations != 1 {
		t.Fatalf("proposals=%d relations=%d, want 1 applied pair", proposals,
			relations)
	}
}

// TestRelationPrerequisiteOffWriterWithoutIndexerCompletes pins the untouched
// half: a writer that decides nothing keeps the pure deferral semantics - no
// indexer, no relation pass, job completes exactly as before relations.
func TestRelationPrerequisiteOffWriterWithoutIndexerCompletes(t *testing.T) {
	chain := newWorkerChainWithoutIndexerOff(t)
	jobID := chain.enqueue(t)
	job := chain.runUntil(t, jobID, func(j *model.EpisodeProjectionJob) bool {
		return j.Status == model.EpisodeProjectionCompleted
	})
	if job.ErrorCode != "" {
		t.Fatalf("an off writer with no indexer ended as error %q",
			job.ErrorCode)
	}
	if chain.classifier.calls != 0 {
		t.Fatalf("the classifier ran %d times under an off writer",
			chain.classifier.calls)
	}
}

// newWorkerChainWithoutIndexerOff builds the no-indexer shape with an OFF
// writer attached, so the guard is proven on the mode too and not only on a
// nil pointer.
func newWorkerChainWithoutIndexerOff(t *testing.T) *workerChain {
	chain := newWorkerChain(t, episoderelation.ModeOff, false)
	chain.startOnce(t)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = chain.worker.Stop(stopCtx)
	stopCancel()
	db := chain.db
	episodes := episodedao.NewRepository(db)
	loader, err := episode.NewDatabaseEvidenceLoader(db, 256)
	if err != nil {
		t.Fatalf("evidence loader: %v", err)
	}
	chainGen := &chainGenerator{text: map[string]string{
		chainEarlyID: chainEarlyText, chainLateID: chainLateText}}
	projector, err := episode.NewProjector(loader, chainGen, episodes, chainAllowAll{})
	if err != nil {
		t.Fatalf("projector: %v", err)
	}
	reconciler, err := episode.NewReconciler(episodes, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	config := episode.DefaultWorkerConfig()
	config.ScanInterval = 40 * time.Millisecond
	config.ReconcileInterval = time.Hour
	config.RecoveryInterval = time.Hour
	config.RetryDelay = 50 * time.Millisecond
	config.QuietWindow = 0
	worker, err := episode.NewWorker(episodes, projector, reconciler, config)
	if err != nil {
		t.Fatal(err)
	}
	// An off writer: attached, but deciding nothing.
	generator, err := episoderelation.New(episoderelation.Config{
		Source: chain.source, Classifier: chain.classifier,
		Proposals: chain.proposals, Mode: episoderelation.ModeOff})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := episoderelation.NewRelationWriter(generator)
	if err != nil {
		t.Fatal(err)
	}
	worker.UseRelationWriter(writer)
	chain.worker = worker
	chain.running = false
	return chain
}
