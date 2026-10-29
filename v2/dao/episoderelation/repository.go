// Package episoderelation stores the episodic write side's relation
// proposals and accepted edges.
//
// It is a separate repository from the assertion state repository because the
// two answer different questions and one of them is new. A state row says what
// an assertion is now; a relation row says how a pair stands, and a proposal
// row says what a classifier thought whether or not anything was applied.
//
// The one thing this package adds beyond storage is a transaction. Applying a
// replacement is two state writes, and a failure between them leaves a history
// saying a decision was superseded by something that is not in force. That is
// worse than either write alone, so the applying path runs inside one
// transaction or not at all.
package episoderelation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	episodeassertdao "GopherAI/dao/episodeassert"
	"GopherAI/model"
	"GopherAI/service/episodeassert"
	"GopherAI/service/episoderelation"
)

// Repository is the relation store over one database handle.
type Repository struct{ db *gorm.DB }

// NewRepository refuses a nil handle rather than failing at the first write.
func NewRepository(db *gorm.DB) (*Repository, error) {
	if db == nil {
		return nil, fmt.Errorf("the relation repository needs a database")
	}
	return &Repository{db: db}, nil
}

// SaveProposal writes one proposal, idempotently on its own key.
//
// The upsert is on the key rather than a read-then-write, because two
// projections of one Session racing is exactly the case that would otherwise
// leave two rows for one pair.
func (r *Repository) SaveProposal(ctx context.Context,
	proposal model.EpisodeRelationProposal) error {
	if err := proposal.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	proposal.UpdatedAt = now
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = now
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "idempotency_key"}},
		DoUpdates: clause.AssignmentColumns([]string{"kind", "status",
			"rejected_reason", "source_episode_id", "target_episode_id",
			"updated_at"}),
	}).Create(&proposal).Error
	if err != nil {
		return fmt.Errorf("save relation proposal: %w", err)
	}
	return nil
}

// SaveRelation writes one accepted edge, idempotently on the pair.
//
// The upsert is on the database's own unique index, not on a preceding read.
// A read-then-create is two statements with a gap in the middle, and two
// projections of one Session racing through that gap is exactly how one pair
// becomes two rows - which no amount of checking first can prevent, because
// the check is the first of the two statements.
func (r *Repository) SaveRelation(ctx context.Context,
	relation model.EpisodeAssertionRelation) error {
	if err := relation.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	relation.UpdatedAt = now
	if relation.CreatedAt.IsZero() {
		relation.CreatedAt = now
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "source_assertion_id"},
			{Name: "target_assertion_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"kind", "source_episode_id",
			"target_episode_id", "source_kind", "target_kind",
			"source_occurred_at", "target_occurred_at", "decided_by", "applied",
			"updated_at"}),
	}).Create(&relation).Error
	if err != nil {
		return fmt.Errorf("save assertion relation: %w", err)
	}
	return nil
}

// ProposalByKey finds a decision already recorded for one pair, so a replay
// reuses it instead of asking the classifier again.
func (r *Repository) ProposalByKey(ctx context.Context, userID int64,
	key string) (model.EpisodeRelationProposal, bool, error) {
	var row model.EpisodeRelationProposal
	err := r.db.WithContext(ctx).
		Where("user_id = ? AND idempotency_key = ?", userID, strings.TrimSpace(key)).
		First(&row).Error
	switch {
	case err == nil:
		return row, true, nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return model.EpisodeRelationProposal{}, false, nil
	default:
		return model.EpisodeRelationProposal{}, false,
			fmt.Errorf("read the relation proposal: %w", err)
	}
}

// Relations reads one learner's edges. An empty source returns all of them.
func (r *Repository) Relations(ctx context.Context, userID int64,
	sourceAssertionID string) ([]model.EpisodeAssertionRelation, error) {
	out := []model.EpisodeAssertionRelation{}
	if userID <= 0 {
		return out, nil
	}
	query := r.db.WithContext(ctx).Where("user_id = ?", userID)
	if source := strings.TrimSpace(sourceAssertionID); source != "" {
		query = query.Where("source_assertion_id = ?", source)
	}
	if err := query.Order("id asc").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("read assertion relations: %w", err)
	}
	return out, nil
}

// MaxRelationLookupAssertions bounds one batched relation lookup.
//
// A governed read window is a handful of cards and their slots, so this ceiling
// is far above anything a real search produces. It is here because an unbounded
// IN list is a query whose cost is set by the caller's mistake: a lookup that
// accidentally passed a learner's whole history would still run, just slowly,
// and slowly is how that kind of defect survives.
const MaxRelationLookupAssertions = 256

// RelationsForAssertions reads the edges touching one window's assertions.
//
// This is the read side's query, and it is deliberately not Relations(userID,
// "") - that reads every edge the learner has ever had, on every governed
// search, to annotate at most a few slots. One learner's history grows; a
// window does not.
//
// The scope is both ends: an assertion is equally a supplement's source and its
// target, and reading only the source column would silently hide every relation
// from the later assertion's point of view.
func (r *Repository) RelationsForAssertions(ctx context.Context, userID int64,
	assertionIDs []string) ([]model.EpisodeAssertionRelation, error) {
	out := []model.EpisodeAssertionRelation{}
	if userID <= 0 || len(assertionIDs) == 0 {
		return out, nil
	}
	seen := make(map[string]struct{}, len(assertionIDs))
	ids := make([]string, 0, len(assertionIDs))
	for _, id := range assertionIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		ids = append(ids, trimmed)
		if len(ids) >= MaxRelationLookupAssertions {
			break
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []model.EpisodeAssertionRelation
	// One query, both columns. Two queries would be two snapshots, and a
	// relation written between them would appear from one end only.
	if err := r.db.WithContext(ctx).
		Where("user_id = ? AND (source_assertion_id IN ? OR target_assertion_id IN ?)",
			userID, ids, ids).
		Order("id asc").
		Limit(MaxRelationLookupAssertions * 4).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("read relations for assertions: %w", err)
	}
	for _, row := range rows {
		if row.UserID != userID {
			// Defence in depth, exactly as in the candidate repository: the
			// query is scoped, and a scoped query that ever returned a foreign
			// row would be a cross-account read nothing downstream would catch.
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// Proposals reads one learner's proposals in a stable order.
func (r *Repository) Proposals(ctx context.Context,
	userID int64) ([]model.EpisodeRelationProposal, error) {
	out := []model.EpisodeRelationProposal{}
	if userID <= 0 {
		return out, nil
	}
	if err := r.db.WithContext(ctx).Where("user_id = ?", userID).
		Order("id asc").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("read relation proposals: %w", err)
	}
	return out, nil
}

// WithinTx runs fn against a relation store and an assertion state store that
// share one transaction.
//
// Both halves are bound to the same handle deliberately. A transaction that
// covered the relation rows but not the state rows would still allow the
// half-applied replacement it exists to prevent.
func (r *Repository) WithinTx(ctx context.Context,
	fn func(episoderelation.ProposalStore, episodeassert.Store) error) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		relations, err := NewRepository(tx)
		if err != nil {
			return err
		}
		states, err := episodeassertdao.NewRepository(tx)
		if err != nil {
			return err
		}
		return fn(relations, states)
	})
}

// The repository satisfies both seams the applying generator needs.
var (
	_ episoderelation.ProposalStore       = (*Repository)(nil)
	_ episoderelation.AtomicProposalStore = (*Repository)(nil)
	_ episoderelation.ProposalLookup      = (*Repository)(nil)
)
