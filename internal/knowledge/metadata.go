package knowledge

import "encoding/hex"

// ContentHashLength is the hex length a recorded content hash has to have: the
// v2 column is char(64), a SHA-256 digest.
const ContentHashLength = 64

// ValidDocumentMetadata reports whether a document's byte metadata can be
// trusted. Empty hash and non-positive size are legal - they mean "not
// measured", which is what every row written before ingestion recorded metadata,
// and every test that plants chunks directly, looks like - but a half-written
// hash must not be allowed in, because a truncated digest would silently stop
// matching its own content and a negative size would buy quota space.
func ValidDocumentMetadata(contentHash string, fileSize int64) bool {
	if fileSize < 0 {
		return false
	}
	if contentHash == "" {
		return true
	}
	if len(contentHash) != ContentHashLength {
		return false
	}
	_, err := hex.DecodeString(contentHash)
	return err == nil
}
