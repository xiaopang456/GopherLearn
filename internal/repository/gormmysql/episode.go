package gormmysql

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The episode store keeps one derived, versioned card per session plus the
// durable projection jobs a background worker claims. MySQL stays authoritative;
// the retrieval view built on top is a cache that can always be rebuilt.
//
// Two deliberate departures from the original project's SQL, both forced by this
// repository's portability rule (the same code runs under the sqlite test
// engine): there is no SELECT ... FOR UPDATE - every race is decided by a
// guarded conditional UPDATE whose WHERE clause repeats the conditions the
// branch depended on, with RowsAffected as the verdict - and the upsert is a
// read-then-guarded-write inside one transaction rather than an
// ON DUPLICATE KEY with VALUES() expressions, which not every engine parses.
// The invariants are the same; only the mechanism differs.

// episodeMutationMaxAttempts bounds the deadlock retry around mutation
// transactions. A deadlock is transient by definition; retrying it a few times
// with a small backoff is cheaper than failing a projection the model already
// paid for.
const episodeMutationMaxAttempts = 3

var episodeErrorCodePattern = func(code string) bool { return domain.ValidEpisodeErrorCode(code) }

// recoverExpiredEpisodeBatch bounds one recovery pass. A pass that tried to
// reclaim an unbounded backlog in one go would hold locks for as long as that
// took; every write below is a point update keyed by the primary key, and the
// scan and the writes are deliberately separate statements.
const recoverExpiredEpisodeBatch = 200

// maxEpisodeTimeScan bounds one time-anchored fetch before the provenance
// filter runs, so filtering invalid cards cannot turn "the newest valid
// episode" into a false empty answer.
const maxEpisodeTimeScan = 500

// activeEpisodeQuery is the privacy boundary every episode read goes through:
// the row is only reachable while its source session still exists and belongs
// to the same owner. A session delete cascade that raced a reader can therefore
// never serve a card from a conversation that is gone.
func activeEpisodeQuery(db *gorm.DB) *gorm.DB {
	return db.Model(&EpisodeRecord{}).
		Joins("JOIN sessions ON sessions.id = episode_memories.session_id AND sessions.user_id = episode_memories.user_id")
}

func requireEpisodeSession(tx *gorm.DB, userID, sessionID string) error {
	var count int64
	if err := tx.Model(&SessionRecord{}).
		Where("id = ? AND user_id = ?", sessionID, userID).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return repository.ErrEpisodeNotFound
	}
	return nil
}

func (r *Repository) GetEpisodeBySession(ctx context.Context, userID, sessionID string) (*domain.Episode, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record EpisodeRecord
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.session_id = ?", userID, sessionID).
		Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, repository.ErrEpisodeNotFound
	}
	if err != nil {
		return nil, translate(err)
	}
	episode, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &episode, nil
}

func (r *Repository) GetEpisodeByID(ctx context.Context, userID, episodeID string) (*domain.Episode, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(episodeID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var record EpisodeRecord
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.id = ?", userID, episodeID).
		Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, repository.ErrEpisodeNotFound
	}
	if err != nil {
		return nil, translate(err)
	}
	episode, err := record.toDomain()
	if err != nil {
		return nil, err
	}
	return &episode, nil
}

// SaveEpisode publishes a new Episode (expectedVersion 0) or advances an
// existing one under optimistic concurrency. The update is conditioned on the
// exact expected version AND on the watermark moving forward, so a stale
// projector can neither overwrite a newer card nor move coverage back.
func (r *Repository) SaveEpisode(ctx context.Context, episode *domain.Episode, expectedVersion uint64) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || episode == nil {
		return repository.ErrInvalidArgument
	}
	next := *episode
	if expectedVersion == 0 {
		next.Version = 1
	} else {
		next.Version = expectedVersion + 1
	}
	if err := next.Validate(); err != nil {
		return repository.ErrInvalidArgument
	}
	record, err := episodeRecordFrom(next)
	if err != nil {
		return repository.ErrInvalidArgument
	}

	err = r.withEpisodeMutationTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireEpisodeSession(tx, next.UserID, next.SessionID); err != nil {
			return err
		}
		if expectedVersion == 0 {
			if err := tx.Create(&record).Error; err != nil {
				// The duplicate surfaces differently per engine: MySQL reports
				// error 1062, the embedded test engine returns GORM's translated
				// sentinel. Both mean "one card already exists for this session".
				if isDuplicateKey(err) || errors.Is(err, gorm.ErrDuplicatedKey) {
					return repository.ErrEpisodeConflict
				}
				return err
			}
			return nil
		}
		result := tx.Model(&EpisodeRecord{}).
			Where("id = ? AND user_id = ? AND session_id = ? AND version = ? AND source_last_message_id < ?",
				next.ID, next.UserID, next.SessionID, expectedVersion, next.SourceLastMessageID).
			Updates(map[string]any{
				"title":                    record.Title,
				"content_json":             record.ContentJSON,
				"search_text":              record.SearchText,
				"topic_keys_json":          record.TopicKeysJSON,
				"source_episodes_json":     record.SourceEpisodesJSON,
				"source_summary_version":   record.SourceSummaryVersion,
				"source_summary_watermark": record.SourceSummaryWatermark,
				"source_last_message_id":   record.SourceLastMessageID,
				"last_message_at":          record.LastMessageAt,
				"version":                  record.Version,
				"status":                   record.Status,
				"index_status":             record.IndexStatus,
				// The reason has to move with the status. A row that stopped being
				// an abstention while still carrying why it abstained would be
				// memory that also explains why there is none.
				"abstain_reason": record.AbstainReason,
				"updated_at":     time.Now().UTC(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return repository.ErrEpisodeConflict
		}
		return nil
	})
	if err != nil {
		return translateEpisodeError(err)
	}
	episode.Version = next.Version
	episode.CreatedAt = next.CreatedAt
	episode.UpdatedAt = next.UpdatedAt
	return nil
}

func (r *Repository) CommitEpisodeIndexStatus(ctx context.Context, userID, episodeID string, version uint64, status domain.EpisodeIndexStatus) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(episodeID) == "" || !status.Valid() {
		return repository.ErrInvalidArgument
	}
	result := r.db.WithContext(ctx).Model(&EpisodeRecord{}).
		Where("id = ? AND user_id = ? AND version = ?", episodeID, userID, version).
		Update("index_status", string(status))
	if result.Error != nil {
		return translate(result.Error)
	}
	if result.RowsAffected != 1 {
		return repository.ErrEpisodeConflict
	}
	return nil
}

func (r *Repository) ListActiveEpisodesForReindex(ctx context.Context, userID, afterID string, limit int) ([]domain.Episode, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || limit < 1 {
		return nil, repository.ErrInvalidArgument
	}
	var records []EpisodeRecord
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.id > ?",
			userID, string(domain.EpisodeStatusActive), afterID).
		Order("episode_memories.id ASC").Limit(limit).Find(&records).Error
	if err != nil {
		return nil, translate(err)
	}
	return episodesFromRecords(records)
}

func (r *Repository) ListAllActiveEpisodes(ctx context.Context, afterID string, limit int) ([]domain.Episode, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || limit < 1 {
		return nil, repository.ErrInvalidArgument
	}
	var records []EpisodeRecord
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.status = ? AND episode_memories.id > ?", string(domain.EpisodeStatusActive), afterID).
		Order("episode_memories.id ASC").Limit(limit).Find(&records).Error
	if err != nil {
		return nil, translate(err)
	}
	return episodesFromRecords(records)
}

// LoadEpisodeCandidates is the final authority after the retrieval view has
// proposed ids. The reload re-checks owner, active session, active status,
// indexed view and session exclusion, restores the caller's ordering, and then
// re-verifies every card's provenance closure against live rows.
func (r *Repository) LoadEpisodeCandidates(ctx context.Context, userID, currentSession string, ids []string) ([]domain.Episode, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	if len(ids) == 0 {
		return []domain.Episode{}, nil
	}
	var records []EpisodeRecord
	err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.index_status = ? AND episode_memories.session_id <> ? AND episode_memories.id IN ?",
			userID, string(domain.EpisodeStatusActive), string(domain.EpisodeIndexIndexed), currentSession, ids).
		Find(&records).Error
	if err != nil {
		return nil, translate(err)
	}
	byID := make(map[string]EpisodeRecord, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	ordered := make([]EpisodeRecord, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if record, ok := byID[id]; ok {
			ordered = append(ordered, record)
		}
	}
	rows, err := episodesFromRecords(ordered)
	if err != nil {
		return nil, err
	}
	return r.filterValidEpisodeProvenance(ctx, userID, rows)
}

func (r *Repository) ListEpisodesByTime(ctx context.Context, userID, currentSession string, from, to time.Time, limit int) ([]domain.Episode, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || limit < 1 {
		return nil, repository.ErrInvalidArgument
	}
	query := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.session_id <> ?",
			userID, string(domain.EpisodeStatusActive), currentSession)
	if !from.IsZero() {
		query = query.Where("episode_memories.last_message_at >= ?", from)
	}
	if !to.IsZero() {
		query = query.Where("episode_memories.last_message_at < ?", to)
	}
	// A newer derived Episode may be invalid because one of its sources was
	// deleted or updated. Fetch a bounded population before applying the
	// requested limit so "last session" can still return the next valid Episode
	// instead of failing open or producing a false empty result.
	fetchLimit := maxEpisodeTimeScan
	if limit > fetchLimit {
		fetchLimit = limit
	}
	var records []EpisodeRecord
	err := query.Order("episode_memories.last_message_at DESC, episode_memories.id ASC").Limit(fetchLimit).Find(&records).Error
	if err != nil {
		return nil, translate(err)
	}
	rows, err := episodesFromRecords(records)
	if err != nil {
		return nil, err
	}
	eligible, err := r.filterValidEpisodeProvenance(ctx, userID, rows)
	if err != nil {
		return nil, err
	}
	if len(eligible) > limit {
		eligible = eligible[:limit]
	}
	return eligible, nil
}

// filterValidEpisodeProvenance is the final source boundary after a candidate
// list exists: every (episode, version) a card was derived from must still
// belong to the same user and remain an active, indexed row of a live session.
func (r *Repository) filterValidEpisodeProvenance(ctx context.Context, userID string, rows []domain.Episode) ([]domain.Episode, error) {
	if len(rows) == 0 {
		return rows, nil
	}
	ids := make([]string, 0)
	seen := make(map[string]struct{})
	for _, row := range rows {
		for _, ref := range row.SourceEpisodes {
			if _, exists := seen[ref.EpisodeID]; exists {
				continue
			}
			seen[ref.EpisodeID] = struct{}{}
			ids = append(ids, ref.EpisodeID)
		}
	}
	if len(ids) == 0 {
		return rows, nil
	}
	var sources []EpisodeRecord
	if err := activeEpisodeQuery(r.db.WithContext(ctx)).
		Where("episode_memories.user_id = ? AND episode_memories.status = ? AND episode_memories.index_status = ? AND episode_memories.id IN ?",
			userID, string(domain.EpisodeStatusActive), string(domain.EpisodeIndexIndexed), ids).
		Find(&sources).Error; err != nil {
		return nil, translate(err)
	}
	versions := make(map[string]uint64, len(sources))
	for _, source := range sources {
		versions[source.ID] = source.Version
	}
	eligible := make([]domain.Episode, 0, len(rows))
	for _, row := range rows {
		valid := true
		for _, ref := range row.SourceEpisodes {
			if ref.EpisodeID == row.ID || ref.Version == 0 || versions[ref.EpisodeID] != ref.Version {
				valid = false
				break
			}
		}
		if valid {
			eligible = append(eligible, row)
		}
	}
	return eligible, nil
}

// DeleteEpisodesBySession removes the session's job and episode rows inside one
// transaction and returns the episode ids that were deleted, so the caller can
// drop the same ids from the derived retrieval view.
func (r *Repository) DeleteEpisodesBySession(ctx context.Context, userID, sessionID string) ([]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, repository.ErrInvalidArgument
	}
	var deleted []string
	err := r.withEpisodeMutationTransaction(ctx, func(tx *gorm.DB) error {
		deleted = nil
		var ids []string
		if err := tx.Model(&EpisodeRecord{}).
			Where("user_id = ? AND session_id = ?", userID, sessionID).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Delete(&EpisodeJobRecord{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).
			Delete(&EpisodeRecord{}).Error; err != nil {
			return err
		}
		deleted = ids
		return nil
	})
	if err != nil {
		return nil, translateEpisodeError(err)
	}
	return deleted, nil
}

// ListEpisodeProjectionGaps finds sessions whose latest assistant message is
// newer than the stored Episode watermark. Only sessions that ended on a
// complete assistant turn are reported, because a conversation still waiting
// for its answer is not a complete episode source. The scan covers learning
// agent sessions only, matching the source project's reconcile scope (mode =
// agent): a chat session that never gets a publish also never gets a
// background projection.
func (r *Repository) ListEpisodeProjectionGaps(ctx context.Context, limit int) ([]repository.EpisodeProjectionGap, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || limit < 1 || limit > 1000 {
		return nil, repository.ErrInvalidArgument
	}
	var gaps []repository.EpisodeProjectionGap
	err := r.db.WithContext(ctx).Raw(`
		SELECT s.user_id AS user_id, s.id AS session_id, latest.latest_message_id AS latest_message_id
		FROM sessions s
		JOIN (
			SELECT session_id, MAX(id) AS latest_message_id
			FROM messages
			GROUP BY session_id
		) latest ON latest.session_id = s.id
		JOIN messages last_message ON last_message.id = latest.latest_message_id AND last_message.session_id = s.id
		LEFT JOIN episode_memories e ON e.user_id = s.user_id AND e.session_id = s.id
		WHERE s.user_id <> '' AND last_message.role = 'assistant'
		  AND s.mode = 'learning_agent'
		  AND latest.latest_message_id > COALESCE(e.source_last_message_id, '')
		ORDER BY last_message.created_at ASC
		LIMIT ?`, limit).Scan(&gaps).Error
	if err != nil {
		return nil, translate(err)
	}
	return gaps, nil
}

func (r *Repository) UpsertEpisodeDesired(ctx context.Context, userID, sessionID, desiredMessageID string, availableAt time.Time) (*domain.EpisodeProjectionJob, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" ||
		strings.TrimSpace(desiredMessageID) == "" || availableAt.IsZero() {
		return nil, repository.ErrInvalidArgument
	}
	for attempt := 0; attempt < episodeMutationMaxAttempts; attempt++ {
		job, conflict, err := r.upsertEpisodeDesiredOnce(ctx, userID, sessionID, desiredMessageID, availableAt)
		if err != nil {
			return nil, err
		}
		if !conflict {
			return job, nil
		}
	}
	return nil, repository.ErrEpisodeJobConflict
}

// upsertEpisodeDesiredOnce applies the debounce boundary once. A higher desired
// message revives terminal jobs and re-arms pending ones, but never steals a
// live lease; the guarded update carries the desired watermark the read saw, so
// a concurrent writer that moved the row first makes this attempt a no-op.
func (r *Repository) upsertEpisodeDesiredOnce(ctx context.Context, userID, sessionID, desiredMessageID string, availableAt time.Time) (*domain.EpisodeProjectionJob, bool, error) {
	var result *domain.EpisodeProjectionJob
	conflict := false
	err := r.withEpisodeMutationTransaction(ctx, func(tx *gorm.DB) error {
		if err := requireEpisodeSession(tx, userID, sessionID); err != nil {
			if errors.Is(err, repository.ErrEpisodeNotFound) {
				return repository.ErrEpisodeJobNotFound
			}
			return err
		}
		var record EpisodeJobRecord
		err := tx.Where("user_id = ? AND session_id = ?", userID, sessionID).Take(&record).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			now := time.Now().UTC()
			job := domain.EpisodeProjectionJob{
				ID: newID("epjob"), UserID: userID, SessionID: sessionID,
				DesiredMessageID: desiredMessageID, Status: domain.EpisodeProjectionPending,
				AvailableAt: episodeDBTime(availableAt), CreatedAt: episodeDBTime(now), UpdatedAt: episodeDBTime(now),
			}
			if err := job.ValidateForCreate(); err != nil {
				return repository.ErrInvalidArgument
			}
			jobRecord := episodeJobRecordFrom(job)
			if err := tx.Create(&jobRecord).Error; err != nil {
				if isDuplicateKey(err) || errors.Is(err, gorm.ErrDuplicatedKey) {
					conflict = true
					return nil
				}
				return err
			}
			result = &job
			return nil
		case err != nil:
			return err
		}
		if domain.CompareMessageIDs(desiredMessageID, record.DesiredMessageID) <= 0 {
			stored := record.toDomain()
			result = &stored
			return nil
		}
		updates := map[string]any{
			"desired_message_id": desiredMessageID,
			"updated_at":         episodeDBTime(time.Now()),
			// The debounce window is re-armed from the newest message even while
			// a projection is in flight, matching the source project: the live
			// lease is never stolen, but when the worker finishes and requeues
			// the row for the new tail, that requeue waits out the quiet window
			// from the newest message instead of firing on a window that
			// expired while it was projecting.
			"available_at": episodeDBTime(availableAt),
		}
		// A live lease is never stolen: the worker finishes its claimed target
		// and the completion requeues the row for the new tail.
		if record.Status != string(domain.EpisodeProjectionProcessing) {
			updates["status"] = string(domain.EpisodeProjectionPending)
			updates["attempts"] = 0
			updates["claimed_message_id"] = ""
			updates["lease_token"] = ""
			updates["lease_ends_at"] = nil
			updates["completed_at"] = nil
			updates["error_code"] = ""
		}
		claim := tx.Model(&EpisodeJobRecord{}).
			Where("id = ? AND user_id = ? AND desired_message_id = ?", record.ID, userID, record.DesiredMessageID).
			Updates(updates)
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			conflict = true
			return nil
		}
		var fresh EpisodeJobRecord
		if err := tx.Where("id = ?", record.ID).Take(&fresh).Error; err != nil {
			return err
		}
		stored := fresh.toDomain()
		result = &stored
		return nil
	})
	if err != nil {
		if errors.Is(err, repository.ErrEpisodeJobNotFound) {
			return nil, false, repository.ErrEpisodeJobNotFound
		}
		return nil, false, translateEpisodeError(err)
	}
	return result, conflict, nil
}

func (r *Repository) ListDueEpisodeJobs(ctx context.Context, limit, maxAttempts int, now time.Time) ([]domain.EpisodeProjectionJob, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || limit < 1 || limit > 200 || maxAttempts < 1 || maxAttempts > 10 || now.IsZero() {
		return nil, repository.ErrInvalidArgument
	}
	var records []EpisodeJobRecord
	err := r.db.WithContext(ctx).Model(&EpisodeJobRecord{}).
		Select("episode_projection_jobs.*").
		Joins("JOIN sessions ON sessions.id = episode_projection_jobs.session_id AND sessions.user_id = episode_projection_jobs.user_id").
		Where("episode_projection_jobs.status = ? AND episode_projection_jobs.available_at <= ? AND episode_projection_jobs.attempts < ?",
			string(domain.EpisodeProjectionPending), now, maxAttempts).
		Order("episode_projection_jobs.available_at ASC, episode_projection_jobs.created_at ASC").
		Limit(limit).Find(&records).Error
	if err != nil {
		return nil, translate(err)
	}
	jobs := make([]domain.EpisodeProjectionJob, 0, len(records))
	for _, record := range records {
		jobs = append(jobs, record.toDomain())
	}
	return jobs, nil
}

func (r *Repository) ClaimEpisodeJob(ctx context.Context, userID, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*domain.EpisodeProjectionJob, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(jobID) == "" || now.IsZero() ||
		lease <= 0 || maxAttempts < 1 || maxAttempts > 10 {
		return nil, repository.ErrInvalidArgument
	}
	var claimed *domain.EpisodeProjectionJob
	err := r.withEpisodeMutationTransaction(ctx, func(tx *gorm.DB) error {
		var record EpisodeJobRecord
		err := tx.Where("id = ? AND user_id = ?", jobID, userID).Take(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.ErrEpisodeJobNotFound
		}
		if err != nil {
			return err
		}
		if err := requireEpisodeSession(tx, userID, record.SessionID); err != nil {
			if errors.Is(err, repository.ErrEpisodeNotFound) {
				return repository.ErrEpisodeJobNotFound
			}
			return err
		}
		if record.Status != string(domain.EpisodeProjectionPending) ||
			record.AvailableAt.After(now) || record.Attempts >= maxAttempts {
			return repository.ErrEpisodeJobConflict
		}
		leaseEndsAt := now.Add(lease)
		result := tx.Model(&EpisodeJobRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND desired_message_id = ? AND attempts = ?",
				record.ID, userID, string(domain.EpisodeProjectionPending), record.DesiredMessageID, record.Attempts).
			Updates(map[string]any{
				"status":             string(domain.EpisodeProjectionProcessing),
				"attempts":           record.Attempts + 1,
				"claimed_message_id": record.DesiredMessageID,
				"lease_token":        newLeaseToken(),
				"lease_ends_at":      episodeDBTime(leaseEndsAt),
				"error_code":         "",
				"completed_at":       nil,
				"updated_at":         episodeDBTime(now),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return repository.ErrEpisodeJobConflict
		}
		var fresh EpisodeJobRecord
		if err := tx.Where("id = ?", record.ID).Take(&fresh).Error; err != nil {
			return err
		}
		stored := fresh.toDomain()
		claimed = &stored
		return nil
	})
	if err != nil {
		return nil, translateEpisodeError(err)
	}
	return claimed, nil
}

func (r *Repository) CompleteOrRequeueEpisodeJob(ctx context.Context, claimedJob *domain.EpisodeProjectionJob, coveredMessageID string, now time.Time) (*domain.EpisodeProjectionJob, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || claimedJob == nil || strings.TrimSpace(claimedJob.UserID) == "" || strings.TrimSpace(claimedJob.ID) == "" ||
		claimedJob.LeaseToken == "" || strings.TrimSpace(coveredMessageID) == "" || now.IsZero() {
		return nil, repository.ErrInvalidArgument
	}
	if coveredMessageID != claimedJob.ClaimedMessageID {
		return nil, repository.ErrInvalidArgument
	}
	var completed *domain.EpisodeProjectionJob
	err := r.withEpisodeMutationTransaction(ctx, func(tx *gorm.DB) error {
		var record EpisodeJobRecord
		err := tx.Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
			claimedJob.ID, claimedJob.UserID, string(domain.EpisodeProjectionProcessing), claimedJob.LeaseToken).
			Take(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.ErrEpisodeJobConflict
		}
		if err != nil {
			return err
		}
		if record.ClaimedMessageID != coveredMessageID {
			return repository.ErrEpisodeJobConflict
		}
		updates := map[string]any{
			"claimed_message_id": "",
			"lease_token":        "",
			"lease_ends_at":      nil,
			"error_code":         "",
			"updated_at":         episodeDBTime(now),
		}
		// Completion acknowledges exactly the target frozen at claim time. If
		// the desired watermark advanced meanwhile, the same durable row returns
		// to pending with its attempts reset instead of losing the new tail.
		if domain.CompareMessageIDs(record.DesiredMessageID, record.ClaimedMessageID) > 0 {
			updates["status"] = string(domain.EpisodeProjectionPending)
			updates["attempts"] = 0
			updates["completed_at"] = nil
		} else {
			updates["status"] = string(domain.EpisodeProjectionCompleted)
			updates["completed_at"] = episodeDBTime(now)
		}
		result := tx.Model(&EpisodeJobRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
				record.ID, record.UserID, string(domain.EpisodeProjectionProcessing), record.LeaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return repository.ErrEpisodeJobConflict
		}
		var fresh EpisodeJobRecord
		if err := tx.Where("id = ?", record.ID).Take(&fresh).Error; err != nil {
			return err
		}
		stored := fresh.toDomain()
		completed = &stored
		return nil
	})
	if err != nil {
		return nil, translateEpisodeError(err)
	}
	return completed, nil
}

func (r *Repository) RetryEpisodeJob(ctx context.Context, claimedJob *domain.EpisodeProjectionJob, errorCode string, availableAt time.Time, maxAttempts int) (*domain.EpisodeProjectionJob, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if r.db == nil || claimedJob == nil || strings.TrimSpace(claimedJob.UserID) == "" || strings.TrimSpace(claimedJob.ID) == "" ||
		claimedJob.LeaseToken == "" || availableAt.IsZero() || maxAttempts < 1 || maxAttempts > 10 ||
		!episodeErrorCodePattern(errorCode) {
		return nil, repository.ErrInvalidArgument
	}
	var retried *domain.EpisodeProjectionJob
	err := r.withEpisodeMutationTransaction(ctx, func(tx *gorm.DB) error {
		var record EpisodeJobRecord
		err := tx.Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
			claimedJob.ID, claimedJob.UserID, string(domain.EpisodeProjectionProcessing), claimedJob.LeaseToken).
			Take(&record).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return repository.ErrEpisodeJobConflict
		}
		if err != nil {
			return err
		}
		// The comparison reads the claimed target as it was, before the release
		// below clears it: a newer desired target restarts the budget rather
		// than inheriting this failure.
		desiredAdvanced := domain.CompareMessageIDs(record.DesiredMessageID, record.ClaimedMessageID) > 0
		now := time.Now().UTC()
		updates := map[string]any{
			"claimed_message_id": "",
			"lease_token":        "",
			"lease_ends_at":      nil,
			"error_code":         errorCode,
			"updated_at":         episodeDBTime(now),
		}
		switch {
		case desiredAdvanced:
			updates["status"] = string(domain.EpisodeProjectionPending)
			updates["attempts"] = 0
			updates["completed_at"] = nil
			if availableAt.After(record.AvailableAt) {
				updates["available_at"] = episodeDBTime(availableAt)
			}
		case record.Attempts >= maxAttempts:
			updates["status"] = string(domain.EpisodeProjectionFailed)
			updates["completed_at"] = episodeDBTime(now)
		default:
			updates["status"] = string(domain.EpisodeProjectionPending)
			updates["available_at"] = episodeDBTime(availableAt)
			updates["completed_at"] = nil
		}
		result := tx.Model(&EpisodeJobRecord{}).
			Where("id = ? AND user_id = ? AND status = ? AND lease_token = ?",
				record.ID, record.UserID, string(domain.EpisodeProjectionProcessing), record.LeaseToken).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return repository.ErrEpisodeJobConflict
		}
		var fresh EpisodeJobRecord
		if err := tx.Where("id = ?", record.ID).Take(&fresh).Error; err != nil {
			return err
		}
		stored := fresh.toDomain()
		retried = &stored
		return nil
	})
	if err != nil {
		return nil, translateEpisodeError(err)
	}
	return retried, nil
}

// RecoverExpiredEpisodeJobs reclaims leases nobody will release. The scan and
// the writes are separate statements and every write is a point update whose
// WHERE repeats the conditions the branch depended on, so a row that moved on
// between the scan and its update simply matches nothing.
func (r *Repository) RecoverExpiredEpisodeJobs(ctx context.Context, now time.Time, maxAttempts int) (int64, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	if r.db == nil || now.IsZero() || maxAttempts < 1 || maxAttempts > 10 {
		return 0, repository.ErrInvalidArgument
	}
	type expiredJob struct {
		ID               string
		DesiredMessageID string
		ClaimedMessageID string
		Attempts         int
	}
	var candidates []expiredJob
	scan := r.db.WithContext(ctx).Model(&EpisodeJobRecord{}).
		Select("id", "desired_message_id", "claimed_message_id", "attempts").
		Where("status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ?",
			string(domain.EpisodeProjectionProcessing), now).
		Order("lease_ends_at ASC, id ASC").Limit(recoverExpiredEpisodeBatch)
	if err := scan.Scan(&candidates).Error; err != nil {
		return 0, translate(err)
	}
	var affected int64
	for _, candidate := range candidates {
		updates := map[string]any{
			"claimed_message_id": "",
			"lease_token":        "",
			"lease_ends_at":      nil,
			"error_code":         "worker_interrupted",
		}
		// A newer desired target never inherits the dead worker's attempts.
		switch {
		case domain.CompareMessageIDs(candidate.DesiredMessageID, candidate.ClaimedMessageID) > 0:
			updates["status"] = string(domain.EpisodeProjectionPending)
			updates["attempts"] = 0
			updates["completed_at"] = nil
		case candidate.Attempts >= maxAttempts:
			updates["status"] = string(domain.EpisodeProjectionFailed)
			updates["completed_at"] = episodeDBTime(now)
		default:
			updates["status"] = string(domain.EpisodeProjectionPending)
			updates["available_at"] = episodeDBTime(now)
			updates["completed_at"] = nil
		}
		result := r.db.WithContext(ctx).Model(&EpisodeJobRecord{}).
			Where("id = ? AND status = ? AND lease_ends_at IS NOT NULL AND lease_ends_at <= ? AND desired_message_id = ? AND claimed_message_id = ? AND attempts = ?",
				candidate.ID, string(domain.EpisodeProjectionProcessing), now,
				candidate.DesiredMessageID, candidate.ClaimedMessageID, candidate.Attempts).
			Updates(updates)
		if result.Error != nil {
			return affected, translate(result.Error)
		}
		affected += result.RowsAffected
	}
	return affected, nil
}

func (r *Repository) ReleaseOtherEpisodeSessions(ctx context.Context, userID, currentSessionID string, now time.Time) (int64, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	if r.db == nil || strings.TrimSpace(userID) == "" || strings.TrimSpace(currentSessionID) == "" || now.IsZero() {
		return 0, repository.ErrInvalidArgument
	}
	result := r.db.WithContext(ctx).Model(&EpisodeJobRecord{}).
		Where("user_id = ? AND session_id <> ? AND status = ? AND available_at > ?",
			userID, currentSessionID, string(domain.EpisodeProjectionPending), now).
		Update("available_at", episodeDBTime(now))
	if result.Error != nil {
		return 0, translate(result.Error)
	}
	return result.RowsAffected, nil
}

// withEpisodeMutationTransaction wraps a mutation in a short retry loop for
// MySQL deadlock 1213. Everything else passes through untouched.
func (r *Repository) withEpisodeMutationTransaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	var lastErr error
	for attempt := 0; attempt < episodeMutationMaxAttempts; attempt++ {
		lastErr = r.db.WithContext(ctx).Transaction(fn)
		var mysqlErr *mysql.MySQLError
		if !errors.As(lastErr, &mysqlErr) || mysqlErr.Number != 1213 {
			return lastErr
		}
		if attempt+1 == episodeMutationMaxAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return lastErr
}

// translateEpisodeError keeps the episode sentinels distinct from the generic
// repository vocabulary so a caller can tell "no such card" from "that row
// moved" without matching on text.
func translateEpisodeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, repository.ErrEpisodeNotFound) || errors.Is(err, repository.ErrEpisodeConflict) ||
		errors.Is(err, repository.ErrEpisodeJobNotFound) || errors.Is(err, repository.ErrEpisodeJobConflict) {
		return err
	}
	return translate(err)
}

func newLeaseToken() string {
	return newID("lease")
}

func episodesFromRecords(records []EpisodeRecord) ([]domain.Episode, error) {
	episodes := make([]domain.Episode, 0, len(records))
	for _, record := range records {
		episode, err := record.toDomain()
		if err != nil {
			return nil, err
		}
		episodes = append(episodes, episode)
	}
	return episodes, nil
}
