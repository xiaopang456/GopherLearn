package episodeassert

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"GopherAI/model"
)

// Slot-level evidence.
//
// The model could previously only write [E12], and the server could not tell
// whether it had used the card's synopsis, one of its decisions, an open loop
// or a correction. That matters because those slots support different claims: a
// synopsis paraphrases a session and cannot establish that the learner decided
// anything, while a decision that has been replaced can establish what happened
// and must not establish what is true now.
//
// So a reference names the slot:
//
//	[E12.s]   synopsis
//	[E12.d1]  decision 1
//	[E12.o1]  open loop 1
//	[E12.c1]  correction 1

// SlotRef is one issued reference.
type SlotRef struct {
	// Label is the text the model writes, without brackets: "E12.d1".
	Label string
	// Card is the per-turn Episode number, "E12".
	Card        string
	AssertionID string
	EpisodeID   string
	UserID      int64
	Kind        model.AssertionKind
	State       model.AssertionState
	// EpisodeVersion is pinned at issue time so a citation can be refused when
	// the card has moved on.
	EpisodeVersion     uint64
	EvidenceMessageIDs []uint
	Text               string
}

// SupportsPresent reports whether this reference alone can establish what is
// true now: the slot has to be a real assertion, and its state has to be
// current.
func (r SlotRef) SupportsPresent() bool {
	return r.Kind.SupportsPresentTense() && r.State.CanEstablishPresent()
}

// SupportsHistory reports whether it can establish what happened. Every issued
// reference can, including replaced ones - that is the whole correction this
// design makes to STABILIZE-057's materials.
func (r SlotRef) SupportsHistory() bool { return r.State.CanEstablishHistory() }

// Registry is what one turn actually issued.
//
// Validation is against this and nothing else. A reference the turn did not
// issue is invalid however well-formed it looks, which is what stops an answer
// from inventing [E99.d1].
type Registry struct {
	byLabel map[string]SlotRef
	order   []string
}

func NewRegistry() *Registry {
	return &Registry{byLabel: map[string]SlotRef{}}
}

// Issue registers every slot of one Episode under a card number.
func (r *Registry) Issue(card string, assertions []model.Assertion) ([]SlotRef, error) {
	card = strings.TrimSpace(card)
	if !cardPattern.MatchString(card) {
		return nil, fmt.Errorf("malformed card number %q", card)
	}
	issued := make([]SlotRef, 0, len(assertions))
	for _, assertion := range assertions {
		suffix := assertion.Kind.Letter()
		if assertion.Kind != model.AssertionSynopsis {
			suffix = fmt.Sprintf("%s%d", suffix, assertion.Ordinal)
		}
		reference := SlotRef{
			Label: card + "." + suffix, Card: card,
			AssertionID: assertion.ID, EpisodeID: assertion.EpisodeID, UserID: assertion.UserID,
			Kind: assertion.Kind, State: assertion.State, EpisodeVersion: assertion.EpisodeVersion,
			EvidenceMessageIDs: append([]uint(nil), assertion.EvidenceMessageIDs...),
			Text:               assertion.Text,
		}
		r.byLabel[reference.Label] = reference
		r.order = append(r.order, reference.Label)
		issued = append(issued, reference)
	}
	return issued, nil
}

// Lookup returns an issued reference.
func (r *Registry) Lookup(label string) (SlotRef, bool) {
	reference, ok := r.byLabel[strings.TrimSpace(label)]
	return reference, ok
}

// IssuedLabels lists what was issued, in issue order. The judge-side stripper
// uses it so that only real labels are removed and a forged one stays visible.
func (r *Registry) IssuedLabels() []string { return append([]string(nil), r.order...) }

// refMask is a byte a natural-language answer never contains, used to hide a
// reference from the sentence splitter.
const refMask = "\x00"

var (
	cardPattern = regexp.MustCompile(`^E[0-9]{1,4}$`)
	// refPattern deliberately also matches a bare card reference like [E12], so
	// that an answer using the old shape is reported as a slotless citation
	// rather than silently ignored.
	refPattern = regexp.MustCompile(`\[(E[0-9]{1,4})(?:\.([sdoc][0-9]*))?\]`)
	// sentencePattern splits on Chinese and Latin sentence enders, keeping the
	// ender, so neutralising one sentence does not disturb its neighbours.
	sentencePattern = regexp.MustCompile(`[^。！？；\n.!?]+[。！？；\n.!?]?`)
)

// splitSentences cuts an answer into sentences without cutting a reference.
//
// A slot reference contains a full stop - [E12.d1] - so splitting on "." first
// tore every reference in half and made the validator blind to all of them. The
// references are masked with a placeholder that carries no terminator, the text
// is split, and the placeholders are restored.
func splitSentences(answer string) []string {
	placeholders := []string{}
	masked := refPattern.ReplaceAllStringFunc(answer, func(match string) string {
		placeholders = append(placeholders, match)
		return fmt.Sprintf("%s%d%s", refMask, len(placeholders)-1, refMask)
	})
	out := []string{}
	for _, sentence := range sentencePattern.FindAllString(masked, -1) {
		for index, original := range placeholders {
			sentence = strings.ReplaceAll(sentence, fmt.Sprintf("%s%d%s", refMask, index, refMask), original)
		}
		out = append(out, sentence)
	}
	return out
}

// ClaimKind is what a sentence is trying to establish.
type ClaimKind string

const (
	// ClaimPresent asserts what is true now.
	ClaimPresent ClaimKind = "present"
	// ClaimHistory asserts what happened.
	ClaimHistory ClaimKind = "history"
	// ClaimUndetermined means no classifier was available. The state-dependent
	// checks do not run on such a sentence, and the result says so.
	ClaimUndetermined ClaimKind = "undetermined"
)

// SentenceVerdict is one sentence's outcome.
type SentenceVerdict struct {
	Text     string    `json:"text"`
	Claim    ClaimKind `json:"claim"`
	Refs     []string  `json:"refs"`
	Valid    bool      `json:"valid"`
	Reason   string    `json:"reason,omitempty"`
	Neutrali bool      `json:"neutralised"`
}

// ValidationResult is the whole answer's outcome.
type ValidationResult struct {
	Sentences []SentenceVerdict `json:"sentences"`
	// InvalidRefs are labels the turn never issued.
	InvalidRefs []string `json:"invalid_refs"`
	// KindMismatch are labels whose slot cannot support the claim made.
	KindMismatch []string `json:"kind_mismatch"`
	// VersionMismatch are labels whose Episode has moved on.
	VersionMismatch []string `json:"version_mismatch"`
	// ReplacedAsPresent are the specific, serious failure: a replaced assertion
	// used to say what is true now.
	ReplacedAsPresent []string `json:"replaced_as_present"`
	// UnknownAsPresent is legacy data presented as the present.
	UnknownAsPresent []string `json:"unknown_as_present"`
	// SynopsisAsAssertion is a synopsis used to claim a decision or completion.
	SynopsisAsAssertion []string `json:"synopsis_as_assertion"`
	// CitedAfterEmpty is a citation in a turn that retrieved nothing.
	CitedAfterEmpty bool `json:"cited_after_empty"`
	// PresentTenseCheckRan says whether the state-dependent checks were
	// performed at all. They need a classifier this server does not have, so a
	// zero in ReplacedAsPresent means "not examined" unless this is true.
	PresentTenseCheckRan bool `json:"present_tense_check_ran"`
	// PresentTenseVerdict is the reportable status of those checks. It exists
	// so an artefact can never show a bare zero: without a classifier the value
	// is not_evaluated, which is a different claim from "no violations found"
	// and must not be summarised into one.
	PresentTenseVerdict string `json:"present_tense_verdict"`
	// Answer is the text after neutralisation.
	Answer string `json:"answer"`
}

// The three reportable outcomes of the state-dependent checks.
//
// PresentTenseNotEvaluated is the important one. An artefact that printed
// "replaced_used_as_present: 0" when no classifier existed would be reporting a
// measurement it never took, and a reader has no way to tell that apart from a
// clean result.
const (
	PresentTenseNotEvaluated = "not_evaluated"
	PresentTenseClean        = "no_violations"
	PresentTenseViolated     = "violations_found"
)

// ClaimClassifier decides whether a sentence is asserting the present.
//
// There is deliberately no default implementation, and production does not ship
// one. Deciding whether a Chinese sentence states what is true now or describes
// what happened is a natural-language judgement this server cannot make, and a
// keyword table pretending otherwise would be worse than nothing: it would
// report the present-tense checks as having run when they had not.
//
// So the boundary is explicit. What the server verifies deterministically:
//
//   - the reference was issued by this turn;
//   - owner, episode and version agree;
//   - the slot kind matches what is being claimed of it;
//   - the assertion state is current, replaced or unknown;
//   - a synopsis is not being used as decision evidence;
//   - replaced and unknown do not qualify to establish the present;
//   - the source messages are closed.
//
// What still needs a model or a structured answer protocol:
//
//   - whether a given sentence is stating the present or the past;
//   - whether the model presented replaced content as current;
//   - whether it invented history with no evidence at all.
//
// When no classifier is supplied, the state-dependent checks do not run and
// PresentTenseCheckRan is false. A report that quietly showed zero violations
// because nothing was examined would be the exact dishonesty this comment
// exists to prevent.
type ClaimClassifier func(sentence string) ClaimKind

// CurrentEpisodeVersions lets the validator refuse a citation whose card has
// changed since the reference was issued.
type CurrentEpisodeVersions map[string]uint64

// ValidateAnswer checks every citation and neutralises only what fails.
//
// Neutralising the whole answer on one bad citation would throw away sentences
// that were properly supported, which is both worse for the learner and worse
// for diagnosis: the report could no longer say which claim was unfounded.
func ValidateAnswer(answer string, registry *Registry, classify ClaimClassifier,
	versions CurrentEpisodeVersions, retrievedAnything bool) ValidationResult {
	result := ValidationResult{
		InvalidRefs: []string{}, KindMismatch: []string{}, VersionMismatch: []string{},
		ReplacedAsPresent: []string{}, UnknownAsPresent: []string{},
		SynopsisAsAssertion: []string{}, Sentences: []SentenceVerdict{},
	}
	if registry == nil {
		registry = NewRegistry()
	}
	// No classifier means the present-tense checks cannot run. They are skipped
	// and reported as skipped rather than silently passing.
	result.PresentTenseCheckRan = classify != nil
	result.PresentTenseVerdict = PresentTenseNotEvaluated
	if classify == nil {
		classify = func(string) ClaimKind { return ClaimUndetermined }
	}

	var rebuilt strings.Builder
	anyCitation := false
	for _, sentence := range splitSentences(answer) {
		matches := refPattern.FindAllStringSubmatch(sentence, -1)
		verdict := SentenceVerdict{Text: sentence, Claim: classify(sentence), Valid: true, Refs: []string{}}
		if len(matches) == 0 {
			result.Sentences = append(result.Sentences, verdict)
			rebuilt.WriteString(sentence)
			continue
		}
		anyCitation = true
		for _, match := range matches {
			label := match[1]
			if match[2] != "" {
				label = match[1] + "." + match[2]
			}
			verdict.Refs = append(verdict.Refs, label)

			if match[2] == "" {
				// A bare card reference names no slot, so the server cannot
				// tell what was used. It is refused rather than guessed.
				verdict.Valid, verdict.Reason = false, "slotless_reference"
				result.KindMismatch = appendOnce(result.KindMismatch, label)
				continue
			}
			reference, issued := registry.Lookup(label)
			if !issued {
				verdict.Valid, verdict.Reason = false, "not_issued"
				result.InvalidRefs = appendOnce(result.InvalidRefs, label)
				continue
			}
			if current, known := versions[reference.EpisodeID]; known && current != reference.EpisodeVersion {
				verdict.Valid, verdict.Reason = false, "version_mismatch"
				result.VersionMismatch = appendOnce(result.VersionMismatch, label)
				continue
			}
			if verdict.Claim == ClaimPresent {
				switch {
				case !reference.Kind.SupportsPresentTense():
					verdict.Valid, verdict.Reason = false, "synopsis_cannot_assert"
					result.SynopsisAsAssertion = appendOnce(result.SynopsisAsAssertion, label)
				case reference.State == model.AssertionReplaced:
					verdict.Valid, verdict.Reason = false, "replaced_as_present"
					result.ReplacedAsPresent = appendOnce(result.ReplacedAsPresent, label)
				case reference.State == model.AssertionUnknown:
					verdict.Valid, verdict.Reason = false, "unknown_as_present"
					result.UnknownAsPresent = appendOnce(result.UnknownAsPresent, label)
				}
			}
		}
		if !verdict.Valid {
			verdict.Neutrali = true
			rebuilt.WriteString(neutralise(sentence))
		} else {
			rebuilt.WriteString(sentence)
		}
		result.Sentences = append(result.Sentences, verdict)
	}
	if result.PresentTenseCheckRan {
		result.PresentTenseVerdict = PresentTenseClean
		if len(result.ReplacedAsPresent)+len(result.UnknownAsPresent)+
			len(result.SynopsisAsAssertion) > 0 {
			result.PresentTenseVerdict = PresentTenseViolated
		}
	}
	result.CitedAfterEmpty = anyCitation && !retrievedAnything
	result.Answer = rebuilt.String()
	sort.Strings(result.InvalidRefs)
	sort.Strings(result.KindMismatch)
	return result
}

// NeutralisedSentence is what replaces a sentence whose evidence did not hold.
const NeutralisedSentence = "（这一句缺少可验证的证据，已移除。）"

func neutralise(string) string { return NeutralisedSentence }

// StripIssuedRefs removes only the labels this turn issued, leaving forged ones
// in place.
//
// It exists for the judge boundary. BUG-AGENT-154: reference numbers made the
// judge read memory citations as document citations, and because only one arm
// carried them it could tell the arms apart. Stripping with a broad regex would
// have hidden the forged references the deterministic check needs to catch, so
// this removes exactly what was issued and nothing else.
func StripIssuedRefs(answer string, registry *Registry) string {
	if registry == nil {
		return answer
	}
	out := answer
	for _, label := range registry.IssuedLabels() {
		out = strings.ReplaceAll(out, "["+label+"]", "")
	}
	return strings.TrimSpace(out)
}

func appendOnce(existing []string, value string) []string {
	for _, seen := range existing {
		if seen == value {
			return existing
		}
	}
	return append(existing, value)
}
