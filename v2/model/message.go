package model

import (
	"GopherAI/common/rag"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

type MessageSources []rag.Source

func (s MessageSources) Value() (driver.Value, error) {
	if s == nil {
		s = MessageSources{}
	}
	return json.Marshal(s)
}

func (s *MessageSources) Scan(value interface{}) error {
	if value == nil {
		*s = MessageSources{}
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan message sources: unsupported type %T", value)
	}
	if len(data) == 0 || string(data) == "null" {
		*s = MessageSources{}
		return nil
	}
	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("scan message sources: %w", err)
	}
	if *s == nil {
		*s = MessageSources{}
	}
	return nil
}

type Message struct {
	ID              uint                `gorm:"primaryKey;autoIncrement" json:"id"`
	PersistenceKey  *string             `gorm:"type:char(36);uniqueIndex:idx_messages_persistence_key" json:"-"`
	AgentRunID      *string             `gorm:"type:varchar(36);uniqueIndex:idx_messages_agent_run_role,priority:1" json:"-"`
	AgentRunRole    *string             `gorm:"type:varchar(16);uniqueIndex:idx_messages_agent_run_role,priority:2" json:"-"`
	SessionID       string              `gorm:"index;not null;type:varchar(36)" json:"session_id"`
	UserName        string              `gorm:"type:varchar(20)" json:"username"`
	Content         string              `gorm:"type:text" json:"content"`
	IsUser          bool                `gorm:"not null;" json:"is_user"`
	Sources         MessageSources      `gorm:"type:json" json:"sources"`
	EpisodeSources  EpisodeSourceRefs   `gorm:"type:json" json:"-"`
	AnswerBasis     rag.AnswerBasis     `gorm:"type:varchar(32);not null;default:legacy_unspecified" json:"answer_basis"`
	KnowledgeStatus rag.KnowledgeStatus `gorm:"type:varchar(32);not null;default:legacy_unspecified" json:"knowledge_status"`
	KnowledgeReason rag.KnowledgeReason `gorm:"type:varchar(48);not null;default:none" json:"knowledge_reason"`
	CreatedAt       time.Time           `json:"created_at"`
}

const (
	MessageAgentRunRoleUser      = "user"
	MessageAgentRunRoleAssistant = "assistant"
)

type History struct {
	ID              uint                `json:"id"`
	IsUser          bool                `json:"is_user"`
	Content         string              `json:"content"`
	Sources         MessageSources      `json:"sources"`
	AnswerBasis     rag.AnswerBasis     `json:"answer_basis"`
	KnowledgeStatus rag.KnowledgeStatus `json:"knowledge_status"`
	KnowledgeReason rag.KnowledgeReason `json:"knowledge_reason"`
	CreatedAt       time.Time           `json:"created_at"`
}
