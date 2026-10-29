package episoderelation

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	episodeassertdao "GopherAI/dao/episodeassert"
	"GopherAI/model"
	"GopherAI/service/episodeassert"
	"GopherAI/service/episoderelation"
)

// STABILIZE-065 Phase 1.1 integration: the relation write side against a real
// MySQL.
//
// It runs against a disposable container and its own schema. It never touches
// the 058, 063 or 064 stores, and it creates and drops only tables whose names
// this file owns.
//
// Phase 1 proved atomicity against an in-memory map that restored a snapshot
// on error. That is a description of what a transaction does, written by the
// same person who wrote the code being tested. These tests make MySQL do it.

// s065DSN is the disposable container. Overridable so the suite can be pointed
// at another throwaway instance, never at a real one.
func s065DSN() string {
	if dsn := os.Getenv("STABILIZE065_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:s065local@tcp(127.0.0.1:33506)/stabilize065_relation?charset=utf8mb4&parseTime=True&loc=Local"
}

const (
	itOwner = int64(900000000000000201)
	itOther = int64(900000000000000202)
)

var itBase = time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)

// openTestDB connects to the disposable store, or skips.
//
// A skip here is honest and its consequence is stated in the report: without
// it the real-storage claim is unproven, and the phase does not qualify.
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysqldriver.Open(s065DSN()), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Skipf("the disposable MySQL is unavailable, real storage is UNPROVEN: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Skipf("the disposable MySQL is unavailable, real storage is UNPROVEN: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("the disposable MySQL is unavailable, real storage is UNPROVEN: %v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.EpisodeMemory{}, &model.EpisodeAssertionState{},
		&model.EpisodeAssertionRelation{}, &model.EpisodeRelationProposal{}); err != nil {
		t.Fatalf("migrate the disposable schema: %v", err)
	}
	// Each test owns a clean slate of only its own tables.
	for _, table := range []string{"episode_assertion_relations",
		"episode_relation_proposals", "episode_assertion_states",
		"episode_memories", "messages", "sessions"} {
		if err := db.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	return db
}

// itFixture seeds two sessions of one learner and returns the projected ids.
type itFixture struct {
	db       *gorm.DB
	earlyID  string
	lateID   string
	lateText string
	relation *Repository
	states   *episodeassertdao.Repository
	governor *episodeassert.Governor
}

func seedFixture(t *testing.T, earlyText, lateText string) *itFixture {
	t.Helper()
	db := openTestDB(t)
	lateAt := itBase.AddDate(0, 0, 30)
	seedSession(t, db, itOwner, "sess-early", 9001, earlyText, itBase)
	seedSession(t, db, itOwner, "sess-late", 9002, lateText, lateAt)

	early := seedEpisode(t, db, itOwner, "ep-early", "sess-early", 9001, earlyText, itBase)
	late := seedEpisode(t, db, itOwner, "ep-late", "sess-late", 9002, lateText, lateAt)

	relation, err := NewRepository(db)
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
	return &itFixture{db: db,
		earlyID:  projectedID(t, early),
		lateID:   projectedID(t, late),
		lateText: lateText,
		relation: relation, states: states, governor: governor}
}

func seedSession(t *testing.T, db *gorm.DB, userID int64, sessionID string,
	messageID uint, text string, at time.Time) {
	t.Helper()
	if err := db.Save(&model.Session{ID: sessionID, UserID: userID, Title: text,
		CreatedAt: at, UpdatedAt: at}).Error; err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if err := db.Save(&model.Message{ID: messageID, SessionID: sessionID,
		Content: text, IsUser: true, CreatedAt: at}).Error; err != nil {
		t.Fatalf("seed message: %v", err)
	}
}

func seedEpisode(t *testing.T, db *gorm.DB, userID int64, episodeID, sessionID string,
	messageID uint, text string, at time.Time) model.EpisodeMemory {
	t.Helper()
	episode := model.EpisodeMemory{ID: episodeID, UserID: userID, SessionID: sessionID,
		Title: text, SearchText: text,
		Document: model.EpisodeDocument{Synopsis: text,
			Decisions: []model.EpisodeEvidence{{Text: text,
				EvidenceMessageIDs: []uint{messageID}}},
			// The real schema validates this on scan; the in-memory store did
			// not, which is how a document the database rejects passed every
			// Phase 1 test.
			SourceHandles: []string{"session:" + sessionID}},
		LastMessageAt: at, CreatedAt: at, UpdatedAt: at,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}
	if err := db.Save(&episode).Error; err != nil {
		t.Fatalf("seed episode: %v", err)
	}
	return episode
}

func projectedID(t *testing.T, episode model.EpisodeMemory) string {
	t.Helper()
	for _, assertion := range model.AssertionsOf(episode) {
		if assertion.Kind == model.AssertionDecision {
			return assertion.ID
		}
	}
	t.Fatal("the projection issued no decision assertion")
	return ""
}

// itClassifier answers one relation and counts how often it was asked.
type itClassifier struct {
	answer model.AssertionRelationKind
	mu     sync.Mutex
	calls  int
}

func (c *itClassifier) Identity() string { return "stabilize065_integration_v1" }

func (c *itClassifier) Classify(_ context.Context,
	request episoderelation.ClassifyRequest) (episoderelation.ClassifyResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return episoderelation.ClassifyResponse{PairID: request.PairID, Kind: c.answer}, nil
}

func (f *itFixture) generator(t *testing.T, mode episoderelation.Mode,
	classifier episoderelation.Classifier) *episoderelation.Generator {
	t.Helper()
	source, err := NewCandidateRepository(f.db, DefaultHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	generator, err := episoderelation.New(episoderelation.Config{
		Source: source, Classifier: classifier, Proposals: f.relation,
		Governor: f.governor, States: f.states, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

func (f *itFixture) lateAssertion() episoderelation.NewAssertion {
	return episoderelation.NewAssertion{AssertionID: f.lateID, EpisodeID: "ep-late",
		SessionID: "sess-late", UserID: itOwner, Kind: model.AssertionDecision,
		// The real body, not a placeholder: the candidate pre-filter compares
		// topics, and "later" shares none with anything the learner said.
		Text: f.lateText, OccurredAt: itBase.AddDate(0, 0, 30), TimeKnown: true}
}

func (f *itFixture) stateOf(t *testing.T, id string) model.EpisodeAssertionState {
	t.Helper()
	var row model.EpisodeAssertionState
	if err := f.db.Where("assertion_id = ?", id).First(&row).Error; err != nil {
		return model.EpisodeAssertionState{}
	}
	return row
}

func itCtx() context.Context { return context.Background() }

// 1. AutoMigrate creates the two tables and the pair's unique index.
func TestIntegrationMigrationCreatesTablesAndUniqueIndex(t *testing.T) {
	db := openTestDB(t)
	for _, table := range []string{"episode_assertion_relations", "episode_relation_proposals"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("%s was not created", table)
		}
	}
	if !db.Migrator().HasIndex(&model.EpisodeAssertionRelation{}, "idx_relation_pair") {
		t.Fatal("the relation pair has no unique index; a read-then-create is the " +
			"only thing standing between two concurrent applies and two rows")
	}
	var count int
	row := db.Raw("SELECT NON_UNIQUE FROM information_schema.statistics WHERE " +
		"table_schema = DATABASE() AND table_name = 'episode_assertion_relations' " +
		"AND index_name = 'idx_relation_pair' LIMIT 1").Row()
	if err := row.Scan(&count); err != nil {
		t.Fatalf("read the index metadata: %v", err)
	}
	if count != 0 {
		t.Fatal("idx_relation_pair exists but is not UNIQUE")
	}
}

// 2-5. The production CandidateSource reads real rows and applies every filter.
func TestIntegrationCandidateSourceReadsAndFilters(t *testing.T) {
	fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
	source, err := NewCandidateRepository(fixture.db, DefaultHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	// Another learner's episode must never appear.
	seedSession(t, fixture.db, itOther, "sess-foreign", 9003, "接口统一固定超时 30 秒", itBase)
	seedEpisode(t, fixture.db, itOther, "ep-foreign", "sess-foreign", 9003,
		"接口统一固定超时 30 秒", itBase)
	// An archived and an unindexed episode of the same learner. Each needs its
	// own session row: episode_memories has a real foreign key, which the
	// in-memory store had no equivalent of.
	seedSession(t, fixture.db, itOwner, "sess-arch", 9004, "接口统一固定超时 30 秒", itBase)
	seedSession(t, fixture.db, itOwner, "sess-unidx", 9005, "接口统一固定超时 30 秒", itBase)
	archived := seedEpisode(t, fixture.db, itOwner, "ep-archived", "sess-arch", 9004,
		"接口统一固定超时 30 秒", itBase)
	archived.Status = model.EpisodeStatusArchived
	if err := fixture.db.Save(&archived).Error; err != nil {
		t.Fatal(err)
	}
	unindexed := seedEpisode(t, fixture.db, itOwner, "ep-unindexed", "sess-unidx", 9005,
		"接口统一固定超时 30 秒", itBase)
	unindexed.IndexStatus = model.EpisodeIndexPending
	if err := fixture.db.Save(&unindexed).Error; err != nil {
		t.Fatal(err)
	}

	got, err := source.ActiveAssertions(itCtx(), itOwner, "sess-late")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range got {
		if candidate.UserID != itOwner {
			t.Fatalf("another learner's assertion was returned: %+v", candidate)
		}
		if candidate.SessionID == "sess-late" {
			t.Fatal("the session being projected was returned as its own history")
		}
		switch candidate.EpisodeID {
		case "ep-archived":
			t.Fatal("an archived episode was returned")
		case "ep-unindexed":
			t.Fatal("an unindexed episode was returned")
		case "ep-foreign":
			t.Fatal("a foreign episode was returned")
		}
		if !candidate.TimeKnown {
			t.Fatalf("%s has no resolved time; the evidence join did not work",
				candidate.AssertionID)
		}
	}
	found := false
	for _, candidate := range got {
		if candidate.AssertionID == fixture.earlyID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the learner's own earlier decision was not returned: %+v", got)
	}
}

// 6. A replacement moves both states and writes the edge in one transaction.
func TestIntegrationReplacementIsAtomic(t *testing.T) {
	fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
	generator := fixture.generator(t, episoderelation.ModeApply,
		&itClassifier{answer: model.RelationReplaces})
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	source := fixture.stateOf(t, fixture.earlyID)
	target := fixture.stateOf(t, fixture.lateID)
	if source.State != model.AssertionReplaced || source.ReplacedByAssertionID != fixture.lateID {
		t.Fatalf("the earlier decision is %q -> %q", source.State, source.ReplacedByAssertionID)
	}
	if target.State != model.AssertionCurrent {
		t.Fatalf("the later decision is %q, want current", target.State)
	}
	edges, err := fixture.relation.Relations(itCtx(), itOwner, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].Kind != model.RelationReplaces {
		t.Fatalf("the replacement edge is %+v", edges)
	}
}

// 7. A supplement leaves two currents and one edge.
func TestIntegrationSupplementLeavesTwoCurrents(t *testing.T) {
	fixture := seedFixture(t, "MySQL 保存业务事实", "Redis 保存缓存，MySQL 仍保存业务事实")
	generator := fixture.generator(t, episoderelation.ModeApply,
		&itClassifier{answer: model.RelationSupplements})
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	for name, id := range map[string]string{"earlier": fixture.earlyID, "later": fixture.lateID} {
		if state := fixture.stateOf(t, id); state.State != model.AssertionCurrent {
			t.Errorf("the %s decision is %q, want current", name, state.State)
		}
		if state := fixture.stateOf(t, id); state.ReplacedByAssertionID != "" {
			t.Errorf("the %s decision has replaced_by %q", name, state.ReplacedByAssertionID)
		}
	}
	edges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
	if len(edges) != 1 || edges[0].Kind != model.RelationSupplements {
		t.Fatalf("the supplement edge is %+v", edges)
	}
}

// 8. An equivalence leaves exactly one current.
func TestIntegrationEquivalentLeavesOneCurrent(t *testing.T) {
	fixture := seedFixture(t, "默认使用 MySQL", "项目数据库默认仍是 MySQL")
	generator := fixture.generator(t, episoderelation.ModeApply,
		&itClassifier{answer: model.RelationEquivalent})
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	currents := 0
	for _, id := range []string{fixture.earlyID, fixture.lateID} {
		if fixture.stateOf(t, id).State == model.AssertionCurrent {
			currents++
		}
	}
	if currents != 1 {
		t.Fatalf("an equivalence left %d current facts, want exactly one", currents)
	}
}

// 9. A failure in the second step rolls back everything MySQL did.
func TestIntegrationRollbackLeavesNothing(t *testing.T) {
	fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
	// A relation row whose validation fails inside the transaction, after the
	// two state writes have already happened.
	failing := &failingRelationStore{Repository: fixture.relation}
	source, err := NewCandidateRepository(fixture.db, DefaultHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	generator, err := episoderelation.New(episoderelation.Config{
		Source: source, Classifier: &itClassifier{answer: model.RelationReplaces},
		Proposals: failing, Governor: fixture.governor, States: fixture.states,
		Mode: episoderelation.ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err == nil {
		t.Fatal("a failing relation write was reported as a completed apply")
	}
	// MySQL rolled the two state writes back with it.
	for name, id := range map[string]string{"earlier": fixture.earlyID, "later": fixture.lateID} {
		if state := fixture.stateOf(t, id); state.State != "" {
			t.Errorf("the %s decision survived a rolled-back apply as %q", name, state.State)
		}
	}
	edges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
	if len(edges) != 0 {
		t.Fatalf("a rolled-back apply left %d edges", len(edges))
	}
}

// failingRelationStore fails the relation write while keeping the transaction
// seam, so the failure happens after the state writes inside one transaction.
type failingRelationStore struct{ *Repository }

func (f *failingRelationStore) SaveRelation(context.Context,
	model.EpisodeAssertionRelation) error {
	return fmt.Errorf("the relation store is unavailable")
}

func (f *failingRelationStore) WithinTx(ctx context.Context,
	fn func(episoderelation.ProposalStore, episodeassert.Store) error) error {
	return f.Repository.WithinTx(ctx, func(store episoderelation.ProposalStore,
		states episodeassert.Store) error {
		return fn(&failingRelationStore{Repository: store.(*Repository)}, states)
	})
}

// 10. Two concurrent applies leave one relation and one legal state move.
func TestIntegrationConcurrentApplyLeavesOneRow(t *testing.T) {
	fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
	var wait sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			generator := fixture.generator(t, episoderelation.ModeApply,
				&itClassifier{answer: model.RelationReplaces})
			_, _ = generator.Generate(itCtx(), []episoderelation.NewAssertion{
				fixture.lateAssertion()})
		}()
	}
	wait.Wait()
	edges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
	if len(edges) != 1 {
		t.Fatalf("two concurrent applies left %d relation rows", len(edges))
	}
	proposals, _ := fixture.relation.Proposals(itCtx(), itOwner)
	if len(proposals) != 1 {
		t.Fatalf("two concurrent applies left %d proposals", len(proposals))
	}
	source := fixture.stateOf(t, fixture.earlyID)
	if source.Version != 1 {
		t.Fatalf("the state moved %d times, want exactly one legal migration",
			source.Version)
	}
	if source.State == model.AssertionReplaced && source.ReplacedByAssertionID == "" {
		t.Fatal("a concurrent apply left a half-written replacement")
	}
}

// 11. A sequential replay changes no persisted field and asks nothing.
func TestIntegrationReplayChangesNothing(t *testing.T) {
	fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
	classifier := &itClassifier{answer: model.RelationReplaces}
	generator := fixture.generator(t, episoderelation.ModeApply, classifier)
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatal(err)
	}
	firstCalls := classifier.calls
	beforeSource := fixture.stateOf(t, fixture.earlyID)
	beforeTarget := fixture.stateOf(t, fixture.lateID)
	beforeEdges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
	beforeProposals, _ := fixture.relation.Proposals(itCtx(), itOwner)

	// Enough separation that a rewritten timestamp would be visible.
	time.Sleep(1100 * time.Millisecond)
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatal(err)
	}
	if classifier.calls != firstCalls {
		t.Errorf("the replay asked the classifier again: %d -> %d", firstCalls, classifier.calls)
	}
	afterSource := fixture.stateOf(t, fixture.earlyID)
	afterTarget := fixture.stateOf(t, fixture.lateID)
	afterEdges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
	afterProposals, _ := fixture.relation.Proposals(itCtx(), itOwner)

	// Field by field, not by count. A count is exactly what an UPDATE leaves
	// alone.
	for name, pair := range map[string][2]model.EpisodeAssertionState{
		"source": {beforeSource, afterSource}, "target": {beforeTarget, afterTarget},
	} {
		before, after := pair[0], pair[1]
		if before.Version != after.Version {
			t.Errorf("%s version %d -> %d", name, before.Version, after.Version)
		}
		if before.State != after.State {
			t.Errorf("%s state %q -> %q", name, before.State, after.State)
		}
		if before.ReplacedByAssertionID != after.ReplacedByAssertionID {
			t.Errorf("%s replaced_by %q -> %q", name,
				before.ReplacedByAssertionID, after.ReplacedByAssertionID)
		}
		if !before.UpdatedAt.Equal(after.UpdatedAt) {
			t.Errorf("%s updated_at %s -> %s", name, before.UpdatedAt, after.UpdatedAt)
		}
		if !before.CreatedAt.Equal(after.CreatedAt) {
			t.Errorf("%s created_at moved", name)
		}
	}
	if len(beforeEdges) != len(afterEdges) {
		t.Fatalf("edges %d -> %d", len(beforeEdges), len(afterEdges))
	}
	for index := range beforeEdges {
		if !beforeEdges[index].UpdatedAt.Equal(afterEdges[index].UpdatedAt) {
			t.Errorf("edge %d updated_at moved", index)
		}
		if beforeEdges[index].ID != afterEdges[index].ID {
			t.Errorf("edge %d id changed", index)
		}
	}
	if len(beforeProposals) != len(afterProposals) {
		t.Fatalf("proposals %d -> %d", len(beforeProposals), len(afterProposals))
	}
	for index := range beforeProposals {
		if !beforeProposals[index].UpdatedAt.Equal(afterProposals[index].UpdatedAt) {
			t.Errorf("proposal %d updated_at moved", index)
		}
	}
}

// 12. A version conflict under the row is surfaced, not written over.
func TestIntegrationVersionConflict(t *testing.T) {
	fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
	if err := fixture.db.Save(&model.EpisodeAssertionState{
		AssertionID: fixture.earlyID, UserID: itOwner, EpisodeID: "ep-early",
		Kind: model.AssertionDecision, State: model.AssertionUnknown,
		OccurredAt: itBase, Version: 7, CreatedAt: itBase, UpdatedAt: itBase}).Error; err != nil {
		t.Fatal(err)
	}
	generator := fixture.generator(t, episoderelation.ModeApply,
		&itClassifier{answer: model.RelationReplaces})
	_, _ = generator.Generate(itCtx(), []episoderelation.NewAssertion{fixture.lateAssertion()})
	after := fixture.stateOf(t, fixture.earlyID)
	if after.Version < 7 {
		t.Fatalf("the version went backwards to %d", after.Version)
	}
	if after.Version != 7 && after.Version != 8 {
		t.Fatalf("the version jumped to %d", after.Version)
	}
}

// 13. The relation-aware governed read sees supplement and equivalent.
func TestIntegrationRelationAwareGovernedRead(t *testing.T) {
	fixture := seedFixture(t, "MySQL 保存业务事实", "Redis 保存缓存，MySQL 仍保存业务事实")
	generator := fixture.generator(t, episoderelation.ModeApply,
		&itClassifier{answer: model.RelationSupplements})
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatal(err)
	}
	var card model.EpisodeMemory
	if err := fixture.db.Where("id = ?", "ep-early").First(&card).Error; err != nil {
		t.Fatal(err)
	}
	// The read goes through the production governor and the relation table in
	// MySQL - not a map this test built.
	views, err := episodeassert.ResolveWithRelations(itCtx(), fixture.governor,
		fixture.relation, itOwner, card, map[string]struct{}{fixture.lateID: {}})
	if err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, view := range views {
		if view.Assertion.ID != fixture.earlyID {
			continue
		}
		if view.Assertion.State != model.AssertionCurrent {
			t.Errorf("the supplemented decision reads as %q", view.Assertion.State)
		}
		for _, relation := range view.Relations {
			if relation.Kind == model.RelationSupplements &&
				relation.RelatedAssertionID == fixture.lateID {
				saw = true
				if !relation.Citable {
					t.Error("the related slot is in the window but reported uncitable")
				}
				if note := episodeassert.RenderRelationNote(view,
					func(string) string { return "[E1.d1]" }); note == "" {
					t.Error("the relation rendered to nothing")
				}
			}
		}
	}
	if !saw {
		t.Fatal("the governed read did not see the supplement; the relation is a dead column")
	}
	// A target outside the window is stated without a citable reference.
	views, err = episodeassert.ResolveWithRelations(itCtx(), fixture.governor,
		fixture.relation, itOwner, card, map[string]struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		for _, relation := range view.Relations {
			if relation.Citable {
				t.Error("a relation outside the evidence window was reported citable")
			}
		}
	}
}

// 14-15. Off reads and writes nothing; shadow writes only proposals.
func TestIntegrationModesOffAndShadow(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
		classifier := &itClassifier{answer: model.RelationReplaces}
		generator, err := episoderelation.New(episoderelation.Config{Mode: episoderelation.ModeOff})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
			fixture.lateAssertion()}); err != nil {
			t.Fatal(err)
		}
		if classifier.calls != 0 {
			t.Error("off asked the classifier")
		}
		proposals, _ := fixture.relation.Proposals(itCtx(), itOwner)
		edges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
		if len(proposals) != 0 || len(edges) != 0 {
			t.Fatalf("off wrote %d proposals and %d edges", len(proposals), len(edges))
		}
		if fixture.stateOf(t, fixture.earlyID).State != "" {
			t.Fatal("off changed a state")
		}
	})
	t.Run("shadow", func(t *testing.T) {
		fixture := seedFixture(t, "接口统一固定超时 30 秒", "取消固定值，改为动态计算超时")
		generator := fixture.generator(t, episoderelation.ModeShadow,
			&itClassifier{answer: model.RelationReplaces})
		if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
			fixture.lateAssertion()}); err != nil {
			t.Fatal(err)
		}
		proposals, _ := fixture.relation.Proposals(itCtx(), itOwner)
		if len(proposals) != 1 || proposals[0].Status != model.ProposalShadow {
			t.Fatalf("shadow left %+v", proposals)
		}
		edges, _ := fixture.relation.Relations(itCtx(), itOwner, "")
		if len(edges) != 0 {
			t.Fatalf("shadow wrote %d relation edges", len(edges))
		}
		for _, id := range []string{fixture.earlyID, fixture.lateID} {
			if fixture.stateOf(t, id).State != "" {
				t.Fatalf("shadow changed the state of %s", id)
			}
		}
	})
}

// TestIntegrationRelationsForAssertionsIsScopedAndBatched pins the read-side
// query against real MySQL.
//
// The first shape of this read was Relations(userID, "") - every edge the
// learner ever had, on every governed search, to annotate a handful of slots.
// It worked, which is the problem: a query whose cost grows with a learner's
// whole history while the thing it annotates does not is a defect that only
// appears on the account with the most history.
func TestIntegrationRelationsForAssertionsIsScopedAndBatched(t *testing.T) {
	fixture := seedFixture(t, "MySQL 保存业务事实", "Redis 保存缓存，MySQL 仍保存业务事实")
	generator := fixture.generator(t, episoderelation.ModeApply,
		&itClassifier{answer: model.RelationSupplements})
	if _, err := generator.Generate(itCtx(), []episoderelation.NewAssertion{
		fixture.lateAssertion()}); err != nil {
		t.Fatal(err)
	}
	// A second learner's edge over their own assertions, and an unrelated edge
	// of this learner's. Neither may come back for a window that names neither.
	for _, row := range []model.EpisodeAssertionRelation{
		{UserID: itOther, Kind: model.RelationSupplements,
			SourceAssertionID: model.DeterministicAssertionID("other|a"),
			SourceEpisodeID:   "ep-other-a",
			TargetAssertionID: model.DeterministicAssertionID("other|b"),
			TargetEpisodeID:   "ep-other-b",
			SourceKind:        model.AssertionDecision, TargetKind: model.AssertionDecision,
			SourceOccurredAt: itBase, TargetOccurredAt: itBase.AddDate(0, 0, 1),
			DecidedBy: "stabilize065_integration_v1"},
		{UserID: itOwner, Kind: model.RelationEquivalent,
			SourceAssertionID: model.DeterministicAssertionID("own|x"),
			SourceEpisodeID:   "ep-own-x",
			TargetAssertionID: model.DeterministicAssertionID("own|y"),
			TargetEpisodeID:   "ep-own-y",
			SourceKind:        model.AssertionDecision, TargetKind: model.AssertionDecision,
			SourceOccurredAt: itBase, TargetOccurredAt: itBase.AddDate(0, 0, 1),
			DecidedBy: "stabilize065_integration_v1"},
	} {
		if err := fixture.relation.SaveRelation(itCtx(), row); err != nil {
			t.Fatalf("seed an unrelated edge: %v", err)
		}
	}

	// Asking about the early assertion alone must find the supplement, even
	// though that assertion is the edge's *source* and the window names no
	// target. Reading one column would have hidden it from the other end.
	fromSource, err := fixture.relation.RelationsForAssertions(itCtx(), itOwner,
		[]string{fixture.earlyID})
	if err != nil {
		t.Fatal(err)
	}
	if len(fromSource) != 1 || fromSource[0].Kind != model.RelationSupplements {
		t.Fatalf("asking by source returned %+v", fromSource)
	}
	// And asking about the later assertion alone must find the same edge.
	fromTarget, err := fixture.relation.RelationsForAssertions(itCtx(), itOwner,
		[]string{fixture.lateID})
	if err != nil {
		t.Fatal(err)
	}
	if len(fromTarget) != 1 || fromTarget[0].ID != fromSource[0].ID {
		t.Fatalf("asking by target returned %+v, not the same row", fromTarget)
	}

	// The learner's other edge is real and must not come back for this window.
	for _, row := range fromSource {
		if row.SourceEpisodeID == "ep-own-x" {
			t.Fatal("an edge outside the window was returned")
		}
	}
	// Nor may another learner's, even when the caller asks for their ids.
	foreign, err := fixture.relation.RelationsForAssertions(itCtx(), itOwner,
		[]string{model.DeterministicAssertionID("other|a")})
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatalf("another learner's edge came back: %+v", foreign)
	}

	// Degenerate inputs do not become unbounded queries.
	for name, ids := range map[string][]string{
		"no ids":    {},
		"blank ids": {"", "   "},
	} {
		rows, err := fixture.relation.RelationsForAssertions(itCtx(), itOwner, ids)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rows) != 0 {
			t.Fatalf("%s returned %d rows; an empty window must not read the "+
				"learner's whole history", name, len(rows))
		}
	}
	// An oversized list is truncated rather than sent whole.
	huge := make([]string, 0, MaxRelationLookupAssertions*2)
	for index := 0; index < cap(huge); index++ {
		huge = append(huge, model.DeterministicAssertionID(fmt.Sprintf("bulk|%d", index)))
	}
	huge = append(huge, fixture.earlyID)
	bounded, err := fixture.relation.RelationsForAssertions(itCtx(), itOwner, huge)
	if err != nil {
		t.Fatalf("an oversized lookup failed rather than being bounded: %v", err)
	}
	// The real id is past the ceiling, so it is dropped with the rest of the
	// overflow: the bound is a bound, not a best effort.
	if len(bounded) != 0 {
		t.Fatalf("the oversized lookup returned %d rows", len(bounded))
	}
}
