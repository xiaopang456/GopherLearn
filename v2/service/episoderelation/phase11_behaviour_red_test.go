package episoderelation

import (
	"testing"
	"time"

	"GopherAI/model"
)

// Phase 1.1 behavioural red lights: the defects that have behaviour to assert.

// Red 4: after a supplement both assertions are still unknown.
//
// Phase 1 wrote the edge and stopped. "Both remain true" was a sentence in a
// document and nothing in the store said it, so a reader asking "is the MySQL
// decision still in force?" got "unknown" - which is what the absence of a
// relation means. A supplement that leaves both ends unknown has recorded that
// two things are related without recording that either is true.
func TestRedSupplementLeavesBothUnknown(t *testing.T) {
	fixture := newPairFixture(t, "MySQL 保存业务事实",
		"Redis 保存缓存和向量索引，MySQL 仍保存业务事实",
		model.AssertionDecision, model.AssertionDecision)
	// The earlier decision is in force before the supplement arrives.
	markCurrentForTest(t, fixture, fixture.earlier.AssertionID, "ep-old", baseTime)
	generator := fixture.generator(t, ModeApply,
		&scriptedClassifier{answer: model.RelationSupplements})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	source := fixture.store.states[fixture.earlier.AssertionID]
	target := fixture.store.states[fixture.later.AssertionID]
	if source.State != model.AssertionCurrent {
		t.Errorf("RED: the supplemented decision is %q, want current", source.State)
	}
	if target.State != model.AssertionCurrent {
		t.Errorf("RED: the supplementing decision is %q, want current - a supplement "+
			"that leaves both ends unknown records a relation between two things "+
			"it does not say are true", target.State)
	}
}

// A supplement may not resurrect a superseded fact.
func TestRedSupplementRefusedOverReplacedSource(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"超时策略补充：批处理任务额外放宽",
		model.AssertionDecision, model.AssertionDecision)
	fixture.store.states[fixture.earlier.AssertionID] = model.EpisodeAssertionState{
		AssertionID: fixture.earlier.AssertionID, UserID: owner, EpisodeID: "ep-old",
		Kind: model.AssertionDecision, State: model.AssertionReplaced,
		ReplacedByAssertionID: "lg_" + "0123456789abcdef0123456789abcdef",
		ReplacedByEpisodeID:   "ep-other",
		OccurredAt:            baseTime, Version: 1}
	generator := fixture.generator(t, ModeApply,
		&scriptedClassifier{answer: model.RelationSupplements})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if report.ByKind[model.RelationSupplements] != 0 {
		t.Error("RED: a supplement was accepted over an already superseded fact")
	}
	if state := fixture.store.states[fixture.earlier.AssertionID]; state.State != model.AssertionReplaced {
		t.Errorf("RED: the superseded fact was revived to %q", state.State)
	}
}

// Red: equivalent must leave at most one canonical current, and must not
// revive a superseded source.
func TestRedEquivalentCanonicalCurrent(t *testing.T) {
	t.Run("source already current keeps the source", func(t *testing.T) {
		fixture := newPairFixture(t, "默认使用 MySQL", "项目数据库默认仍是 MySQL",
			model.AssertionDecision, model.AssertionDecision)
		markCurrentForTest(t, fixture, fixture.earlier.AssertionID, "ep-old", baseTime)
		generator := fixture.generator(t, ModeApply,
			&scriptedClassifier{answer: model.RelationEquivalent})
		if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
			t.Fatal(err)
		}
		if state := fixture.store.states[fixture.earlier.AssertionID]; state.State != model.AssertionCurrent {
			t.Errorf("RED: the canonical current was lost: %q", state.State)
		}
		if state, ok := fixture.store.states[fixture.later.AssertionID]; ok &&
			state.State == model.AssertionCurrent {
			t.Error("RED: an equivalence created a second current fact")
		}
	})
	t.Run("source unknown promotes the later one", func(t *testing.T) {
		fixture := newPairFixture(t, "默认使用 MySQL", "项目数据库默认仍是 MySQL",
			model.AssertionDecision, model.AssertionDecision)
		generator := fixture.generator(t, ModeApply,
			&scriptedClassifier{answer: model.RelationEquivalent})
		if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
			t.Fatal(err)
		}
		if state := fixture.store.states[fixture.later.AssertionID]; state.State != model.AssertionCurrent {
			t.Errorf("RED: with no canonical current, the later restatement stayed %q",
				state.State)
		}
		if state, ok := fixture.store.states[fixture.earlier.AssertionID]; ok &&
			state.State == model.AssertionCurrent {
			t.Error("RED: both ends of an equivalence became current")
		}
	})
	t.Run("source replaced is refused", func(t *testing.T) {
		fixture := newPairFixture(t, "默认使用 MySQL", "项目数据库默认仍是 MySQL",
			model.AssertionDecision, model.AssertionDecision)
		fixture.store.states[fixture.earlier.AssertionID] = model.EpisodeAssertionState{
			AssertionID: fixture.earlier.AssertionID, UserID: owner, EpisodeID: "ep-old",
			Kind: model.AssertionDecision, State: model.AssertionReplaced,
			ReplacedByAssertionID: "lg_" + "0123456789abcdef0123456789abcdef",
			ReplacedByEpisodeID:   "ep-other",
			OccurredAt:            baseTime, Version: 1}
		generator := fixture.generator(t, ModeApply,
			&scriptedClassifier{answer: model.RelationEquivalent})
		report, err := generator.Generate(background(), []NewAssertion{fixture.later})
		if err != nil {
			t.Fatal(err)
		}
		if report.ByKind[model.RelationEquivalent] != 0 {
			t.Error("RED: an equivalence to a superseded fact was accepted")
		}
		if state, ok := fixture.store.states[fixture.later.AssertionID]; ok &&
			state.State == model.AssertionCurrent {
			t.Error("RED: a superseded fact was revived through an equivalence")
		}
	})
}

// Red 6 and 7: a replay re-classifies and advances the state version.
//
// Phase 1's rehearsal compared row counts and called that idempotence. Row
// counts are exactly what an UPDATE does not change: the second apply read the
// version, marked replaced again, marked current again, advanced both versions
// and moved both updated_at timestamps, and the count stayed the same
// throughout. A proof that cannot observe the thing going wrong is not a proof.
func TestRedReplayReclassifiesAndAdvancesVersion(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	classifier := &scriptedClassifier{answer: model.RelationReplaces}
	generator := fixture.generator(t, ModeApply, classifier)
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatal(err)
	}
	firstCalls := classifier.calls
	beforeSource := fixture.store.states[fixture.earlier.AssertionID]
	beforeTarget := fixture.store.states[fixture.later.AssertionID]

	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatal(err)
	}
	if classifier.calls != firstCalls {
		t.Errorf("RED: the replay classified again (%d -> %d calls); an applied "+
			"decision should be reused, not re-decided", firstCalls, classifier.calls)
	}
	afterSource := fixture.store.states[fixture.earlier.AssertionID]
	afterTarget := fixture.store.states[fixture.later.AssertionID]
	if afterSource.Version != beforeSource.Version {
		t.Errorf("RED: the replay advanced the source version %d -> %d",
			beforeSource.Version, afterSource.Version)
	}
	if afterTarget.Version != beforeTarget.Version {
		t.Errorf("RED: the replay advanced the target version %d -> %d",
			beforeTarget.Version, afterTarget.Version)
	}
	if !afterSource.UpdatedAt.Equal(beforeSource.UpdatedAt) {
		t.Error("RED: the replay moved the source updated_at")
	}
	if afterSource.ReplacedByAssertionID != beforeSource.ReplacedByAssertionID {
		t.Error("RED: the replay changed replaced_by")
	}
}

// markCurrentForTest puts one assertion in force before a scenario runs.
func markCurrentForTest(t *testing.T, fixture *pairFixture, assertionID, episodeID string,
	at time.Time) {
	t.Helper()
	_ = at
	fixture.store.states[assertionID] = model.EpisodeAssertionState{
		AssertionID: assertionID, UserID: owner, EpisodeID: episodeID,
		Kind: model.AssertionDecision, State: model.AssertionCurrent,
		OccurredAt: baseTime, Version: 1}
}
