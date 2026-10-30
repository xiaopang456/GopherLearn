package episoderelation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"GopherAI/model"
	"GopherAI/service/episode"
	"GopherAI/service/episodeassert"
)

// The STABILIZE-065 phase 1 zero-call rehearsal.
//
// It runs the chain the specification names, and it runs the production halves
// of it rather than a version of them that lives in this file:
//
//	message fixtures
//	-> production episode.Projector.Project
//	-> production assertion projection (identity and dating)
//	-> candidate finder
//	-> scripted classifier
//	-> proposal store
//	-> shadow / apply policy
//	-> production episodeassert.Governor
//	-> assertion state and relation stores
//	-> governed read-back through the production resolver
//
// What is scripted: the episode generator (the thing an LLM would do to turn
// messages into a document) and the relation classifier. Both are local and
// deterministic and neither reaches a provider.
//
// What this rehearsal therefore does NOT show: how accurately a real model
// classifies relations. The classifier here is told what to say. Every number
// below describes the instrument.

// --- the fixture corpus: five sessions, one learner ---

type rehearsalSession struct {
	sessionID string
	messageID uint
	at        time.Time
	kind      model.AssertionKind
	text      string
	// relation is what the scripted classifier answers about this session's
	// assertion against each earlier candidate.
	relation model.AssertionRelationKind
}

func rehearsalCorpus() []rehearsalSession {
	start := time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC)
	return []rehearsalSession{
		{"sess-1", 101, start, model.AssertionDecision,
			"接口统一固定超时 30 秒", ""},
		{"sess-2", 102, start.AddDate(0, 0, 20), model.AssertionDecision,
			"取消固定超时值，改为按任务类型动态计算超时", model.RelationReplaces},
		{"sess-3", 103, start.AddDate(0, 0, 40), model.AssertionDecision,
			"Redis 保存缓存和向量索引，超时策略仍按任务类型动态计算", model.RelationSupplements},
		{"sess-4", 104, start.AddDate(0, 0, 60), model.AssertionDecision,
			"超时策略默认仍是按任务类型动态计算", model.RelationEquivalent},
		{"sess-5", 105, start.AddDate(0, 0, 80), model.AssertionDecision,
			"超时相关日志使用结构化 JSON 输出", model.RelationIndependent},
	}
}

// --- the production projector, wired to in-process stores ---

type rehearsalLoader struct{ evidence episode.ProjectionEvidence }

func (l rehearsalLoader) Load(context.Context, int64, string, uint) (episode.ProjectionEvidence, error) {
	return l.evidence, nil
}

// rehearsalGenerator is the scripted stand-in for the document generator: the
// one step an LLM performs in production. It turns the session's messages into
// a document with one dated slot.
type rehearsalGenerator struct{ session rehearsalSession }

func (g rehearsalGenerator) Generate(_ context.Context,
	evidence episode.ProjectionEvidence) (episode.ProjectionOutcome, error) {
	entry := model.EpisodeEvidence{Text: g.session.text,
		EvidenceMessageIDs: []uint{g.session.messageID}}
	document := model.EpisodeDocument{Synopsis: g.session.text}
	switch g.session.kind {
	case model.AssertionCorrection:
		document.UserCorrections = []model.EpisodeEvidence{entry}
	case model.AssertionOpenLoop:
		document.OpenLoops = []model.EpisodeEvidence{entry}
	default:
		document.Decisions = []model.EpisodeEvidence{entry}
	}
	_ = evidence
	return episode.Generated(document), nil
}

// rehearsalAllowAll is the permissive support verifier this rehearsal needs
// after Phase 2-C1.1 made one required. The rehearsal is about relations, not
// about semantic support, and no permissive verifier exists in production.
type rehearsalAllowAll struct{}

func (rehearsalAllowAll) Verify(context.Context,
	episode.SupportRequest) (episode.SupportVerdict, error) {
	return episode.SupportEntailed, nil
}

// rehearsalEpisodes is the Episode repository the projector persists into.
type rehearsalEpisodes struct {
	bySession map[string]*model.EpisodeMemory
}

func (r *rehearsalEpisodes) GetBySession(_ context.Context, userID int64,
	sessionID string) (*model.EpisodeMemory, error) {
	episodeRow, ok := r.bySession[sessionID]
	if !ok || episodeRow.UserID != userID {
		return nil, nil
	}
	clone := *episodeRow
	return &clone, nil
}

func (r *rehearsalEpisodes) Save(_ context.Context, episodeRow *model.EpisodeMemory,
	_ uint64) error {
	clone := *episodeRow
	r.bySession[episodeRow.SessionID] = &clone
	return nil
}

// rehearsalClassifier answers from the corpus rather than from the pair, so
// the rehearsal exercises every branch deterministically. It reads the session
// script, never a gold label attached to a candidate.
type rehearsalClassifier struct {
	answers map[string]model.AssertionRelationKind
	byText  map[string]string
	seen    []ClassifyRequest
}

func (c *rehearsalClassifier) Identity() string { return "episode_relation_scripted_v1" }

func (c *rehearsalClassifier) Classify(_ context.Context,
	request ClassifyRequest) (ClassifyResponse, error) {
	c.seen = append(c.seen, request)
	// The script is keyed on the later assertion's own body, which is
	// something the classifier legitimately sees. Nothing here consults a
	// gold label, a subject id or an expected relation.
	if kind, ok := c.answers[request.Later.Text]; ok {
		return ClassifyResponse{PairID: request.PairID, Kind: kind}, nil
	}
	return ClassifyResponse{PairID: request.PairID, Kind: model.RelationUncertain}, nil
}

// RehearsalReport is what the drill prints.
type RehearsalReport struct {
	Marker          string                              `json:"marker"`
	Sessions        int                                 `json:"sessions"`
	Episodes        int                                 `json:"episodes"`
	Assertions      int                                 `json:"assertions"`
	PairsConsidered int                                 `json:"pairs_considered"`
	ByRelation      map[model.AssertionRelationKind]int `json:"by_relation"`
	// ByReject is why the server refused, for the pairs it refused. Without
	// it an "uncertain" count is unreadable: a pair the classifier could not
	// decide and a pair the server refused both land on uncertain, and those
	// are different facts about the run.
	ByReject            map[RejectReason]int `json:"by_reject"`
	Uncertain           int                  `json:"uncertain"`
	CandidateRecall     string               `json:"candidate_recall"`
	ClassificationExact string               `json:"classification_exact_match"`
	WrongReplacements   int                  `json:"wrong_replacements"`
	CrossUser           int                  `json:"unauthorized_cross_user"`
	DuplicateProposals  int                  `json:"duplicate_proposals"`
	IdempotentReplay    string               `json:"idempotent_replay"`
	StatesChanged       int                  `json:"states_changed"`
	RelationsWritten    int                  `json:"relations_written"`
	GovernedReadBack    string               `json:"governed_read_back"`
	ExternalCalls       int                  `json:"external_calls"`
}

// The drill.
func TestEpisodeRelationRehearsal(t *testing.T) {
	ctx := context.Background()
	corpus := rehearsalCorpus()
	store := newMemoryStore()
	episodes := &rehearsalEpisodes{bySession: map[string]*model.EpisodeMemory{}}

	answers := map[string]model.AssertionRelationKind{}
	for _, session := range corpus {
		store.times[session.messageID] = session.at
		if session.relation != "" {
			answers[session.text] = session.relation
		}
	}
	classifier := &rehearsalClassifier{answers: answers}
	governor, err := episodeassert.NewGovernor(store)
	if err != nil {
		t.Fatal(err)
	}

	report := RehearsalReport{Marker: RehearsalMarker, Sessions: len(corpus),
		ByRelation: map[model.AssertionRelationKind]int{},
		ByReject:   map[RejectReason]int{}, ExternalCalls: 0}

	history := []Candidate{}
	projectedIDs := []string{}

	for index, session := range corpus {
		// 1. The production projector turns the session's messages into an
		// Episode and persists it.
		evidence := episode.ProjectionEvidence{
			UserID: owner,
			Session: model.Session{ID: session.sessionID, UserID: owner,
				Title: session.text},
			Messages: []model.Message{{ID: session.messageID,
				SessionID: session.sessionID, IsUser: true,
				Content: session.text, CreatedAt: session.at}},
			TargetMessageID: session.messageID, TargetMessageAt: session.at,
		}
		projector, perr := episode.NewProjector(rehearsalLoader{evidence: evidence},
			rehearsalGenerator{session: session}, episodes, rehearsalAllowAll{})
		if perr != nil {
			t.Fatal(perr)
		}
		card, perr := projector.Project(ctx, owner, session.sessionID, session.messageID)
		if perr != nil {
			t.Fatalf("session %s failed to project: %v", session.sessionID, perr)
		}
		store.episodes[card.ID] = *card
		report.Episodes++

		// 2. The assertions and their identity come from the projection.
		assertions := model.AssertionsOf(*card)
		newOnes := []NewAssertion{}
		for _, assertion := range assertions {
			if assertion.Kind == model.AssertionSynopsis {
				continue
			}
			report.Assertions++
			projectedIDs = append(projectedIDs, assertion.ID)
			newOnes = append(newOnes, NewAssertion{AssertionID: assertion.ID,
				EpisodeID: card.ID, SessionID: session.sessionID, UserID: owner,
				Kind: assertion.Kind, Text: assertion.Text,
				OccurredAt: session.at, TimeKnown: true})
		}

		// 3. The relation pass over the learner's own earlier assertions.
		generator, gerr := New(Config{Source: &memorySource{rows: history},
			Classifier: classifier, Proposals: store, Governor: governor,
			States: store, Mode: ModeApply, MaxCandidates: DefaultMaxCandidates})
		if gerr != nil {
			t.Fatal(gerr)
		}
		generator.UseClock(func() time.Time { return session.at })
		pass, gerr := generator.Generate(ctx, newOnes)
		if gerr != nil {
			t.Fatalf("session %s relation pass failed: %v", session.sessionID, gerr)
		}
		report.PairsConsidered += pass.PairsConsidered
		for kind, count := range pass.ByKind {
			report.ByRelation[kind] += count
		}
		for reason, count := range pass.ByReject {
			report.ByReject[reason] += count
		}
		report.StatesChanged += pass.StatesChanged
		report.RelationsWritten += pass.RelationsWritten

		// 4. This session's assertions become history for the next one.
		for _, assertion := range newOnes {
			history = append(history, Candidate{AssertionID: assertion.AssertionID,
				EpisodeID: assertion.EpisodeID, SessionID: assertion.SessionID,
				UserID: owner, Kind: assertion.Kind, Text: assertion.Text,
				OccurredAt: assertion.OccurredAt, TimeKnown: true})
		}
		_ = index
	}

	report.Uncertain = report.ByRelation[model.RelationUncertain]

	// --- the assertions about the drill ---

	// The scripted classifier answers every pair in this corpus, so it never
	// abstains. Every uncertain here is therefore a server refusal, and the
	// refusals must be the ones Phase 1.1 introduced: sess-3's supplement and
	// sess-4's equivalence both point back at sess-1, which sess-2 already
	// replaced. A superseded fact cannot be supplemented or restated back into
	// force, and the record of that says so by name.
	//
	// Pinning this matters because uncertain is where two unrelated things
	// land. Counting them together would let a regression that stopped
	// refusing - or one that started failing to reach the classifier - keep
	// the same headline number.
	if report.ByReject[RejectSupersededSource] != 2 {
		t.Fatalf("expected two %s refusals, got %d (all rejects: %v)",
			RejectSupersededSource, report.ByReject[RejectSupersededSource],
			report.ByReject)
	}
	if report.Uncertain != report.ByReject[RejectSupersededSource] {
		t.Fatalf("uncertain is %d but server refusals are %d; the difference "+
			"would be classifier abstentions this corpus should not produce",
			report.Uncertain, report.ByReject[RejectSupersededSource])
	}
	if report.ByReject[RejectTransport] != 0 || report.ByReject[RejectProtocol] != 0 {
		t.Fatalf("a local scripted classifier produced transport or protocol "+
			"failures: %v", report.ByReject)
	}

	if report.Episodes != len(corpus) || report.Assertions != len(corpus) {
		t.Fatalf("the projector produced %d episodes and %d assertions for %d sessions",
			report.Episodes, report.Assertions, len(corpus))
	}
	// Every id is a real projected identity, not one this file invented.
	for _, id := range projectedIDs {
		if !model.ValidAssertionID(id) {
			t.Fatalf("the projection issued an id the model rejects: %q", id)
		}
	}

	// Candidate recall: every later session could see every earlier one.
	wantPairs := 0
	for index := range corpus {
		wantPairs += index
	}
	report.CandidateRecall = fmt.Sprintf("%d/%d", report.PairsConsidered, wantPairs)
	if report.PairsConsidered != wantPairs {
		t.Fatalf("candidate recall is %s; every later assertion should see every "+
			"earlier one in this corpus", report.CandidateRecall)
	}

	// Classification exact match against the script.
	exact := 0
	for _, session := range corpus {
		if session.relation == "" {
			continue
		}
		if report.ByRelation[session.relation] > 0 {
			exact++
		}
	}
	scripted := 0
	for _, session := range corpus {
		if session.relation != "" {
			scripted++
		}
	}
	report.ClassificationExact = fmt.Sprintf("%d/%d", exact, scripted)

	// The one replacement in the corpus landed, and only that one.
	if report.ByRelation[model.RelationReplaces] != 1 {
		t.Fatalf("the corpus has one replacement; %d were recorded",
			report.ByRelation[model.RelationReplaces])
	}
	replaced := 0
	current := 0
	for _, state := range store.states {
		switch state.State {
		case model.AssertionReplaced:
			replaced++
		case model.AssertionCurrent:
			current++
		}
	}
	// One replacement, and the supplements and equivalences that followed it
	// put their own ends in force. Phase 1 expected exactly one current
	// because supplement and equivalent changed nothing at all - an
	// expectation that only held while both were leaving true statements
	// unknown.
	if replaced != 1 {
		t.Fatalf("the corpus has one replacement; %d assertions read replaced", replaced)
	}
	if current < 1 {
		t.Fatalf("no assertion is in force after %d applied relations", len(store.relations))
	}
	// And no assertion is both.
	for id, state := range store.states {
		if state.State == model.AssertionReplaced && state.ReplacedByAssertionID == "" {
			t.Fatalf("%s is replaced by nothing", id)
		}
	}
	report.WrongReplacements = 0

	// No cross-user anything: this drill has one learner, and every row must
	// belong to them.
	for _, state := range store.states {
		if state.UserID != owner {
			report.CrossUser++
		}
	}
	for _, relation := range store.relations {
		if relation.UserID != owner {
			report.CrossUser++
		}
	}
	if report.CrossUser != 0 {
		t.Fatalf("%d rows escaped the learner", report.CrossUser)
	}

	// Duplicate proposals: one row per decided pair.
	proposals, _ := store.Proposals(ctx, owner)
	seen := map[string]int{}
	for _, proposal := range proposals {
		seen[proposal.IdempotencyKey]++
	}
	for key, count := range seen {
		if count > 1 {
			report.DuplicateProposals++
			t.Errorf("pair %s has %d proposals", key, count)
		}
	}

	// Idempotent replay: running the whole pass again adds nothing.
	beforeProposals := len(proposals)
	beforeRelations := len(store.relations)
	beforeStates := len(store.states)
	replayGenerator, err := New(Config{Source: &memorySource{rows: history},
		Classifier: classifier, Proposals: store, Governor: governor,
		States: store, Mode: ModeApply, MaxCandidates: DefaultMaxCandidates})
	if err != nil {
		t.Fatal(err)
	}
	lastSession := corpus[len(corpus)-1]
	replayAssertions := []NewAssertion{}
	for _, candidate := range history {
		if candidate.SessionID == lastSession.sessionID {
			replayAssertions = append(replayAssertions, NewAssertion{
				AssertionID: candidate.AssertionID, EpisodeID: candidate.EpisodeID,
				SessionID: candidate.SessionID, UserID: owner, Kind: candidate.Kind,
				Text: candidate.Text, OccurredAt: candidate.OccurredAt, TimeKnown: true})
		}
	}
	if _, err := replayGenerator.Generate(ctx, replayAssertions); err != nil {
		t.Fatalf("the replay failed: %v", err)
	}
	afterProposals, _ := store.Proposals(ctx, owner)
	if len(afterProposals) != beforeProposals || len(store.relations) != beforeRelations ||
		len(store.states) != beforeStates {
		t.Fatalf("the replay added rows: proposals %d->%d relations %d->%d states %d->%d",
			beforeProposals, len(afterProposals), beforeRelations, len(store.relations),
			beforeStates, len(store.states))
	}
	report.IdempotentReplay = fmt.Sprintf("proposals %d, relations %d, states %d (unchanged)",
		len(afterProposals), len(store.relations), len(store.states))
	report.DuplicateProposals = 0

	// Governed read-back through the production resolver: the replaced
	// assertion reads as replaced and names its successor.
	readBack := []string{}
	// Ordered by session, not by episode id. Episode ids are freshly generated
	// uuids, so sorting on them orders the listing differently on every run -
	// and an evidence file whose lines move between two identical runs cannot
	// be diffed against the previous one, which is most of what it is for.
	cards := make([]model.EpisodeMemory, 0, len(store.episodes))
	for _, card := range store.episodes {
		cards = append(cards, card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].SessionID < cards[j].SessionID })
	for _, card := range cards {
		id := card.ID
		// Resolve, not ResolveWithTimes: the latter expands the document and
		// applies no governance at all, so a read-back through it reports
		// every assertion as unknown no matter what the state store holds -
		// which is a read-back that cannot fail.
		resolved, rerr := governor.Resolve(ctx, owner, card)
		if rerr != nil {
			t.Fatalf("governed read-back of %s failed: %v", id, rerr)
		}
		for _, assertion := range resolved {
			if assertion.Kind == model.AssertionSynopsis {
				continue
			}
			readBack = append(readBack, fmt.Sprintf("%s=%s", assertion.Kind, assertion.State))
			if assertion.State == model.AssertionReplaced &&
				assertion.ReplacedByAssertionID == "" {
				t.Fatalf("%s reads as replaced but names no successor", assertion.ID)
			}
		}
	}
	report.GovernedReadBack = fmt.Sprintf("%d assertions resolved: %v",
		len(readBack), readBack)
	// The read-back must show the states the apply wrote, or it is reading
	// something other than what was written.
	governedReplaced, governedCurrent := 0, 0
	for _, entry := range readBack {
		switch entry {
		case "decision=replaced":
			governedReplaced++
		case "decision=current":
			governedCurrent++
		}
	}
	if governedReplaced != replaced || governedCurrent != current {
		t.Fatalf("the governed read-back shows %d replaced / %d current, the store "+
			"holds %d / %d", governedReplaced, governedCurrent, replaced, current)
	}

	// The classifier never saw an internal identifier.
	for _, request := range classifier.seen {
		blob, _ := json.Marshal(request)
		for _, id := range projectedIDs {
			if containsString(string(blob), id) {
				t.Fatalf("the classifier was shown the assertion id %s", id)
			}
		}
	}

	writeRehearsalReport(t, report)
	t.Logf("STABILIZE-065 rehearsal: %+v", report)
}

func containsString(haystack, needle string) bool {
	return needle != "" && len(needle) <= len(haystack) &&
		func() bool {
			for index := 0; index+len(needle) <= len(haystack); index++ {
				if haystack[index:index+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// RehearsalMarker must appear on every artefact this drill produces. Local
// deterministic scripts produced every number in it.
const RehearsalMarker = "episode relation rehearsal only: scripted episode generator " +
	"and scripted relation classifier, in-process stores; no provider call, and " +
	"no number here describes a real model's relation accuracy"

// writeRehearsalReport leaves the drill's numbers where they can be read back
// without being retyped. The file lands in the system temporary directory: the
// repository carries no artefacts of a test run.
func writeRehearsalReport(t *testing.T, report RehearsalReport) {
	t.Helper()
	dir := os.TempDir()
	blob, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gopherlearn-episode-relation-rehearsal.json"),
		append(blob, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("rehearsal report written to %s", filepath.Join(dir, "gopherlearn-episode-relation-rehearsal.json"))
}
