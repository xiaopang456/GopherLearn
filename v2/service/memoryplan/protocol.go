// Package memoryplan is the STABILIZE-032 model-planned long-term memory read
// path.
//
// It changes how topic semantic memory and Episodes are *read*. It changes
// nothing about how they are written, governed, versioned, expired, superseded
// or projected, and it does not touch global semantic memory, TopicProgress,
// RAG or the agent's own prompt.
//
// The shape is two model calls with a server-side boundary between them. A
// Planner sees a bounded catalogue of the learner's own topics and episodes —
// metadata only, no bodies — and says which of them this question needs. The
// server then rebuilds the candidates from MySQL and re-checks ownership,
// status, version, expiry, supersession and source closure, so that whatever
// reaches the second call is already known to be the learner's own and still in
// force. Only then does a Verifier see the bodies and decide, per candidate,
// whether it answers the question, personalises the answer, contradicts a
// premise in it, is merely on a related subject, or is too compressed to tell.
//
// The reason for the split is the finding STABILIZE-031 ended on: owner, status,
// version and source closure were all correct and it was still not enough,
// because a memory can be the learner's own, in force, and on the same subject
// while not supporting the question at all. That is a relation between two
// pieces of text, and no threshold over word overlap or vector distance decided
// it correctly in either direction.
package memoryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
)

// PlanSchemaVersion and VerdictSchemaVersion pin the two contracts.
const (
	PlanSchemaVersion    = "memory.read-plan.v1"
	VerdictSchemaVersion = "memory.verdict.v1"
)

// PlanToolName and VerdictToolName are the forced tool calls.
const (
	PlanToolName    = "select_memory"
	VerdictToolName = "judge_candidates"
)

// The bounds the design fixes. They are constants rather than configuration
// because a limit that can be raised from a flag is not a limit: BUG-AGENT-125
// recorded what raising a cap after seeing results costs.
const (
	MaxPlanTopicKeys  = 3
	MaxPlanEpisodeIDs = 5
	MaxTopicMemories  = 8
	MaxPerTopicMemory = 4
	MaxEpisodes       = 12
	MaxEpisodeQuery   = 200
)

// ErrProtocolFailure marks a response that could not be decoded or that named
// something the server never showed the model.
//
// It is never defaulted into an empty plan. A plan invented from a failed decode
// would be indistinguishable from a plan the model actually made, and the whole
// point of the two stages is that the server knows which is which.
var ErrProtocolFailure = errors.New("memory plan protocol failure")

// Decision is what kinds of long-term memory this question needs.
type Decision string

const (
	DecisionNone     Decision = "none"
	DecisionSemantic Decision = "semantic"
	DecisionEpisodic Decision = "episodic"
	DecisionMixed    Decision = "mixed"
)

func (d Decision) Valid() bool {
	switch d {
	case DecisionNone, DecisionSemantic, DecisionEpisodic, DecisionMixed:
		return true
	}
	return false
}

func (d Decision) needsSemantic() bool { return d == DecisionSemantic || d == DecisionMixed }
func (d Decision) needsEpisodic() bool { return d == DecisionEpisodic || d == DecisionMixed }

// Purpose is what the memory would be used for. It is diagnostics rather than
// control flow: nothing downstream branches on it, and it exists so that a
// wrong injection can be attributed to a wrong intent rather than to a wrong id.
type Purpose string

const (
	PurposePersonalization Purpose = "personalization"
	PurposeHistoryAnswer   Purpose = "history_answer"
	PurposePremiseCheck    Purpose = "premise_check"
	PurposeMixed           Purpose = "mixed"
)

func (p Purpose) Valid() bool {
	switch p {
	case PurposePersonalization, PurposeHistoryAnswer, PurposePremiseCheck, PurposeMixed:
		return true
	}
	return false
}

// TimeScopeKind narrows an episodic read by time.
type TimeScopeKind string

const (
	TimeAny     TimeScopeKind = "any"
	TimeLatest  TimeScopeKind = "latest"
	TimeBefore  TimeScopeKind = "before"
	TimeAfter   TimeScopeKind = "after"
	TimeBetween TimeScopeKind = "between"
)

func (k TimeScopeKind) Valid() bool {
	switch k {
	case TimeAny, TimeLatest, TimeBefore, TimeAfter, TimeBetween:
		return true
	}
	return false
}

// EvidenceKind filters which structured Episode entries are wanted. It is a
// filter over what the server already selected, never a way around the reload.
type EvidenceKind string

const (
	EvidenceSynopsis   EvidenceKind = "synopsis"
	EvidenceDecision   EvidenceKind = "decision"
	EvidenceOpenLoop   EvidenceKind = "open_loop"
	EvidenceCorrection EvidenceKind = "correction"
)

func (k EvidenceKind) Valid() bool {
	switch k {
	case EvidenceSynopsis, EvidenceDecision, EvidenceOpenLoop, EvidenceCorrection:
		return true
	}
	return false
}

// TimeScope is the plan's time filter.
type TimeScope struct {
	Kind TimeScopeKind `json:"kind"`
	From string        `json:"from"`
	To   string        `json:"to"`
}

// Range parses the scope into a half-open interval. A zero bound means unbounded
// on that side.
func (t TimeScope) Range() (from, to time.Time, err error) {
	parse := func(value string) (time.Time, error) {
		if strings.TrimSpace(value) == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339, strings.TrimSpace(value))
	}
	if from, err = parse(t.From); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: time_scope.from is not RFC3339", ErrProtocolFailure)
	}
	if to, err = parse(t.To); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: time_scope.to is not RFC3339", ErrProtocolFailure)
	}
	return from, to, nil
}

// MemoryReadPlan is the Planner's whole output.
type MemoryReadPlan struct {
	SchemaVersion string         `json:"schema_version"`
	Decision      Decision       `json:"decision"`
	Purpose       Purpose        `json:"purpose"`
	TopicKeys     []string       `json:"topic_keys"`
	EpisodeIDs    []string       `json:"episode_ids"`
	EpisodeQuery  string         `json:"episode_query"`
	TimeScope     TimeScope      `json:"time_scope"`
	EvidenceKinds []EvidenceKind `json:"evidence_kinds"`
}

// CatalogueIndex is what the server actually showed the model, used to refuse a
// plan that names anything else.
type CatalogueIndex struct {
	TopicKeys  map[string]struct{}
	EpisodeIDs map[string]struct{}
}

// ValidateAgainst enforces every rule the design states, in one place.
//
// The rules are not defensive programming. A plan that names an id the
// catalogue never contained is a model that invented a record, and looking that
// id up "just in case" is how an invented record becomes a real lookup.
func (p MemoryReadPlan) ValidateAgainst(known CatalogueIndex) error {
	if p.SchemaVersion != PlanSchemaVersion {
		return fmt.Errorf("%w: schema_version %q is not %q", ErrProtocolFailure, p.SchemaVersion, PlanSchemaVersion)
	}
	if !p.Decision.Valid() {
		return fmt.Errorf("%w: decision %q is not one of none, semantic, episodic, mixed", ErrProtocolFailure, p.Decision)
	}
	if !p.Purpose.Valid() {
		return fmt.Errorf("%w: purpose %q is unknown", ErrProtocolFailure, p.Purpose)
	}
	query := strings.TrimSpace(p.EpisodeQuery)
	if p.Decision == DecisionNone {
		if len(p.TopicKeys) != 0 || len(p.EpisodeIDs) != 0 || query != "" {
			return fmt.Errorf("%w: a none decision may not carry candidates", ErrProtocolFailure)
		}
	}
	if len(p.TopicKeys) > MaxPlanTopicKeys {
		return fmt.Errorf("%w: %d topic keys exceeds the %d a plan may name",
			ErrProtocolFailure, len(p.TopicKeys), MaxPlanTopicKeys)
	}
	if len(p.EpisodeIDs) > MaxPlanEpisodeIDs {
		return fmt.Errorf("%w: %d episode ids exceeds the %d a plan may name",
			ErrProtocolFailure, len(p.EpisodeIDs), MaxPlanEpisodeIDs)
	}
	if err := checkKnown("topic key", p.TopicKeys, known.TopicKeys); err != nil {
		return err
	}
	if err := checkKnown("episode id", p.EpisodeIDs, known.EpisodeIDs); err != nil {
		return err
	}
	if len([]rune(query)) > MaxEpisodeQuery {
		return fmt.Errorf("%w: episode_query is longer than %d runes", ErrProtocolFailure, MaxEpisodeQuery)
	}
	if p.Decision.needsSemantic() && len(p.TopicKeys) == 0 {
		return fmt.Errorf("%w: a %s decision needs at least one catalogue topic key", ErrProtocolFailure, p.Decision)
	}
	if p.Decision.needsEpisodic() && len(p.EpisodeIDs) == 0 && len(p.TopicKeys) == 0 && query == "" {
		return fmt.Errorf("%w: an %s decision needs an episode id, a topic key or a query", ErrProtocolFailure, p.Decision)
	}
	if !p.TimeScope.Kind.Valid() {
		return fmt.Errorf("%w: time_scope.kind %q is unknown", ErrProtocolFailure, p.TimeScope.Kind)
	}
	from, to, err := p.TimeScope.Range()
	if err != nil {
		return err
	}
	switch p.TimeScope.Kind {
	case TimeBetween:
		if from.IsZero() || to.IsZero() || !from.Before(to) {
			return fmt.Errorf("%w: a between scope needs from < to", ErrProtocolFailure)
		}
	case TimeBefore:
		if to.IsZero() {
			return fmt.Errorf("%w: a before scope needs a to bound", ErrProtocolFailure)
		}
	case TimeAfter:
		if from.IsZero() {
			return fmt.Errorf("%w: an after scope needs a from bound", ErrProtocolFailure)
		}
	case TimeAny, TimeLatest:
		if !from.IsZero() || !to.IsZero() {
			return fmt.Errorf("%w: a %s scope may not carry bounds", ErrProtocolFailure, p.TimeScope.Kind)
		}
	}
	seenKinds := make(map[EvidenceKind]struct{}, len(p.EvidenceKinds))
	for _, kind := range p.EvidenceKinds {
		if !kind.Valid() {
			return fmt.Errorf("%w: evidence kind %q is unknown", ErrProtocolFailure, kind)
		}
		if _, duplicate := seenKinds[kind]; duplicate {
			return fmt.Errorf("%w: evidence kind %q is repeated", ErrProtocolFailure, kind)
		}
		seenKinds[kind] = struct{}{}
	}
	return nil
}

func checkKnown(label string, values []string, known map[string]struct{}) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return fmt.Errorf("%w: an empty %s was named", ErrProtocolFailure, label)
		}
		if _, duplicate := seen[trimmed]; duplicate {
			return fmt.Errorf("%w: %s %q is repeated", ErrProtocolFailure, label, trimmed)
		}
		seen[trimmed] = struct{}{}
		if _, exists := known[trimmed]; !exists {
			return fmt.Errorf("%w: %s %q was not in the catalogue", ErrProtocolFailure, label, trimmed)
		}
	}
	return nil
}

// Hash is the plan's identity.
//
// Two arms of the experiment must be able to prove they consumed the same plan
// rather than two plans that happened to agree, and a later reader must be able
// to tell a re-run from a repeat. The hash covers every field in a fixed order,
// so a reordered list is a different plan — which it is, because candidate order
// is what the deterministic relation ordering is built on.
func (p MemoryReadPlan) Hash() string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "v=%s|d=%s|p=%s|", p.SchemaVersion, p.Decision, p.Purpose)
	fmt.Fprintf(&builder, "t=%s|e=%s|q=%s|", strings.Join(p.TopicKeys, ","),
		strings.Join(p.EpisodeIDs, ","), strings.TrimSpace(p.EpisodeQuery))
	fmt.Fprintf(&builder, "ts=%s,%s,%s|k=", p.TimeScope.Kind, p.TimeScope.From, p.TimeScope.To)
	for index, kind := range p.EvidenceKinds {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(string(kind))
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// WantsKind reports whether the plan asked for one evidence kind. An empty list
// means every kind.
func (p MemoryReadPlan) WantsKind(kind EvidenceKind) bool {
	if len(p.EvidenceKinds) == 0 {
		return true
	}
	for _, wanted := range p.EvidenceKinds {
		if wanted == kind {
			return true
		}
	}
	return false
}

// PlanTool is the forced tool the Planner must answer with.
func PlanTool() *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: PlanToolName,
		Desc: "State which of the learner's own long-term memories this question needs. " +
			"Name only topic keys and episode ids that appear in the catalogue you were given.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"schema_version": {Type: schema.String, Required: true, Enum: []string{PlanSchemaVersion},
				Desc: "Always " + PlanSchemaVersion},
			"decision": {Type: schema.String, Required: true,
				Enum: []string{string(DecisionNone), string(DecisionSemantic), string(DecisionEpisodic), string(DecisionMixed)},
				Desc: "none when the question needs no personal memory at all."},
			"purpose": {Type: schema.String, Required: true,
				Enum: []string{string(PurposePersonalization), string(PurposeHistoryAnswer),
					string(PurposePremiseCheck), string(PurposeMixed)},
				Desc: "What the memory would be used for."},
			"topic_keys": {Type: schema.Array, Required: true,
				Desc:     "At most three topic keys, copied exactly from the catalogue. Empty for a none decision.",
				ElemInfo: &schema.ParameterInfo{Type: schema.String},
			},
			"episode_ids": {Type: schema.Array, Required: true,
				Desc:     "At most five episode ids, copied exactly from the catalogue. Empty for a none decision.",
				ElemInfo: &schema.ParameterInfo{Type: schema.String},
			},
			"episode_query": {Type: schema.String, Required: true,
				Desc: "A short restatement of the people, events and relations the question itself mentions, " +
					"used to search older sessions. Empty string when not needed. Never add a fact the " +
					"question and the catalogue did not give you."},
			"time_scope": {Type: schema.Object, Required: true,
				Desc: "How to narrow by time.",
				SubParams: map[string]*schema.ParameterInfo{
					"kind": {Type: schema.String, Required: true,
						Enum: []string{string(TimeAny), string(TimeLatest), string(TimeBefore),
							string(TimeAfter), string(TimeBetween)}},
					"from": {Type: schema.String, Required: true, Desc: "RFC3339 or empty string."},
					"to":   {Type: schema.String, Required: true, Desc: "RFC3339 or empty string."},
				},
			},
			"evidence_kinds": {Type: schema.Array, Required: true,
				Desc: "Which structured episode entries matter. Empty array means all of them.",
				ElemInfo: &schema.ParameterInfo{Type: schema.String,
					Enum: []string{string(EvidenceSynopsis), string(EvidenceDecision),
						string(EvidenceOpenLoop), string(EvidenceCorrection)}},
			},
		}),
	}
}

// DecodePlan reads exactly one forced tool call, strictly.
//
// Unknown fields are refused rather than ignored. A model that answered a
// schema this code does not know is a model whose other fields cannot be trusted
// to mean what they are read as, and the retry the caller then makes is cheaper
// than a plan built from a guess.
func DecodePlan(message *schema.Message) (MemoryReadPlan, error) {
	if message == nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != PlanToolName {
		return MemoryReadPlan{}, fmt.Errorf("%w: the planner did not call %s exactly once", ErrProtocolFailure, PlanToolName)
	}
	arguments := message.ToolCalls[0].Function.Arguments
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var plan MemoryReadPlan
	if err := decoder.Decode(&plan); err != nil {
		return MemoryReadPlan{}, fmt.Errorf("%w: %v", ErrProtocolFailure, err)
	}
	if err := ensureEOF(decoder); err != nil {
		return MemoryReadPlan{}, err
	}
	return plan, nil
}

// ensureEOF refuses anything after the arguments object.
//
// Only io.EOF is clean. A readable token means a second value followed the
// first; any other error means the tail was not valid JSON at all. Treating
// "there was an error reading the tail" as success is how trailing prose gets
// silently discarded along with whatever it was attached to.
func ensureEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing content after the tool arguments", ErrProtocolFailure)
	}
	return nil
}

// Relation is the Verifier's judgement about one candidate.
type Relation string

const (
	// RelationAnswer is direct personal evidence for what the learner said,
	// decided or was confused about.
	RelationAnswer Relation = "answer_evidence"
	// RelationPersonalization cannot replace a technical fact but legitimately
	// changes where an explanation starts or what it emphasises.
	RelationPersonalization Relation = "personalization_context"
	// RelationContradiction directly denies a personal premise in the question.
	RelationContradiction Relation = "contradicts_premise"
	// RelationRelated is on a neighbouring subject and supports nothing.
	RelationRelated Relation = "related_only"
	// RelationInsufficient is too compressed or missing the evidence field to
	// judge from.
	RelationInsufficient Relation = "insufficient"
)

func (r Relation) Valid() bool {
	switch r {
	case RelationAnswer, RelationPersonalization, RelationContradiction, RelationRelated, RelationInsufficient:
		return true
	}
	return false
}

// Injectable reports whether a relation may reach the assembled context.
//
// related_only and insufficient never do. That is the whole safety half of the
// task: STABILIZE-031's added false-premise injections were every one of them a
// record that was the learner's own, active and on the subject, and related_only
// is the verdict that names exactly that.
func (r Relation) Injectable() bool {
	return r == RelationContradiction || r == RelationAnswer || r == RelationPersonalization
}

// InjectionRank orders the injectable relations. Contradiction first: a question
// built on a premise the learner's own history denies is answered wrongly by
// anything else, however relevant.
func (r Relation) InjectionRank() int {
	switch r {
	case RelationContradiction:
		return 0
	case RelationAnswer:
		return 1
	case RelationPersonalization:
		return 2
	}
	return 3
}

// CandidateVerdict is one judgement.
type CandidateVerdict struct {
	CandidateID string   `json:"candidate_id"`
	Relation    Relation `json:"relation"`
	FieldRefs   []string `json:"field_refs"`
}

// VerdictSet is the Verifier's whole output.
type VerdictSet struct {
	SchemaVersion string             `json:"schema_version"`
	Verdicts      []CandidateVerdict `json:"verdicts"`
}

// VerdictTool is the forced tool the Verifier must answer with.
func VerdictTool() *schema.ToolInfo {
	return &schema.ToolInfo{
		Name: VerdictToolName,
		Desc: "Judge every candidate against the question. Return exactly one verdict per candidate given, " +
			"no more and no fewer.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"schema_version": {Type: schema.String, Required: true, Enum: []string{VerdictSchemaVersion},
				Desc: "Always " + VerdictSchemaVersion},
			"verdicts": {Type: schema.Array, Required: true,
				Desc: "One entry per candidate, in any order, covering every candidate exactly once.",
				ElemInfo: &schema.ParameterInfo{Type: schema.Object, Required: true,
					SubParams: map[string]*schema.ParameterInfo{
						"candidate_id": {Type: schema.String, Required: true,
							Desc: "Copied exactly from the candidate you are judging."},
						"relation": {Type: schema.String, Required: true,
							Enum: []string{string(RelationAnswer), string(RelationPersonalization),
								string(RelationContradiction), string(RelationRelated), string(RelationInsufficient)}},
						"field_refs": {Type: schema.Array, Required: true,
							Desc: "For a memory: [\"content\"]. For an episode: synopsis, decisions[n], " +
								"open_loops[n] or user_corrections[n], using only indices that exist. " +
								"Empty array when the relation is related_only or insufficient.",
							ElemInfo: &schema.ParameterInfo{Type: schema.String}},
					},
				},
			},
		}),
	}
}

// DecodeVerdicts reads exactly one forced tool call, strictly.
func DecodeVerdicts(message *schema.Message) (VerdictSet, error) {
	if message == nil || len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != VerdictToolName {
		return VerdictSet{}, fmt.Errorf("%w: the verifier did not call %s exactly once", ErrProtocolFailure, VerdictToolName)
	}
	decoder := json.NewDecoder(strings.NewReader(message.ToolCalls[0].Function.Arguments))
	decoder.DisallowUnknownFields()
	var set VerdictSet
	if err := decoder.Decode(&set); err != nil {
		return VerdictSet{}, fmt.Errorf("%w: %v", ErrProtocolFailure, err)
	}
	if err := ensureEOF(decoder); err != nil {
		return VerdictSet{}, err
	}
	if set.SchemaVersion != VerdictSchemaVersion {
		return VerdictSet{}, fmt.Errorf("%w: schema_version %q is not %q",
			ErrProtocolFailure, set.SchemaVersion, VerdictSchemaVersion)
	}
	return set, nil
}

var fieldRefPattern = regexp.MustCompile(`^(decisions|open_loops|user_corrections)\[(\d+)\]$`)

// FieldRefTarget names one addressable field of one candidate.
type FieldRefTarget struct {
	List  string
	Index int
}

// ParseFieldRef reads a field reference without resolving it.
func ParseFieldRef(ref string) (FieldRefTarget, error) {
	trimmed := strings.TrimSpace(ref)
	switch trimmed {
	case "content":
		return FieldRefTarget{List: "content"}, nil
	case "synopsis":
		return FieldRefTarget{List: "synopsis"}, nil
	}
	matches := fieldRefPattern.FindStringSubmatch(trimmed)
	if matches == nil {
		return FieldRefTarget{}, fmt.Errorf("%w: field ref %q is not a field this system has", ErrProtocolFailure, ref)
	}
	index, err := strconv.Atoi(matches[2])
	if err != nil || index < 0 {
		return FieldRefTarget{}, fmt.Errorf("%w: field ref %q has no usable index", ErrProtocolFailure, ref)
	}
	return FieldRefTarget{List: matches[1], Index: index}, nil
}

// SortVerdicts puts the verdicts into candidate order, so a report reads the
// same way whatever order the model answered in.
func SortVerdicts(verdicts []CandidateVerdict, order []string) []CandidateVerdict {
	position := make(map[string]int, len(order))
	for index, id := range order {
		position[id] = index
	}
	sorted := append([]CandidateVerdict(nil), verdicts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return position[sorted[i].CandidateID] < position[sorted[j].CandidateID]
	})
	return sorted
}
