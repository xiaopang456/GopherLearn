package redisindex

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// The episodic view's own real-server suite, gated by its own variable: an
// ordinary `go test ./...` runs the offline suites and never dials anything.
// The knowledge index's suite does not stand in for this one - the episode
// schema, its channels and its filters are a different instrument, and a
// schema or filter regression that only shows on RediSearch must fail here,
// not in a knowledge test that happens to pass.
//
// The same throwaway-server rule applies: the suite writes under
// gopherlearn:memory:episode: for user ids it generates for this run and
// resets those users before it leaves.
const EnvEpisodeRedisTestAddress = "GOPHERLEARN_EPISODE_REDIS_TEST_ADDRESS"

func TestEpisodeIndexAgainstRealRedis(t *testing.T) {
	address := strings.TrimSpace(os.Getenv(EnvEpisodeRedisTestAddress))
	if address == "" {
		t.Skipf("%s is unset; the default suite needs no Docker and no Redis server", EnvEpisodeRedisTestAddress)
	}
	const dimensions = 8
	index, err := NewEpisodeIndex(Options{Address: address, Dimensions: dimensions, Database: redisDatabaseForSuite(t)})
	if err != nil {
		t.Fatalf("NewEpisodeIndex: %v", err)
	}
	defer func() { _ = index.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Ensure: create, then tolerate the existing index (a second start), which
	// on the real server also means the field-by-field schema verification ran
	// against what FT.INFO actually reports.
	if err := index.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if err := index.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex twice (a second start must tolerate an existing index): %v", err)
	}

	owner := fmt.Sprintf("user-ep-%s", time.Now().UTC().Format("150405.000000000"))
	neighbour := owner + "-other"
	identity := "episode-embedder-a"
	otherIdentity := "episode-embedder-b"

	embedder := retrieval.NewDeterministicEmbedder(dimensions)
	card := episodeIndexDoc(ctx, embedder, owner, "session-a", "ep-card-a", "active", "用户确定了 JSON 编解码器 的选型 决定", identity)
	other := episodeIndexDoc(ctx, embedder, neighbour, "session-x", "ep-card-b", "active", "完全无关的内容 完全无关的内容", identity)
	for _, document := range []EpisodeDocument{card, other} {
		if err := index.Store(ctx, document); err != nil {
			t.Fatalf("store %s: %v", document.EpisodeID, err)
		}
	}

	// Vector channel: the query that matches card's terms finds it, and the
	// other user's card never crosses the owner filter. The query embedding is
	// the deterministic embedder's own reading of the question, so the card
	// stored with its text's embedding is the true nearest neighbour.
	query, err := embedder.Embed(ctx, []string{"JSON 编解码器 决定"})
	if err != nil || len(query) != 1 {
		t.Fatalf("embed query: %v", err)
	}
	hits, err := index.SearchByVector(ctx, owner, "", identity, query[0], 5)
	if err != nil {
		t.Fatalf("SearchByVector: %v", err)
	}
	if len(hits) != 1 || hits[0].EpisodeID != "ep-card-a" {
		t.Fatalf("vector hits = %+v, want only the owner's card", hits)
	}
	// The neighbour's own query legitimately returns its own card; the leak
	// check is that the owner's card is never among its hits.
	foreign, err := index.SearchByVector(ctx, neighbour, "", identity, query[0], 5)
	if err != nil {
		t.Fatalf("the neighbour's own vector query failed: %v", err)
	}
	for _, hit := range foreign {
		if hit.EpisodeID == "ep-card-a" {
			t.Fatalf("another user's card leaked through the vector channel: %+v", foreign)
		}
	}

	// Session exclusion: the reader's own session is filtered out of the
	// candidate set by the index itself.
	if self, err := index.SearchByVector(ctx, owner, "session-a", identity, query[0], 5); err != nil || len(self) != 0 {
		t.Fatalf("the current session survived the exclusion: %v %+v", err, self)
	}

	// Text and bigram channel: a Han term the Chinese dictionary does not hold
	// must still match through the bigram tag. The caller supplies the term
	// list a real question produces - the whole run for the TEXT clause and its
	// two-character pairs for the TAG clause.
	textHits, err := index.SearchByText(ctx, owner, "", identity, []string{"编解码器", "编解", "解码", "码器"}, 5)
	if err != nil {
		t.Fatalf("SearchByText: %v", err)
	}
	if len(textHits) != 1 || textHits[0].EpisodeID != "ep-card-a" {
		t.Fatalf("text hits = %+v, want the owner's card via the bigram channel", textHits)
	}

	// Exact topics: the normalized key matches; a foreign user's query for the
	// same key matches nothing.
	topics, err := index.SearchExactTopics(ctx, owner, "", identity, []string{"json-编解码器"}, 5)
	if err != nil {
		t.Fatalf("SearchExactTopics: %v", err)
	}
	if len(topics) != 1 || topics[0].EpisodeID != "ep-card-a" {
		t.Fatalf("topic hits = %+v", topics)
	}
	// The neighbour's own card carries the same topic key by construction, so
	// its own query returns it; the isolation check is that the owner's card is
	// never among the neighbour's hits.
	foreignTopics, err := index.SearchExactTopics(ctx, neighbour, "", identity, []string{"json-编解码器"}, 5)
	if err != nil {
		t.Fatalf("the neighbour's own topic query failed: %v", err)
	}
	for _, hit := range foreignTopics {
		if hit.EpisodeID == "ep-card-a" {
			t.Fatalf("topic search crossed owners: %+v", foreignTopics)
		}
	}

	// Identity isolation: a card embedded by a different embedder is invisible
	// to a query that filters on this identity, which is how a model change
	// becomes an explicit re-index instead of a silent accuracy drop.
	if err := index.Store(ctx, episodeIndexDoc(ctx, embedder, owner, "session-b", "ep-card-c", "active", "另一个嵌入器的卡 另一个嵌入器的卡", otherIdentity)); err != nil {
		t.Fatal(err)
	}
	if mismatched, err := index.SearchByVector(ctx, owner, "", identity, query[0], 5); err != nil || len(mismatched) != 1 {
		t.Fatalf("a foreign-identity card leaked into the results: %v %+v", err, mismatched)
	}
	if byIdentity, err := index.SearchByVector(ctx, owner, "", otherIdentity, query[0], 5); err != nil || len(byIdentity) != 1 {
		t.Fatalf("the foreign-identity card was not reachable under its own identity: %v %+v", err, byIdentity)
	}

	// Version update: re-storing the same episode rewrites the view in place -
	// the new content is searchable and the old attributes did not survive
	// beside it.
	updated := episodeIndexDoc(ctx, embedder, owner, "session-a", "ep-card-a", "active", "更新后的完全不同主题 流式重排器 更新后的完全不同主题 流式重排器", identity)
	updated.TopicKeys = []string{"流式重排器"}
	if err := index.Store(ctx, updated); err != nil {
		t.Fatalf("re-store: %v", err)
	}
	stale, err := index.SearchByText(ctx, owner, "", identity, []string{"编解码器", "编解", "解码", "码器"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("the previous version's text survived the update: %+v", stale)
	}
	fresh, err := index.SearchExactTopics(ctx, owner, "", identity, []string{"流式重排器"}, 5)
	if err != nil || len(fresh) != 1 || fresh[0].EpisodeID != "ep-card-a" {
		t.Fatalf("the updated version is not searchable: %v %+v", err, fresh)
	}

	// Delete: a removed card leaves every channel, and a count of zero means
	// the view and the store can be compared honestly.
	if err := index.Delete(ctx, owner, []string{"ep-card-a", "ep-card-c"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if gone, err := index.SearchByText(ctx, owner, "", identity, []string{"重排器"}, 5); err != nil || len(gone) != 0 {
		t.Fatalf("a deleted card survived search: %v %+v", err, gone)
	}
	count, err := index.CountUser(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("count after delete = %d, want 0", count)
	}

	// Reset: whatever a failed delete left behind is swept by the user reset,
	// which is the repair path a reindex uses.
	if err := index.Store(ctx, episodeIndexDoc(ctx, embedder, owner, "session-c", "ep-card-d", "active", "重建素材 重建素材", identity)); err != nil {
		t.Fatal(err)
	}
	if err := index.ResetUser(ctx, owner); err != nil {
		t.Fatalf("ResetUser: %v", err)
	}
	if count, err := index.CountUser(ctx, owner); err != nil || count != 0 {
		t.Fatalf("count after reset = %d (%v), want 0", count, err)
	}
	_ = neighbour
}

// episodeIndexDoc builds one view row whose vector is what the deterministic
// embedder makes of the card's own text, so the nearest neighbour of a query
// embedding is known before the server answers instead of assumed.
func episodeIndexDoc(ctx context.Context, embedder retrieval.Embedder, userID, sessionID, episodeID, status, text, identity string) EpisodeDocument {
	vectors, err := embedder.Embed(ctx, []string{text})
	if err != nil || len(vectors) != 1 {
		panic(fmt.Sprintf("episode index test embed: %v", err))
	}
	return EpisodeDocument{
		EpisodeID:     episodeID,
		UserID:        userID,
		SessionID:     sessionID,
		Status:        status,
		LastMessageAt: time.Now().UTC().Unix(),
		Title:         "情景卡",
		TopicKeys:     []string{"json-编解码器"},
		SearchText:    text,
		Identity:      identity,
		Vector:        vectors[0],
	}
}
