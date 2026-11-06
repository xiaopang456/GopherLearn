// Package indexaudit compares what the record says the knowledge index holds
// with what Redis actually holds.
//
// The comparison is read-only. It never deletes a key and never writes to
// Redis; what it produces is the list of documents whose index is thinner than
// the record says, together with the reasons the index itself does not agree
// with the keys under it. The derived-view collector (v2/internal/derivedgc)
// reads the same source, counts the same keys and uses the same judgement, so
// the two never disagree about which document is short.
//
// The comparison lives here so that the collector and the startup audit share
// one implementation. A second implementation would eventually answer a
// different question than the collector, and the operator would have two
// truths about the same index.
package indexaudit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	redisCommon "GopherAI/common/redis"
	"GopherAI/model"

	redisCli "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Identity is one indexed document version.
type Identity struct {
	UserID     int64
	DocumentID string
	Version    int
}

// Document is one document version and the two block counts that were
// compared.
type Document struct {
	Identity
	// Expected is the block count the record carries.
	Expected int
	// Actual is the number of blocks the index holds.
	Actual int
}

// Reconciliation is the outcome of comparing the record with the index.
type Reconciliation struct {
	// Recorded is the number of document versions the record knows.
	Recorded int
	// Expected is the number of blocks the record says the index holds.
	Expected int
	// Kept is the number of blocks the index holds for recorded versions.
	Kept int
	// Missing are the document versions whose index holds fewer blocks than
	// the record says - the shape a restart on an old snapshot leaves behind.
	Missing []Document
	// Extra are the document versions whose index holds more blocks than the
	// record says.
	Extra []Document
	// Blockers are the reasons the comparison could not be completed. They
	// are categories, never document text.
	Blockers []string
}

// Reconcile compares the recorded block counts with the observed ones.
//
// Both maps are keyed by document version. A version the record knows but the
// index does not hold is missing; a version the index holds more blocks for
// than the record says is extra. A mismatch is also a blocker: the collector
// refuses to delete anything while a recorded tuple is short, because a
// half-indexed document that is cleaned up as an orphan loses the only copy
// of its content.
func Reconcile(recorded map[Identity]int, observed map[Identity]int) Reconciliation {
	result := Reconciliation{Recorded: len(recorded)}
	for identity, want := range recorded {
		result.Expected += want
		got := observed[identity]
		result.Kept += got
		switch {
		case got < want:
			result.Missing = append(result.Missing, Document{Identity: identity, Expected: want, Actual: got})
		case got > want:
			result.Extra = append(result.Extra, Document{Identity: identity, Expected: want, Actual: got})
		}
		if got != want {
			result.Blockers = append(result.Blockers, fmt.Sprintf("active knowledge tuple has %d hashes, expected %d", got, want))
		}
	}
	sortDocuments(result.Missing)
	sortDocuments(result.Extra)
	sort.Strings(result.Blockers)
	return result
}

// Source reads the block counts the record carries.
type Source interface {
	// Source returns the recorded block count of every document version that
	// is considered indexed.
	Source(ctx context.Context) (map[Identity]int, error)
}

// Counter counts the blocks the index holds.
type Counter interface {
	// Counts returns the number of blocks per document version, together with
	// the reasons the count could not be completed.
	Counts(ctx context.Context) (map[Identity]int, []string, error)
}

// MySQLSource reads the recorded block counts from MySQL.
//
// Only the active version of a document is compared, and only a document that
// carries a chunk count: a document that is still being indexed has no count
// to compare against, and comparing it would report a difference that is not
// a loss.
type MySQLSource struct {
	DB *gorm.DB
	// ExcludeInFlight drops the documents that are queued or being indexed.
	// Such a document is on its way to a new active version; its block count
	// is not the one the index is expected to hold.
	//
	// The collector does not set it: for the collector a document that is
	// being rebuilt still owns the blocks of its previous version, and
	// treating them as orphans would delete the only copy of that content.
	ExcludeInFlight bool
}

func (s MySQLSource) Source(ctx context.Context) (map[Identity]int, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("index audit requires mysql")
	}
	type row struct {
		UserID                    int64
		ID                        string
		ActiveVersion, ChunkCount int
	}
	query := s.DB.WithContext(ctx).Model(&model.KnowledgeDocument{}).Select("user_id, id, active_version, chunk_count").
		Where("active_name IS NOT NULL AND active_version > 0 AND chunk_count > 0")
	if s.ExcludeInFlight {
		query = query.Where("index_status NOT IN ?", []model.KnowledgeDocumentStatus{
			model.KnowledgeStatusPending,
			model.KnowledgeStatusIndexing,
		})
	}
	var rows []row
	if err := query.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load knowledge index source: %w", err)
	}
	result := make(map[Identity]int, len(rows))
	for _, row := range rows {
		result[Identity{UserID: row.UserID, DocumentID: row.ID, Version: row.ActiveVersion}] = row.ChunkCount
	}
	return result, nil
}

// RedisCounter counts the blocks Redis holds for the knowledge index.
//
// The keys are counted with a scan, never with KEYS: the index is the working
// set of a live deployment, and a blocking enumeration would stall it. Only
// HASH keys are counted, because a key of another type under the managed
// prefix is not a block of a document; such a key is reported as a blocker
// instead of being counted as content.
type RedisCounter struct {
	Client *redisCli.Client
	// Prefix is the managed key prefix. It defaults to the knowledge chunk
	// prefix.
	Prefix string
}

func (c RedisCounter) prefix() string {
	if c.Prefix != "" {
		return c.Prefix
	}
	return redisCommon.KnowledgeChunkKeyPrefix
}

func (c RedisCounter) Counts(ctx context.Context) (map[Identity]int, []string, error) {
	if c.Client == nil {
		return nil, nil, fmt.Errorf("index audit requires redis")
	}
	prefix := c.prefix()
	counts := make(map[Identity]int)
	var blockers []string
	var cursor uint64
	for {
		keys, next, err := c.Client.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			return nil, nil, fmt.Errorf("scan knowledge chunks: %w", err)
		}
		// The type of every key is asked for in one round trip per batch: a
		// startup comparison walks the whole index, and one round trip per
		// key would turn a large corpus into a long start.
		pipeline := c.Client.Pipeline()
		types := make([]*redisCli.StatusCmd, 0, len(keys))
		for _, key := range keys {
			types = append(types, pipeline.Type(ctx, key))
		}
		if _, err = pipeline.Exec(ctx); err != nil && !errors.Is(err, redisCli.Nil) {
			return nil, nil, fmt.Errorf("inspect redis key types: %w", err)
		}
		for index, key := range keys {
			typeName, typeErr := types[index].Result()
			if typeErr != nil {
				return nil, nil, fmt.Errorf("inspect redis key type: %w", typeErr)
			}
			if typeName != "hash" {
				blockers = append(blockers, "managed prefix contains a non-HASH key")
				continue
			}
			_, identity, parseError := ParseKnowledgeKey(prefix, key)
			if parseError != "" {
				blockers = append(blockers, "managed HASH key has an invalid identity")
				continue
			}
			counts[identity]++
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return counts, blockers, nil
}

// IndexChecker looks at the index rather than at the keys under it.
//
// The block counts cannot answer this question. An index that was dropped
// leaves every block in place, so every document counts exactly as the record
// says - and retrieval still returns nothing, because there is nothing to
// search. A comparison that only counted keys would report that index as
// consistent.
type IndexChecker interface {
	// CheckIndexes returns the reasons the search index does not agree with
	// the keys it is supposed to index. They are categories, never document
	// text; an empty result means the index is sound.
	CheckIndexes(ctx context.Context) ([]string, error)
}

// knowledgeIndexFamilyName is the managed index family whose keys the
// knowledge record accounts for.
const knowledgeIndexFamilyName = "knowledge"

// KnowledgeIndexChecker reads the knowledge search index through the same
// audit the manual index tooling uses, so the startup report and that tool can
// never disagree about whether an index is sound.
//
// It reads and nothing more: FT._LIST for whether the index exists, FT.INFO
// for what it says about itself. It creates no index and changes no key, and
// it does not requeue a document.
type KnowledgeIndexChecker struct {
	Client *redisCli.Client
}

func (c KnowledgeIndexChecker) CheckIndexes(ctx context.Context) ([]string, error) {
	if c.Client == nil {
		return nil, fmt.Errorf("index audit requires redis")
	}
	family, ok := redisCommon.ManagedSearchIndexFamilyByName(knowledgeIndexFamilyName)
	if !ok {
		return nil, fmt.Errorf("no managed search index family named %q", knowledgeIndexFamilyName)
	}
	audit, err := redisCommon.AuditManagedSearchIndex(ctx, c.Client, family)
	if err != nil {
		return nil, err
	}
	return audit.CurrentProblems, nil
}

// ParseKnowledgeKey reads the document identity out of a managed knowledge
// chunk key, which is "<prefix><user id>:<document id>:<version>:<block id>".
func ParseKnowledgeKey(prefix, key string) (int64, Identity, string) {
	parts := strings.SplitN(strings.TrimPrefix(key, prefix), ":", 4)
	if len(parts) != 4 || parts[1] == "" || parts[3] == "" {
		return 0, Identity{}, "invalid knowledge key"
	}
	userID, err1 := strconv.ParseInt(parts[0], 10, 64)
	version, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || userID <= 0 || version <= 0 {
		return 0, Identity{}, "invalid knowledge key"
	}
	return userID, Identity{UserID: userID, DocumentID: parts[1], Version: version}, ""
}

func sortDocuments(documents []Document) {
	sort.Slice(documents, func(i, j int) bool {
		left, right := documents[i], documents[j]
		if left.UserID != right.UserID {
			return left.UserID < right.UserID
		}
		if left.DocumentID != right.DocumentID {
			return left.DocumentID < right.DocumentID
		}
		return left.Version < right.Version
	})
}
