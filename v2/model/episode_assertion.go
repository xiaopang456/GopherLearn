package model

import (
	"strings"
	"time"
)

// Assertion-level time governance for Episode memory.
//
// STABILIZE-057 established that Episode retrieval works: recall@6 of 97.2%,
// zero cross-user, archived, expired, deleted-source or current-session leaks.
// What it also established is that the system has no way to say "this decision
// was later replaced by that one". A reversed decision stays active and is,
// to every reader, indistinguishable from the decision that replaced it.
//
// The fix is not a status on the Episode. One Episode routinely carries a
// decision that was overturned and an open loop that is still outstanding, and
// a single card-level flag says the wrong thing about one of them whichever
// value it takes. Governance therefore lives at the assertion.
//
// It is also not a filter. "Which approach did I abandon", "where was I
// corrected", "what did I switch away from" are questions that *need* the old
// record. A replaced assertion must stay retrievable and citable; what it must
// not do is stand in for the present.

// AssertionKind is which slot of an Episode document an assertion came from.
type AssertionKind string

const (
	// AssertionSynopsis is the card's summary. It is deliberately a kind of its
	// own and deliberately weak: a synopsis paraphrases a session, so it can
	// support "this was discussed" but never "the learner decided", "finished"
	// or "was corrected".
	AssertionSynopsis AssertionKind = "synopsis"
	// AssertionDecision is something the learner settled.
	AssertionDecision AssertionKind = "decision"
	// AssertionOpenLoop is something left outstanding.
	AssertionOpenLoop AssertionKind = "open_loop"
	// AssertionCorrection is a place the learner's understanding was corrected.
	AssertionCorrection AssertionKind = "correction"
	// AssertionPreference is a standing instruction about how the learner wants
	// to be worked with. It is not a decision about the project, and storing it
	// as one both mislabels it and corrupts any count of decisions.
	AssertionPreference AssertionKind = "preference"
	// AssertionDurableExperience is a stable understanding the learner states
	// about themselves. It is not a correction - nothing was wrong before it -
	// and it is not a decision, because nothing was chosen.
	AssertionDurableExperience AssertionKind = "durable_experience"
)

func (k AssertionKind) Valid() bool {
	switch k {
	case AssertionSynopsis, AssertionDecision, AssertionOpenLoop, AssertionCorrection,
		AssertionPreference, AssertionDurableExperience:
		return true
	}
	return false
}

// Letter is the single character used in a slot reference and in a stable id.
func (k AssertionKind) Letter() string {
	switch k {
	case AssertionSynopsis:
		return "s"
	case AssertionDecision:
		return "d"
	case AssertionOpenLoop:
		return "o"
	case AssertionCorrection:
		return "c"
	case AssertionPreference:
		return "p"
	case AssertionDurableExperience:
		return "x"
	}
	return ""
}

func assertionKindFromLetter(letter string) (AssertionKind, bool) {
	switch letter {
	case "s":
		return AssertionSynopsis, true
	case "d":
		return AssertionDecision, true
	case "o":
		return AssertionOpenLoop, true
	case "c":
		return AssertionCorrection, true
	case "p":
		return AssertionPreference, true
	case "x":
		return AssertionDurableExperience, true
	}
	return "", false
}

// SupportsPresentTense reports whether this kind can, on its own, establish
// what is true now. Every real assertion kind can; a synopsis cannot.
//
// A preference and a durable experience both state something the learner holds
// now - "from now on", "I understand that" - so they belong here. What they do
// not get is a place in the replacement matrix or in the relation classifier's
// comparable kinds: superseding a preference is a different relation from
// replacing a decision, and it needs its own design rather than borrowing one.
func (k AssertionKind) SupportsPresentTense() bool {
	switch k {
	case AssertionDecision, AssertionOpenLoop, AssertionCorrection,
		AssertionPreference, AssertionDurableExperience:
		return true
	}
	return false
}

// AssertionState is the time relation, and the third value is the point of it.
type AssertionState string

const (
	// AssertionCurrent still represents the present.
	AssertionCurrent AssertionState = "current"
	// AssertionReplaced was true and has been superseded by a later assertion.
	// It remains retrievable and citable as history.
	AssertionReplaced AssertionState = "replaced"
	// AssertionUnknown is legacy data with no recorded relation.
	//
	// It exists because the alternatives are both untrue. Marking old
	// assertions current would announce that a batch of never-checked records
	// is the present; marking them replaced would announce they are all dead.
	// Unknown says the only honest thing: this was recorded, and nothing here
	// establishes that it still holds.
	AssertionUnknown AssertionState = "unknown"
)

func (s AssertionState) Valid() bool {
	switch s {
	case AssertionCurrent, AssertionReplaced, AssertionUnknown:
		return true
	}
	return false
}

// CanEstablishPresent reports whether an assertion in this state may be used to
// say what is true now.
func (s AssertionState) CanEstablishPresent() bool { return s == AssertionCurrent }

// CanEstablishHistory reports whether it may be used to describe what happened.
// Every state can: that is the correction STABILIZE-057's materials needed.
func (s AssertionState) CanEstablishHistory() bool { return s.Valid() }

// SlotOrdinal is the per-turn position of a slot inside its kind.
//
// It is what a reference label is built from - [E12.d1] - and it is explicitly
// NOT an identity. v1 conflated the two and a re-projection silently re-pointed
// governance relations; see episode_assertion_identity.go.
type SlotOrdinal = int

// Assertion is one slot of an Episode, resolved for reading.
//
// It is assembled at read time from the document plus the governance table; it
// is never stored in this shape.
type Assertion struct {
	ID         string
	EpisodeID  string
	UserID     int64
	Kind       AssertionKind
	Ordinal    int
	Text       string
	OccurredAt time.Time
	// TimeKnown says whether OccurredAt was actually resolved. A legacy
	// assertion whose evidence cannot be dated must not be used to establish
	// that something was decided on a particular day.
	TimeKnown bool
	State     AssertionState
	// ReplacedByAssertionID names the assertion that took over, when the state
	// is replaced. It lets an answer say "this was later replaced by ..." with
	// the replacement's own evidence rather than a bare claim.
	ReplacedByAssertionID string
	// EvidenceMessageIDs are the learner messages this slot rests on. Messages
	// remain the fact source; the Episode is derived and stays traceable.
	EvidenceMessageIDs []uint
	// EpisodeVersion is carried so a citation can be rejected when the card has
	// moved on since the reference was issued.
	EpisodeVersion uint64
}

// AssertionsOf resolves every slot of an Episode, with no governance applied.
//
// States default to unknown, which is what a legacy Episode with no recorded
// relation must read as. A caller that has governance rows applies them with
// ApplyAssertionStates; a caller that does not gets honest unknowns rather than
// optimistic currents.
// EvidenceClock resolves when the cited messages were written.
//
// It is injected because the model package must not reach into a DAO, and
// because a caller that cannot resolve times has to be able to say so rather
// than silently substitute the card's timestamp.
type EvidenceClock func(messageIDs []uint) (time.Time, bool)

// AssertionsOf resolves every slot of an Episode, with no governance applied.
//
// States default to unknown, which is what a legacy Episode with no recorded
// relation must read as. Identities come from the document when they are
// stored and are derived from the fact's fingerprint when they are not.
func AssertionsOf(episode EpisodeMemory) []Assertion {
	return AssertionsOfWithClock(episode, nil)
}

// AssertionsOfWithClock is AssertionsOf with real assertion times.
//
// Without a clock an assertion's time is left zero and TimeKnown false. That is
// deliberate: a legacy assertion whose evidence times cannot be resolved must
// not be used to establish that something was decided on a given day, and
// borrowing the card's LastMessageAt would hide exactly that.
func AssertionsOfWithClock(episode EpisodeMemory, clock EvidenceClock) []Assertion {
	out := make([]Assertion, 0, 1+episode.Document.AssertionCount())
	base := Assertion{
		EpisodeID: episode.ID, UserID: episode.UserID,
		State: AssertionUnknown, EpisodeVersion: episode.Version,
	}
	if text := strings.TrimSpace(episode.Document.Synopsis); text != "" {
		synopsis := base
		// A synopsis has no evidence of its own, so it carries the card's time.
		// It is also the one kind that cannot establish a decision, so a
		// card-level time here cannot be mistaken for "decided on that day".
		synopsis.ID = LegacyAssertionID(episode.ID, AssertionSynopsis, nil, text)
		synopsis.Kind, synopsis.Text = AssertionSynopsis, text
		synopsis.OccurredAt, synopsis.TimeKnown = episode.LastMessageAt.UTC(), !episode.LastMessageAt.IsZero()
		out = append(out, synopsis)
	}
	for _, group := range episode.Document.AssertionGroups() {
		for index, entry := range *group.Entries {
			if strings.TrimSpace(entry.Text) == "" {
				continue
			}
			item := base
			item.ID = strings.TrimSpace(entry.ID)
			if item.ID == "" {
				item.ID = LegacyAssertionID(episode.ID, group.Kind, entry.EvidenceMessageIDs, entry.Text)
			}
			item.Kind, item.Ordinal, item.Text = group.Kind, index+1, entry.Text
			item.EvidenceMessageIDs = append([]uint(nil), entry.EvidenceMessageIDs...)
			switch {
			case !entry.OccurredAt.IsZero():
				item.OccurredAt, item.TimeKnown = entry.OccurredAt.UTC(), true
			case clock != nil:
				if resolved, ok := clock(entry.EvidenceMessageIDs); ok {
					item.OccurredAt, item.TimeKnown = resolved.UTC(), true
				}
			}
			out = append(out, item)
		}
	}
	sortAssertions(out)
	return out
}

// sortAssertions gives a deterministic order: synopsis, decisions, open loops,
// corrections, each by ordinal. Two reads of the same Episode must produce the
// same reference numbers or a citation issued in one turn means something else
// in the next.
func sortAssertions(items []Assertion) {
	rank := map[AssertionKind]int{
		AssertionSynopsis: 0, AssertionDecision: 1, AssertionOpenLoop: 2, AssertionCorrection: 3,
	}
	for outer := 1; outer < len(items); outer++ {
		current := items[outer]
		inner := outer - 1
		for inner >= 0 && (rank[items[inner].Kind] > rank[current.Kind] ||
			(rank[items[inner].Kind] == rank[current.Kind] && items[inner].Ordinal > current.Ordinal)) {
			items[inner+1] = items[inner]
			inner--
		}
		items[inner+1] = current
	}
}

// ApplyAssertionStates overlays recorded governance onto resolved assertions.
//
// Anything with no row keeps unknown. This function deliberately has no
// "default to current" mode: that default is the mistake the third state
// exists to prevent.
func ApplyAssertionStates(items []Assertion, states map[string]EpisodeAssertionState) []Assertion {
	out := make([]Assertion, 0, len(items))
	for _, item := range items {
		if state, ok := states[item.ID]; ok && state.State.Valid() {
			item.State = state.State
			item.ReplacedByAssertionID = state.ReplacedByAssertionID
		}
		out = append(out, item)
	}
	return out
}
