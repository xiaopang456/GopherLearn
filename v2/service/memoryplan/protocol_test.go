package memoryplan

import (
	"errors"
	"strings"
	"testing"

	"GopherAI/model"

	"github.com/cloudwego/eino/schema"
)

func planMessage(arguments string) *schema.Message {
	return &schema.Message{ToolCalls: []schema.ToolCall{{
		Function: schema.FunctionCall{Name: PlanToolName, Arguments: arguments},
	}}}
}

func verdictMessage(arguments string) *schema.Message {
	return &schema.Message{ToolCalls: []schema.ToolCall{{
		Function: schema.FunctionCall{Name: VerdictToolName, Arguments: arguments},
	}}}
}

func testIndex() CatalogueIndex {
	return CatalogueIndex{
		TopicKeys:  map[string]struct{}{"漏桶算法": {}, "死信队列": {}, "优雅关闭": {}, "内存逃逸": {}},
		EpisodeIDs: map[string]struct{}{"e-1": {}, "e-2": {}, "e-3": {}, "e-4": {}, "e-5": {}, "e-6": {}},
	}
}

func plan(decision Decision, topics, episodes []string, query string) MemoryReadPlan {
	return MemoryReadPlan{
		SchemaVersion: PlanSchemaVersion, Decision: decision, Purpose: PurposeHistoryAnswer,
		TopicKeys: topics, EpisodeIDs: episodes, EpisodeQuery: query,
		TimeScope: TimeScope{Kind: TimeAny}, EvidenceKinds: []EvidenceKind{},
	}
}

// All five shapes a plan may legally take.
func TestEveryLegalDecisionShapeIsAccepted(t *testing.T) {
	index := testIndex()
	for name, candidate := range map[string]MemoryReadPlan{
		"none":                  plan(DecisionNone, nil, nil, ""),
		"semantic":              plan(DecisionSemantic, []string{"漏桶算法"}, nil, ""),
		"episodic by id":        plan(DecisionEpisodic, nil, []string{"e-1"}, ""),
		"episodic by query":     plan(DecisionEpisodic, nil, nil, "关于死信的那次讨论"),
		"episodic by topic key": plan(DecisionEpisodic, []string{"死信队列"}, nil, ""),
		"mixed":                 plan(DecisionMixed, []string{"漏桶算法"}, []string{"e-1"}, ""),
	} {
		if err := candidate.ValidateAgainst(index); err != nil {
			t.Fatalf("%s should be a legal plan: %v", name, err)
		}
	}
}

// "none" means nothing, and a plan that says none while naming candidates is a
// model that answered two questions at once. Reading either half of that as
// authoritative would be a guess.
func TestANoneDecisionMayNotCarryCandidates(t *testing.T) {
	index := testIndex()
	for name, candidate := range map[string]MemoryReadPlan{
		"topic":   plan(DecisionNone, []string{"漏桶算法"}, nil, ""),
		"episode": plan(DecisionNone, nil, []string{"e-1"}, ""),
		"query":   plan(DecisionNone, nil, nil, "随便找找"),
	} {
		if err := candidate.ValidateAgainst(index); err == nil {
			t.Fatalf("a none decision carrying a %s must be refused", name)
		}
	}
}

// An id that was never in the catalogue is a record the model invented, and
// looking it up "just in case" is how an invented record becomes a real lookup.
func TestInventedIdentifiersAreRefused(t *testing.T) {
	index := testIndex()
	for name, candidate := range map[string]MemoryReadPlan{
		"invented topic key": plan(DecisionSemantic, []string{"从未存在的主题"}, nil, ""),
		"invented episode":   plan(DecisionEpisodic, nil, []string{"e-999"}, ""),
		"repeated topic key": plan(DecisionSemantic, []string{"漏桶算法", "漏桶算法"}, nil, ""),
		"repeated episode":   plan(DecisionEpisodic, nil, []string{"e-1", "e-1"}, ""),
		"empty topic key":    plan(DecisionSemantic, []string{""}, nil, ""),
	} {
		if err := candidate.ValidateAgainst(index); !errors.Is(err, ErrProtocolFailure) {
			t.Fatalf("%s must be a protocol failure, got %v", name, err)
		}
	}
}

func TestTheCapsOnAPlanAreEnforced(t *testing.T) {
	index := testIndex()
	tooManyTopics := plan(DecisionSemantic, []string{"漏桶算法", "死信队列", "优雅关闭", "内存逃逸"}, nil, "")
	if err := tooManyTopics.ValidateAgainst(index); err == nil {
		t.Fatalf("%d topic keys must be refused", MaxPlanTopicKeys+1)
	}
	tooManyEpisodes := plan(DecisionEpisodic, nil, []string{"e-1", "e-2", "e-3", "e-4", "e-5", "e-6"}, "")
	if err := tooManyEpisodes.ValidateAgainst(index); err == nil {
		t.Fatalf("%d episode ids must be refused", MaxPlanEpisodeIDs+1)
	}
}

func TestTimeScopesAreCheckedRatherThanTrusted(t *testing.T) {
	index := testIndex()
	build := func(scope TimeScope) MemoryReadPlan {
		candidate := plan(DecisionEpisodic, nil, []string{"e-1"}, "")
		candidate.TimeScope = scope
		return candidate
	}
	for name, scope := range map[string]TimeScope{
		"unknown kind":        {Kind: "sometime"},
		"unparseable from":    {Kind: TimeAfter, From: "last tuesday"},
		"between without to":  {Kind: TimeBetween, From: "2026-08-01T00:00:00Z"},
		"between reversed":    {Kind: TimeBetween, From: "2026-09-01T00:00:00Z", To: "2026-08-01T00:00:00Z"},
		"before without to":   {Kind: TimeBefore},
		"after without from":  {Kind: TimeAfter},
		"latest with a bound": {Kind: TimeLatest, From: "2026-08-01T00:00:00Z"},
	} {
		if err := build(scope).ValidateAgainst(index); err == nil {
			t.Fatalf("time scope %q must be refused", name)
		}
	}
	valid := build(TimeScope{Kind: TimeBetween, From: "2026-08-01T00:00:00Z", To: "2026-09-01T00:00:00Z"})
	if err := valid.ValidateAgainst(index); err != nil {
		t.Fatalf("a well-formed between scope must be accepted: %v", err)
	}
}

func TestUnknownAndRepeatedEvidenceKindsAreRefused(t *testing.T) {
	index := testIndex()
	unknown := plan(DecisionEpisodic, nil, []string{"e-1"}, "")
	unknown.EvidenceKinds = []EvidenceKind{"resolution"}
	if err := unknown.ValidateAgainst(index); err == nil {
		t.Fatal("an unknown evidence kind must be refused")
	}
	repeated := plan(DecisionEpisodic, nil, []string{"e-1"}, "")
	repeated.EvidenceKinds = []EvidenceKind{EvidenceDecision, EvidenceDecision}
	if err := repeated.ValidateAgainst(index); err == nil {
		t.Fatal("a repeated evidence kind must be refused")
	}
}

// Unknown fields are refused rather than ignored. A model that answered a schema
// this code does not know is a model whose other fields cannot be trusted to
// mean what they are read as.
func TestDecodeRefusesExtraFieldsAndTrailingText(t *testing.T) {
	base := `{"schema_version":"memory.read-plan.v1","decision":"none","purpose":"personalization",` +
		`"topic_keys":[],"episode_ids":[],"episode_query":"","time_scope":{"kind":"any","from":"","to":""},` +
		`"evidence_kinds":[]}`
	if _, err := DecodePlan(planMessage(base)); err != nil {
		t.Fatalf("the well-formed plan must decode: %v", err)
	}
	extra := strings.Replace(base, `"decision":"none"`, `"decision":"none","confidence":0.9`, 1)
	if _, err := DecodePlan(planMessage(extra)); !errors.Is(err, ErrProtocolFailure) {
		t.Fatalf("an extra field must be a protocol failure, got %v", err)
	}
	if _, err := DecodePlan(planMessage(base + ` trailing`)); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("trailing content must be a protocol failure")
	}
	if _, err := DecodePlan(planMessage(`{"decision":`)); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("malformed JSON must be a protocol failure")
	}
}

func TestDecodeRequiresExactlyOneCallOfTheRightTool(t *testing.T) {
	if _, err := DecodePlan(nil); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("no message is a protocol failure")
	}
	if _, err := DecodePlan(&schema.Message{Content: "here is my plan"}); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("prose instead of a tool call is a protocol failure")
	}
	wrongTool := &schema.Message{ToolCalls: []schema.ToolCall{{
		Function: schema.FunctionCall{Name: "something_else", Arguments: "{}"},
	}}}
	if _, err := DecodePlan(wrongTool); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("the wrong tool is a protocol failure")
	}
	twice := planMessage("{}")
	twice.ToolCalls = append(twice.ToolCalls, twice.ToolCalls[0])
	if _, err := DecodePlan(twice); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("two calls are a protocol failure")
	}
}

// Order is part of the plan's identity because candidate order is what the
// deterministic relation ordering rests on. Two plans that differ only in the
// order of their ids produce different pools, so they are different plans.
func TestPlanHashCoversContentAndOrder(t *testing.T) {
	first := plan(DecisionMixed, []string{"漏桶算法", "死信队列"}, []string{"e-1", "e-2"}, "")
	same := plan(DecisionMixed, []string{"漏桶算法", "死信队列"}, []string{"e-1", "e-2"}, "")
	reordered := plan(DecisionMixed, []string{"死信队列", "漏桶算法"}, []string{"e-1", "e-2"}, "")
	different := plan(DecisionMixed, []string{"漏桶算法", "死信队列"}, []string{"e-1", "e-3"}, "")
	if first.Hash() != same.Hash() {
		t.Fatal("identical plans must hash identically")
	}
	if first.Hash() == reordered.Hash() {
		t.Fatal("a reordered plan is a different plan")
	}
	if first.Hash() == different.Hash() {
		t.Fatal("a plan naming different episodes must hash differently")
	}
}

func TestWantsKindTreatsAnEmptyFilterAsEverything(t *testing.T) {
	all := plan(DecisionEpisodic, nil, []string{"e-1"}, "")
	for _, kind := range []EvidenceKind{EvidenceSynopsis, EvidenceDecision, EvidenceOpenLoop, EvidenceCorrection} {
		if !all.WantsKind(kind) {
			t.Fatalf("an empty filter must want %s", kind)
		}
	}
	narrowed := all
	narrowed.EvidenceKinds = []EvidenceKind{EvidenceCorrection}
	if narrowed.WantsKind(EvidenceDecision) || !narrowed.WantsKind(EvidenceCorrection) {
		t.Fatal("a narrowed filter must want only what it names")
	}
}

// --------------------------------------------------------------- verdicts ---

func semanticCandidate(id string) Candidate {
	return Candidate{ID: id, Kind: CandidateSemantic, Memory: model.LongTermMemory{
		ID: id, Scope: model.MemoryScopeTopic, Subject: "s", Content: "c",
	}}
}

func episodeCandidate(id string, decisions, openLoops int, withMessages bool) Candidate {
	document := model.EpisodeDocument{Synopsis: "一次讨论"}
	messages := []uint{7}
	if !withMessages {
		messages = nil
	}
	for index := 0; index < decisions; index++ {
		document.Decisions = append(document.Decisions, model.EpisodeEvidence{
			Text: "决定", EvidenceMessageIDs: messages,
		})
	}
	for index := 0; index < openLoops; index++ {
		document.OpenLoops = append(document.OpenLoops, model.EpisodeEvidence{
			Text: "未完", EvidenceMessageIDs: messages,
		})
	}
	return Candidate{ID: id, Kind: CandidateEpisode, Episode: model.EpisodeMemory{ID: id, Document: document}}
}

func verdictSet(entries ...CandidateVerdict) VerdictSet {
	return VerdictSet{SchemaVersion: VerdictSchemaVersion, Verdicts: entries}
}

func TestEveryRelationIsAcceptedWithItsOwnFieldRule(t *testing.T) {
	candidates := []Candidate{
		semanticCandidate("m-1"), semanticCandidate("m-2"),
		episodeCandidate("e-1", 2, 1, true), episodeCandidate("e-2", 1, 0, true),
		episodeCandidate("e-3", 1, 0, true),
	}
	set := verdictSet(
		CandidateVerdict{CandidateID: "m-1", Relation: RelationAnswer, FieldRefs: []string{"content"}},
		CandidateVerdict{CandidateID: "m-2", Relation: RelationPersonalization, FieldRefs: []string{"content"}},
		CandidateVerdict{CandidateID: "e-1", Relation: RelationContradiction, FieldRefs: []string{"decisions[1]"}},
		CandidateVerdict{CandidateID: "e-2", Relation: RelationRelated, FieldRefs: []string{}},
		CandidateVerdict{CandidateID: "e-3", Relation: RelationInsufficient, FieldRefs: []string{}},
	)
	if _, err := ValidateVerdicts(set, candidates); err != nil {
		t.Fatalf("all five relations must be acceptable: %v", err)
	}
}

// The candidate set must match exactly. A missing verdict would be silently kept
// or silently dropped depending on which default someone picked, and neither
// default is a measurement.
func TestTheVerdictSetMustCoverExactlyTheCandidatesSent(t *testing.T) {
	candidates := []Candidate{semanticCandidate("m-1"), semanticCandidate("m-2")}
	missing := verdictSet(CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated})
	if _, err := ValidateVerdicts(missing, candidates); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("a missing verdict must be a protocol failure")
	}
	invented := verdictSet(
		CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated},
		CandidateVerdict{CandidateID: "m-9", Relation: RelationRelated},
	)
	if _, err := ValidateVerdicts(invented, candidates); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("a verdict for a candidate that was never sent must be a protocol failure")
	}
	duplicated := verdictSet(
		CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated},
		CandidateVerdict{CandidateID: "m-1", Relation: RelationAnswer, FieldRefs: []string{"content"}},
	)
	if _, err := ValidateVerdicts(duplicated, candidates); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("judging one candidate twice must be a protocol failure")
	}
}

func TestFieldReferencesMustAddressSomethingTheCandidateHas(t *testing.T) {
	candidates := []Candidate{semanticCandidate("m-1"), episodeCandidate("e-1", 1, 0, true)}
	for name, refs := range map[string][2][]string{
		"episode shape on a memory":  {{"decisions[0]"}, {"decisions[0]"}},
		"index past the end":         {{"content"}, {"decisions[4]"}},
		"a list that does not exist": {{"content"}, {"resolutions[0]"}},
		"content on an episode":      {{"content"}, {"content"}},
		"a synopsis that is absent":  {{"content"}, {"open_loops[0]"}},
	} {
		set := verdictSet(
			CandidateVerdict{CandidateID: "m-1", Relation: RelationAnswer, FieldRefs: refs[0]},
			CandidateVerdict{CandidateID: "e-1", Relation: RelationAnswer, FieldRefs: refs[1]},
		)
		if _, err := ValidateVerdicts(set, candidates); !errors.Is(err, ErrProtocolFailure) {
			t.Fatalf("%s must be a protocol failure, got %v", name, err)
		}
	}
}

// A record the answer will rest on has to still be traceable to real messages.
// An entry whose provenance is gone is a compressed claim with nothing behind it.
func TestTheStrongestRelationsNeedTraceableEvidence(t *testing.T) {
	candidates := []Candidate{episodeCandidate("e-1", 1, 0, false)}
	for _, relation := range []Relation{RelationAnswer, RelationContradiction} {
		set := verdictSet(CandidateVerdict{CandidateID: "e-1", Relation: relation, FieldRefs: []string{"decisions[0]"}})
		if _, err := ValidateVerdicts(set, candidates); !errors.Is(err, ErrProtocolFailure) {
			t.Fatalf("%s resting on a source-less entry must be refused", relation)
		}
	}
	// Personalisation may rest on the same entry: it changes how an answer is
	// pitched rather than asserting a fact the learner will be told they said.
	set := verdictSet(CandidateVerdict{CandidateID: "e-1", Relation: RelationPersonalization, FieldRefs: []string{"decisions[0]"}})
	if _, err := ValidateVerdicts(set, candidates); err != nil {
		t.Fatalf("personalisation on a compressed entry is allowed: %v", err)
	}
}

// An uninjectable verdict may name the field it looked at, and that reference is
// inert. The provider pre-check is why: the model routinely answers "I read
// decisions[0]; it does not answer this", which is a correct judgement, and the
// earlier rule turned it into a protocol failure, a retry, and a failed stage.
// What still has to hold is that the reference addresses something real, so an
// invented index is caught whatever the relation.
func TestAnUninjectableVerdictMayCiteTheFieldItLookedAt(t *testing.T) {
	candidates := []Candidate{semanticCandidate("m-1"), episodeCandidate("e-1", 1, 0, true)}
	citing := verdictSet(
		CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated, FieldRefs: []string{"content"}},
		CandidateVerdict{CandidateID: "e-1", Relation: RelationInsufficient, FieldRefs: []string{"decisions[0]"}},
	)
	verdicts, err := ValidateVerdicts(citing, candidates)
	if err != nil {
		t.Fatalf("an inert reference must be accepted: %v", err)
	}
	if kept := SelectInjectable(verdicts, candidates); len(kept) != 0 {
		t.Fatal("naming a field must not make an uninjectable relation injectable")
	}
	invented := verdictSet(
		CandidateVerdict{CandidateID: "m-1", Relation: RelationRelated, FieldRefs: []string{"content"}},
		CandidateVerdict{CandidateID: "e-1", Relation: RelationRelated, FieldRefs: []string{"decisions[9]"}},
	)
	if _, err := ValidateVerdicts(invented, candidates); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("an out-of-range index must still be refused, whatever the relation")
	}
}

// A provenance-less entry is still allowed to be judged unrelated: the rule
// exists to stop the learner being told they said something the record cannot
// support, and an uninjected candidate tells them nothing.
func TestAnUninjectableVerdictNeedsNoProvenance(t *testing.T) {
	candidates := []Candidate{episodeCandidate("e-1", 1, 0, false)}
	set := verdictSet(CandidateVerdict{CandidateID: "e-1", Relation: RelationRelated, FieldRefs: []string{"decisions[0]"}})
	if _, err := ValidateVerdicts(set, candidates); err != nil {
		t.Fatalf("an unrelated verdict on a compressed entry must be accepted: %v", err)
	}
}

func TestAnInjectableVerdictMustCiteAField(t *testing.T) {
	candidates := []Candidate{semanticCandidate("m-1")}
	bare := verdictSet(CandidateVerdict{CandidateID: "m-1", Relation: RelationAnswer, FieldRefs: []string{}})
	if _, err := ValidateVerdicts(bare, candidates); err == nil {
		t.Fatal("an answer_evidence verdict citing nothing must be refused")
	}
}

// The safety half of the whole task: the two relations that name "on the subject
// but not supporting" never reach the context.
func TestRelatedAndInsufficientAreNeverInjectable(t *testing.T) {
	for _, relation := range []Relation{RelationRelated, RelationInsufficient} {
		if relation.Injectable() {
			t.Fatalf("%s must never be injectable", relation)
		}
	}
	for _, relation := range []Relation{RelationAnswer, RelationPersonalization, RelationContradiction} {
		if !relation.Injectable() {
			t.Fatalf("%s must be injectable", relation)
		}
	}
}

// Contradiction first, then answer, then personalisation; inside one relation the
// candidate pool's own order. A question built on a premise the learner's history
// denies is answered wrongly by anything else, however relevant.
func TestInjectionOrderIsRelationThenPoolPosition(t *testing.T) {
	candidates := []Candidate{
		semanticCandidate("m-1"), semanticCandidate("m-2"), semanticCandidate("m-3"),
		semanticCandidate("m-4"), semanticCandidate("m-5"),
	}
	verdicts := []CandidateVerdict{
		{CandidateID: "m-1", Relation: RelationPersonalization},
		{CandidateID: "m-2", Relation: RelationAnswer},
		{CandidateID: "m-3", Relation: RelationContradiction},
		{CandidateID: "m-4", Relation: RelationAnswer},
		{CandidateID: "m-5", Relation: RelationRelated},
	}
	got := SelectInjectable(verdicts, candidates)
	want := []string{"m-3", "m-2", "m-4", "m-1"}
	if len(got) != len(want) {
		t.Fatalf("kept %d candidates, wanted %d", len(got), len(want))
	}
	for index, id := range want {
		if got[index].ID != id {
			t.Fatalf("position %d is %s, wanted %s", index, got[index].ID, id)
		}
	}
}

func TestDecodeVerdictsIsAsStrictAsDecodePlan(t *testing.T) {
	base := `{"schema_version":"memory.verdict.v1","verdicts":[{"candidate_id":"m-1",` +
		`"relation":"related_only","field_refs":[]}]}`
	if _, err := DecodeVerdicts(verdictMessage(base)); err != nil {
		t.Fatalf("the well-formed verdict set must decode: %v", err)
	}
	extra := strings.Replace(base, `"relation":"related_only"`, `"relation":"related_only","score":1`, 1)
	if _, err := DecodeVerdicts(verdictMessage(extra)); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("an extra field must be a protocol failure")
	}
	wrongVersion := strings.Replace(base, VerdictSchemaVersion, "memory.verdict.v2", 1)
	if _, err := DecodeVerdicts(verdictMessage(wrongVersion)); !errors.Is(err, ErrProtocolFailure) {
		t.Fatal("a different schema version must be a protocol failure")
	}
}

func TestParseFieldRefAcceptsOnlyRealFields(t *testing.T) {
	for _, ref := range []string{"content", "synopsis", "decisions[0]", "open_loops[3]", "user_corrections[11]"} {
		if _, err := ParseFieldRef(ref); err != nil {
			t.Fatalf("%q is a real field: %v", ref, err)
		}
	}
	for _, ref := range []string{"", "message_ids[0]", "decisions", "decisions[]", "decisions[-1]",
		"session_id", "decisions[0].text", "Decisions[0]"} {
		if _, err := ParseFieldRef(ref); err == nil {
			t.Fatalf("%q is not a field this system has", ref)
		}
	}
}
