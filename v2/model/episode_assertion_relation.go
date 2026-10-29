package model

import (
	"fmt"
	"strings"
	"time"
)

// The write-side relation between two assertions, and the proposal that
// suggested it.
//
// STABILIZE-063 and -064 gave an assertion three states - current, replaced,
// unknown - and one edge, ReplacedByAssertionID. That is enough to say "this
// decision was superseded by that one" and nothing else, and the eight
// replacements the 064 corpus carried were seeded by hand rather than inferred
// from anything a learner said.
//
// A state and a relation are different things and were being made to share one
// field. "Replaced" is a property of an assertion now; "supplements" is a
// property of a pair, and both members stay current. Writing a supplement into
// ReplacedByAssertionID would mark a true statement as superseded - the exact
// harm the governance layer exists to prevent - so supplements and
// equivalences get their own row rather than a re-used one.

// AssertionRelationKind is the closed vocabulary the write-side classifier may
// produce. Nothing outside this set may be persisted.
type AssertionRelationKind string

const (
	// RelationEquivalent means the two assertions state the same fact. The
	// later one must not become a second, competing current fact.
	RelationEquivalent AssertionRelationKind = "equivalent"
	// RelationReplaces means the later assertion overturns the earlier one.
	// This is the only relation that changes a state.
	RelationReplaces AssertionRelationKind = "replaces"
	// RelationSupplements means the later assertion adds to the earlier one
	// and both remain true.
	RelationSupplements AssertionRelationKind = "supplements"
	// RelationIndependent means the two are about related topics and neither
	// decides anything about the other.
	RelationIndependent AssertionRelationKind = "independent"
	// RelationUncertain means the evidence does not support any of the above.
	//
	// It is a first-class answer, not a failure. A classifier that must choose
	// between four confident labels will choose one, and the cost of a wrong
	// "replaces" is a true statement marked superseded.
	RelationUncertain AssertionRelationKind = "uncertain"
)

// ValidAssertionRelationKind reports whether a value is in the closed set.
func ValidAssertionRelationKind(kind AssertionRelationKind) bool {
	switch kind {
	case RelationEquivalent, RelationReplaces, RelationSupplements,
		RelationIndependent, RelationUncertain:
		return true
	}
	return false
}

// ChangesState reports whether a relation, once applied, alters an assertion
// state. Only replacement does; everything else is recorded and changes
// nothing, which is what makes shadow mode meaningful for the rest.
func (k AssertionRelationKind) ChangesState() bool { return k == RelationReplaces }

// EpisodeAssertionRelation is one accepted, durable edge between two
// assertions of one learner.
//
// Source is the earlier assertion and Target the later one, always. The
// direction is not a matter of which the classifier happened to see first: a
// relation whose target predates its source is refused, because accepting it
// would let an older record present itself as the newer one.
type EpisodeAssertionRelation struct {
	ID     uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID int64  `gorm:"not null;index:idx_relation_owner_source,priority:1;uniqueIndex:idx_relation_pair,priority:1" json:"user_id"`
	// SourceAssertionID is the earlier assertion; TargetAssertionID the later.
	// The pair is unique in the database, not merely checked before writing.
	// A read-then-create is two statements with a gap, and two projections of
	// one Session racing through that gap is exactly how one pair becomes two
	// rows. The constraint closes the gap; the upsert handles the conflict.
	SourceAssertionID string                `gorm:"type:varchar(200);not null;index:idx_relation_owner_source,priority:2;uniqueIndex:idx_relation_pair,priority:2" json:"source_assertion_id"`
	SourceEpisodeID   string                `gorm:"type:varchar(36);not null" json:"source_episode_id"`
	TargetAssertionID string                `gorm:"type:varchar(200);not null;uniqueIndex:idx_relation_pair,priority:3" json:"target_assertion_id"`
	TargetEpisodeID   string                `gorm:"type:varchar(36);not null" json:"target_episode_id"`
	Kind              AssertionRelationKind `gorm:"type:varchar(16);not null;index" json:"kind"`
	// SourceKind and TargetKind are denormalised so a reader can reject a
	// relation between kinds that may not relate without a second lookup.
	SourceKind AssertionKind `gorm:"type:varchar(16);not null" json:"source_kind"`
	TargetKind AssertionKind `gorm:"type:varchar(16);not null" json:"target_kind"`
	// SourceOccurredAt and TargetOccurredAt are the assertion times the
	// ordering was checked against, kept so a later audit can see what the
	// decision was made on rather than re-deriving it.
	SourceOccurredAt time.Time `gorm:"not null" json:"source_occurred_at"`
	TargetOccurredAt time.Time `gorm:"not null" json:"target_occurred_at"`
	// DecidedBy names what produced the relation: the classifier identity for
	// a model decision, or an operator for a manual one. It is never free
	// text from a model.
	DecidedBy string `gorm:"type:varchar(64);not null" json:"decided_by"`
	// Applied records whether this relation changed any state. A replacement
	// row with Applied false was recorded in shadow and altered nothing.
	Applied   bool      `gorm:"not null;default:false" json:"applied"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EpisodeAssertionRelation) TableName() string { return "episode_assertion_relations" }

// Validate checks one relation row in isolation.
//
// Existence, ownership against the store, and cycles need other rows and stay
// in the service layer, exactly as they do for assertion states.
func (r *EpisodeAssertionRelation) Validate() error {
	if r == nil {
		return fmt.Errorf("assertion relation is nil")
	}
	if r.UserID <= 0 {
		return fmt.Errorf("assertion relation needs an owner")
	}
	source, target := strings.TrimSpace(r.SourceAssertionID), strings.TrimSpace(r.TargetAssertionID)
	if source == "" || target == "" {
		return fmt.Errorf("assertion relation needs both ends")
	}
	if source == target {
		return fmt.Errorf("an assertion cannot relate to itself")
	}
	if !ValidAssertionID(source) || !ValidAssertionID(target) {
		return fmt.Errorf("assertion relation ids must be server-issued")
	}
	if strings.TrimSpace(r.SourceEpisodeID) == "" || strings.TrimSpace(r.TargetEpisodeID) == "" {
		return fmt.Errorf("assertion relation needs both cards named")
	}
	if !ValidAssertionRelationKind(r.Kind) {
		return fmt.Errorf("assertion relation kind %q is not in the closed set", r.Kind)
	}
	// Uncertain is a proposal outcome, not an edge. Persisting it as a
	// relation would turn "we do not know" into a recorded connection.
	if r.Kind == RelationUncertain {
		return fmt.Errorf("an uncertain outcome is not a relation and is not stored as one")
	}
	// Independent is likewise the absence of an edge.
	if r.Kind == RelationIndependent {
		return fmt.Errorf("independent assertions have no relation row")
	}
	if !r.SourceKind.Valid() || !r.TargetKind.Valid() {
		return fmt.Errorf("assertion relation needs both kinds")
	}
	if r.SourceOccurredAt.IsZero() || r.TargetOccurredAt.IsZero() {
		return fmt.Errorf("assertion relation needs both times")
	}
	if r.TargetOccurredAt.Before(r.SourceOccurredAt) {
		return fmt.Errorf("the later assertion of a relation cannot predate the earlier one")
	}
	if strings.TrimSpace(r.DecidedBy) == "" {
		return fmt.Errorf("assertion relation needs to say what decided it")
	}
	return nil
}

// RelationProposalStatus is what happened to a proposal.
type RelationProposalStatus string

const (
	// ProposalShadow was recorded and deliberately changed nothing.
	ProposalShadow RelationProposalStatus = "shadow"
	// ProposalApplied changed state or wrote a relation.
	ProposalApplied RelationProposalStatus = "applied"
	// ProposalRejected failed a server-side check.
	ProposalRejected RelationProposalStatus = "rejected"
)

// EpisodeRelationProposal is one classifier decision, recorded whether or not
// anything was applied.
//
// It exists so a shadow run leaves evidence. A generator that ran, decided and
// wrote nothing would be indistinguishable from a generator that never ran -
// the "absence is not evidence" rule this project has already paid for once.
type EpisodeRelationProposal struct {
	ID     uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	UserID int64  `gorm:"not null;index:idx_proposal_identity,priority:1" json:"user_id"`
	// IdempotencyKey is owner + source + target + classifier identity. A
	// second run over the same pair updates this row instead of adding one,
	// so re-projection and retry do not multiply proposals.
	IdempotencyKey    string                 `gorm:"type:varchar(300);not null;uniqueIndex" json:"idempotency_key"`
	SourceAssertionID string                 `gorm:"type:varchar(200);not null;index:idx_proposal_identity,priority:2" json:"source_assertion_id"`
	SourceEpisodeID   string                 `gorm:"type:varchar(36);not null" json:"source_episode_id"`
	TargetAssertionID string                 `gorm:"type:varchar(200);not null" json:"target_assertion_id"`
	TargetEpisodeID   string                 `gorm:"type:varchar(36);not null" json:"target_episode_id"`
	Kind              AssertionRelationKind  `gorm:"type:varchar(16);not null" json:"kind"`
	Status            RelationProposalStatus `gorm:"type:varchar(16);not null" json:"status"`
	// DecidedBy is the classifier identity.
	DecidedBy string `gorm:"type:varchar(64);not null" json:"decided_by"`
	// RejectedReason is a closed code when the server refused the relation.
	// It is never the model's own words: a classifier's free text could carry
	// the learner's message back into a durable row.
	RejectedReason string    `gorm:"type:varchar(64)" json:"rejected_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (EpisodeRelationProposal) TableName() string { return "episode_relation_proposals" }

// Validate checks one proposal row in isolation. Unlike a relation, a proposal
// may carry uncertain and independent: those are decisions worth recording
// precisely because they changed nothing.
func (p *EpisodeRelationProposal) Validate() error {
	if p == nil {
		return fmt.Errorf("relation proposal is nil")
	}
	if p.UserID <= 0 {
		return fmt.Errorf("relation proposal needs an owner")
	}
	if strings.TrimSpace(p.IdempotencyKey) == "" {
		return fmt.Errorf("relation proposal needs an idempotency key")
	}
	source, target := strings.TrimSpace(p.SourceAssertionID), strings.TrimSpace(p.TargetAssertionID)
	if source == "" || target == "" || source == target {
		return fmt.Errorf("relation proposal needs two distinct ends")
	}
	if !ValidAssertionRelationKind(p.Kind) {
		return fmt.Errorf("relation proposal kind %q is not in the closed set", p.Kind)
	}
	switch p.Status {
	case ProposalShadow, ProposalApplied, ProposalRejected:
	default:
		return fmt.Errorf("relation proposal status %q is not in the closed set", p.Status)
	}
	if strings.TrimSpace(p.DecidedBy) == "" {
		return fmt.Errorf("relation proposal needs to say what decided it")
	}
	return nil
}

// RelationIdempotencyKey is the one way a proposal is identified.
//
// It is owner, direction and classifier identity together: the same pair
// judged by a different classifier is a different proposal, and the same pair
// judged again by the same classifier is the same one.
func RelationIdempotencyKey(userID int64, source, target, decidedBy string) string {
	return fmt.Sprintf("%d|%s|%s|%s", userID, strings.TrimSpace(source),
		strings.TrimSpace(target), strings.TrimSpace(decidedBy))
}
