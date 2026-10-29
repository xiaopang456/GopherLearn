package learning

import (
	"GopherAI/model"
	"context"
	"fmt"
	"strings"
)

// ListTopicQuestions returns every recently generated question for one owner
// and deterministic topic key, regardless of whether it was answered. A shown
// or skipped question is still exposed content and must participate in future
// novelty checks.
func (r *Repository) ListTopicQuestions(ctx context.Context, userID int64, topicKey string, limit int) ([]model.LearningQuestion, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	topicKey = strings.TrimSpace(topicKey)
	if userID <= 0 || topicKey == "" || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("invalid topic question history request")
	}
	var questions []model.LearningQuestion
	err := r.db.WithContext(ctx).
		Table("learning_questions AS q").
		Select("q.*").
		Joins("JOIN learning_sessions AS ls ON ls.id = q.learning_session_id AND ls.user_id = q.user_id").
		Where("q.user_id = ? AND ls.topic_key = ?", userID, topicKey).
		Order("q.created_at DESC, q.id DESC").
		Limit(limit).
		Find(&questions).Error
	return questions, err
}
