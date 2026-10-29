package episoderelation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"GopherAI/model"
	"GopherAI/service/episodeassert"
)

// STABILIZE-065 red lights for the episodic write-side relation generator.
//
// Every one is a behavioural assertion about the production chain. A source
// scan appears in exactly one place, as a guard beside a behavioural test, and
// never instead of one.

var baseTime = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

const (
	owner      = int64(900000000000000101)
	otherOwner = int64(900000000000000102)
)

// assertionID builds a server-shaped assertion id the model layer accepts.
func assertionID(t *testing.T, seed string) string {
	t.Helper()
	id := model.LegacyAssertionID("ep-"+seed, model.AssertionDecision, nil, seed)
	if !model.ValidAssertionID(id) {
		t.Fatalf("the model issued an id it does not accept: %q", id)
	}
	return id
}

// --- the in-process stores ---

type memorySource struct {
	rows []Candidate
	err  error
}

func (m *memorySource) ActiveAssertions(_ context.Context, userID int64,
	excludeSession string) ([]Candidate, error) {
	if m.err != nil {
		return nil, m.err
	}
	out := []Candidate{}
	for _, row := range m.rows {
		// The source is owner-scoped, as the production repository is.
		if row.UserID != userID || row.SessionID == excludeSession {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// leakySource returns another learner's rows. It exists to prove the finder
// refuses them rather than trusting its source.
type leakySource struct{ rows []Candidate }

func (l *leakySource) ActiveAssertions(context.Context, int64, string) ([]Candidate, error) {
	return l.rows, nil
}

type memoryStore struct {
	proposals map[string]model.EpisodeRelationProposal
	relations []model.EpisodeAssertionRelation
	states    map[string]model.EpisodeAssertionState
	episodes  map[string]model.EpisodeMemory
	times     map[uint]time.Time
	saveErr   error
	relErr    error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{proposals: map[string]model.EpisodeRelationProposal{},
		states:   map[string]model.EpisodeAssertionState{},
		episodes: map[string]model.EpisodeMemory{},
		times:    map[uint]time.Time{}}
}

func (m *memoryStore) SaveProposal(_ context.Context, p model.EpisodeRelationProposal) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	if err := p.Validate(); err != nil {
		return err
	}
	m.proposals[p.IdempotencyKey] = p
	return nil
}

func (m *memoryStore) SaveRelation(_ context.Context, r model.EpisodeAssertionRelation) error {
	if m.relErr != nil {
		return m.relErr
	}
	if err := r.Validate(); err != nil {
		return err
	}
	for index, existing := range m.relations {
		if existing.UserID == r.UserID && existing.SourceAssertionID == r.SourceAssertionID &&
			existing.TargetAssertionID == r.TargetAssertionID {
			m.relations[index] = r
			return nil
		}
	}
	m.relations = append(m.relations, r)
	return nil
}

func (m *memoryStore) Relations(_ context.Context, userID int64,
	source string) ([]model.EpisodeAssertionRelation, error) {
	out := []model.EpisodeAssertionRelation{}
	for _, r := range m.relations {
		if r.UserID == userID && (source == "" || r.SourceAssertionID == source) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memoryStore) Proposals(_ context.Context, userID int64) ([]model.EpisodeRelationProposal, error) {
	out := []model.EpisodeRelationProposal{}
	for _, p := range m.proposals {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	return out, nil
}

// The episodeassert.Store half.
func (m *memoryStore) LoadStates(_ context.Context, userID int64,
	ids []string) (map[string]model.EpisodeAssertionState, error) {
	out := map[string]model.EpisodeAssertionState{}
	for _, id := range ids {
		if row, ok := m.states[id]; ok && row.UserID == userID {
			out[id] = row
		}
	}
	return out, nil
}

func (m *memoryStore) LoadEpisode(_ context.Context, userID int64,
	episodeID string) (model.EpisodeMemory, error) {
	episode, ok := m.episodes[episodeID]
	if !ok || episode.UserID != userID {
		return model.EpisodeMemory{}, fmt.Errorf("episode %s is not readable", episodeID)
	}
	return episode, nil
}

func (m *memoryStore) SaveState(_ context.Context, state model.EpisodeAssertionState,
	expectedVersion uint64) error {
	if err := state.Validate(); err != nil {
		return err
	}
	existing, ok := m.states[state.AssertionID]
	if ok && existing.Version != expectedVersion {
		return model.ErrAssertionVersionConflict
	}
	if !ok && expectedVersion != 0 {
		return model.ErrAssertionVersionConflict
	}
	m.states[state.AssertionID] = state
	return nil
}

func (m *memoryStore) MessageTimes(_ context.Context, _ int64,
	ids []uint) (map[uint]time.Time, error) {
	out := map[uint]time.Time{}
	for _, id := range ids {
		if at, ok := m.times[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}

// WithinTx makes the memory store atomic: it snapshots, runs, and restores on
// failure. A real repository uses one database transaction.
func (m *memoryStore) WithinTx(ctx context.Context,
	fn func(ProposalStore, episodeassert.Store) error) error {
	proposals := map[string]model.EpisodeRelationProposal{}
	for k, v := range m.proposals {
		proposals[k] = v
	}
	relations := append([]model.EpisodeAssertionRelation(nil), m.relations...)
	states := map[string]model.EpisodeAssertionState{}
	for k, v := range m.states {
		states[k] = v
	}
	if err := fn(m, m); err != nil {
		m.proposals, m.relations, m.states = proposals, relations, states
		return err
	}
	return nil
}

// nonAtomicStore is the same store with the transaction seam deliberately
// absent. It embeds nothing, so WithinTx is not promoted into it.
type nonAtomicStore struct{ inner *memoryStore }

func (n nonAtomicStore) SaveProposal(ctx context.Context, p model.EpisodeRelationProposal) error {
	return n.inner.SaveProposal(ctx, p)
}
func (n nonAtomicStore) SaveRelation(ctx context.Context, r model.EpisodeAssertionRelation) error {
	return n.inner.SaveRelation(ctx, r)
}
func (n nonAtomicStore) Relations(ctx context.Context, userID int64,
	source string) ([]model.EpisodeAssertionRelation, error) {
	return n.inner.Relations(ctx, userID, source)
}
func (n nonAtomicStore) Proposals(ctx context.Context,
	userID int64) ([]model.EpisodeRelationProposal, error) {
	return n.inner.Proposals(ctx, userID)
}

// --- the scripted classifier ---

type scriptedClassifier struct {
	answer   model.AssertionRelationKind
	identity string
	// wrongPair makes it answer about a pair it was not asked about.
	wrongPair bool
	// transportErr makes it unreachable.
	transportErr bool
	// seen records the requests, so a test can prove what it was shown.
	seen  []ClassifyRequest
	calls int
}

func (s *scriptedClassifier) Identity() string {
	if s.identity == "" {
		return "scripted_relation_classifier_v1"
	}
	return s.identity
}

func (s *scriptedClassifier) Classify(_ context.Context,
	request ClassifyRequest) (ClassifyResponse, error) {
	s.calls++
	s.seen = append(s.seen, request)
	if s.transportErr {
		return ClassifyResponse{}, fmt.Errorf("the classifier could not be reached")
	}
	pair := request.PairID
	if s.wrongPair {
		pair = "some-other-pair"
	}
	return ClassifyResponse{PairID: pair, Kind: s.answer}, nil
}

// --- the fixtures ---

type pairFixture struct {
	earlier Candidate
	later   NewAssertion
	store   *memoryStore
	source  *memorySource
}

func newPairFixture(t *testing.T, earlierText, laterText string,
	earlierKind, laterKind model.AssertionKind) *pairFixture {
	t.Helper()
	store := newMemoryStore()
	laterTime := baseTime.AddDate(0, 0, 30)
	store.times[11] = baseTime
	store.times[22] = laterTime

	oldEpisode := episodeWith("ep-old", "sess-old", owner, 11, earlierKind,
		earlierText, baseTime)
	newEpisode := episodeWith("ep-new", "sess-new", owner, 22, laterKind,
		laterText, laterTime)
	store.episodes["ep-old"] = oldEpisode
	store.episodes["ep-new"] = newEpisode

	// The identities come from the real projection, not from the test. An id
	// the test invented would prove the relation layer works on ids the
	// projector never issues.
	earlierID := projectedAssertionID(t, oldEpisode, earlierKind)
	laterID := projectedAssertionID(t, newEpisode, laterKind)

	earlier := Candidate{AssertionID: earlierID, EpisodeID: "ep-old",
		SessionID: "sess-old", UserID: owner, Kind: earlierKind,
		Text: earlierText, OccurredAt: baseTime, TimeKnown: true,
		State: model.AssertionUnknown}
	later := NewAssertion{AssertionID: laterID, EpisodeID: "ep-new",
		SessionID: "sess-new", UserID: owner, Kind: laterKind,
		Text: laterText, OccurredAt: laterTime, TimeKnown: true}
	return &pairFixture{earlier: earlier, later: later, store: store,
		source: &memorySource{rows: []Candidate{earlier}}}
}

// projectedAssertionID reads the id the production assertion projection issued
// for one Episode's slot of the given kind.
func projectedAssertionID(t *testing.T, episode model.EpisodeMemory,
	kind model.AssertionKind) string {
	t.Helper()
	for _, assertion := range model.AssertionsOf(episode) {
		if assertion.Kind == kind {
			return assertion.ID
		}
	}
	t.Fatalf("the projection issued no %s assertion for %s", kind, episode.ID)
	return ""
}

// episodeWith builds a readable Episode carrying one dated assertion slot, so
// the governor can resolve and order both ends of a relation exactly as it
// does for a projected card.
func episodeWith(episodeID, sessionID string, userID int64, messageID uint,
	kind model.AssertionKind, text string, at time.Time) model.EpisodeMemory {
	evidence := []model.EpisodeEvidence{{Text: text, EvidenceMessageIDs: []uint{messageID}}}
	document := model.EpisodeDocument{Synopsis: text}
	switch kind {
	case model.AssertionCorrection:
		document.UserCorrections = evidence
	case model.AssertionOpenLoop:
		document.OpenLoops = evidence
	default:
		document.Decisions = evidence
	}
	return model.EpisodeMemory{ID: episodeID, UserID: userID, SessionID: sessionID,
		Title: text, Document: document, SearchText: text,
		LastMessageAt: at, CreatedAt: at, UpdatedAt: at,
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}
}

func (f *pairFixture) generator(t *testing.T, mode Mode,
	classifier Classifier) *Generator {
	t.Helper()
	governor, err := episodeassert.NewGovernor(f.store)
	if err != nil {
		t.Fatal(err)
	}
	generator, err := New(Config{Source: f.source, Classifier: classifier,
		Proposals: f.store, Governor: governor, States: f.store, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	generator.UseClock(func() time.Time { return baseTime.AddDate(0, 0, 60) })
	return generator
}

func background() context.Context { return context.Background() }

// --- Red 1: there is an entry point at all ---

// RED: production projection finishes and nothing asks how the new assertions
// relate to the learner's history.
func TestRedNoRelationEntryPointAfterProjection(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeShadow, &scriptedClassifier{answer: model.RelationReplaces})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("the relation pass failed: %v", err)
	}
	if report.PairsConsidered == 0 {
		t.Fatal("RED: projection produced assertions and no pair was ever considered")
	}
	if report.ProposalsWritten == 0 {
		t.Fatal("RED: the pass decided nothing durable; a run that records nothing " +
			"cannot be told from a run that never happened")
	}
}

// --- Reds 2-5: the five relations land correctly ---

func TestRedReplacesIsRecordedAndApplied(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationReplaces})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if report.ByKind[model.RelationReplaces] != 1 {
		t.Fatalf("RED: the replacement was not recorded: %+v", report.ByKind)
	}
	source := fixture.store.states[fixture.earlier.AssertionID]
	target := fixture.store.states[fixture.later.AssertionID]
	if source.State != model.AssertionReplaced {
		t.Fatalf("RED: the earlier decision is %q, want replaced", source.State)
	}
	if source.ReplacedByAssertionID != fixture.later.AssertionID {
		t.Fatalf("RED: the earlier decision points at %q", source.ReplacedByAssertionID)
	}
	if target.State != model.AssertionCurrent {
		t.Fatalf("RED: the later decision is %q, want current", target.State)
	}
}

// RED: a supplement is written into ReplacedByAssertionID and marks a true
// statement superseded.
func TestRedSupplementIsNotDisguisedAsReplacement(t *testing.T) {
	fixture := newPairFixture(t, "MySQL 保存业务事实",
		"Redis 保存缓存和向量索引，MySQL 仍保存业务事实",
		model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationSupplements})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if state, ok := fixture.store.states[fixture.earlier.AssertionID]; ok &&
		state.State == model.AssertionReplaced {
		t.Fatal("RED: a supplement marked the earlier assertion replaced")
	}
	if state, ok := fixture.store.states[fixture.earlier.AssertionID]; ok &&
		state.ReplacedByAssertionID != "" {
		t.Fatalf("RED: a supplement wrote into replaced_by: %q", state.ReplacedByAssertionID)
	}
	relations, _ := fixture.store.Relations(background(), owner, fixture.earlier.AssertionID)
	if len(relations) != 1 || relations[0].Kind != model.RelationSupplements {
		t.Fatalf("RED: the supplement has no explicit relation row: %+v", relations)
	}
}

// RED: an equivalent restatement becomes a second competing current fact.
func TestRedEquivalentDoesNotCreateASecondCurrentFact(t *testing.T) {
	fixture := newPairFixture(t, "默认使用 MySQL",
		"项目数据库默认仍是 MySQL", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationEquivalent})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	currents := 0
	for _, state := range fixture.store.states {
		if state.State == model.AssertionCurrent {
			currents++
		}
	}
	if currents > 1 {
		t.Fatalf("RED: an equivalence produced %d competing current facts", currents)
	}
	relations, _ := fixture.store.Relations(background(), owner, fixture.earlier.AssertionID)
	if len(relations) != 1 || relations[0].Kind != model.RelationEquivalent {
		t.Fatalf("RED: the equivalence was not recorded: %+v", relations)
	}
	// And the original Episode history is untouched.
	if _, ok := fixture.store.episodes["ep-old"]; !ok {
		t.Fatal("RED: the equivalence deleted the earlier Episode")
	}
}

func TestRedIndependentWritesNoEdge(t *testing.T) {
	fixture := newPairFixture(t, "Agent 使用工具调用完成检索",
		"日志使用结构化 JSON 输出到检索目录", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationIndependent})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if relations, _ := fixture.store.Relations(background(), owner, ""); len(relations) != 0 {
		t.Fatalf("RED: independent assertions were given an edge: %+v", relations)
	}
	if len(fixture.store.states) != 0 {
		t.Fatalf("RED: independent assertions changed a state: %+v", fixture.store.states)
	}
}

// RED: uncertain is optimistically promoted to current.
func TestRedUncertainStaysUnknown(t *testing.T) {
	fixture := newPairFixture(t, "缓存层使用 Redis",
		"缓存策略还在讨论中", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationUncertain})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if state, ok := fixture.store.states[fixture.later.AssertionID]; ok &&
		state.State == model.AssertionCurrent {
		t.Fatal("RED: an uncertain relation made the new assertion current")
	}
	if relations, _ := fixture.store.Relations(background(), owner, ""); len(relations) != 0 {
		t.Fatal("RED: an uncertain outcome was stored as a relation")
	}
	proposals, _ := fixture.store.Proposals(background(), owner)
	if len(proposals) != 1 || proposals[0].Kind != model.RelationUncertain {
		t.Fatalf("RED: the uncertain decision was not recorded: %+v", proposals)
	}
}

// --- Reds 6-9: the candidate boundary ---

// RED: another learner's assertion reaches a candidate set.
func TestRedCrossUserCandidateLeak(t *testing.T) {
	foreign := Candidate{AssertionID: assertionID(t, "foreign"), EpisodeID: "ep-foreign",
		SessionID: "sess-foreign", UserID: otherOwner, Kind: model.AssertionDecision,
		Text: "接口统一固定超时 30 秒", OccurredAt: baseTime, TimeKnown: true}
	finder, err := NewFinder(&leakySource{rows: []Candidate{foreign}}, 8)
	if err != nil {
		t.Fatal(err)
	}
	set, err := finder.Find(background(), NewAssertion{
		AssertionID: assertionID(t, "mine"), EpisodeID: "ep-mine", SessionID: "sess-mine",
		UserID: owner, Kind: model.AssertionDecision, Text: "接口超时改为动态计算",
		OccurredAt: baseTime.AddDate(0, 0, 10), TimeKnown: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Candidates) != 0 {
		t.Fatalf("RED: another learner's assertion entered the candidate set: %+v",
			set.Candidates)
	}
	if set.Excluded[FilterForeignOwner] != 1 {
		t.Fatalf("RED: the foreign owner was dropped without being recorded: %+v",
			set.Excluded)
	}
}

// RED: the session being projected is treated as its own history.
func TestRedCurrentSessionIsNotItsOwnHistory(t *testing.T) {
	same := Candidate{AssertionID: assertionID(t, "same"), EpisodeID: "ep-a",
		SessionID: "sess-live", UserID: owner, Kind: model.AssertionDecision,
		Text: "接口统一固定超时 30 秒", OccurredAt: baseTime, TimeKnown: true}
	finder, _ := NewFinder(&leakySource{rows: []Candidate{same}}, 8)
	set, _ := finder.Find(background(), NewAssertion{
		AssertionID: assertionID(t, "new"), EpisodeID: "ep-b", SessionID: "sess-live",
		UserID: owner, Kind: model.AssertionDecision, Text: "接口超时改为动态计算",
		OccurredAt: baseTime.AddDate(0, 0, 1), TimeKnown: true})
	if len(set.Candidates) != 0 {
		t.Fatal("RED: the live session was offered as its own history")
	}
	if set.Excluded[FilterSameSession] != 1 {
		t.Fatalf("RED: the same-session exclusion was not recorded: %+v", set.Excluded)
	}
}

// RED: a candidate later than the new assertion is accepted, letting an older
// record present itself as the newer one.
func TestRedTargetEarlierThanSourceRefused(t *testing.T) {
	future := Candidate{AssertionID: assertionID(t, "future"), EpisodeID: "ep-future",
		SessionID: "sess-future", UserID: owner, Kind: model.AssertionDecision,
		Text: "接口统一固定超时 30 秒", OccurredAt: baseTime.AddDate(0, 0, 90), TimeKnown: true}
	finder, _ := NewFinder(&leakySource{rows: []Candidate{future}}, 8)
	set, _ := finder.Find(background(), NewAssertion{
		AssertionID: assertionID(t, "now"), EpisodeID: "ep-now", SessionID: "sess-now",
		UserID: owner, Kind: model.AssertionDecision, Text: "接口超时改为动态计算",
		OccurredAt: baseTime, TimeKnown: true})
	if len(set.Candidates) != 0 {
		t.Fatal("RED: a later assertion was offered as something the new one could replace")
	}
	if set.Excluded[FilterNotEarlier] != 1 {
		t.Fatalf("RED: the ordering exclusion was not recorded: %+v", set.Excluded)
	}
}

// RED: an open loop is related by replacement, standing in for a resolution
// the learner never made.
func TestRedIncompatibleKindsRefused(t *testing.T) {
	for name, kinds := range map[string][2]model.AssertionKind{
		"open loop as source": {model.AssertionOpenLoop, model.AssertionDecision},
		"open loop as target": {model.AssertionDecision, model.AssertionOpenLoop},
		"synopsis":            {model.AssertionSynopsis, model.AssertionDecision},
	} {
		if ComparableKinds(kinds[0], kinds[1]) {
			t.Errorf("RED: %s was treated as comparable", name)
		}
	}
	// And end to end: a classifier confidently saying "replaces" over an open
	// loop must still be refused by the server.
	fixture := newPairFixture(t, "还需要决定超时策略", "接口超时改为动态计算",
		model.AssertionOpenLoop, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationReplaces})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if report.ByKind[model.RelationReplaces] != 0 {
		t.Fatal("RED: an open loop was closed by a replacement")
	}
	if state, ok := fixture.store.states[fixture.earlier.AssertionID]; ok &&
		state.State == model.AssertionReplaced {
		t.Fatal("RED: the open loop was marked replaced")
	}
}

// --- Reds 10-11: idempotence and re-projection ---

// RED: running the pass twice produces two proposals for one pair.
func TestRedRepeatedRunDoesNotDuplicate(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	classifier := &scriptedClassifier{answer: model.RelationReplaces}
	generator := fixture.generator(t, ModeShadow, classifier)
	for run := 0; run < 3; run++ {
		if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
			t.Fatalf("run %d failed: %v", run+1, err)
		}
	}
	proposals, _ := fixture.store.Proposals(background(), owner)
	if len(proposals) != 1 {
		t.Fatalf("RED: three runs over one pair produced %d proposals", len(proposals))
	}
}

// RED: a re-projection that keeps the same assertion identity drifts to a
// different relation row.
func TestRedReProjectionKeepsTheSameRelation(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	classifier := &scriptedClassifier{answer: model.RelationReplaces}
	generator := fixture.generator(t, ModeShadow, classifier)
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatal(err)
	}
	first, _ := fixture.store.Proposals(background(), owner)
	// Re-project: the same Session yields the same stable identities.
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatal(err)
	}
	second, _ := fixture.store.Proposals(background(), owner)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("RED: re-projection changed the proposal count %d -> %d",
			len(first), len(second))
	}
	if first[0].IdempotencyKey != second[0].IdempotencyKey {
		t.Fatal("RED: re-projection produced a different proposal identity")
	}
}

// --- Reds 12-13: the model may not decide, and may not see the answers ---

// RED: a classifier answering about a different pair is believed.
func TestRedClassifierCannotAnswerAboutAnotherPair(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	generator := fixture.generator(t, ModeApply,
		&scriptedClassifier{answer: model.RelationReplaces, wrongPair: true})
	report, err := generator.Generate(background(), []NewAssertion{fixture.later})
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if report.ByReject[RejectProtocol] != 1 {
		t.Fatalf("RED: an answer about another pair was accepted: %+v", report.ByReject)
	}
	if _, ok := fixture.store.states[fixture.earlier.AssertionID]; ok {
		t.Fatal("RED: a protocol failure still changed a state")
	}
}

// RED: the classifier sees gold, expected relations or experiment labels.
func TestRedClassifierSeesNoGoldOrExperimentLabels(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	classifier := &scriptedClassifier{answer: model.RelationReplaces}
	generator := fixture.generator(t, ModeShadow, classifier)
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
		t.Fatal(err)
	}
	if len(classifier.seen) == 0 {
		t.Fatal("the classifier was never asked")
	}
	for _, request := range classifier.seen {
		blob := fmt.Sprintf("%+v", request)
		for _, forbidden := range []string{"gold", "expected", "subject_id",
			"experiment", "label", "sess-", "ep-old", "ep-new", fixture.earlier.AssertionID,
			fixture.later.AssertionID} {
			if strings.Contains(strings.ToLower(blob), strings.ToLower(forbidden)) {
				t.Errorf("RED: the classifier was shown %q: %s", forbidden, blob)
			}
		}
	}
}

// --- Reds 14-15: the shadow and apply boundary ---

// RED: shadow mode changed a state.
func TestRedShadowChangesNothing(t *testing.T) {
	for _, answer := range []model.AssertionRelationKind{
		model.RelationReplaces, model.RelationSupplements, model.RelationEquivalent,
	} {
		fixture := newPairFixture(t, "接口统一固定超时 30 秒",
			"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
		generator := fixture.generator(t, ModeShadow, &scriptedClassifier{answer: answer})
		if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err != nil {
			t.Fatal(err)
		}
		if len(fixture.store.states) != 0 {
			t.Errorf("RED: shadow mode changed a state on %s: %+v", answer,
				fixture.store.states)
		}
		if len(fixture.store.relations) != 0 {
			t.Errorf("RED: shadow mode wrote a relation edge on %s", answer)
		}
		proposals, _ := fixture.store.Proposals(background(), owner)
		if len(proposals) != 1 || proposals[0].Status != model.ProposalShadow {
			t.Errorf("RED: shadow mode left no shadow proposal on %s: %+v", answer, proposals)
		}
	}
}

// RED: a partial apply leaves the earlier assertion replaced by something
// that is not current.
func TestRedPartialApplyLeavesNoHalfRelation(t *testing.T) {
	fixture := newPairFixture(t, "接口统一固定超时 30 秒",
		"取消固定值，改为按任务类型动态计算", model.AssertionDecision, model.AssertionDecision)
	// The relation write fails after the states were marked.
	fixture.store.relErr = fmt.Errorf("the relation store is unavailable")
	generator := fixture.generator(t, ModeApply, &scriptedClassifier{answer: model.RelationReplaces})
	if _, err := generator.Generate(background(), []NewAssertion{fixture.later}); err == nil {
		t.Fatal("a failing relation store was reported as success")
	}
	source, sourceOK := fixture.store.states[fixture.earlier.AssertionID]
	target, targetOK := fixture.store.states[fixture.later.AssertionID]
	if sourceOK && source.State == model.AssertionReplaced &&
		(!targetOK || target.State != model.AssertionCurrent) {
		t.Fatal("RED: a failed apply left the earlier decision superseded by " +
			"something that is not in force")
	}
	if len(fixture.store.states) != 0 {
		t.Fatalf("RED: a failed apply left state behind: %+v", fixture.store.states)
	}
}

// An applying generator without an atomic store is refused at construction,
// not halfway through a replacement.
func TestApplyRefusesWithoutAnAtomicStore(t *testing.T) {
	fixture := newPairFixture(t, "a", "b", model.AssertionDecision, model.AssertionDecision)
	governor, _ := episodeassert.NewGovernor(fixture.store)
	_, err := New(Config{Source: fixture.source,
		Classifier: &scriptedClassifier{answer: model.RelationReplaces},
		Proposals:  nonAtomicStore{inner: fixture.store}, Governor: governor,
		States: fixture.store, Mode: ModeApply})
	if err == nil {
		t.Fatal("an applying generator was built over a store that cannot be atomic")
	}
	if !strings.Contains(err.Error(), string(RejectNotAtomic)) {
		t.Fatalf("the refusal does not name the reason: %v", err)
	}
}

// The default mode changes nothing at all.
func TestDefaultModeIsOff(t *testing.T) {
	if DefaultMode != ModeOff {
		t.Fatalf("the default relation mode is %q", DefaultMode)
	}
	generator, err := New(Config{})
	if err != nil {
		t.Fatalf("an off generator needs no dependencies: %v", err)
	}
	report, err := generator.Generate(background(), []NewAssertion{{AssertionID: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.PairsConsidered != 0 || report.ProposalsWritten != 0 {
		t.Fatalf("the off generator did something: %+v", report)
	}
}

// ProposalByKey lets the generator reuse a decision it already made. The real
// repository answers the same question from one indexed column.
func (m *memoryStore) ProposalByKey(_ context.Context, userID int64,
	key string) (model.EpisodeRelationProposal, bool, error) {
	proposal, ok := m.proposals[key]
	if !ok || proposal.UserID != userID {
		return model.EpisodeRelationProposal{}, false, nil
	}
	return proposal, true, nil
}
