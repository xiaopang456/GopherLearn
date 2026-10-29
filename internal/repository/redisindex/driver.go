package redisindex

import (
	"context"

	"github.com/xiaopang456/GopherLearn/internal/knowledge"
)

// Driver is the index-side half of what knowledge.IndexPool and knowledge.CleanupPool
// need: the operations that turn a claimed version into vectors and back out again.
//
// It is a separate type rather than three more methods on Synchronizer because the
// decorator already carries a DeleteDocument meaning "retire this document through the
// whole lifecycle", and a second removal-shaped method on the same object meaning "delete
// these stored rows" is exactly how a driver ends up calling the wrong one.
type Driver struct {
	synchronizer *Synchronizer
}

var (
	_ knowledge.IndexRunner = (*Driver)(nil)
	_ knowledge.RowCleaner  = (*Driver)(nil)
)

// Driver returns the adapter the queue pools are given. The pool itself is constructed
// over the Lifecycle, which the decorator forwards, so one object answers both halves.
func (s *Synchronizer) Driver() *Driver { return &Driver{synchronizer: s} }

// IndexVersion embeds and writes one document version's chunks. The document is read
// back through the owning user first: a driver works a queue of everybody's documents,
// and writing vectors for an id nobody is entitled to name is not something an index
// should be able to do on the strength of having been handed the id.
func (d *Driver) IndexVersion(ctx context.Context, userID, documentID string, version int) (int, error) {
	document, err := d.synchronizer.Store.GetDocument(ctx, userID, documentID)
	if err != nil {
		return 0, err
	}
	return d.synchronizer.indexDocument(ctx, userID, document, version)
}

// SweepExcept keeps the version that is answering and drops the rest, which is the
// target index's own removal shape - see knowledge.RowCleaner.
func (d *Driver) SweepExcept(ctx context.Context, userID, documentID string, keep int) error {
	return d.synchronizer.index.DeleteVersions(ctx, userID, documentID, keep)
}

// DropDocument removes every stored row of one document.
func (d *Driver) DropDocument(ctx context.Context, userID, documentID string) error {
	return d.synchronizer.index.DeleteDocument(ctx, userID, documentID)
}
