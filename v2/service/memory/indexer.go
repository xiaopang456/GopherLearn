package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/embedding"
)

// The index is a derived view of MySQL, and the only failure mode that matters
// is the silent one: a memory that was never indexed produces no error, no
// warning, and no result — it simply stops existing as far as recall is
// concerned. Every operation here therefore reports counts the caller can
// compare against the source of truth.

type MemoryIndexWriter interface {
	Store(ctx context.Context, items []memorydao.IndexedMemory) error
	Delete(ctx context.Context, userID int64, memoryIDs []string) error
	CountUser(ctx context.Context, userID int64) (int, error)
	ResetUser(ctx context.Context, userID int64) error
}

// IndexSourceReader is the MySQL side of the reconciliation. It is deliberately
// narrower than the memory repository: reindexing must not be able to write.
type IndexSourceReader interface {
	ListAllActive(ctx context.Context, userID int64, memoryType model.MemoryScope, minConfidence float64, limit int) ([]model.LongTermMemory, error)
}

type Indexer struct {
	index     MemoryIndexWriter
	embedder  embedding.Embedder
	source    IndexSourceReader
	dimension int
	batchSize int
}

func NewIndexer(index MemoryIndexWriter, embedder embedding.Embedder, source IndexSourceReader, dimension int) (*Indexer, error) {
	if index == nil || embedder == nil || source == nil {
		return nil, fmt.Errorf("memory indexer requires an index, an embedder and a source")
	}
	if dimension <= 0 {
		return nil, fmt.Errorf("memory indexer requires a positive dimension")
	}
	return &Indexer{index: index, embedder: embedder, source: source, dimension: dimension, batchSize: 16}, nil
}

// IndexMemories embeds and stores the given memories.
//
// Only `topic_note` is indexed. `profile` is injected whole rather than
// retrieved, so indexing it would put rows into the candidate pool that the
// retriever must then filter back out — and B1's implicit_preference results
// show similarity search cannot rank them usefully in the first place.
func (i *Indexer) IndexMemories(ctx context.Context, memories []model.LongTermMemory) (int, error) {
	eligible := make([]model.LongTermMemory, 0, len(memories))
	for _, memory := range memories {
		if !indexable(memory) {
			continue
		}
		eligible = append(eligible, memory)
	}
	if len(eligible) == 0 {
		return 0, nil
	}
	indexed := 0
	for start := 0; start < len(eligible); start += i.batchSize {
		end := start + i.batchSize
		if end > len(eligible) {
			end = len(eligible)
		}
		batch := eligible[start:end]
		texts := make([]string, 0, len(batch))
		for _, memory := range batch {
			texts = append(texts, embeddingText(memory))
		}
		vectors, err := i.embedder.EmbedStrings(ctx, texts)
		if err != nil {
			return indexed, fmt.Errorf("embed memories: %w", err)
		}
		if len(vectors) != len(batch) {
			return indexed, fmt.Errorf("embed memories: provider returned %d vectors for %d inputs", len(vectors), len(batch))
		}
		items := make([]memorydao.IndexedMemory, 0, len(batch))
		for offset, memory := range batch {
			vector, err := ToFloat32Vector(vectors[offset], i.dimension)
			if err != nil {
				return indexed, fmt.Errorf("memory %s: %w", memory.ID, err)
			}
			items = append(items, memorydao.IndexedMemory{Memory: memory, Vector: vector})
		}
		if err := i.index.Store(ctx, items); err != nil {
			return indexed, err
		}
		indexed += len(items)
	}
	return indexed, nil
}

// Forget removes memories from the index. Archiving, superseding and deleting
// all reach here: a memory that is no longer active must stop occupying a
// candidate slot immediately, not at the next reconciliation.
func (i *Indexer) Forget(ctx context.Context, userID int64, memoryIDs []string) error {
	return i.index.Delete(ctx, userID, memoryIDs)
}

// ApplyDreamResult keeps the index in step with governance. It is called after
// the transaction commits, never inside it: an embedding call is a network
// round trip, and holding a row lock across one is how a memory write becomes
// a database incident.
func (i *Indexer) ApplyDreamResult(ctx context.Context, memory *model.LongTermMemory) error {
	if memory == nil {
		return nil
	}
	if !indexable(*memory) {
		return i.Forget(ctx, memory.UserID, []string{memory.ID})
	}
	_, err := i.IndexMemories(ctx, []model.LongTermMemory{*memory})
	return err
}

// ReindexReport is what makes an incomplete backfill visible. Coverage below
// 100% means some memories are unreachable by recall, which no other signal in
// the system reports.
type ReindexReport struct {
	UserID  int64
	Source  int
	Indexed int
	Stored  int
}

func (r ReindexReport) Complete() bool { return r.Source == r.Indexed && r.Stored >= r.Indexed }

// Reindex rebuilds one user's index from MySQL. The index is dropped for that
// user first, so a memory deleted while the index was stale does not survive
// the rebuild.
func (i *Indexer) Reindex(ctx context.Context, userID int64, minConfidence float64, limit int) (ReindexReport, error) {
	memories, err := i.source.ListAllActive(ctx, userID, model.MemoryScopeTopic, minConfidence, limit)
	if err != nil {
		return ReindexReport{UserID: userID}, fmt.Errorf("list memories to index: %w", err)
	}
	report := ReindexReport{UserID: userID, Source: len(memories)}
	if err := i.index.ResetUser(ctx, userID); err != nil {
		return report, err
	}
	indexed, err := i.IndexMemories(ctx, memories)
	report.Indexed = indexed
	if err != nil {
		return report, err
	}
	stored, err := i.index.CountUser(ctx, userID)
	if err != nil {
		return report, err
	}
	report.Stored = stored
	return report, nil
}

func indexable(memory model.LongTermMemory) bool {
	return memory.Scope == model.MemoryScopeTopic &&
		memory.Status == model.LongTermMemoryActive &&
		strings.TrimSpace(memory.ID) != "" &&
		memory.UserID > 0
}

// embeddingText matches what EXP-002B embedded, subject then content. The
// cosine floor of 0.535 was calibrated against vectors built this way; changing
// the composition invalidates the threshold.
func embeddingText(memory model.LongTermMemory) string {
	return strings.TrimSpace(memory.Subject) + "。" + strings.TrimSpace(memory.Content)
}
