package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	// EmbeddingVectorCacheProtocol is bumped whenever the meaning of the two
	// stored reuse fields changes. An older protocol simply stops matching, so
	// old vectors expire on their own instead of being migrated.
	EmbeddingVectorCacheProtocol = "knowledge-chunk-vector-cache-v1"

	// EmbeddingContentVersion must be raised together with any change to
	// buildEmbeddingContent. The stored bytes would otherwise still compare
	// equal for a chunk whose real embedder input has changed.
	EmbeddingContentVersion = "knowledge-embedding-content-v1"
)

// EmbeddingInputHash identifies the exact bytes handed to the embedder. It is
// deliberately not ContentHash: ContentHash only covers RawContent, while the
// embedder also sees the file name, title path and symbols.
func EmbeddingInputHash(embeddingContent string) string {
	sum := sha256.Sum256([]byte(embeddingContent))
	return hex.EncodeToString(sum[:])
}

// EmbeddingFingerprint identifies the function that produced a vector. Base URL
// and API key are intentionally excluded: they are credentials, and rotating an
// endpoint does not change the numbers a model returns.
func EmbeddingFingerprint(model string, dimension int, contentVersion string) string {
	payload := strings.Join([]string{
		EmbeddingVectorCacheProtocol, model, strconv.Itoa(dimension), contentVersion,
	}, "\x1e")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}
