package episoderelation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"GopherAI/model"
	"GopherAI/service/episodeassert"
)

// Mode is what the generator is allowed to do with what it decides.
//
// The default is Off. A relation generator that started changing states the
// day it merged would be changing what every learner's memory says, on the
// strength of a classifier nobody has measured on real sessions yet. Shadow is
// the mode that produces the evidence to measure it with.
type Mode string

const (
	// ModeOff does nothing at all: no candidates, no classification, no rows.
	ModeOff Mode = "off"
	// ModeShadow classifies and records proposals, and changes no state and
	// writes no relation edge.
	ModeShadow Mode = "shadow"
	// ModeApply records proposals and applies what the server accepted.
	ModeApply Mode = "apply"
)

// ValidMode reports whether a value is in the closed set.
func ValidMode(mode Mode) bool {
	switch mode {
	case ModeOff, ModeShadow, ModeApply:
		return true
	}
	return false
}

// DefaultMode is Off, and it is a decision rather than an oversight: this
// stage ships the structure, not a change to what any learner's memory says.
const DefaultMode = ModeOff

// RejectReason is the closed vocabulary for a server-side refusal. The
// classifier's own words never reach a row.
type RejectReason string

const (
	RejectProtocol         RejectReason = "classifier_protocol"
	RejectTransport        RejectReason = "classifier_transport"
	RejectKindIncomparable RejectReason = "kind_incomparable"
	RejectNotEarlier       RejectReason = "target_not_later_than_source"
	RejectForeignOwner     RejectReason = "foreign_owner"
	RejectGovernor         RejectReason = "governor_refused"
	RejectStore            RejectReason = "store_failure"
	RejectNotAtomic        RejectReason = "apply_requires_atomic_store"
	// RejectSupersededSource refuses a supplement or equivalence whose earlier
	// end has already been replaced. Either would quietly revive a fact the
	// learner superseded.
	RejectSupersededSource RejectReason = "source_already_replaced"
)

// ProposalLookup lets the generator find a decision it already made. A store
// without it simply never reuses, which is correct but wasteful.
type ProposalLookup interface {
	ProposalByKey(ctx context.Context, userID int64, key string) (
		model.EpisodeRelationProposal, bool, error)
}

// ProposalStore persists what the generator decided. Writing a proposal is
// what makes a shadow run leave evidence.
type ProposalStore interface {
	// SaveProposal is idempotent on the proposal's own key: the same pair
	// judged again by the same classifier updates one row rather than adding
	// a second.
	SaveProposal(ctx context.Context, proposal model.EpisodeRelationProposal) error
	// SaveRelation records an accepted supplement or equivalence.
	SaveRelation(ctx context.Context, relation model.EpisodeAssertionRelation) error
	// Relations reads one learner's edges, for read-back and for idempotence.
	Relations(ctx context.Context, userID int64, sourceAssertionID string) ([]model.EpisodeAssertionRelation, error)
	// Proposals reads one learner's proposals.
	Proposals(ctx context.Context, userID int64) ([]model.EpisodeRelationProposal, error)
}

// AtomicProposalStore is the optional seam an applying generator needs.
//
// Applying a replacement is two writes - the earlier assertion becomes
// replaced and points at the later one, and the later one becomes current -
// and a failure between them leaves a history that says a decision was
// superseded by something that is not in force. That is worse than either
// write alone, so an applying generator refuses to start without a store that
// can do both or neither.
type AtomicProposalStore interface {
	ProposalStore
	// WithinTx runs fn against a store bound to one transaction. A non-nil
	// error from fn rolls everything back.
	WithinTx(ctx context.Context, fn func(ProposalStore, episodeassert.Store) error) error
}

// Outcome is one pair's result.
type Outcome struct {
	SourceAssertionID string
	TargetAssertionID string
	Kind              model.AssertionRelationKind
	Status            model.RelationProposalStatus
	RejectedReason    RejectReason
	// Reused says the decision was read from an earlier run rather than made
	// again. A replay of an applied pass is entirely Reused outcomes.
	Reused bool
}

// Report is what one generator run did, in numbers a rehearsal can print.
type Report struct {
	Mode               Mode
	NewAssertions      int
	PairsConsidered    int
	Outcomes           []Outcome
	ByKind             map[model.AssertionRelationKind]int
	ByReject           map[RejectReason]int
	ExcludedCandidates map[CandidateFilterReason]int
	StatesChanged      int
	RelationsWritten   int
	ProposalsWritten   int
	// Reused is how many pairs were answered from an earlier run's record.
	Reused int
}

// Generator is the write-side relation generator.
type Generator struct {
	finder     *Finder
	classifier Classifier
	proposals  ProposalStore
	governor   *episodeassert.Governor
	states     episodeassert.Store
	mode       Mode
	now        func() time.Time
}

// Config assembles a generator.
type Config struct {
	Source        CandidateSource
	Classifier    Classifier
	Proposals     ProposalStore
	Governor      *episodeassert.Governor
	States        episodeassert.Store
	Mode          Mode
	MaxCandidates int
}

// New refuses a configuration that could not do what its mode claims.
//
// In particular an applying generator without an atomic store is refused here
// rather than halfway through a replacement, because the failure it prevents
// is the one that cannot be cleaned up afterwards.
func New(config Config) (*Generator, error) {
	mode := config.Mode
	if mode == "" {
		mode = DefaultMode
	}
	if !ValidMode(mode) {
		return nil, fmt.Errorf("relation generator mode %q is not off, shadow or apply", mode)
	}
	if mode == ModeOff {
		return &Generator{mode: ModeOff, now: time.Now}, nil
	}
	finder, err := NewFinder(config.Source, config.MaxCandidates)
	if err != nil {
		return nil, err
	}
	if config.Classifier == nil {
		return nil, fmt.Errorf("a %s generator needs a classifier", mode)
	}
	if config.Proposals == nil {
		return nil, fmt.Errorf("a %s generator needs somewhere to record what it decided", mode)
	}
	if mode == ModeApply {
		if config.Governor == nil || config.States == nil {
			return nil, fmt.Errorf("an applying generator needs the governor and the state store")
		}
		if _, atomic := config.Proposals.(AtomicProposalStore); !atomic {
			return nil, fmt.Errorf(
				"an applying generator needs an atomic store: a replacement is two "+
					"writes, and a failure between them leaves a decision superseded "+
					"by something that is not in force (%s)", RejectNotAtomic)
		}
	}
	return &Generator{finder: finder, classifier: config.Classifier,
		proposals: config.Proposals, governor: config.Governor,
		states: config.States, mode: mode, now: time.Now}, nil
}

// UseClock replaces the clock, for deterministic drills.
func (g *Generator) UseClock(clock func() time.Time) {
	if clock != nil {
		g.now = clock
	}
}

// Mode is what this generator will do.
func (g *Generator) Mode() Mode { return g.mode }

// Generate runs the write-side relation pass over one Episode's new
// assertions.
//
// It never returns an error for a pair it could not decide: an undecidable
// pair is uncertain, and a Session's projection must not fail because a
// relation was unclear. It returns an error only when the run could not be
// recorded at all, because a run that decided things and recorded none of them
// is indistinguishable from a run that never happened.
func (g *Generator) Generate(ctx context.Context, assertions []NewAssertion) (Report, error) {
	report := Report{Mode: g.mode, NewAssertions: len(assertions),
		ByKind:             map[model.AssertionRelationKind]int{},
		ByReject:           map[RejectReason]int{},
		ExcludedCandidates: map[CandidateFilterReason]int{},
		Outcomes:           []Outcome{}}
	if g.mode == ModeOff {
		report.NewAssertions = 0
		return report, nil
	}
	for _, assertion := range assertions {
		set, err := g.finder.Find(ctx, assertion)
		if err != nil {
			return report, err
		}
		for reason, count := range set.Excluded {
			report.ExcludedCandidates[reason] += count
		}
		for _, candidate := range set.Candidates {
			report.PairsConsidered++
			outcome, err := g.decidePair(ctx, candidate, assertion)
			if err != nil {
				return report, err
			}
			report.Outcomes = append(report.Outcomes, outcome)
			report.ByKind[outcome.Kind]++
			if outcome.Reused {
				report.Reused++
				continue
			}
			if outcome.RejectedReason != "" {
				report.ByReject[outcome.RejectedReason]++
			}
			switch outcome.Status {
			case model.ProposalApplied:
				report.ProposalsWritten++
				if outcome.Kind.ChangesState() {
					report.StatesChanged += 2
				} else {
					report.RelationsWritten++
				}
			case model.ProposalShadow, model.ProposalRejected:
				report.ProposalsWritten++
			}
		}
	}
	return report, nil
}

// decidePair classifies one pair, validates the answer, records it, and
// applies it only if the mode says so.
func (g *Generator) decidePair(ctx context.Context, earlier Candidate,
	later NewAssertion) (Outcome, error) {
	outcome := Outcome{SourceAssertionID: earlier.AssertionID,
		TargetAssertionID: later.AssertionID}
	// The idempotency key identifies the pair to this server. The handle the
	// classifier echoes is a digest of it: the key names two assertion ids and
	// an owner, and a classifier has no business seeing either. The digest
	// still detects an answer about another pair, which is the only thing the
	// handle is for.
	pairKey := model.RelationIdempotencyKey(later.UserID, earlier.AssertionID,
		later.AssertionID, g.classifier.Identity())
	pairID := OpaquePairHandle(pairKey)
	request := ClassifyRequest{PairID: pairID,
		Earlier: ClassifySide{Kind: earlier.Kind, Text: earlier.Text},
		Later:   ClassifySide{Kind: later.Kind, Text: later.Text}}

	// An applied or shadowed decision over the same pair, by the same
	// classifier identity, is reused rather than re-decided.
	//
	// Phase 1 compared row counts and called that idempotence. Row counts are
	// exactly what an UPDATE does not move: the replay re-read the version,
	// marked replaced again, marked current again, advanced both versions and
	// both timestamps, and the count never changed. Reuse is the only thing
	// that actually leaves the store alone.
	if existing, found, lookupErr := g.existingDecision(ctx, later.UserID, pairKey); lookupErr != nil {
		return outcome, lookupErr
	} else if found {
		return reusedOutcome(existing), nil
	}

	kind := AbstainOnProtocolFailure
	reject := RejectReason("")
	response, err := g.classifier.Classify(ctx, request)
	if err != nil {
		// Transport: the classifier could not be reached. Uncertain, and the
		// reason is recorded so an outage is not read as a corpus with no
		// relations in it.
		reject = RejectTransport
	} else if validated, verr := ValidateResponse(request, response); verr != nil {
		reject = RejectProtocol
	} else {
		kind = validated
	}
	outcome.Kind = kind

	// Server-side checks on the pair itself, whatever the classifier said. A
	// relation that fails one of these is refused even if the classifier was
	// confident: the model may propose, and only the server may accept.
	if reject == "" && kind != model.RelationUncertain && kind != model.RelationIndependent {
		switch {
		case earlier.UserID != later.UserID:
			reject = RejectForeignOwner
		case !ComparableKinds(earlier.Kind, later.Kind):
			reject = RejectKindIncomparable
		case !earlier.OccurredAt.Before(later.OccurredAt):
			reject = RejectNotEarlier
		}
	}

	status := model.ProposalShadow
	if reject != "" {
		status = model.ProposalRejected
		outcome.RejectedReason = reject
		if kind != model.RelationUncertain {
			// A refused relation is not silently downgraded to its own
			// proposal: the recorded kind stays uncertain so nothing later
			// reads a rejected "replaces" as a near-miss.
			outcome.Kind = model.RelationUncertain
			kind = model.RelationUncertain
		}
	}

	proposal := model.EpisodeRelationProposal{
		UserID: later.UserID,
		// The server key, not the handle the classifier echoed. The handle is
		// a digest of this and exists only to detect an answer about another
		// pair; keying the row on it would make the stored identity
		// unreadable to anyone auditing which pair a decision was about.
		IdempotencyKey:    pairKey,
		SourceAssertionID: earlier.AssertionID, SourceEpisodeID: earlier.EpisodeID,
		TargetAssertionID: later.AssertionID, TargetEpisodeID: later.EpisodeID,
		Kind: kind, Status: status, DecidedBy: g.classifier.Identity(),
		RejectedReason: string(reject),
	}

	// Shadow stops here, by construction. There is no branch below this line
	// that a shadow run can reach.
	if g.mode != ModeApply || status != model.ProposalShadow ||
		kind == model.RelationUncertain || kind == model.RelationIndependent {
		if err := g.proposals.SaveProposal(ctx, proposal); err != nil {
			return outcome, fmt.Errorf("record the relation proposal: %w", err)
		}
		outcome.Status = status
		return outcome, nil
	}

	applied, reason, err := g.apply(ctx, earlier, later, kind, proposal)
	if err != nil {
		return outcome, err
	}
	if !applied {
		outcome.RejectedReason = reason
		outcome.Kind = model.RelationUncertain
		outcome.Status = model.ProposalRejected
		return outcome, nil
	}
	outcome.Status = model.ProposalApplied
	return outcome, nil
}

// apply performs the accepted relation atomically, or performs none of it.
func (g *Generator) apply(ctx context.Context, earlier Candidate, later NewAssertion,
	kind model.AssertionRelationKind, proposal model.EpisodeRelationProposal) (bool, RejectReason, error) {
	atomic, ok := g.proposals.(AtomicProposalStore)
	if !ok {
		return false, RejectNotAtomic, nil
	}
	relation := model.EpisodeAssertionRelation{
		UserID:            later.UserID,
		SourceAssertionID: earlier.AssertionID, SourceEpisodeID: earlier.EpisodeID,
		TargetAssertionID: later.AssertionID, TargetEpisodeID: later.EpisodeID,
		Kind: kind, SourceKind: earlier.Kind, TargetKind: later.Kind,
		SourceOccurredAt: earlier.OccurredAt, TargetOccurredAt: later.OccurredAt,
		DecidedBy: g.classifier.Identity(), Applied: true,
	}
	var refusal RejectReason
	txErr := atomic.WithinTx(ctx, func(store ProposalStore, states episodeassert.Store) error {
		governor, gerr := episodeassert.NewGovernor(states)
		if gerr != nil {
			return gerr
		}
		governor.UseClock(g.now)
		switch kind {
		case model.RelationReplaces:
			// Two writes, one transaction. The earlier assertion becomes
			// replaced and points at the later one; the later one becomes
			// current. Neither half is a true statement on its own.
			existing, verr := currentVersion(ctx, states, earlier.UserID, earlier.AssertionID)
			if verr != nil {
				return verr
			}
			if rerr := governor.MarkReplaced(ctx, episodeassert.ReplaceRequest{
				UserID: later.UserID, SourceEpisodeID: earlier.EpisodeID,
				SourceAssertionID: earlier.AssertionID,
				TargetEpisodeID:   later.EpisodeID, TargetAssertionID: later.AssertionID,
				ExpectedVersion: existing,
			}); rerr != nil {
				if errors.Is(rerr, episodeassert.ErrRelationRejected) {
					refusal = RejectGovernor
					return errRefused
				}
				return rerr
			}
			targetVersion, verr := currentVersion(ctx, states, later.UserID, later.AssertionID)
			if verr != nil {
				return verr
			}
			if cerr := governor.MarkCurrent(ctx, later.UserID, later.EpisodeID,
				later.AssertionID, targetVersion); cerr != nil {
				if errors.Is(cerr, episodeassert.ErrRelationRejected) {
					refusal = RejectGovernor
					return errRefused
				}
				return cerr
			}
		case model.RelationSupplements:
			// Both ends are true, so both must say so. A supplement that left
			// them unknown would have recorded that two things are related
			// without recording that either holds - and unknown is exactly
			// what the absence of a relation already means.
			sourceState, serr := stateOf(ctx, states, earlier.UserID, earlier.AssertionID)
			if serr != nil {
				return serr
			}
			if sourceState.State == model.AssertionReplaced {
				// A superseded fact cannot be supplemented back into force.
				refusal = RejectSupersededSource
				return errRefused
			}
			if sourceState.State != model.AssertionCurrent {
				if cerr := governor.MarkCurrent(ctx, earlier.UserID, earlier.EpisodeID,
					earlier.AssertionID, sourceState.Version); cerr != nil {
					return governorRefusal(cerr, &refusal)
				}
			}
			targetState, terr := stateOf(ctx, states, later.UserID, later.AssertionID)
			if terr != nil {
				return terr
			}
			if targetState.State != model.AssertionCurrent {
				if cerr := governor.MarkCurrent(ctx, later.UserID, later.EpisodeID,
					later.AssertionID, targetState.Version); cerr != nil {
					return governorRefusal(cerr, &refusal)
				}
			}
		case model.RelationEquivalent:
			// At most one canonical current. Two rows saying the same thing
			// are not two facts, and promoting both would make the read side
			// show one fact twice.
			sourceState, serr := stateOf(ctx, states, earlier.UserID, earlier.AssertionID)
			if serr != nil {
				return serr
			}
			switch sourceState.State {
			case model.AssertionReplaced:
				// Restating a superseded fact does not revive it. Accepting
				// this would let any later paraphrase undo a replacement.
				refusal = RejectSupersededSource
				return errRefused
			case model.AssertionCurrent:
				// The canonical current stays where it is; the restatement is
				// recorded as equivalent and promoted to nothing.
			default:
				// Nothing is in force yet, so the later wording becomes the
				// canonical one and the earlier stays unknown.
				targetState, terr := stateOf(ctx, states, later.UserID, later.AssertionID)
				if terr != nil {
					return terr
				}
				if targetState.State != model.AssertionCurrent {
					if cerr := governor.MarkCurrent(ctx, later.UserID, later.EpisodeID,
						later.AssertionID, targetState.Version); cerr != nil {
						return governorRefusal(cerr, &refusal)
					}
				}
			}
		}
		if serr := store.SaveRelation(ctx, relation); serr != nil {
			return serr
		}
		proposal.Status = model.ProposalApplied
		return store.SaveProposal(ctx, proposal)
	})
	if errors.Is(txErr, errRefused) {
		proposal.Status = model.ProposalRejected
		proposal.Kind = model.RelationUncertain
		proposal.RejectedReason = string(refusal)
		if serr := g.proposals.SaveProposal(ctx, proposal); serr != nil {
			return false, refusal, fmt.Errorf("record the refused proposal: %w", serr)
		}
		return false, refusal, nil
	}
	if txErr != nil {
		return false, RejectStore, fmt.Errorf("apply the relation: %w", txErr)
	}
	return true, "", nil
}

// errRefused unwinds the transaction on a server-side refusal without turning
// the refusal into a failure of the run.
var errRefused = errors.New("relation refused by the server")

// governorRefusal turns a governor rejection into a transaction unwind and
// anything else into a real failure. A refusal is a fact about this pair; a
// store error is a fact about the run.
func governorRefusal(err error, refusal *RejectReason) error {
	if errors.Is(err, episodeassert.ErrRelationRejected) {
		*refusal = RejectGovernor
		return errRefused
	}
	return err
}

// stateOf reads one assertion governance row, or a zero row when it has none.
// A missing row is version zero and state unknown, which is what the absence
// of governance already means.
func stateOf(ctx context.Context, states episodeassert.Store, userID int64,
	assertionID string) (model.EpisodeAssertionState, error) {
	rows, err := states.LoadStates(ctx, userID, []string{assertionID})
	if err != nil {
		return model.EpisodeAssertionState{}, err
	}
	if row, ok := rows[strings.TrimSpace(assertionID)]; ok {
		return row, nil
	}
	return model.EpisodeAssertionState{}, nil
}

// existingDecision looks for a decision already recorded for this exact pair
// and classifier identity.
//
// Applied and shadow are both reused: the question has an answer on file, and
// asking again would either spend a provider call to get the same answer or,
// worse, a different one. Rejected is not reused, because a refusal can be
// caused by state that has since changed - it is re-decided under the same key
// rather than accumulating rows, and a genuinely fresh judgement is obtained
// by advancing the classifier identity, which makes a different key.
func (g *Generator) existingDecision(ctx context.Context, userID int64,
	pairKey string) (model.EpisodeRelationProposal, bool, error) {
	lookup, ok := g.proposals.(ProposalLookup)
	if !ok {
		return model.EpisodeRelationProposal{}, false, nil
	}
	proposal, found, err := lookup.ProposalByKey(ctx, userID, pairKey)
	if err != nil {
		return model.EpisodeRelationProposal{}, false, fmt.Errorf(
			"read the existing relation decision: %w", err)
	}
	if !found {
		return model.EpisodeRelationProposal{}, false, nil
	}
	switch proposal.Status {
	case model.ProposalApplied, model.ProposalShadow:
		return proposal, true, nil
	}
	return model.EpisodeRelationProposal{}, false, nil
}

// reusedOutcome reports a decision that was read rather than made.
func reusedOutcome(proposal model.EpisodeRelationProposal) Outcome {
	return Outcome{SourceAssertionID: proposal.SourceAssertionID,
		TargetAssertionID: proposal.TargetAssertionID, Kind: proposal.Kind,
		Status: proposal.Status, Reused: true}
}

// currentVersion reads the version a state row is at, so an optimistic write
// advances by exactly one. A missing row is version zero.
func currentVersion(ctx context.Context, states episodeassert.Store, userID int64,
	assertionID string) (uint64, error) {
	rows, err := states.LoadStates(ctx, userID, []string{assertionID})
	if err != nil {
		return 0, err
	}
	if row, ok := rows[strings.TrimSpace(assertionID)]; ok {
		return row.Version, nil
	}
	return 0, nil
}

// DefaultPromptVersion is the shipped relation prompt revision. It is part of
// the classifier identity, so changing the prompt produces new proposal keys
// rather than reinterpreting old decisions under the old ones.
const DefaultPromptVersion = "v1"

// ParseMode turns a configured string into a mode, refusing anything outside
// the closed set rather than falling back to a default. A typo that silently
// became "off" would look like a deployment that opted out; a typo that
// silently became "apply" would be worse.
func ParseMode(value string) (Mode, error) {
	mode := Mode(strings.TrimSpace(value))
	if mode == "" {
		return DefaultMode, nil
	}
	if !ValidMode(mode) {
		return "", fmt.Errorf("episode relation mode %q is not off, shadow or apply", value)
	}
	return mode, nil
}
