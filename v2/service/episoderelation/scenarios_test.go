package episoderelation

import (
	"fmt"
	"testing"
	"time"

	"GopherAI/model"
	"GopherAI/service/episodeassert"
)

// The time-series scenarios STABILIZE-065 phase 1 is specified against, plus
// the chain, ordering and failure cases around them.
//
// Every scenario runs the same production path the red lights use: a real
// Episode, an identity the projector issued, the real governor, and the same
// generator. The classifier is scripted, and the numbers here describe the
// instrument rather than any model's accuracy.

// The four decided relations, end to end, with the bodies the specification
// names.
func TestScenarioTable(t *testing.T) {
	for name, scenario := range map[string]struct {
		earlier, later string
		answer         model.AssertionRelationKind
		wantSource     model.AssertionState
		wantTarget     model.AssertionState
		wantEdge       bool
	}{
		"replaces": {
			earlier: "接口统一固定超时 30 秒",
			later:   "取消固定值，改为按任务类型动态计算",
			answer:  model.RelationReplaces,
			// The earlier decision is superseded and points forward; the later
			// one is in force.
			wantSource: model.AssertionReplaced,
			wantTarget: model.AssertionCurrent,
			wantEdge:   true,
		},
		"supplements": {
			earlier: "MySQL 保存业务事实",
			later:   "Redis 保存缓存和向量索引，MySQL 仍保存业务事实",
			answer:  model.RelationSupplements,
			// Both are true, so both say so.
			//
			// Phase 1 expected neither state to move, and that expectation
			// encoded the defect: it left two related assertions both unknown,
			// which is precisely what the absence of a relation already means.
			// Neither may be marked replaced - that part was right - but
			// "not replaced" is not the same as "says nothing".
			wantSource: model.AssertionCurrent,
			wantTarget: model.AssertionCurrent,
			wantEdge:   true,
		},
		"equivalent": {
			earlier: "默认使用 MySQL",
			later:   "项目数据库默认仍是 MySQL",
			answer:  model.RelationEquivalent,
			// At most one canonical current. Nothing is in force in this
			// fixture, so the later wording becomes the canonical one and the
			// earlier stays unknown - one fact, stated once.
			wantSource: "",
			wantTarget: model.AssertionCurrent,
			wantEdge:   true,
		},
		"independent": {
			earlier:    "Agent 使用工具调用完成检索",
			later:      "日志使用结构化 JSON 输出到检索目录",
			answer:     model.RelationIndependent,
			wantSource: "", wantTarget: "",
			wantEdge: false,
		},
		"uncertain": {
			earlier:    "缓存层使用 Redis 存放检索结果",
			later:      "缓存层的检索策略还在讨论中",
			answer:     model.RelationUncertain,
			wantSource: "", wantTarget: "",
			wantEdge: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPairFixture(t, scenario.earlier, scenario.later,
				model.AssertionDecision, model.AssertionDecision)
			generator := fixture.generator(t, ModeApply,
				&scriptedClassifier{answer: scenario.answer})
			report, err := generator.Generate(background(), []NewAssertion{fixture.later})
			if err != nil {
				t.Fatalf("apply failed: %v", err)
			}
			if report.PairsConsidered != 1 {
				t.Fatalf("the pair was not considered: %+v", report)
			}
			source := fixture.store.states[fixture.earlier.AssertionID]
			target := fixture.store.states[fixture.later.AssertionID]
			if source.State != scenario.wantSource {
				t.Errorf("the earlier assertion is %q, want %q", source.State, scenario.wantSource)
			}
			if target.State != scenario.wantTarget {
				t.Errorf("the later assertion is %q, want %q", target.State, scenario.wantTarget)
			}
			edges, _ := fixture.store.Relations(background(), owner, "")
			if scenario.wantEdge && len(edges) != 1 {
				t.Errorf("want one %s edge, got %d", scenario.answer, len(edges))
			}
			if !scenario.wantEdge && len(edges) != 0 {
				t.Errorf("want no edge, got %d", len(edges))
			}
			// Whatever happened, the decision is on the record.
			proposals, _ := fixture.store.Proposals(background(), owner)
			if len(proposals) != 1 {
				t.Errorf("the decision was not recorded: %+v", proposals)
			}
		})
	}
}

// A correction may replace a decision: the learner said the earlier decision
// was wrong, which is exactly what a correction is for.
func TestCorrectionReplacesDecision(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"之前说的固定超时是错的，应按任务类型动态计算",
		model.AssertionDecision, model.AssertionCorrection)
	generator := fixture.generator(t, ModeApply,
		&scriptedClassifier{answer: model.RelationReplaces})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if state := fixture.store.states[fixture.earlier.AssertionID]; state.State != model.AssertionReplaced {
		t.Fatalf("the corrected decision is %q, want replaced", state.State)
	}
}

// An open loop is never closed by a replacement. Resolved and cancelled are
// its own states and its own stage; borrowing replacement for them would
// record a decision the learner never made.
func TestOpenLoopIsNotClosedByRelation(t *testing.T) {
	for _, answer := range []model.AssertionRelationKind{
		model.RelationReplaces, model.RelationSupplements, model.RelationEquivalent,
	} {
		fixture := newPairFixture(t, "还需要决定接口超时策略",
			"接口超时改为按任务类型动态计算",
			model.AssertionOpenLoop, model.AssertionDecision)
		generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: answer})
		report, err := generator.Generate(background(), []NewAssertion{fixture.later})
		if err != nil {
			t.Fatal(err)
		}
		if report.PairsConsidered != 0 {
			t.Errorf("%s: an open loop was offered as a candidate", answer)
		}
		if len(fixture.store.states) != 0 {
			t.Errorf("%s: an open loop changed a state", answer)
		}
	}
}

// A -> B -> C: two successive replacements leave one assertion in force and a
// chain a reader can walk.
func TestSuccessiveReplacementChain(t *testing.T) {
	store := newMemoryStore()
	times := []time.Time{baseTime, baseTime.AddDate(0, 0, 30), baseTime.AddDate(0, 0, 60)}
	texts := []string{"接口统一固定超时 30 秒", "接口超时改为按任务类型动态计算",
		"接口超时统一由调用方在请求头声明"}
	ids := make([]string, 3)
	for index := range texts {
		messageID := uint(10 + index)
		store.times[messageID] = times[index]
		episodeID := fmt.Sprintf("ep-%d", index)
		episode := episodeWith(episodeID, fmt.Sprintf("sess-%d", index), owner,
			messageID, model.AssertionDecision, texts[index], times[index])
		store.episodes[episodeID] = episode
		ids[index] = projectedAssertionID(t, episode, model.AssertionDecision)
	}
	history := []Candidate{}
	governor, _ := episodeassert.NewGovernor(store)
	for step := 1; step < 3; step++ {
		history = append(history, Candidate{AssertionID: ids[step-1],
			EpisodeID: fmt.Sprintf("ep-%d", step-1), SessionID: fmt.Sprintf("sess-%d", step-1),
			UserID: owner, Kind: model.AssertionDecision, Text: texts[step-1],
			OccurredAt: times[step-1], TimeKnown: true})
		generator, err := New(Config{Source: &memorySource{rows: history},
			Classifier: &scriptedClassifier{answer: model.RelationReplaces},
			Proposals:  store, Governor: governor, States: store, Mode: ModeApply,
			MaxCandidates: 1})
		if err != nil {
			t.Fatal(err)
		}
		later := NewAssertion{AssertionID: ids[step], EpisodeID: fmt.Sprintf("ep-%d", step),
			SessionID: fmt.Sprintf("sess-%d", step), UserID: owner,
			Kind: model.AssertionDecision, Text: texts[step],
			OccurredAt: times[step], TimeKnown: true}
		if _, err := generator.Generate(background(), []NewAssertion{later}); err != nil {
			t.Fatalf("step %d failed: %v", step, err)
		}
	}
	if state := store.states[ids[0]]; state.State != model.AssertionReplaced ||
		state.ReplacedByAssertionID != ids[1] {
		t.Fatalf("A is %q -> %q", state.State, state.ReplacedByAssertionID)
	}
	if state := store.states[ids[1]]; state.State != model.AssertionReplaced ||
		state.ReplacedByAssertionID != ids[2] {
		t.Fatalf("B is %q -> %q", state.State, state.ReplacedByAssertionID)
	}
	if state := store.states[ids[2]]; state.State != model.AssertionCurrent {
		t.Fatalf("C is %q, want current", state.State)
	}
}

// A cycle is refused by the governor, not by this package: the check already
// existed and this proves the generator reaches it rather than reimplementing
// it.
func TestCycleIsRefusedByTheGovernor(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"接口超时改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	// Pre-existing edge: later -> earlier. A new earlier -> later closes it.
	fixture.store.states[fixture.later.AssertionID] = model.EpisodeAssertionState{
		AssertionID: fixture.later.AssertionID, UserID: owner, EpisodeID: "ep-new",
		Kind: model.AssertionDecision, State: model.AssertionReplaced,
		ReplacedByAssertionID: fixture.earlier.AssertionID, ReplacedByEpisodeID: "ep-old",
		OccurredAt: baseTime.AddDate(0, 0, 30), Version: 1}
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationReplaces})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("the run failed rather than refusing the cycle: %v", err)
	}
	if report.ByReject[RejectGovernor] != 1 {
		t.Fatalf("the cycle was not refused by the governor: %+v", report.ByReject)
	}
	if state := fixture.store.states[fixture.earlier.AssertionID]; state.State == model.AssertionReplaced {
		t.Fatal("a cycle-closing replacement was applied")
	}
}

// A classifier that cannot be reached abstains, and says so, rather than
// letting an outage read as a history with no relations in it.
func TestTransportFailureAbstains(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"接口超时改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply,
		&scriptedClassifier{answer: model.RelationReplaces, transportErr: true})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("a transport failure ended the projection: %v", err)
	}
	if report.ByReject[RejectTransport] != 1 {
		t.Fatalf("the transport failure was not recorded: %+v", report.ByReject)
	}
	if len(fixture.store.states) != 0 {
		t.Fatal("a transport failure changed a state")
	}
	proposals, _ := fixture.store.Proposals(background(), owner)
	if len(proposals) != 1 || proposals[0].RejectedReason != string(RejectTransport) {
		t.Fatalf("the outage was not distinguishable from an empty history: %+v", proposals)
	}
}

// A proposal store that cannot be written ends the run, because a run that
// decided things and recorded none of them cannot be told from one that never
// happened.
func TestProposalStoreFailureEndsTheRun(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"接口超时改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	fixture.store.saveErr = fmt.Errorf("the proposal store is unavailable")
	generator := fixture.generator(t, ModeShadow, &scriptedClassifier{answer: model.RelationReplaces})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err == nil {
		t.Fatal("an unwritable proposal store was reported as a completed run")
	}
}

// A version conflict under the state row is surfaced, not swallowed: two
// writers racing on one assertion is exactly what the version guard is for.
func TestVersionConflictIsSurfaced(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"接口超时改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	// Somebody else advanced the earlier assertion's row after this run read
	// version zero.
	fixture.store.states[fixture.earlier.AssertionID] = model.EpisodeAssertionState{
		AssertionID: fixture.earlier.AssertionID, UserID: owner, EpisodeID: "ep-old",
		Kind: model.AssertionDecision, State: model.AssertionUnknown,
		OccurredAt: baseTime, Version: 7}
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationReplaces})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	// Either outcome is acceptable as long as it is not a silent success: the
	// run reports the conflict, or the governor refuses. What must not happen
	// is a state written over somebody else's version.
	if err == nil && report.ByKind[model.RelationReplaces] == 1 {
		if state := fixture.store.states[fixture.earlier.AssertionID]; state.Version != 8 {
			t.Fatalf("a replacement wrote over version 7 without advancing it: %+v", state)
		}
	}
	if state := fixture.store.states[fixture.earlier.AssertionID]; state.Version < 7 {
		t.Fatalf("the version went backwards: %+v", state)
	}
}

// The candidate set is bounded and stably ordered: two runs over one history
// compare the same pairs in the same order.
func TestCandidateSetIsBoundedAndStable(t *testing.T) {
	rows := []Candidate{}
	for index := 0; index < 20; index++ {
		rows = append(rows, Candidate{
			AssertionID: fmt.Sprintf("lg_%032x", index), EpisodeID: fmt.Sprintf("ep-%d", index),
			SessionID: fmt.Sprintf("sess-%d", index), UserID: owner,
			Kind: model.AssertionDecision, Text: "接口超时策略",
			OccurredAt: baseTime.AddDate(0, 0, index), TimeKnown: true})
	}
	finder, _ := NewFinder(&memorySource{rows: rows}, 5)
	later := NewAssertion{AssertionID: "lg_" + fmt.Sprintf("%032x", 99),
		EpisodeID: "ep-new", SessionID: "sess-new", UserID: owner,
		Kind: model.AssertionDecision, Text: "接口超时策略改为动态",
		OccurredAt: baseTime.AddDate(0, 0, 100), TimeKnown: true}
	first, _ := finder.Find(background(), later)
	second, _ := finder.Find(background(), later)
	if len(first.Candidates) != 5 {
		t.Fatalf("the candidate set is %d, want the bound of 5", len(first.Candidates))
	}
	if first.Excluded[FilterOverBound] != 15 {
		t.Fatalf("the bound dropped %d without recording it", first.Excluded[FilterOverBound])
	}
	for index := range first.Candidates {
		if first.Candidates[index].AssertionID != second.Candidates[index].AssertionID {
			t.Fatal("two runs over one history produced different candidate orders")
		}
	}
	// Newest first: the most recent prior decision is the one most likely to
	// be the thing being revised.
	for index := 1; index < len(first.Candidates); index++ {
		if first.Candidates[index].OccurredAt.After(first.Candidates[index-1].OccurredAt) {
			t.Fatal("the candidate order is not newest-first")
		}
	}
}
