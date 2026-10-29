package rag

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLocalRerankerProductionProtocolIntegration(t *testing.T) {
	if os.Getenv("GOPHERAI_RERANK_INTEGRATION") != "1" {
		t.Skip("set GOPHERAI_RERANK_INTEGRATION=1 for the local CUDA service")
	}
	token := os.Getenv("RAG_RERANK_AUTH_TOKEN")
	ranker, err := NewLocalReranker("http://127.0.0.1:18081", token, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ranker.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	searcher := &retrievalFakeSearcher{results: []RedisChunkSearchResult{
		{RedisKey: "irrelevant", ChunkID: "irrelevant", DocID: "doc", DocVersion: 1, RawContent: "番茄炒蛋需要鸡蛋和番茄。", Distance: 0.1},
		{RedisKey: "framing", ChunkID: "framing", DocID: "doc", DocVersion: 1, RawContent: "TCP是字节流协议，没有消息边界。应用层可以使用长度字段处理粘包和半包。", Distance: 0.2},
		{RedisKey: "buffer", ChunkID: "buffer", DocID: "doc", DocVersion: 1, RawContent: "PacketBuffer先缓存字节，读取固定头部中的长度，数据完整后再提取一帧。", Distance: 0.3},
	}}
	service, err := NewRAGService(context.Background(), &retrievalFakeEmbedder{dimension: 4}, searcher, func(context.Context, int64) ([]ActiveDocument, error) {
		return []ActiveDocument{{DocID: "doc", Version: 1, OriginalName: "rpc.md"}}, nil
	}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.UseReranker(context.Background(), ranker); err != nil {
		t.Fatal(err)
	}
	results, err := service.Retrieve(context.Background(), 73, "TCP粘包的本质和解决方法是什么？", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].RerankApplied || !results[1].RerankApplied || results[0].ChunkID == "irrelevant" || results[1].ChunkID == "irrelevant" {
		t.Fatalf("unexpected local rerank result: %+v", results)
	}
}
