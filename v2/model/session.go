package model

import (
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

type SessionMode string

const (
	SessionModeNormal    SessionMode = "normal"
	SessionModeRAG       SessionMode = "rag"
	SessionModeAgent     SessionMode = "agent"
	SessionModeMCPLegacy SessionMode = "mcp_legacy"
	SessionModeOllama    SessionMode = "ollama"
)

func ParseSessionMode(value string) (SessionMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", string(SessionModeNormal):
		return SessionModeNormal, nil
	case "2", string(SessionModeRAG):
		return SessionModeRAG, nil
	case "3", "mcp", string(SessionModeMCPLegacy):
		return SessionModeMCPLegacy, nil
	case "4", string(SessionModeOllama):
		return SessionModeOllama, nil
	case "5", string(SessionModeAgent):
		return SessionModeAgent, nil
	default:
		return "", fmt.Errorf("unsupported session mode %q", value)
	}
}

func (m SessionMode) Valid() bool {
	_, err := ParseSessionMode(string(m))
	return err == nil
}

func (m SessionMode) ModelType() (string, error) {
	switch m {
	case SessionModeNormal:
		return "1", nil
	case SessionModeRAG:
		return "2", nil
	case SessionModeMCPLegacy:
		return "3", nil
	case SessionModeOllama:
		return "4", nil
	case SessionModeAgent:
		return "5", nil
	default:
		return "", fmt.Errorf("unsupported persisted session mode %q", m)
	}
}

type Session struct {
	ID                 string         `gorm:"primaryKey;type:varchar(36)" json:"id"`
	UserID             int64          `gorm:"not null;default:0;index:idx_sessions_user_updated,priority:1;uniqueIndex:idx_sessions_user_creation_request,priority:1" json:"user_id"`
	UserName           string         `gorm:"index;not null" json:"username"`
	Mode               SessionMode    `gorm:"type:varchar(32);not null;default:normal;index" json:"mode"`
	Title              string         `gorm:"type:varchar(100)" json:"title"`
	CreationRequestKey *string        `gorm:"type:varchar(64);uniqueIndex:idx_sessions_user_creation_request,priority:2" json:"-"`
	CreationInputHash  string         `gorm:"type:char(64)" json:"-"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `gorm:"index:idx_sessions_user_updated,priority:2" json:"updated_at"`
	DeletedAt          gorm.DeletedAt `gorm:"index" json:"-"`
}

type SessionInfo struct {
	SessionID string      `json:"sessionId"`
	Title     string      `json:"name"`
	Mode      SessionMode `json:"mode"`
	ModelType string      `json:"modelType"`
}

func (s *Session) ValidateForCreate() error {
	if s == nil {
		return fmt.Errorf("session is nil")
	}
	if strings.TrimSpace(s.ID) == "" {
		return fmt.Errorf("session id is required")
	}
	if s.UserID <= 0 {
		return fmt.Errorf("session user id must be positive")
	}
	if strings.TrimSpace(s.UserName) == "" {
		return fmt.Errorf("session username is required")
	}
	if !s.Mode.Valid() {
		return fmt.Errorf("invalid session mode %q", s.Mode)
	}
	if s.CreationRequestKey != nil {
		if !ValidRequestKey(*s.CreationRequestKey) {
			return fmt.Errorf("invalid session creation request key")
		}
		if len(s.CreationInputHash) != 64 {
			return fmt.Errorf("session creation input hash is required")
		}
	}
	return nil
}
