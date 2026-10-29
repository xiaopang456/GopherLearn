package gormmysql

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// EpisodeRecord is one episodic-memory card: the derived, versioned projection
// of one session. (user_id, session_id) is unique - a re-projection overwrites
// the same row and moves version forward - so there is exactly one current card
// per conversation and every older version is invalidated by the watermark.
//
// There is no foreign key to sessions and no database cascade, matching the
// message_sources table's rule: the delete cascade is explicit in the
// repository so the same statement sequence runs on every engine, and the
// source watermark keeps a card auditable against the messages it claims.
type EpisodeRecord struct {
	ID        string `gorm:"column:id;type:varchar(64);primaryKey"`
	UserID    string `gorm:"column:user_id;type:varchar(96);not null;uniqueIndex:idx_episode_owner_session,priority:1;index:idx_episode_owner_time,priority:1"`
	SessionID string `gorm:"column:session_id;type:varchar(96);not null;uniqueIndex:idx_episode_owner_session,priority:2;index:idx_episode_owner_time,priority:2"`
	// Title is bounded to 160 runes, which utf8mb4 can store in up to 640 bytes.
	Title       string `gorm:"column:title;type:varchar(768);not null"`
	ContentJSON string `gorm:"column:content_json;type:mediumtext;not null"`
	SearchText  string `gorm:"column:search_text;type:text;not null"`
	// TopicKeysJSON and SourceEpisodesJSON are JSON arrays stored as text. A
	// packed column rather than child tables, because neither list is ever
	// joined on or counted per element - both are read back whole with the card.
	TopicKeysJSON          string `gorm:"column:topic_keys_json;type:text;not null"`
	SourceEpisodesJSON     string `gorm:"column:source_episodes_json;type:text"`
	SourceSummaryVersion   int64  `gorm:"column:source_summary_version;type:bigint;not null;default:0"`
	SourceSummaryWatermark string `gorm:"column:source_summary_watermark;type:varchar(96);not null;default:''"`
	// SourceLastMessageID is the projection watermark. Message ids compare
	// lexicographically in creation order (domain.CompareMessageIDs); every
	// "already covered" comparison here is that string comparison.
	SourceLastMessageID string    `gorm:"column:source_last_message_id;type:varchar(96);not null;index"`
	LastMessageAt       time.Time `gorm:"column:last_message_at;not null"`
	Version             uint64    `gorm:"column:version;type:bigint;not null;default:1"`
	Status              string    `gorm:"column:status;type:varchar(16);not null;index"`
	AbstainReason       string    `gorm:"column:abstain_reason;type:varchar(32);not null;default:''"`
	IndexStatus         string    `gorm:"column:index_status;type:varchar(16);not null;index"`
	CreatedAt           time.Time `gorm:"column:created_at;not null"`
	UpdatedAt           time.Time `gorm:"column:updated_at;not null"`
}

func (EpisodeRecord) TableName() string { return "episode_memories" }

// EpisodeJobRecord is the durable debounce-and-lease record one background
// worker claims per session. desired_message_id may advance while a worker owns
// claimed_message_id; completion then requeues the same row instead of losing
// the newly arrived tail.
type EpisodeJobRecord struct {
	ID               string     `gorm:"column:id;type:varchar(64);primaryKey"`
	UserID           string     `gorm:"column:user_id;type:varchar(96);not null;uniqueIndex:idx_episode_jobs_owner_session,priority:1;index:idx_episode_jobs_status,priority:2"`
	SessionID        string     `gorm:"column:session_id;type:varchar(96);not null;uniqueIndex:idx_episode_jobs_owner_session,priority:2;index"`
	DesiredMessageID string     `gorm:"column:desired_message_id;type:varchar(96);not null"`
	ClaimedMessageID string     `gorm:"column:claimed_message_id;type:varchar(96);not null;default:''"`
	Status           string     `gorm:"column:status;type:varchar(16);not null;index:idx_episode_jobs_status,priority:1"`
	Attempts         int        `gorm:"column:attempts;type:int;not null;default:0"`
	AvailableAt      time.Time  `gorm:"column:available_at;not null;index"`
	LeaseToken       string     `gorm:"column:lease_token;type:varchar(64)"`
	LeaseEndsAt      *time.Time `gorm:"column:lease_ends_at;index"`
	ErrorCode        string     `gorm:"column:error_code;type:varchar(64)"`
	CreatedAt        time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;not null"`
	CompletedAt      *time.Time `gorm:"column:completed_at"`
}

func (EpisodeJobRecord) TableName() string { return "episode_projection_jobs" }

func (r EpisodeRecord) toDomain() (domain.Episode, error) {
	status := domain.EpisodeStatus(r.Status)
	if !status.Valid() {
		return domain.Episode{}, fmt.Errorf("%w: episode %q has status %q", errCorruptStoredData, r.ID, r.Status)
	}
	indexStatus := domain.EpisodeIndexStatus(r.IndexStatus)
	if !indexStatus.Valid() {
		return domain.Episode{}, fmt.Errorf("%w: episode %q has index status %q", errCorruptStoredData, r.ID, r.IndexStatus)
	}
	var document domain.EpisodeDocument
	if err := json.Unmarshal([]byte(r.ContentJSON), &document); err != nil {
		return domain.Episode{}, fmt.Errorf("%w: episode %q document: %v", errCorruptStoredData, r.ID, err)
	}
	var topicKeys []string
	if err := json.Unmarshal([]byte(r.TopicKeysJSON), &topicKeys); err != nil {
		return domain.Episode{}, fmt.Errorf("%w: episode %q topic keys: %v", errCorruptStoredData, r.ID, err)
	}
	var sources domain.EpisodeSourceRefs
	if strings.TrimSpace(r.SourceEpisodesJSON) != "" {
		if err := json.Unmarshal([]byte(r.SourceEpisodesJSON), &sources); err != nil {
			return domain.Episode{}, fmt.Errorf("%w: episode %q source refs: %v", errCorruptStoredData, r.ID, err)
		}
	}
	abstainReason := domain.AbstainReason(r.AbstainReason)
	if abstainReason != "" && !abstainReason.Valid() {
		return domain.Episode{}, fmt.Errorf("%w: episode %q has abstain reason %q", errCorruptStoredData, r.ID, r.AbstainReason)
	}
	return domain.Episode{
		ID:                     r.ID,
		UserID:                 r.UserID,
		SessionID:              r.SessionID,
		Title:                  r.Title,
		Document:               document,
		SearchText:             r.SearchText,
		TopicKeys:              topicKeys,
		SourceEpisodes:         sources,
		SourceSummaryVersion:   r.SourceSummaryVersion,
		SourceSummaryWatermark: r.SourceSummaryWatermark,
		SourceLastMessageID:    r.SourceLastMessageID,
		LastMessageAt:          r.LastMessageAt.UTC(),
		Version:                r.Version,
		Status:                 status,
		AbstainReason:          abstainReason,
		IndexStatus:            indexStatus,
		CreatedAt:              r.CreatedAt.UTC(),
		UpdatedAt:              r.UpdatedAt.UTC(),
	}, nil
}

// episodeDBTime rounds a Go time down to the millisecond precision the
// datetime(3) columns store. MySQL would otherwise round it to the nearest
// millisecond on write, and a value that comes back up to half a millisecond
// LATER than the Go value that wrote it makes every `available_at.After(now)`
// style check engine-dependent at the boundary.
func episodeDBTime(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

func episodeRecordFrom(episode domain.Episode) (EpisodeRecord, error) {
	contentJSON, err := json.Marshal(episode.Document)
	if err != nil {
		return EpisodeRecord{}, fmt.Errorf("encode episode document: %w", err)
	}
	topicKeysJSON, err := json.Marshal(episode.TopicKeys)
	if err != nil {
		return EpisodeRecord{}, fmt.Errorf("encode episode topic keys: %w", err)
	}
	sourceJSON := ""
	if len(episode.SourceEpisodes) > 0 {
		encoded, err := json.Marshal(episode.SourceEpisodes)
		if err != nil {
			return EpisodeRecord{}, fmt.Errorf("encode episode source refs: %w", err)
		}
		sourceJSON = string(encoded)
	}
	return EpisodeRecord{
		ID:                     episode.ID,
		UserID:                 episode.UserID,
		SessionID:              episode.SessionID,
		Title:                  episode.Title,
		ContentJSON:            string(contentJSON),
		SearchText:             episode.SearchText,
		TopicKeysJSON:          string(topicKeysJSON),
		SourceEpisodesJSON:     sourceJSON,
		SourceSummaryVersion:   episode.SourceSummaryVersion,
		SourceSummaryWatermark: episode.SourceSummaryWatermark,
		SourceLastMessageID:    episode.SourceLastMessageID,
		LastMessageAt:          episodeDBTime(episode.LastMessageAt),
		Version:                episode.Version,
		Status:                 string(episode.Status),
		AbstainReason:          string(episode.AbstainReason),
		IndexStatus:            string(episode.IndexStatus),
		CreatedAt:              episodeDBTime(episode.CreatedAt),
		UpdatedAt:              episodeDBTime(episode.UpdatedAt),
	}, nil
}

func episodeJobRecordFrom(job domain.EpisodeProjectionJob) EpisodeJobRecord {
	return EpisodeJobRecord{
		ID:               job.ID,
		UserID:           job.UserID,
		SessionID:        job.SessionID,
		DesiredMessageID: job.DesiredMessageID,
		ClaimedMessageID: job.ClaimedMessageID,
		Status:           string(job.Status),
		Attempts:         job.Attempts,
		AvailableAt:      episodeDBTime(job.AvailableAt),
		LeaseToken:       job.LeaseToken,
		LeaseEndsAt:      episodeDBTimePointer(job.LeaseEndsAt),
		ErrorCode:        job.ErrorCode,
		CreatedAt:        episodeDBTime(job.CreatedAt),
		UpdatedAt:        episodeDBTime(job.UpdatedAt),
		CompletedAt:      episodeDBTimePointer(job.CompletedAt),
	}
}

// episodeDBTimePointer is episodeDBTime for the nullable time columns.
func episodeDBTimePointer(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	value := episodeDBTime(*t)
	return &value
}

func (r EpisodeJobRecord) toDomain() domain.EpisodeProjectionJob {
	return domain.EpisodeProjectionJob{
		ID:               r.ID,
		UserID:           r.UserID,
		SessionID:        r.SessionID,
		DesiredMessageID: r.DesiredMessageID,
		ClaimedMessageID: r.ClaimedMessageID,
		Status:           domain.EpisodeProjectionStatus(r.Status),
		Attempts:         r.Attempts,
		AvailableAt:      r.AvailableAt.UTC(),
		LeaseToken:       r.LeaseToken,
		LeaseEndsAt:      r.LeaseEndsAt,
		ErrorCode:        r.ErrorCode,
		CreatedAt:        r.CreatedAt.UTC(),
		UpdatedAt:        r.UpdatedAt.UTC(),
		CompletedAt:      r.CompletedAt,
	}
}
