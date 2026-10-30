// Package safety keeps a real run from touching data it does not own.
//
// A real run writes chunks into Redis. The product's chunk index lives in
// database 0, and a developer's database 0 may hold their application's data, so
// the run refuses to write into a database unless the database is empty or
// carries a fingerprint key the evaluation itself left there. The fingerprint
// also says whether an existing evaluation index still matches the corpus,
// model and chunking, so an unchanged index is reused instead of paid for twice.
package safety

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"GopherAI/common/rag"
)

// FingerprintKey is the Redis key the evaluation stores its fingerprint under.
const FingerprintKey = "eval:l1:fingerprint"

// ErrForeignDatabase is returned when the database holds data the evaluation did
// not write. No flag overrides it.
var ErrForeignDatabase = errors.New("the Redis database holds data that is not an evaluation index")

// ErrStaleIndex is returned when the database holds an evaluation index built
// from a different corpus, model or chunking, or one whose build did not finish.
// -rebuild replaces it.
var ErrStaleIndex = errors.New("the evaluation index does not match this run")

// Fingerprint is everything an index depends on.
type Fingerprint struct {
	CorpusHash     string
	EmbeddingModel string
	Dimension      int
	Chunker        rag.ChunkerConfig
	ContentVersion string
	EvalUserID     int64
}

// NewFingerprint builds the fingerprint of an index over a corpus.
func NewFingerprint(corpusHash, embeddingModel string, dimension int, chunker rag.ChunkerConfig, userID int64) Fingerprint {
	return Fingerprint{
		CorpusHash: corpusHash, EmbeddingModel: embeddingModel, Dimension: dimension,
		Chunker: chunker, ContentVersion: rag.EmbeddingContentVersion, EvalUserID: userID,
	}
}

// Hash is the fingerprint's stable digest.
func (f Fingerprint) Hash() string {
	text := fmt.Sprintf("corpus=%s\nmodel=%s\ndim=%d\nmaxTokens=%d\noverlap=%d\nmaxChunks=%d\ncontent=%s\nuser=%d",
		f.CorpusHash, f.EmbeddingModel, f.Dimension, f.Chunker.MaxTokens, f.Chunker.OverlapTokens, f.Chunker.MaxChunks,
		f.ContentVersion, f.EvalUserID)
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// KV is the small slice of Redis the guard needs.
type KV interface {
	// DBSize is the number of keys in the database.
	DBSize(ctx context.Context) (int64, error)
	// Get returns a string value; found is false when the key does not exist.
	Get(ctx context.Context, key string) (value string, found bool, err error)
	// Set stores a string value.
	Set(ctx context.Context, key, value string) error
	// DeleteEvalData removes the evaluation's chunks and its fingerprint key,
	// and nothing else.
	DeleteEvalData(ctx context.Context, userID int64) error
}

// Decision is what the guard allows.
type Decision int

const (
	// Build means the database is the evaluation's (or empty): build the index.
	Build Decision = iota
	// Reuse means an index with this exact fingerprint is already there.
	Reuse
)

func (d Decision) String() string {
	if d == Reuse {
		return "reuse"
	}
	return "build"
}

// Check decides what a run may do with the database.
//
//	empty database                         -> Build
//	fingerprint key equal to fp            -> Reuse
//	fingerprint key different              -> ErrStaleIndex, or with rebuild, delete the
//	                                          evaluation's data and Build
//	data but no fingerprint key            -> ErrForeignDatabase, always
func Check(ctx context.Context, kv KV, fp Fingerprint, rebuild bool) (Decision, error) {
	size, err := kv.DBSize(ctx)
	if err != nil {
		return Build, fmt.Errorf("inspect Redis database: %w", err)
	}
	if size == 0 {
		return Build, nil
	}
	stored, found, err := kv.Get(ctx, FingerprintKey)
	if err != nil {
		return Build, fmt.Errorf("read evaluation fingerprint: %w", err)
	}
	if !found {
		return Build, fmt.Errorf("%w: %d keys and no %s key. Use a dedicated, empty Redis database; this run never writes into one it does not own",
			ErrForeignDatabase, size, FingerprintKey)
	}
	if stored == fp.Hash() && !rebuild {
		return Reuse, nil
	}
	if !rebuild {
		if strings.HasPrefix(stored, buildingPrefix) {
			return Build, fmt.Errorf("%w: an earlier build did not finish (fingerprint %s). Pass -rebuild to replace it",
				ErrStaleIndex, short(strings.TrimPrefix(stored, buildingPrefix)))
		}
		return Build, fmt.Errorf("%w: it was built for fingerprint %s, this run needs %s. Pass -rebuild to replace it",
			ErrStaleIndex, short(stored), short(fp.Hash()))
	}
	if err := kv.DeleteEvalData(ctx, fp.EvalUserID); err != nil {
		return Build, fmt.Errorf("remove the old evaluation index: %w", err)
	}
	return Build, nil
}

// buildingPrefix marks a fingerprint whose index build has started but not
// finished.
const buildingPrefix = "building:"

// Begin claims the database before the first chunk is written, so a build that
// fails halfway leaves a database that is recognisably the evaluation's, and not
// one the next run must treat as foreign. The marker is not a valid fingerprint,
// so the next run refuses to reuse the partial index and asks for -rebuild.
func Begin(ctx context.Context, kv KV, fp Fingerprint) error {
	return kv.Set(ctx, FingerprintKey, buildingPrefix+fp.Hash())
}

// Commit records the fingerprint once the index was built successfully.
func Commit(ctx context.Context, kv KV, fp Fingerprint) error {
	return kv.Set(ctx, FingerprintKey, fp.Hash())
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
