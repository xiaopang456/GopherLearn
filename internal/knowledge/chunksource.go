package knowledge

import (
	"context"
	"errors"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// ErrNotConfigured reports a source built without a store.
var ErrNotConfigured = errors.New("knowledge: chunk source has no store")

// ChunkSource adapts a Store to the retrieval candidate source. Keeping the
// adapter here means the retrieval package never learns how chunks are stored,
// and the store never learns how they are ranked.
type ChunkSource struct {
	store Store
}

func NewChunkSource(store Store) *ChunkSource {
	return &ChunkSource{store: store}
}

// Chunks implements retrieval.Source. Ownership is enforced by the store, so a
// scope naming somebody else's collection fails instead of returning their
// chunks.
func (s *ChunkSource) Chunks(ctx context.Context, userID string, collectionIDs []string) ([]domain.DocumentChunk, error) {
	if s == nil || s.store == nil {
		return nil, ErrNotConfigured
	}
	if strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	return s.store.ChunksFor(ctx, userID, collectionIDs)
}
