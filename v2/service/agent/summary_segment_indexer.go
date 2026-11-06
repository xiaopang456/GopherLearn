package agent

import (
	"context"
	"fmt"
	"log"
	"time"

	"GopherAI/model"
	memoryservice "GopherAI/service/memory"

	"github.com/cloudwego/eino/components/embedding"
)

// The segments' derived search vector.
//
// The index is derived: MySQL holds the segment, and the index holds the
// vector that finds it. Nothing is lost when the index is lost, and the
// repair below puts it back.
//
// The write happens after MySQL has committed and never before. A segment
// that is indexed but not stored would be found by a search and then not
// exist; a segment that is stored but not indexed is merely invisible to a
// search, and the repair finds it.

// SummarySegmentIndex is the derived view the indexer writes.
type SummarySegmentIndex interface {
	// EnsureIndex creates the index, or verifies an existing one.
	EnsureIndex(context.Context) error
	// Store writes one segment's row.
	Store(context.Context, model.SessionSummarySegment, []float32) error
	// Delete removes a session's segments from the index.
	Delete(context.Context, int64, string, []int) error
	// CountUser counts one user's indexed segments.
	CountUser(context.Context, int64) (int, error)
	// CountAll counts every indexed segment.
	CountAll(context.Context) (int, error)
	// ResetUser removes one user's complete derived view.
	ResetUser(context.Context, int64) error
}

// SummarySegmentIndexSource is the MySQL side of the index.
type SummarySegmentIndexSource interface {
	// UpdateSummarySegmentIndexStatus records whether the segment's vector is
	// in place.
	UpdateSummarySegmentIndexStatus(ctx context.Context, userID int64, sessionID string, seq int, status string) error
	// ListSummarySegmentsForUser returns one user's segments.
	ListSummarySegmentsForUser(ctx context.Context, userID int64, limit int) ([]model.SessionSummarySegment, error)
	// CountSummarySegmentsForUser counts one user's segments.
	CountSummarySegmentsForUser(ctx context.Context, userID int64) (int, error)
	// CountSummarySegments counts every segment.
	CountSummarySegments(ctx context.Context) (int, error)
	// ListSummarySegmentOwners returns the users who have segments.
	ListSummarySegmentOwners(ctx context.Context, limit int) ([]int64, error)
}

// SummarySegmentIndexRepairer puts the derived index back in step with MySQL.
//
// It is what a deployment attaches to the worker: the index is written as the
// segments are generated, and the only thing that can make it wrong is a
// loss.
type SummarySegmentIndexRepairer interface {
	// RunRepairOnce runs one repair pass.
	RunRepairOnce(ctx context.Context, limit int) (SummarySegmentReindexReport, error)
}

// SummarySegmentIndexer publishes the segments' derived vectors.
type SummarySegmentIndexer struct {
	index     SummarySegmentIndex
	source    SummarySegmentIndexSource
	embedder  embedding.Embedder
	dimension int
}

// NewSummarySegmentIndexer builds an indexer.
func NewSummarySegmentIndexer(index SummarySegmentIndex, source SummarySegmentIndexSource,
	embedder embedding.Embedder, dimension int) (*SummarySegmentIndexer, error) {
	if index == nil || source == nil || embedder == nil || dimension <= 0 {
		return nil, fmt.Errorf("summary segment indexer dependencies are required")
	}
	return &SummarySegmentIndexer{index: index, source: source, embedder: embedder, dimension: dimension}, nil
}

// Index publishes one segment's vector.
//
// A failure is recorded on the segment and never turns a successful
// generation into another attempt: the segment is in MySQL and is readable
// there, and only the search for it is missing.
func (i *SummarySegmentIndexer) Index(ctx context.Context, segment *model.SessionSummarySegment) (returnErr error) {
	if segment == nil {
		return nil
	}
	vectors, err := i.embedder.EmbedStrings(ctx, []string{segment.SearchText()})
	if err == nil && len(vectors) != 1 {
		err = fmt.Errorf("summary segment embedder returned %d vectors", len(vectors))
	}
	var vector []float32
	if err == nil {
		vector, err = memoryservice.ToFloat32Vector(vectors[0], i.dimension)
	}
	if err == nil {
		err = i.index.Store(ctx, *segment, vector)
	}
	status := model.SessionSummarySegmentIndexIndexed
	if err != nil {
		status = model.SessionSummarySegmentIndexFailed
	}
	statusCtx, cancelStatus := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelStatus()
	if statusErr := i.source.UpdateSummarySegmentIndexStatus(statusCtx, segment.UserID, segment.SessionID,
		segment.Seq, status); statusErr != nil {
		if err != nil {
			return fmt.Errorf("index summary segment: %v; mark %s: %w", err, status, statusErr)
		}
		return fmt.Errorf("mark summary segment %s: %w", status, statusErr)
	}
	if err != nil {
		return fmt.Errorf("index summary segment: %w", err)
	}
	segment.IndexStatus = status
	return nil
}

// SummarySegmentReindexReport is what one repair pass did.
type SummarySegmentReindexReport struct {
	// Owners are the users the pass looked at.
	Owners int
	// Rebuilt are the users whose rows were rebuilt.
	Rebuilt int
	// Stored are the segments the pass wrote.
	Stored int
}

// Complete reports whether every user the pass looked at is in step with
// MySQL.
func (r SummarySegmentReindexReport) Complete() bool { return r.Owners == r.Rebuilt }

// Reindex rebuilds one user's rows from MySQL.
//
// It is the repair for a lost index: the rows are dropped and written again
// from the segments MySQL holds. A user with no segments loses nothing and
// is left alone.
func (i *SummarySegmentIndexer) Reindex(ctx context.Context, userID int64) (SummarySegmentReindexReport, error) {
	report := SummarySegmentReindexReport{}
	if userID <= 0 {
		return report, fmt.Errorf("reindex requires user id")
	}
	if err := i.index.EnsureIndex(ctx); err != nil {
		return report, err
	}
	segments, err := i.source.ListSummarySegmentsForUser(ctx, userID, 0)
	if err != nil {
		return report, err
	}
	if err := i.index.ResetUser(ctx, userID); err != nil {
		return report, err
	}
	for index := range segments {
		if err := i.Index(ctx, &segments[index]); err != nil {
			return report, err
		}
		report.Stored++
	}
	report.Rebuilt = 1
	return report, nil
}

// Repair puts the index back in step with MySQL.
//
// The check is the count: MySQL says how many segments a user has, and the
// index says how many rows it holds. A difference means rows are missing -
// Redis was flushed, the index was never created for that user, or a write
// failed - and the user's rows are rebuilt from MySQL.
//
// A user whose counts agree is left alone: rebuilding on every pass would
// re-embed every segment of every user, which is a provider call per segment
// for nothing.
func (i *SummarySegmentIndexer) Repair(ctx context.Context, limit int) (SummarySegmentReindexReport, error) {
	report := SummarySegmentReindexReport{}
	// The cheap check comes first: the whole table against the whole index.
	// The two agree in the normal case, and the per-user walk - which scans
	// the index once per user - only runs when they do not.
	expected, err := i.source.CountSummarySegments(ctx)
	if err != nil {
		return report, err
	}
	stored, err := i.index.CountAll(ctx)
	if err != nil {
		return report, err
	}
	if expected == stored {
		return report, nil
	}
	owners, err := i.source.ListSummarySegmentOwners(ctx, limit)
	if err != nil {
		return report, err
	}
	report.Owners = len(owners)
	for _, owner := range owners {
		expectedForUser, err := i.source.CountSummarySegmentsForUser(ctx, owner)
		if err != nil {
			return report, err
		}
		storedForUser, err := i.index.CountUser(ctx, owner)
		if err != nil {
			return report, err
		}
		if expectedForUser == storedForUser {
			continue
		}
		rebuilt, err := i.Reindex(ctx, owner)
		if err != nil {
			return report, err
		}
		report.Rebuilt++
		report.Stored += rebuilt.Stored
	}
	return report, nil
}

// RunRepairOnce runs one repair pass and logs what it did.
//
// It is a repair, not the normal path: the normal path writes each segment's
// vector as it is generated, and the repair exists so that a lost index does
// not stay lost.
func (i *SummarySegmentIndexer) RunRepairOnce(ctx context.Context, limit int) (SummarySegmentReindexReport, error) {
	report, err := i.Repair(ctx, limit)
	if err != nil {
		return report, err
	}
	if report.Rebuilt > 0 {
		log.Printf("[summary-segment] index repaired: owners=%d rebuilt=%d stored=%d",
			report.Owners, report.Rebuilt, report.Stored)
	}
	return report, nil
}
