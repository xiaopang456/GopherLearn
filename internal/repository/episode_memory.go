package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// episodeJobToken is the lease token length in raw bytes; hex doubles it.
const episodeJobTokenBytes = 16

// maxEpisodeTimeScan bounds one time-anchored fetch before the provenance
// filter runs, so filtering invalid cards cannot turn "the newest valid
// episode" into a false empty answer.
const maxEpisodeTimeScan = 500

// recoverExpiredEpisodeBatch bounds one recovery pass: reclaiming an unbounded
// backlog in one go would hold the write lock for as long as that took.
const recoverExpiredEpisodeBatch = 200

func newEpisodeID(prefix string) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic("repository: episode id entropy unavailable: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(buffer)
}

func newEpisodeLeaseToken() string {
	buffer := make([]byte, episodeJobTokenBytes)
	if _, err := rand.Read(buffer); err != nil {
		panic("repository: episode lease entropy unavailable: " + err.Error())
	}
	return hex.EncodeToString(buffer)
}

type episodeSessionKey struct {
	userID    string
	sessionID string
}

// episodeState is the in-memory shape of the episode store. Episodes are keyed
// by id and indexed by their unique (user, session) pair; jobs are keyed by id
// and indexed the same way, because a session has at most one durable job.
type episodeState struct {
	episodes      map[string]domain.Episode
	episodeBySess map[episodeSessionKey]string
	jobs          map[string]domain.EpisodeProjectionJob
	jobBySess     map[episodeSessionKey]string
}

func (r *InMemoryRepository) episodeStateLocked() *episodeState {
	if r.episodes == nil {
		r.episodes = make(map[string]domain.Episode)
	}
	if r.episodeBySess == nil {
		r.episodeBySess = make(map[episodeSessionKey]string)
	}
	if r.episodeJobs == nil {
		r.episodeJobs = make(map[string]domain.EpisodeProjectionJob)
	}
	if r.episodeJobBySess == nil {
		r.episodeJobBySess = make(map[episodeSessionKey]string)
	}
	return &episodeState{
		episodes:      r.episodes,
		episodeBySess: r.episodeBySess,
		jobs:          r.episodeJobs,
		jobBySess:     r.episodeJobBySess,
	}
}

// activeEpisodeSessionLocked is the privacy boundary every episode read and
// write goes through: the session must exist and belong to the caller. A
// deleted session's rows are unreachable even if a crash left them behind.
func (r *InMemoryRepository) activeEpisodeSessionLocked(userID, sessionID string) bool {
	session, exists := r.sessions[sessionID]
	return exists && session.UserID == userID
}

func (r *InMemoryRepository) GetEpisodeBySession(_ context.Context, userID, sessionID string) (*domain.Episode, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	if !r.activeEpisodeSessionLocked(userID, sessionID) {
		return nil, ErrEpisodeNotFound
	}
	id, exists := state.episodeBySess[episodeSessionKey{userID, sessionID}]
	if !exists {
		return nil, ErrEpisodeNotFound
	}
	episode, exists := state.episodes[id]
	if !exists {
		return nil, ErrEpisodeNotFound
	}
	return cloneEpisode(episode), nil
}

func (r *InMemoryRepository) GetEpisodeByID(_ context.Context, userID, episodeID string) (*domain.Episode, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(episodeID) == "" {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	episode, exists := state.episodes[episodeID]
	if !exists || episode.UserID != userID || !r.activeEpisodeSessionLocked(userID, episode.SessionID) {
		return nil, ErrEpisodeNotFound
	}
	return cloneEpisode(episode), nil
}

// SaveEpisode publishes a new Episode or advances an existing one. The whole
// decision happens under the write lock, so the version check and the write
// cannot be split the way two database clients can split them.
func (r *InMemoryRepository) SaveEpisode(_ context.Context, episode *domain.Episode, expectedVersion uint64) error {
	if episode == nil {
		return ErrInvalidArgument
	}
	next := *episode
	if expectedVersion == 0 {
		next.Version = 1
	} else {
		next.Version = expectedVersion + 1
	}
	if err := next.Validate(); err != nil {
		return ErrInvalidArgument
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	if !r.activeEpisodeSessionLocked(next.UserID, next.SessionID) {
		return ErrEpisodeNotFound
	}
	key := episodeSessionKey{next.UserID, next.SessionID}
	if expectedVersion == 0 {
		if _, exists := state.episodeBySess[key]; exists {
			return ErrEpisodeConflict
		}
		now := time.Now().UTC()
		if next.CreatedAt.IsZero() {
			next.CreatedAt = now
		}
		next.UpdatedAt = now
		state.episodes[next.ID] = next
		state.episodeBySess[key] = next.ID
		*episode = next
		return nil
	}
	storedID, exists := state.episodeBySess[key]
	if !exists || storedID != next.ID {
		return ErrEpisodeConflict
	}
	stored, exists := state.episodes[storedID]
	if !exists || stored.Version != expectedVersion {
		return ErrEpisodeConflict
	}
	if domain.CompareMessageIDs(stored.SourceLastMessageID, next.SourceLastMessageID) >= 0 {
		return ErrEpisodeConflict
	}
	next.CreatedAt = stored.CreatedAt
	next.UpdatedAt = time.Now().UTC()
	state.episodes[storedID] = next
	*episode = next
	return nil
}

func (r *InMemoryRepository) CommitEpisodeIndexStatus(_ context.Context, userID, episodeID string, version uint64, status domain.EpisodeIndexStatus) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(episodeID) == "" || !status.Valid() {
		return ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	episode, exists := state.episodes[episodeID]
	if !exists || episode.UserID != userID || episode.Version != version {
		return ErrEpisodeConflict
	}
	episode.IndexStatus = status
	episode.UpdatedAt = time.Now().UTC()
	state.episodes[episodeID] = episode
	return nil
}

func (r *InMemoryRepository) ListActiveEpisodesForReindex(_ context.Context, userID, afterID string, limit int) ([]domain.Episode, error) {
	if strings.TrimSpace(userID) == "" || limit < 1 {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	var rows []domain.Episode
	for _, episode := range state.episodes {
		if episode.UserID != userID || episode.Status != domain.EpisodeStatusActive || episode.ID <= afterID {
			continue
		}
		if !r.activeEpisodeSessionLocked(userID, episode.SessionID) {
			continue
		}
		rows = append(rows, episode)
	}
	sortEpisodeRows(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return cloneEpisodes(rows), nil
}

func (r *InMemoryRepository) ListAllActiveEpisodes(_ context.Context, afterID string, limit int) ([]domain.Episode, error) {
	if limit < 1 {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	var rows []domain.Episode
	for _, episode := range state.episodes {
		if episode.Status != domain.EpisodeStatusActive || episode.ID <= afterID {
			continue
		}
		if !r.activeEpisodeSessionLocked(episode.UserID, episode.SessionID) {
			continue
		}
		rows = append(rows, episode)
	}
	sortEpisodeRows(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return cloneEpisodes(rows), nil
}

// LoadEpisodeCandidates is the final authority after the retrieval view has
// proposed ids: every candidate is re-read through the owner/session/status
// boundary, and only survivors whose provenance closure still points at live
// rows are returned.
func (r *InMemoryRepository) LoadEpisodeCandidates(_ context.Context, userID, currentSession string, ids []string) ([]domain.Episode, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	ordered := make([]domain.Episode, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		episode, exists := state.episodes[id]
		if !exists || episode.UserID != userID || episode.Status != domain.EpisodeStatusActive ||
			episode.IndexStatus != domain.EpisodeIndexIndexed || episode.SessionID == currentSession {
			continue
		}
		if !r.activeEpisodeSessionLocked(userID, episode.SessionID) {
			continue
		}
		ordered = append(ordered, episode)
	}
	eligible := r.filterEpisodeProvenanceLocked(state, userID, ordered)
	return cloneEpisodes(eligible), nil
}

func (r *InMemoryRepository) ListEpisodesByTime(_ context.Context, userID, currentSession string, from, to time.Time, limit int) ([]domain.Episode, error) {
	if strings.TrimSpace(userID) == "" || limit < 1 {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	var rows []domain.Episode
	for _, episode := range state.episodes {
		if episode.UserID != userID || episode.Status != domain.EpisodeStatusActive || episode.SessionID == currentSession {
			continue
		}
		if !r.activeEpisodeSessionLocked(userID, episode.SessionID) {
			continue
		}
		if !from.IsZero() && episode.LastMessageAt.Before(from) {
			continue
		}
		if !to.IsZero() && !episode.LastMessageAt.Before(to) {
			continue
		}
		rows = append(rows, episode)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].LastMessageAt.Equal(rows[j].LastMessageAt) {
			return rows[i].LastMessageAt.After(rows[j].LastMessageAt)
		}
		return rows[i].ID < rows[j].ID
	})
	fetchLimit := maxEpisodeTimeScan
	if limit > fetchLimit {
		fetchLimit = limit
	}
	if len(rows) > fetchLimit {
		rows = rows[:fetchLimit]
	}
	eligible := r.filterEpisodeProvenanceLocked(state, userID, rows)
	if len(eligible) > limit {
		eligible = eligible[:limit]
	}
	return cloneEpisodes(eligible), nil
}

// filterEpisodeProvenanceLocked is the source boundary after a candidate list
// exists: every (episode, version) a card was derived from must still belong to
// the same user and remain an active, indexed row. A card whose source moved on
// or disappeared is not evidence about anything.
func (r *InMemoryRepository) filterEpisodeProvenanceLocked(state *episodeState, userID string, rows []domain.Episode) []domain.Episode {
	if len(rows) == 0 {
		return rows
	}
	versions := make(map[string]uint64)
	for _, row := range rows {
		for _, ref := range row.SourceEpisodes {
			versions[ref.EpisodeID] = 0
		}
	}
	if len(versions) == 0 {
		return rows
	}
	for id := range versions {
		episode, exists := state.episodes[id]
		if !exists || episode.UserID != userID || episode.Status != domain.EpisodeStatusActive ||
			episode.IndexStatus != domain.EpisodeIndexIndexed {
			continue
		}
		if !r.activeEpisodeSessionLocked(userID, episode.SessionID) {
			continue
		}
		versions[id] = episode.Version
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
	return eligible
}

// DeleteEpisodesBySession removes the session's job and episode rows together.
// It returns the episode ids it deleted so the caller can drop the same ids
// from the derived retrieval view.
func (r *InMemoryRepository) DeleteEpisodesBySession(_ context.Context, userID, sessionID string) ([]string, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	key := episodeSessionKey{userID, sessionID}
	var deleted []string
	if id, exists := state.episodeBySess[key]; exists {
		delete(state.episodes, id)
		delete(state.episodeBySess, key)
		deleted = append(deleted, id)
	}
	if jobID, exists := state.jobBySess[key]; exists {
		delete(state.jobs, jobID)
		delete(state.jobBySess, key)
	}
	return deleted, nil
}

func (r *InMemoryRepository) ListEpisodeProjectionGaps(_ context.Context, limit int) ([]EpisodeProjectionGap, error) {
	if limit < 1 {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	type candidate struct {
		gap      EpisodeProjectionGap
		lastSeen time.Time
	}
	var candidates []candidate
	for _, session := range r.sessions {
		// The reconcile scan covers the same sessions the trigger does. The
		// source project's scan filters mode = agent, so a chat session that
		// never gets a publish also never gets a background projection.
		if session.Mode != domain.SessionModeLearningAgent {
			continue
		}
		messages := r.messages[session.ID]
		if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
			continue
		}
		latest := messages[len(messages)-1]
		watermark := ""
		if id, exists := state.episodeBySess[episodeSessionKey{session.UserID, session.ID}]; exists {
			if episode, exists := state.episodes[id]; exists {
				watermark = episode.SourceLastMessageID
			}
		}
		if domain.CompareMessageIDs(watermark, latest.ID) >= 0 {
			continue
		}
		candidates = append(candidates, candidate{
			gap:      EpisodeProjectionGap{UserID: session.UserID, SessionID: session.ID, LatestMessageID: latest.ID},
			lastSeen: latest.CreatedAt,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].lastSeen.Before(candidates[j].lastSeen)
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	gaps := make([]EpisodeProjectionGap, 0, len(candidates))
	for _, item := range candidates {
		gaps = append(gaps, item.gap)
	}
	return gaps, nil
}

func (r *InMemoryRepository) UpsertEpisodeDesired(_ context.Context, userID, sessionID, desiredMessageID string, availableAt time.Time) (*domain.EpisodeProjectionJob, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" ||
		strings.TrimSpace(desiredMessageID) == "" || availableAt.IsZero() {
		return nil, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	if !r.activeEpisodeSessionLocked(userID, sessionID) {
		return nil, ErrEpisodeJobNotFound
	}
	key := episodeSessionKey{userID, sessionID}
	now := time.Now().UTC()
	if jobID, exists := state.jobBySess[key]; exists {
		job := state.jobs[jobID]
		// A higher desired message revives and re-arms; it never steals a live
		// lease, and it never moves the desired watermark back.
		if domain.CompareMessageIDs(desiredMessageID, job.DesiredMessageID) > 0 {
			job.DesiredMessageID = desiredMessageID
			// The debounce window re-arms from the newest message even while a
			// projection is in flight - the lease is never stolen, but the
			// requeue after it waits out the quiet window from the newest
			// message. This mirrors the durable store exactly.
			job.AvailableAt = availableAt
			if job.Status != domain.EpisodeProjectionProcessing {
				job.Status = domain.EpisodeProjectionPending
				job.Attempts = 0
				job.ClaimedMessageID = ""
				job.LeaseToken = ""
				job.LeaseEndsAt = nil
				job.CompletedAt = nil
				job.ErrorCode = ""
			}
			job.UpdatedAt = now
			state.jobs[jobID] = job
		}
		stored := state.jobs[jobID]
		return cloneEpisodeJob(&stored), nil
	}
	job := domain.EpisodeProjectionJob{
		ID: newEpisodeID("epjob"), UserID: userID, SessionID: sessionID,
		DesiredMessageID: desiredMessageID, Status: domain.EpisodeProjectionPending,
		AvailableAt: availableAt, CreatedAt: now, UpdatedAt: now,
	}
	if err := job.ValidateForCreate(); err != nil {
		return nil, ErrInvalidArgument
	}
	state.jobs[job.ID] = job
	state.jobBySess[key] = job.ID
	return cloneEpisodeJob(&job), nil
}

func (r *InMemoryRepository) ListDueEpisodeJobs(_ context.Context, limit, maxAttempts int, now time.Time) ([]domain.EpisodeProjectionJob, error) {
	if limit < 1 || limit > 200 || maxAttempts < 1 || maxAttempts > 10 || now.IsZero() {
		return nil, ErrInvalidArgument
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	state := r.episodeStateLocked()
	var due []domain.EpisodeProjectionJob
	for _, job := range state.jobs {
		if job.Status != domain.EpisodeProjectionPending || job.AvailableAt.After(now) || job.Attempts >= maxAttempts {
			continue
		}
		if !r.activeEpisodeSessionLocked(job.UserID, job.SessionID) {
			continue
		}
		due = append(due, job)
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].AvailableAt.Equal(due[j].AvailableAt) {
			return due[i].AvailableAt.Before(due[j].AvailableAt)
		}
		return due[i].CreatedAt.Before(due[j].CreatedAt)
	})
	if len(due) > limit {
		due = due[:limit]
	}
	return cloneEpisodeJobs(due), nil
}

func (r *InMemoryRepository) ClaimEpisodeJob(_ context.Context, userID, jobID string, now time.Time, lease time.Duration, maxAttempts int) (*domain.EpisodeProjectionJob, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(jobID) == "" || now.IsZero() ||
		lease <= 0 || maxAttempts < 1 || maxAttempts > 10 {
		return nil, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	job, exists := state.jobs[jobID]
	if !exists || job.UserID != userID {
		return nil, ErrEpisodeJobNotFound
	}
	if !r.activeEpisodeSessionLocked(userID, job.SessionID) {
		return nil, ErrEpisodeJobNotFound
	}
	if job.Status != domain.EpisodeProjectionPending || job.AvailableAt.After(now) || job.Attempts >= maxAttempts {
		return nil, ErrEpisodeJobConflict
	}
	leaseEndsAt := now.Add(lease)
	job.Status = domain.EpisodeProjectionProcessing
	job.Attempts++
	job.ClaimedMessageID = job.DesiredMessageID
	job.LeaseToken = newEpisodeLeaseToken()
	job.LeaseEndsAt = &leaseEndsAt
	job.ErrorCode = ""
	job.CompletedAt = nil
	job.UpdatedAt = now
	state.jobs[jobID] = job
	return cloneEpisodeJob(&job), nil
}

func (r *InMemoryRepository) CompleteOrRequeueEpisodeJob(_ context.Context, claimed *domain.EpisodeProjectionJob, coveredMessageID string, now time.Time) (*domain.EpisodeProjectionJob, error) {
	if claimed == nil || strings.TrimSpace(claimed.UserID) == "" || strings.TrimSpace(claimed.ID) == "" ||
		claimed.LeaseToken == "" || strings.TrimSpace(coveredMessageID) == "" || now.IsZero() {
		return nil, ErrInvalidArgument
	}
	if coveredMessageID != claimed.ClaimedMessageID {
		return nil, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	job, exists := state.jobs[claimed.ID]
	if !exists || job.UserID != claimed.UserID || job.Status != domain.EpisodeProjectionProcessing ||
		job.LeaseToken != claimed.LeaseToken {
		return nil, ErrEpisodeJobConflict
	}
	if job.ClaimedMessageID != coveredMessageID {
		return nil, ErrEpisodeJobConflict
	}
	job.ClaimedMessageID = ""
	job.LeaseToken = ""
	job.LeaseEndsAt = nil
	job.ErrorCode = ""
	if domain.CompareMessageIDs(job.DesiredMessageID, coveredMessageID) > 0 {
		job.Status = domain.EpisodeProjectionPending
		job.Attempts = 0
		job.CompletedAt = nil
	} else {
		job.Status = domain.EpisodeProjectionCompleted
		completedAt := now
		job.CompletedAt = &completedAt
	}
	job.UpdatedAt = now
	state.jobs[job.ID] = job
	return cloneEpisodeJob(&job), nil
}

func (r *InMemoryRepository) RetryEpisodeJob(_ context.Context, claimed *domain.EpisodeProjectionJob, errorCode string, availableAt time.Time, maxAttempts int) (*domain.EpisodeProjectionJob, error) {
	if claimed == nil || strings.TrimSpace(claimed.UserID) == "" || strings.TrimSpace(claimed.ID) == "" ||
		claimed.LeaseToken == "" || availableAt.IsZero() || maxAttempts < 1 || maxAttempts > 10 ||
		!episodeErrorCodeValid(errorCode) {
		return nil, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	job, exists := state.jobs[claimed.ID]
	if !exists || job.UserID != claimed.UserID || job.Status != domain.EpisodeProjectionProcessing ||
		job.LeaseToken != claimed.LeaseToken {
		return nil, ErrEpisodeJobConflict
	}
	// The comparison reads the claimed target as it was, before the release
	// below clears it: a newer desired target restarts the budget rather than
	// inheriting this failure.
	desiredAdvanced := domain.CompareMessageIDs(job.DesiredMessageID, job.ClaimedMessageID) > 0
	job.ClaimedMessageID = ""
	job.LeaseToken = ""
	job.LeaseEndsAt = nil
	job.ErrorCode = errorCode
	now := time.Now().UTC()
	switch {
	case desiredAdvanced:
		job.Status = domain.EpisodeProjectionPending
		job.Attempts = 0
		if availableAt.After(job.AvailableAt) {
			job.AvailableAt = availableAt
		}
		job.CompletedAt = nil
	case job.Attempts >= maxAttempts:
		job.Status = domain.EpisodeProjectionFailed
		job.CompletedAt = &now
	default:
		job.Status = domain.EpisodeProjectionPending
		job.AvailableAt = availableAt
		job.CompletedAt = nil
	}
	job.UpdatedAt = now
	state.jobs[job.ID] = job
	return cloneEpisodeJob(&job), nil
}

// RecoverExpiredEpisodeJobs reclaims leases nobody will release. The branch
// conditions mirror the guarded-update form: a row that moved on between the
// scan and its update simply matches nothing, so the scan-then-write split
// cannot resurrect a job that another caller just finished.
func (r *InMemoryRepository) RecoverExpiredEpisodeJobs(_ context.Context, now time.Time, maxAttempts int) (int64, error) {
	if now.IsZero() || maxAttempts < 1 || maxAttempts > 10 {
		return 0, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	var ids []string
	for id, job := range state.jobs {
		if job.Status != domain.EpisodeProjectionProcessing || job.LeaseEndsAt == nil || job.LeaseEndsAt.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > recoverExpiredEpisodeBatch {
		ids = ids[:recoverExpiredEpisodeBatch]
	}
	var affected int64
	for _, id := range ids {
		job := state.jobs[id]
		if job.Status != domain.EpisodeProjectionProcessing || job.LeaseEndsAt == nil || job.LeaseEndsAt.After(now) {
			continue
		}
		// The comparison reads the claimed target as it was when the worker
		// died, before the fields below clear it: a worker that died on message
		// 10 must not make message 20 inherit its failure.
		desiredAdvanced := domain.CompareMessageIDs(job.DesiredMessageID, job.ClaimedMessageID) > 0
		job.ClaimedMessageID = ""
		job.LeaseToken = ""
		job.LeaseEndsAt = nil
		job.ErrorCode = "worker_interrupted"
		switch {
		case desiredAdvanced:
			job.Status = domain.EpisodeProjectionPending
			job.Attempts = 0
			job.CompletedAt = nil
		case job.Attempts >= maxAttempts:
			job.Status = domain.EpisodeProjectionFailed
			completedAt := now
			job.CompletedAt = &completedAt
		default:
			job.Status = domain.EpisodeProjectionPending
			job.AvailableAt = now
			job.CompletedAt = nil
		}
		job.UpdatedAt = now
		state.jobs[id] = job
		affected++
	}
	return affected, nil
}

func (r *InMemoryRepository) ReleaseOtherEpisodeSessions(_ context.Context, userID, currentSessionID string, now time.Time) (int64, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(currentSessionID) == "" || now.IsZero() {
		return 0, ErrInvalidArgument
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.episodeStateLocked()
	var ids []string
	for id, job := range state.jobs {
		if job.UserID != userID || job.SessionID == currentSessionID ||
			job.Status != domain.EpisodeProjectionPending || !job.AvailableAt.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	// Keep a global primary-key order: concurrent releases exclude different
	// sessions but touch shared rows in this same order.
	sort.Strings(ids)
	var released int64
	for _, id := range ids {
		job := state.jobs[id]
		job.AvailableAt = now
		job.UpdatedAt = now
		state.jobs[id] = job
		released++
	}
	return released, nil
}

// episodeErrorCodeValid delegates to the domain's closed-vocabulary check.
func episodeErrorCodeValid(code string) bool { return domain.ValidEpisodeErrorCode(code) }

func sortEpisodeRows(rows []domain.Episode) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
}

func cloneEpisode(episode domain.Episode) *domain.Episode {
	copy := episode
	copy.TopicKeys = append([]string(nil), episode.TopicKeys...)
	copy.Document.Topics = append([]domain.EpisodeTopic(nil), episode.Document.Topics...)
	copy.Document.Decisions = cloneEpisodeEvidenceList(episode.Document.Decisions)
	copy.Document.OpenLoops = cloneEpisodeEvidenceList(episode.Document.OpenLoops)
	copy.Document.UserCorrections = cloneEpisodeEvidenceList(episode.Document.UserCorrections)
	copy.Document.Preferences = cloneEpisodeEvidenceList(episode.Document.Preferences)
	copy.Document.DurableExperiences = cloneEpisodeEvidenceList(episode.Document.DurableExperiences)
	copy.Document.SourceHandles = append([]string(nil), episode.Document.SourceHandles...)
	copy.SourceEpisodes = append(domain.EpisodeSourceRefs(nil), episode.SourceEpisodes...)
	return &copy
}

func cloneEpisodeEvidenceList(entries []domain.EpisodeEvidence) []domain.EpisodeEvidence {
	if len(entries) == 0 {
		return nil
	}
	out := make([]domain.EpisodeEvidence, len(entries))
	for index, entry := range entries {
		out[index] = entry
		out[index].EvidenceMessageIDs = append([]string(nil), entry.EvidenceMessageIDs...)
	}
	return out
}

func cloneEpisodes(rows []domain.Episode) []domain.Episode {
	out := make([]domain.Episode, len(rows))
	for index, row := range rows {
		out[index] = *cloneEpisode(row)
	}
	return out
}

func cloneEpisodeJob(job *domain.EpisodeProjectionJob) *domain.EpisodeProjectionJob {
	copy := *job
	if job.LeaseEndsAt != nil {
		leaseEndsAt := *job.LeaseEndsAt
		copy.LeaseEndsAt = &leaseEndsAt
	}
	if job.CompletedAt != nil {
		completedAt := *job.CompletedAt
		copy.CompletedAt = &completedAt
	}
	return &copy
}

func cloneEpisodeJobs(jobs []domain.EpisodeProjectionJob) []domain.EpisodeProjectionJob {
	out := make([]domain.EpisodeProjectionJob, len(jobs))
	for index := range jobs {
		out[index] = *cloneEpisodeJob(&jobs[index])
	}
	return out
}
