package redis

import "testing"

func TestCaptchaCooldownKeyDoesNotCollideWithCaptcha(t *testing.T) {
	codeKey := GenerateCaptcha("user@example.com")
	cooldownKey := GenerateCaptchaCooldown("user@example.com")
	if codeKey == cooldownKey || cooldownKey != codeKey+":cooldown" {
		t.Fatalf("code key=%q cooldown key=%q", codeKey, cooldownKey)
	}
}

func TestKnowledgeChunkKeysShareIndexPrefixAndScopeDocumentVersion(t *testing.T) {
	t.Parallel()

	prefix := GenerateKnowledgeDocumentChunkPrefix(42, "doc-abc", 3)
	wantPrefix := "gopherai:knowledge:chunk:42:doc-abc:3:"
	if prefix != wantPrefix {
		t.Fatalf("document prefix = %q, want %q", prefix, wantPrefix)
	}
	if got := GenerateKnowledgeChunkKey(42, "doc-abc", 3, "chunk_123"); got != wantPrefix+"chunk_123" {
		t.Fatalf("chunk key = %q", got)
	}
	if len(prefix) < len(KnowledgeChunkKeyPrefix) || prefix[:len(KnowledgeChunkKeyPrefix)] != KnowledgeChunkKeyPrefix {
		t.Fatalf("key does not match shared index prefix: %q", prefix)
	}
}

func TestKnowledgeChunkSchemaPinsEmbeddingDimension(t *testing.T) {
	t.Parallel()

	if KnowledgeChunkVectorDimension != 1024 {
		t.Fatalf("schema v1 dimension = %d, want 1024", KnowledgeChunkVectorDimension)
	}
}

func TestEscapeTagValuePreservesUnicodeAndEscapesSyntax(t *testing.T) {
	if got, want := EscapeTagValue("rpc-粘包 v2"), `rpc\-粘包\ v2`; got != want {
		t.Fatalf("EscapeTagValue()=%q want %q", got, want)
	}
}
