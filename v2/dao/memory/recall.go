package memory

import (
	"GopherAI/model"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type RecallQuery struct {
	UserID        int64
	Scopes        []model.MemoryScope
	Tags          []string
	TopicKey      string
	MinConfidence float64
	Limit         int
}

func (q RecallQuery) Validate() error {
	if q.UserID <= 0 || len(q.Scopes) == 0 || len(q.Scopes) > 2 || q.MinConfidence < 0 || q.MinConfidence > 1 || q.Limit < 1 || q.Limit > 100 {
		return fmt.Errorf("invalid memory recall query")
	}
	if len(q.Tags) == 0 && strings.TrimSpace(q.TopicKey) == "" {
		return fmt.Errorf("memory recall requires tags or topic")
	}
	if len(q.Tags) > 12 || len([]rune(q.TopicKey)) > 160 {
		return fmt.Errorf("memory recall tags or topic exceed limit")
	}
	for _, tag := range q.Tags {
		if !model.ValidMemoryTag(tag) {
			return fmt.Errorf("invalid memory recall tag")
		}
	}
	for _, scope := range q.Scopes {
		if !scope.Valid() {
			return fmt.Errorf("invalid memory recall scope")
		}
	}
	return nil
}

// Recall performs the security and relevance hard filters in MySQL. The
// service layer applies explainable fine ranking to this bounded candidate set.
func (r *Repository) Recall(ctx context.Context, query RecallQuery) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repository is not initialized")
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	db := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND status = ? AND confidence >= ? AND memory_scope IN ?", query.UserID, model.LongTermMemoryActive, query.MinConfidence, query.Scopes).
		Where("(valid_until IS NULL OR valid_until > ?)", time.Now())
	tagsJSON, err := json.Marshal(query.Tags)
	if err != nil {
		return nil, fmt.Errorf("encode memory recall tags: %w", err)
	}
	if strings.TrimSpace(query.TopicKey) == "" {
		db = db.Where("JSON_OVERLAPS(tags, CAST(? AS JSON))", string(tagsJSON))
	} else if len(query.Tags) == 0 {
		db = db.Where("topic_key = ?", query.TopicKey)
	} else {
		db = db.Where("(JSON_OVERLAPS(tags, CAST(? AS JSON)) OR topic_key = ?)", string(tagsJSON), query.TopicKey)
	}
	// BUG-AGENT-025: the candidate cut used to be ordered by confidence, which
	// the model assigns and which clusters tightly, so at a few hundred rows it
	// behaved like a random truncation — a genuinely relevant memory could be
	// dropped here and the Go ranker would never see it, capping recall no
	// matter how good the ranking was.
	//
	// The cut is now made on the same signals the service ranks by: an exact
	// topic hit, then the number of overlapping tags. Business ordering stays
	// as the tie-break.
	var memories []model.LongTermMemory
	if err := orderByRelevance(db, query).Limit(query.Limit).Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

func orderByRelevance(db *gorm.DB, query RecallQuery) *gorm.DB {
	if strings.TrimSpace(query.TopicKey) != "" {
		db = db.Order(gorm.Expr("(CASE WHEN topic_key = ? THEN 1 ELSE 0 END) DESC", query.TopicKey))
	}
	if len(query.Tags) > 0 {
		terms := make([]string, 0, len(query.Tags))
		vars := make([]any, 0, len(query.Tags))
		for _, tag := range query.Tags {
			encoded, err := json.Marshal(tag)
			if err != nil {
				continue
			}
			terms = append(terms, "JSON_CONTAINS(tags, ?)")
			vars = append(vars, string(encoded))
		}
		if len(terms) > 0 {
			db = db.Order(gorm.Expr("("+strings.Join(terms, " + ")+") DESC", vars...))
		}
	}
	return db.Order("user_confirmed DESC, confidence DESC, importance DESC, last_used_at DESC, updated_at DESC, id ASC")
}

// ListAllActive returns every active memory of one type, with no relevance
// filter at all.
//
// AGENT-018 uses it for `profile`, where retrieval is the wrong frame: "explain
// the overall flow before the code" applies to every technical question, yet
// its similarity to any particular one is near zero, so tag, BM25 and vector
// search all miss it by construction. There are few of these, so they are
// simply all injected.
//
// The ordering is the eviction order when the cap is reached: user-confirmed
// first, then stable before time_bound, then most recently updated. A stable
// fact the user confirmed themselves is therefore the last thing to be dropped.
func (r *Repository) ListAllActive(ctx context.Context, userID int64, scope model.MemoryScope, minConfidence float64, limit int) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repository is not initialized")
	}
	if userID <= 0 || !scope.Valid() || minConfidence < 0 || minConfidence > 1 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid active memory listing")
	}
	var memories []model.LongTermMemory
	err := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND status = ? AND memory_scope = ? AND confidence >= ?",
			userID, model.LongTermMemoryActive, scope, minConfidence).
		Where("(valid_until IS NULL OR valid_until > ?)", time.Now()).
		Order("user_confirmed DESC, durability ASC, importance DESC, updated_at DESC, id ASC").
		Limit(limit).
		Find(&memories).Error
	return memories, err
}

func (r *Repository) MarkUsed(ctx context.Context, userID int64, memoryIDs []string, now time.Time) error {
	if r == nil || r.db == nil || userID <= 0 || len(memoryIDs) == 0 || len(memoryIDs) > 20 || now.IsZero() {
		return fmt.Errorf("invalid memory usage update")
	}
	result := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND status = ? AND id IN ?", userID, model.LongTermMemoryActive, memoryIDs).
		Where("(valid_until IS NULL OR valid_until > ?)", now).
		Updates(map[string]any{"last_used_at": now, "usage_count": gorm.Expr("usage_count + 1")})
	return result.Error
}

// LoadByIDs reloads full rows for the identifiers a similarity search returned.
//
// AGENT-020 keeps content out of the Redis index on purpose, so this is the
// only path by which retrieved text reaches the caller. Reading from MySQL also
// means a correction applied after indexing is visible immediately: the index
// may be stale in what it ranks, never in what it shows.
func (r *Repository) LoadByIDs(ctx context.Context, userID int64, memoryIDs []string) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repository is not initialized")
	}
	if userID <= 0 || len(memoryIDs) == 0 || len(memoryIDs) > 100 {
		return nil, fmt.Errorf("invalid memory identifier lookup")
	}
	var memories []model.LongTermMemory
	err := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND status = ? AND memory_scope IN ? AND id IN ?", userID, model.LongTermMemoryActive, []model.MemoryScope{model.MemoryScopeGlobal, model.MemoryScopeTopic}, memoryIDs).
		Where("(valid_until IS NULL OR valid_until > ?)", time.Now()).
		Find(&memories).Error
	return memories, err
}

// ListTopicKeys reports the topics that carry notes, with how many each holds.
//
// AGENT-021 needs this to build the shared topic catalogue: a topic the learner
// has written notes about but never been quizzed on still has to be offerable,
// otherwise the memory side keeps minting keys the learning side has never seen
// and the two namespaces drift apart again.
func (r *Repository) ListTopicKeys(ctx context.Context, userID int64, limit int) ([]TopicKeyRow, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repository is not initialized")
	}
	if userID <= 0 || limit < 1 || limit > 200 {
		return nil, fmt.Errorf("invalid topic key listing")
	}
	var rows []TopicKeyRow
	err := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Select("topic_key, MIN(subject) AS topic, COUNT(*) AS notes").
		Where("user_id = ? AND status = ? AND memory_scope = ? AND topic_key <> ''",
			userID, model.LongTermMemoryActive, model.MemoryScopeTopic).
		Where("(valid_until IS NULL OR valid_until > ?)", time.Now()).
		Group("topic_key").
		Order("notes DESC, topic_key ASC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}

type TopicKeyRow struct {
	TopicKey string `gorm:"column:topic_key"`
	Topic    string `gorm:"column:topic"`
	Notes    int    `gorm:"column:notes"`
}

// ListByTopicKey returns every active memory filed under one topic.
//
// AGENT-021 made `topic_key` the retrieval unit, so this replaces ranking for
// the topic-note path: with a topic fixed there is nothing to rank. The limit
// is a database guard against a pathological topic, not a context budget — the
// caller trims to fit.
func (r *Repository) ListByTopicKey(ctx context.Context, userID int64, scope model.MemoryScope, topicKey string, minConfidence float64, limit int) ([]model.LongTermMemory, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("memory repository is not initialized")
	}
	if userID <= 0 || scope != model.MemoryScopeTopic || strings.TrimSpace(topicKey) == "" || minConfidence < 0 || minConfidence > 1 || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid memory listing for topic")
	}
	var memories []model.LongTermMemory
	err := r.db.WithContext(ctx).Model(&model.LongTermMemory{}).
		Where("user_id = ? AND status = ? AND memory_scope = ? AND topic_key = ? AND confidence >= ?",
			userID, model.LongTermMemoryActive, scope, topicKey, minConfidence).
		Where("(valid_until IS NULL OR valid_until > ?)", time.Now()).
		Order("user_confirmed DESC, durability ASC, importance DESC, updated_at DESC, id ASC").
		Limit(limit).
		Find(&memories).Error
	return memories, err
}

// TopicMemoryWindow is a bounded ordered view plus the number of eligible rows
// before the bound was applied. Both values are read from one repeatable-read
// transaction and use the same caller-supplied instant for expiry.
type TopicMemoryWindow struct {
	Memories      []model.LongTermMemory
	EligibleCount int64
}

// ListTopicWindow reads exact topic keys without semantic widening. The count
// and rows deliberately share one snapshot: two unrelated Count/List calls can
// disagree when a memory expires or is updated between them, which would turn
// the tool's completeness flag into a guess.
func (r *Repository) ListTopicWindow(ctx context.Context, userID int64, scope model.MemoryScope,
	topicKeys []string, minConfidence float64, now time.Time, limit int) (TopicMemoryWindow, error) {
	if r == nil || r.db == nil {
		return TopicMemoryWindow{}, fmt.Errorf("memory repository is not initialized")
	}
	if userID <= 0 || scope != model.MemoryScopeTopic || len(topicKeys) == 0 || len(topicKeys) > 16 ||
		minConfidence < 0 || minConfidence > 1 || now.IsZero() || limit < 1 || limit > 100 {
		return TopicMemoryWindow{}, fmt.Errorf("invalid memory topic window")
	}
	for _, key := range topicKeys {
		if strings.TrimSpace(key) == "" {
			return TopicMemoryWindow{}, fmt.Errorf("invalid memory topic window")
		}
	}

	window := TopicMemoryWindow{Memories: []model.LongTermMemory{}}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := func() *gorm.DB {
			return tx.Model(&model.LongTermMemory{}).
				Where("user_id = ? AND status = ? AND memory_scope = ? AND topic_key IN ? AND confidence >= ?",
					userID, model.LongTermMemoryActive, scope, topicKeys, minConfidence).
				Where("(valid_until IS NULL OR valid_until > ?)", now)
		}
		if err := query().Count(&window.EligibleCount).Error; err != nil {
			return err
		}
		return query().Order("user_confirmed DESC, durability ASC, importance DESC, updated_at DESC, id ASC").
			Limit(limit).Find(&window.Memories).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return TopicMemoryWindow{}, err
	}
	return window, nil
}

// ArchiveExpired performs the lifecycle cleanup after the recall predicate has
// already made expiry effective. Correctness never depends on this update: an
// expired row is excluded even if the maintenance worker is delayed.
func (r *Repository) ArchiveExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	if r == nil || r.db == nil || now.IsZero() || limit < 1 || limit > 500 {
		return 0, fmt.Errorf("invalid expired memory archive")
	}
	var archived int64
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []model.LongTermMemory
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("status = ? AND durability = ? AND valid_until IS NOT NULL AND valid_until <= ?", model.LongTermMemoryActive, model.MemoryDurabilityTimeBound, now).
			Order("valid_until ASC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		for i := range rows {
			before := rows[i]
			after := before
			after.Status = model.LongTermMemoryArchived
			result := tx.Model(&model.LongTermMemory{}).
				Where("id = ? AND user_id = ? AND status = ? AND valid_until <= ?", before.ID, before.UserID, model.LongTermMemoryActive, now).
				Update("status", model.LongTermMemoryArchived)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			revision := model.MemoryRevision{
				ID: uuid.NewString(), UserID: before.UserID, MemoryID: before.ID, Action: model.MemoryDreamArchive,
				Actor: model.MemoryRevisionSystem, Reason: "valid_until expired", BeforeSnapshot: memorySnapshot(&before), AfterSnapshot: memorySnapshot(&after),
				BeforeHash: before.ContentHash, AfterHash: after.ContentHash, CreatedAt: now,
			}
			if err := revision.Validate(); err != nil {
				return err
			}
			if err := tx.Create(&revision).Error; err != nil {
				return err
			}
			archived++
		}
		return nil
	})
	return archived, err
}
