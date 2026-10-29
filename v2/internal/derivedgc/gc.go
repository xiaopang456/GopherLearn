package derivedgc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	redisCommon "GopherAI/common/redis"
	"GopherAI/model"

	redisCli "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

const ApplyConfirmation = "DELETE_ORPHAN_DERIVED_HASHES"

type FamilyReport struct {
	Family, Prefix                   string
	Expected, RedisHashes, IndexDocs int
	Kept, Orphans, UnexpectedTypes   int
	OrphansByUser                    map[int64]int
	Blockers                         []string
}

func (r FamilyReport) Ready() bool {
	return len(r.Blockers) == 0 && r.Kept == r.Expected && r.RedisHashes == r.Kept+r.Orphans && r.IndexDocs == r.RedisHashes
}

type Report struct {
	Families []FamilyReport
	Deleted  int
}

func (r Report) Ready() bool {
	for _, family := range r.Families {
		if !family.Ready() {
			return false
		}
	}
	return len(r.Families) > 0
}

type audit struct {
	report     Report
	candidates []string
}

type Cleaner struct {
	db    *gorm.DB
	redis *redisCli.Client
}

func New(db *gorm.DB, client *redisCli.Client) (*Cleaner, error) {
	if db == nil || client == nil {
		return nil, fmt.Errorf("derived gc requires mysql and redis")
	}
	return &Cleaner{db: db, redis: client}, nil
}

func (c *Cleaner) Audit(ctx context.Context, now time.Time) (Report, error) {
	result, err := c.audit(ctx, now)
	return result.report, err
}

func (c *Cleaner) Apply(ctx context.Context, now time.Time, writersStopped bool, confirmation string) (Report, error) {
	if !writersStopped {
		return Report{}, fmt.Errorf("apply requires an explicit writers-stopped assertion")
	}
	if confirmation != ApplyConfirmation {
		return Report{}, fmt.Errorf("apply confirmation does not match")
	}
	first, err := c.audit(ctx, now)
	if err != nil {
		return Report{}, err
	}
	if !first.report.Ready() {
		return first.report, fmt.Errorf("first audit has blockers or incomplete source coverage")
	}
	second, err := c.audit(ctx, now)
	if err != nil {
		return Report{}, err
	}
	if !second.report.Ready() {
		return second.report, fmt.Errorf("second audit has blockers or incomplete source coverage")
	}
	if first.fingerprint() != second.fingerprint() {
		return second.report, fmt.Errorf("derived view changed between audits")
	}
	for start := 0; start < len(second.candidates); start += 200 {
		end := start + 200
		if end > len(second.candidates) {
			end = len(second.candidates)
		}
		if err := c.redis.Unlink(ctx, second.candidates[start:end]...).Err(); err != nil {
			return second.report, fmt.Errorf("unlink orphan derived hashes: %w", err)
		}
	}
	post, err := c.audit(ctx, now)
	if err != nil {
		return Report{}, err
	}
	for _, family := range post.report.Families {
		if family.Orphans != 0 || family.RedisHashes != family.Expected || !family.Ready() {
			return post.report, fmt.Errorf("post-delete audit is incomplete")
		}
	}
	post.report.Deleted = len(second.candidates)
	return post.report, nil
}

type knowledgeIdentity struct {
	UserID     int64
	DocumentID string
	Version    int
}
type exactIdentity struct {
	UserID int64
	ID     string
}

func (c *Cleaner) audit(ctx context.Context, now time.Time) (audit, error) {
	knowledge, err := c.knowledgeSource(ctx)
	if err != nil {
		return audit{}, err
	}
	episodes, err := c.episodeSource(ctx)
	if err != nil {
		return audit{}, err
	}
	memories, err := c.memorySource(ctx, now)
	if err != nil {
		return audit{}, err
	}

	knowledgeKeys, err := c.scan(ctx, "knowledge", redisCommon.KnowledgeChunkKeyPrefix, redisCommon.KnowledgeChunkIndexName)
	if err != nil {
		return audit{}, err
	}
	episodeKeys, err := c.scan(ctx, "episode", redisCommon.EpisodeKeyPrefix, redisCommon.EpisodeIndexName)
	if err != nil {
		return audit{}, err
	}
	memoryKeys, err := c.scan(ctx, "memory", redisCommon.MemoryKeyPrefix, redisCommon.MemoryIndexName)
	if err != nil {
		return audit{}, err
	}

	kr, kc := reconcileKnowledge(knowledgeKeys, knowledge)
	er, ec := reconcileExact(episodeKeys, episodes)
	mr, mc := reconcileExact(memoryKeys, memories)
	all := append(append(kc, ec...), mc...)
	sort.Strings(all)
	return audit{report: Report{Families: []FamilyReport{kr, er, mr}}, candidates: all}, nil
}

func (c *Cleaner) knowledgeSource(ctx context.Context) (map[knowledgeIdentity]int, error) {
	type row struct {
		UserID                    int64
		ID                        string
		ActiveVersion, ChunkCount int
	}
	var rows []row
	err := c.db.WithContext(ctx).Model(&model.KnowledgeDocument{}).Select("user_id, id, active_version, chunk_count").
		Where("active_name IS NOT NULL AND active_version > 0 AND chunk_count > 0").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load knowledge gc source: %w", err)
	}
	result := make(map[knowledgeIdentity]int, len(rows))
	for _, row := range rows {
		result[knowledgeIdentity{row.UserID, row.ID, row.ActiveVersion}] = row.ChunkCount
	}
	return result, nil
}

func (c *Cleaner) episodeSource(ctx context.Context) (map[exactIdentity]struct{}, error) {
	type row struct {
		UserID int64
		ID     string
	}
	var rows []row
	err := c.db.WithContext(ctx).Model(&model.EpisodeMemory{}).Select("user_id, id").
		Where("status = ? AND index_status = ?", model.EpisodeStatusActive, model.EpisodeIndexIndexed).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load episode gc source: %w", err)
	}
	result := make(map[exactIdentity]struct{}, len(rows))
	for _, row := range rows {
		result[exactIdentity{row.UserID, row.ID}] = struct{}{}
	}
	return result, nil
}

func (c *Cleaner) memorySource(ctx context.Context, now time.Time) (map[exactIdentity]struct{}, error) {
	type row struct {
		UserID int64
		ID     string
	}
	var rows []row
	err := c.db.WithContext(ctx).Model(&model.LongTermMemory{}).Select("user_id, id").
		Where("status = ? AND memory_scope = ?", model.LongTermMemoryActive, model.MemoryScopeTopic).
		Where("valid_until IS NULL OR valid_until > ?", now).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load memory gc source: %w", err)
	}
	result := make(map[exactIdentity]struct{}, len(rows))
	for _, row := range rows {
		result[exactIdentity{row.UserID, row.ID}] = struct{}{}
	}
	return result, nil
}

type scannedKey struct {
	Key        string
	UserID     int64
	identity   any
	Hash       bool
	ParseError string
}
type scannedFamily struct {
	Name, Prefix string
	IndexDocs    int
	Keys         []scannedKey
	ScanBlockers []string
}

func (c *Cleaner) scan(ctx context.Context, name, prefix, index string) (scannedFamily, error) {
	result := scannedFamily{Name: name, Prefix: prefix}
	info, err := c.redis.FTInfo(ctx, index).Result()
	if err != nil {
		return result, fmt.Errorf("inspect %s current index: %w", name, err)
	}
	result.IndexDocs = info.NumDocs
	var cursor uint64
	for {
		keys, next, err := c.redis.Scan(ctx, cursor, prefix+"*", 500).Result()
		if err != nil {
			return result, fmt.Errorf("scan %s hashes: %w", name, err)
		}
		for _, key := range keys {
			typeName, err := c.redis.Type(ctx, key).Result()
			if err != nil {
				return result, fmt.Errorf("inspect redis key type: %w", err)
			}
			item := scannedKey{Key: key, Hash: typeName == "hash"}
			if item.Hash {
				switch name {
				case "knowledge":
					item.UserID, item.identity, item.ParseError = parseKnowledgeKey(prefix, key)
				default:
					item.UserID, item.identity, item.ParseError = parseExactKey(prefix, key)
				}
			}
			result.Keys = append(result.Keys, item)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return result, nil
}

func reconcileKnowledge(scanned scannedFamily, source map[knowledgeIdentity]int) (FamilyReport, []string) {
	r := baseReport(scanned)
	kept := make(map[knowledgeIdentity]int)
	candidates := make([]string, 0)
	for _, key := range scanned.Keys {
		if blockKey(&r, key) {
			continue
		}
		identity := key.identity.(knowledgeIdentity)
		if _, ok := source[identity]; ok {
			r.Kept++
			kept[identity]++
			continue
		}
		r.Orphans++
		r.OrphansByUser[key.UserID]++
		candidates = append(candidates, key.Key)
	}
	for identity, expected := range source {
		r.Expected += expected
		if kept[identity] != expected {
			r.Blockers = append(r.Blockers, fmt.Sprintf("active knowledge tuple has %d hashes, expected %d", kept[identity], expected))
		}
	}
	sort.Strings(r.Blockers)
	return r, candidates
}

func reconcileExact(scanned scannedFamily, source map[exactIdentity]struct{}) (FamilyReport, []string) {
	r := baseReport(scanned)
	r.Expected = len(source)
	candidates := make([]string, 0)
	for _, key := range scanned.Keys {
		if blockKey(&r, key) {
			continue
		}
		identity := key.identity.(exactIdentity)
		if _, ok := source[identity]; ok {
			r.Kept++
			continue
		}
		r.Orphans++
		r.OrphansByUser[key.UserID]++
		candidates = append(candidates, key.Key)
	}
	if r.Kept != r.Expected {
		r.Blockers = append(r.Blockers, fmt.Sprintf("kept=%d expected=%d", r.Kept, r.Expected))
	}
	sort.Strings(r.Blockers)
	return r, candidates
}

func baseReport(scanned scannedFamily) FamilyReport {
	r := FamilyReport{Family: scanned.Name, Prefix: scanned.Prefix, IndexDocs: scanned.IndexDocs, OrphansByUser: map[int64]int{}, Blockers: append([]string(nil), scanned.ScanBlockers...)}
	for _, key := range scanned.Keys {
		if key.Hash {
			r.RedisHashes++
		}
	}
	return r
}

func blockKey(r *FamilyReport, key scannedKey) bool {
	if !key.Hash {
		r.UnexpectedTypes++
		r.Blockers = append(r.Blockers, "managed prefix contains a non-HASH key")
		return true
	}
	if key.ParseError != "" {
		r.Blockers = append(r.Blockers, "managed HASH key has an invalid identity")
		return true
	}
	return false
}

func parseKnowledgeKey(prefix, key string) (int64, any, string) {
	parts := strings.SplitN(strings.TrimPrefix(key, prefix), ":", 4)
	if len(parts) != 4 || parts[1] == "" || parts[3] == "" {
		return 0, nil, "invalid knowledge key"
	}
	userID, err1 := strconv.ParseInt(parts[0], 10, 64)
	version, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || userID <= 0 || version <= 0 {
		return 0, nil, "invalid knowledge key"
	}
	return userID, knowledgeIdentity{userID, parts[1], version}, ""
}

func parseExactKey(prefix, key string) (int64, any, string) {
	parts := strings.SplitN(strings.TrimPrefix(key, prefix), ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return 0, nil, "invalid exact key"
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID <= 0 {
		return 0, nil, "invalid exact key"
	}
	return userID, exactIdentity{userID, parts[1]}, ""
}

func (a audit) fingerprint() string {
	h := sha256.New()
	for _, family := range a.report.Families {
		fmt.Fprintf(h, "%s:%d:%d:%d:%d:%d|", family.Family, family.Expected, family.RedisHashes, family.IndexDocs, family.Kept, family.Orphans)
		for _, blocker := range family.Blockers {
			fmt.Fprintf(h, "b:%s|", blocker)
		}
	}
	for _, key := range a.candidates {
		fmt.Fprintf(h, "k:%s|", key)
	}
	return hex.EncodeToString(h.Sum(nil))
}
