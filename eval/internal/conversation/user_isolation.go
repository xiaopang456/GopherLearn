package conversation

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	rediscommon "GopherAI/common/redis"
	redis "github.com/redis/go-redis/v9"
)

// indexKeyLister enumerates the keys of one derived index family. The Redis
// client implements it; a test supplies a fixed listing so the attribution
// rules below are decided without a server.
type indexKeyLister interface {
	ListKeys(ctx context.Context, pattern string) ([]string, error)
}

type redisKeyLister struct {
	client *redis.Client
}

func (l redisKeyLister) ListKeys(ctx context.Context, pattern string) ([]string, error) {
	var cursor uint64
	keys := make([]string, 0, 256)
	for {
		batch, next, err := l.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", pattern, err)
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			return keys, nil
		}
	}
}

// MemoryIndexState is one reading of the derived memory indexes for one
// evaluation user.
//
// The foreign counts matter as much as the user's own: the indexes are shared
// between scenarios, so a reading that only reports the evaluation user cannot
// tell "this run wrote nothing" from "this run wrote into someone else's
// partition".
type MemoryIndexState struct {
	Scanned               bool    `json:"scanned"`
	UserID                int64   `json:"user_id"`
	EpisodeEntries        int     `json:"episode_entries"`
	MemoryEntries         int     `json:"memory_entries"`
	CardEntries           int     `json:"card_entries"`
	ForeignEpisodeEntries int     `json:"foreign_episode_entries"`
	ForeignMemoryEntries  int     `json:"foreign_memory_entries"`
	ForeignCardEntries    int     `json:"foreign_card_entries"`
	ForeignUserIDs        []int64 `json:"foreign_user_ids"`
	UnattributedEntries   int     `json:"unattributed_entries"`
	// CardPrefix is the namespace the card arm's rows live in, when the run
	// asked for one. It is reported so that a reader can see which partition
	// was counted.
	CardPrefix string `json:"card_prefix,omitempty"`
	Error      string `json:"error,omitempty"`
}

// MemoryIndexEvidence brackets one answer attempt with the two readings the
// report must show: the state the attempt started from and the state it left
// behind after the background window closed.
type MemoryIndexEvidence struct {
	BeforeAnswer MemoryIndexState `json:"before_answer"`
	AfterAnswer  MemoryIndexState `json:"after_answer"`
}

// indexKeyUser reports the user a derived-index key belongs to. Keys are
// "<prefix><user id>:<entry id>", so an entry id that is not a number, or a key
// without the separator, is not attributed to anybody rather than guessed at.
func indexKeyUser(prefix, key string) (int64, bool) {
	if !strings.HasPrefix(key, prefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(key, prefix)
	head, tail, found := strings.Cut(rest, ":")
	if !found || tail == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(head, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (s *MemoryIndexState) count(prefix string, keys []string, userID int64) {
	for _, key := range keys {
		owner, attributed := indexKeyUser(prefix, key)
		switch {
		case !attributed:
			s.UnattributedEntries++
		case owner == userID:
			switch prefix {
			case rediscommon.EpisodeKeyPrefix:
				s.EpisodeEntries++
			case rediscommon.MemoryKeyPrefix:
				s.MemoryEntries++
			default:
				s.CardEntries++
			}
		default:
			switch prefix {
			case rediscommon.EpisodeKeyPrefix:
				s.ForeignEpisodeEntries++
			case rediscommon.MemoryKeyPrefix:
				s.ForeignMemoryEntries++
			default:
				s.ForeignCardEntries++
			}
			s.ForeignUserIDs = append(s.ForeignUserIDs, owner)
		}
	}
}

// readMemoryIndexState lists the derived index families and attributes every
// key to a user. It never deletes or mutates anything: the report is evidence,
// not a cleanup step.
//
// The card arm's namespace is counted as well. It has its own key prefix, and
// a gate that did not look at it would not see the rows a repeat of the same
// scenario left behind: the evaluation identity does not carry the repeat
// number, so a second repeat of the same item would find the first one's
// cards already in the index.
func readMemoryIndexState(ctx context.Context, lister indexKeyLister, userID int64, cardPrefix string) (MemoryIndexState, error) {
	state := MemoryIndexState{UserID: userID, ForeignUserIDs: []int64{}, CardPrefix: cardPrefix}
	if userID <= 0 {
		return state, fmt.Errorf("memory index reading requires an evaluation user")
	}
	episodes, err := lister.ListKeys(ctx, rediscommon.EpisodeKeyPrefix+"*")
	if err != nil {
		return state, err
	}
	memories, err := lister.ListKeys(ctx, rediscommon.MemoryKeyPrefix+"*")
	if err != nil {
		return state, err
	}
	state.count(rediscommon.EpisodeKeyPrefix, episodes, userID)
	state.count(rediscommon.MemoryKeyPrefix, memories, userID)
	if cardPrefix != "" {
		cards, err := lister.ListKeys(ctx, cardPrefix+"*")
		if err != nil {
			return state, err
		}
		state.count(cardPrefix, cards, userID)
	}
	sort.Slice(state.ForeignUserIDs, func(i, j int) bool { return state.ForeignUserIDs[i] < state.ForeignUserIDs[j] })
	state.ForeignUserIDs = uniqueUserIDs(state.ForeignUserIDs)
	state.Scanned = true
	return state, nil
}

func uniqueUserIDs(ids []int64) []int64 {
	kept := make([]int64, 0, len(ids))
	for index, id := range ids {
		if index == 0 || id != ids[index-1] {
			kept = append(kept, id)
		}
	}
	return kept
}

// observeMemoryIndexes opens one short-lived client against the shared
// evaluation index and reads the state for the evaluation user.
func observeMemoryIndexes(ctx context.Context, env RealEnvironment) (MemoryIndexState, error) {
	client := redis.NewClient(&redis.Options{Addr: env.RedisAddress, Password: env.RedisPassword, Protocol: 2})
	defer client.Close()
	return readMemoryIndexState(ctx, redisKeyLister{client: client}, env.UserID, env.cardIndexPrefix())
}

// Reject stale derived views instead of deleting data or letting one repeat
// alter the initial memory state of the next repeat. Only the evaluation user's
// own entries decide this gate: other scenarios legitimately leave entries in
// the same shared index.
func checkEmptyUserIndexes(ctx context.Context, lister indexKeyLister, userID int64, cardPrefix string) (MemoryIndexState, error) {
	state, err := readMemoryIndexState(ctx, lister, userID, cardPrefix)
	if err != nil {
		return state, err
	}
	if state.EpisodeEntries != 0 || state.MemoryEntries != 0 || state.CardEntries != 0 {
		return state, fmt.Errorf("evaluation user has nonempty derived memory indexes: episodes=%d memories=%d cards=%d",
			state.EpisodeEntries, state.MemoryEntries, state.CardEntries)
	}
	return state, nil
}

func requireEmptyUserIndexes(ctx context.Context, env RealEnvironment) (MemoryIndexState, error) {
	client := redis.NewClient(&redis.Options{Addr: env.RedisAddress, Password: env.RedisPassword, Protocol: 2})
	defer client.Close()
	return checkEmptyUserIndexes(ctx, redisKeyLister{client: client}, env.UserID, env.cardIndexPrefix())
}

// cardIndexPrefix is the key prefix of the card arm's own rows, when the run
// asked for the card path. The old arm has none: its rows live in the
// product's Episode namespace.
func (e RealEnvironment) cardIndexPrefix() string {
	if strings.TrimSpace(e.SessionCardMode) != CardArmModeOn {
		return ""
	}
	return strings.TrimSpace(e.SessionCardIndexKeyPrefix)
}
