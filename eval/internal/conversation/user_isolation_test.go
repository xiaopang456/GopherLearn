package conversation

import (
	rediscommon "GopherAI/common/redis"
	"context"
	"errors"
	"fmt"
	redis "github.com/redis/go-redis/v9"
	"os"
	"strconv"
	"testing"
	"time"
)

// fakeIndexKeys answers the two family listings the reading performs, and
// records the patterns it was asked for so a test can prove the reading looked
// at both indexes instead of returning a value that no listing produced.
type fakeIndexKeys struct {
	listed   []string
	episodes []string
	memories []string
	err      error
}

func (f *fakeIndexKeys) ListKeys(_ context.Context, pattern string) ([]string, error) {
	f.listed = append(f.listed, pattern)
	if f.err != nil {
		return nil, f.err
	}
	switch pattern {
	case rediscommon.EpisodeKeyPrefix + "*":
		return f.episodes, nil
	case rediscommon.MemoryKeyPrefix + "*":
		return f.memories, nil
	}
	return nil, fmt.Errorf("unexpected pattern %s", pattern)
}

func TestIndexKeyUser(t *testing.T) {
	cases := []struct {
		key    string
		want   int64
		owned  bool
		reason string
	}{
		{rediscommon.EpisodeKeyPrefix + "41:episode-one", 41, true, "ordinary episode key"},
		{rediscommon.MemoryKeyPrefix + "7:memory-one", 7, true, "ordinary memory key"},
		{rediscommon.EpisodeKeyPrefix + "41:", 0, false, "empty entry id"},
		{rediscommon.EpisodeKeyPrefix + ":41", 0, false, "missing user id"},
		{rediscommon.EpisodeKeyPrefix + "abc:one", 0, false, "non numeric user id"},
		{rediscommon.EpisodeKeyPrefix + "-3:one", 0, false, "negative user id"},
		{rediscommon.EpisodeKeyPrefix + "41", 0, false, "no entry separator"},
		{"gopherai:other:vec:41:one", 0, false, "key outside the family"},
	}
	for _, c := range cases {
		t.Run(c.reason, func(t *testing.T) {
			prefix := rediscommon.MemoryKeyPrefix
			if len(c.key) > len(rediscommon.EpisodeKeyPrefix) && c.key[:len(rediscommon.EpisodeKeyPrefix)] == rediscommon.EpisodeKeyPrefix {
				prefix = rediscommon.EpisodeKeyPrefix
			}
			got, owned := indexKeyUser(prefix, c.key)
			if got != c.want || owned != c.owned {
				t.Fatalf("indexKeyUser(%q) = %d,%v want %d,%v", c.key, got, owned, c.want, c.owned)
			}
		})
	}
}

func TestMemoryIndexStateNeverCreditsAnotherUser(t *testing.T) {
	const me, other, third = int64(1001), int64(2002), int64(3003)
	keys := func(prefix string, user int64, n int) []string {
		out := make([]string, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, fmt.Sprintf("%s%d:entry-%d", prefix, user, i))
		}
		return out
	}
	lister := &fakeIndexKeys{
		episodes: append(append(append(keys(rediscommon.EpisodeKeyPrefix, me, 2),
			keys(rediscommon.EpisodeKeyPrefix, other, 3)...),
			rediscommon.EpisodeKeyPrefix+"broken"),
			rediscommon.EpisodeKeyPrefix+strconv.FormatInt(third, 10)+":one"),
		memories: append(append(keys(rediscommon.MemoryKeyPrefix, me, 1),
			keys(rediscommon.MemoryKeyPrefix, third, 2)...),
			rediscommon.MemoryKeyPrefix+"broken"),
	}
	state, err := readMemoryIndexState(context.Background(), lister, me)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Scanned || state.UserID != me {
		t.Fatalf("state = %+v", state)
	}
	if state.EpisodeEntries != 2 || state.MemoryEntries != 1 {
		t.Fatalf("own entries = %d/%d want 2/1", state.EpisodeEntries, state.MemoryEntries)
	}
	if state.ForeignEpisodeEntries != 4 || state.ForeignMemoryEntries != 2 {
		t.Fatalf("foreign entries = %d/%d want 4/2", state.ForeignEpisodeEntries, state.ForeignMemoryEntries)
	}
	if len(state.ForeignUserIDs) != 2 || state.ForeignUserIDs[0] != other || state.ForeignUserIDs[1] != third {
		t.Fatalf("foreign users = %v want [%d %d] without duplicates and sorted", state.ForeignUserIDs, other, third)
	}
	if state.UnattributedEntries != 2 {
		t.Fatalf("unattributed = %d want 2", state.UnattributedEntries)
	}
	want := []string{rediscommon.EpisodeKeyPrefix + "*", rediscommon.MemoryKeyPrefix + "*"}
	if len(lister.listed) != len(want) {
		t.Fatalf("listed %v want both index families", lister.listed)
	}
	for i, pattern := range want {
		if lister.listed[i] != pattern {
			t.Fatalf("listed %v want %v", lister.listed, want)
		}
	}
}

func TestMemoryIndexStateFailureIsNotSilentlyClean(t *testing.T) {
	boom := errors.New("scan failed")
	state, err := readMemoryIndexState(context.Background(), &fakeIndexKeys{err: boom}, 7)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v want the scan failure", err)
	}
	if state.Scanned || state.EpisodeEntries != 0 {
		t.Fatalf("a failed read reported entries: %+v", state)
	}
}

func TestMemoryIndexStateRejectsMissingEvaluationUser(t *testing.T) {
	for _, id := range []int64{0, -1} {
		if _, err := readMemoryIndexState(context.Background(), &fakeIndexKeys{}, id); err == nil {
			t.Fatalf("user %d read without an identity error", id)
		}
	}
}

func TestGateKeepsAnotherUsersEntriesOutOfTheDecision(t *testing.T) {
	const me, other = int64(1001), int64(2002)
	lister := &fakeIndexKeys{episodes: []string{rediscommon.EpisodeKeyPrefix + strconv.FormatInt(other, 10) + ":one"}}
	if _, err := checkEmptyUserIndexes(context.Background(), lister, me); err != nil {
		t.Fatalf("foreign entries refused the gate: %v", err)
	}
	lister.episodes = append(lister.episodes, rediscommon.EpisodeKeyPrefix+strconv.FormatInt(me, 10)+":mine")
	state, err := checkEmptyUserIndexes(context.Background(), lister, me)
	if err == nil {
		t.Fatal("the evaluation user's own entry was accepted")
	}
	if state.EpisodeEntries != 1 || state.ForeignEpisodeEntries != 1 {
		t.Fatalf("state = %+v", state)
	}
}

func TestEmptyUserIndexesIntegration(t *testing.T) {
	addr := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("requires disposable Redis")
	}
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	defer client.Close()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	lister := redisKeyLister{client: client}
	base := time.Now().UnixNano() & 0x3fffffffffffffff
	env := RealEnvironment{RedisAddress: addr, RedisPassword: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), UserID: base}
	mine := []string{rediscommon.EpisodeKeyPrefix + strconv.FormatInt(base, 10) + ":test", rediscommon.GenerateMemoryVectorKey(base, "test")}
	foreignKey := rediscommon.GenerateMemoryVectorKey(base+1, "test")
	for _, key := range append(mine, foreignKey) {
		if err := client.HSet(ctx, key, "test", "value").Err(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = client.Del(ctx, append(mine, foreignKey)...) })

	// Another scenario's user is present in the same shared index; only this
	// user's own entries may refuse the attempt.
	if _, err := checkEmptyUserIndexes(ctx, lister, base+1); err == nil {
		t.Fatal("a user with entries passed the gate")
	}
	// The evaluation user is dirty here, so the gate must refuse even though
	// the foreign key alone would not.
	if _, err := requireEmptyUserIndexes(ctx, env); err == nil {
		t.Fatal("dirty user accepted")
	}
	state, err := readMemoryIndexState(ctx, lister, base)
	if err != nil {
		t.Fatal(err)
	}
	if state.EpisodeEntries != 1 || state.MemoryEntries != 1 {
		t.Fatalf("own entries = %d/%d want 1/1", state.EpisodeEntries, state.MemoryEntries)
	}
	if state.MemoryEntries == 0 && state.EpisodeEntries == 0 {
		t.Fatal("the shared index read found nothing")
	}
	found := false
	for _, id := range state.ForeignUserIDs {
		if id == base+1 {
			found = true
		}
	}
	if !found || state.ForeignMemoryEntries < 1 {
		t.Fatalf("foreign user not reported: %+v", state)
	}
	for _, key := range mine {
		if err = client.Del(ctx, key).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := requireEmptyUserIndexes(ctx, env); err != nil {
		t.Fatalf("a clean user with foreign entries was refused: %v", err)
	}
}
