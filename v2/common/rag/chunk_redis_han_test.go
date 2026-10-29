package rag

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	redisCli "github.com/redis/go-redis/v9"
)

// Lexical matching for Chinese has two paths, and only one of them is covered
// by the existing integration test.
//
// The index runs RediSearch with DefaultLanguage "chinese", so its analyzer
// segments Han it has in its dictionary — 服务发现 tokenizes and the TEXT
// clause finds it. Technical jargon is not in that dictionary: 粘包 does not
// tokenize, the TEXT clause cannot match it, and the untokenized bigram TAG
// field is the only path left. That is what the tag field is for.
//
// So a test that queries a dictionary word proves nothing about the tag path,
// and a personal knowledge base is mostly jargon. This test queries both.
func TestLexicalMatchesBothDictionaryAndJargonHan(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis Stack integration test")
	}
	client := redisCli.NewClient(&redisCli.Options{
		Addr: address, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2,
	})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis Stack: %v", err)
	}

	const dimension = 1024
	repository, err := NewRedisChunkRepository(client, dimension)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	if err := repository.EnsureIndex(ctx); err != nil {
		t.Fatalf("ensure index: %v", err)
	}

	userID := int64(880000 + time.Now().UnixNano()%1000)
	docID := fmt.Sprintf("han-%d", userID)
	body := "服务发现 的实现细节；粘包 是 TCP 流式传输的常见问题，需要在应用层拆包。"
	vector := make([]float32, dimension)
	vector[0] = 1

	chunk := Chunk{
		ID: docID + "-c0", Index: 0, Type: ChunkTypeProse,
		DocID: docID, DocVersion: 1, SourceFile: docID + ".md",
		TitlePath: []string{"网络"}, StartLine: 1, EndLine: 3,
		Content: body, RawContent: body, EmbeddingContent: body,
		ContentHash: docID + "-hash", TokenEstimate: EstimateTokens(body),
		PartIndex: 0, PartTotal: 1,
	}
	if err := repository.Store(ctx, []EmbeddedChunk{{UserID: userID, Chunk: chunk, Vector: vector}}); err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() {
		_ = repository.DeleteDocumentVersion(context.Background(), userID, docID, 1)
	})

	active := map[string]int{docID: 1}
	for _, testCase := range []struct {
		name  string
		query string
		why   string
	}{
		{"dictionary word", "服务发现",
			"the analyzer segments this, so the TEXT clause alone can find it"},
		{"jargon", "粘包",
			"the analyzer has no such token, so only the bigram TAG field can find it"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			deadline := time.Now().Add(5 * time.Second)
			for {
				results, err := repository.SearchActiveByUserText(ctx, userID, testCase.query, 10, active)
				if err != nil {
					t.Fatalf("lexical search %q: %v", testCase.query, err)
				}
				if len(results) > 0 {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("lexical search for %q returned nothing; %s", testCase.query, testCase.why)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}
