package summarysegment

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	redisCommon "GopherAI/common/redis"
	"GopherAI/model"

	redisCli "github.com/redis/go-redis/v9"
)

// The segment index against a real Redis.
//
// The index is the only way the server finds an older segment, so the two
// things it has to get right are written here: the row is findable by the
// session it belongs to, and it is not findable from another session.

// segmentForIndex is one segment to put in the index.
func segmentForIndex(userID int64, sessionID string, seq int, topic string) model.SessionSummarySegment {
	content, _ := model.MarshalSessionSummarySegmentContent(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1, Topic: topic,
		Points: []model.SessionSummarySegmentPoint{{Text: topic + "的要点", EvidenceMessageIDs: []uint{1}}},
	})
	return model.SessionSummarySegment{
		UserID: userID, SessionID: sessionID, Seq: seq, StartMessageID: 1, EndMessageID: 30,
		Topic: topic, Content: content, EndAt: time.Now(),
	}
}

func TestRedisSummarySegmentIndexIntegration(t *testing.T) {
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run Redis Stack integration test")
	}
	client := redisCli.NewClient(&redisCli.Options{Addr: address, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	index, err := NewRedisSummarySegmentIndex(client, redisCommon.SummarySegmentVectorDimension)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.EnsureIndex(ctx); err != nil {
		t.Fatal(err)
	}
	userID := time.Now().UnixNano() & 0x3fffffffffffffff
	sessionID := "session-" + strconv.FormatInt(userID, 10)
	otherSession := "other-" + strconv.FormatInt(userID, 10)
	vector := make([]float32, redisCommon.SummarySegmentVectorDimension)
	vector[0] = 1
	for _, segment := range []model.SessionSummarySegment{
		segmentForIndex(userID, sessionID, 1, "拆包"),
		segmentForIndex(userID, otherSession, 7, "别的会话"),
	} {
		if err = index.Store(ctx, segment, vector); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = index.ResetUser(context.Background(), userID)
	})

	hits, err := index.SearchByVector(ctx, userID, sessionID, vector, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Seq != 1 {
		t.Fatalf("the session's own search found %v", hits)
	}

	// Another session's segment is not in this session's search.
	other, err := index.SearchByVector(ctx, userID, otherSession, vector, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Seq != 7 {
		t.Fatalf("the other session's search found %v", other)
	}

	// The recent-use record remembers what the turn reached for.
	if err = index.RecordRecentUse(ctx, userID, sessionID, 1); err != nil {
		t.Fatal(err)
	}
	if err = index.RecordRecentUse(ctx, userID, sessionID, 2); err != nil {
		t.Fatal(err)
	}
	used, err := index.ListRecentUse(ctx, userID, sessionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 2 || used[0] != 2 || used[1] != 1 {
		t.Fatalf("the recent use record holds %v, want the newest first", used)
	}

	// The counts are what the repair compares.
	stored, err := index.CountUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("the index holds %d rows, want 2", stored)
	}
	all, err := index.CountAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all < stored {
		t.Fatalf("the whole index holds %d rows, want at least %d", all, stored)
	}
}
