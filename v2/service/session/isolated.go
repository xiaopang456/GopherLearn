package session

import (
	"GopherAI/common/code"
	sessiondao "GopherAI/dao/session"
	"GopherAI/model"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/google/uuid"
	"log"
	"strings"
)

func GetUserSessions(ctx context.Context, userID int64) ([]model.SessionInfo, error) {
	sessions, err := sessiondao.DefaultRepository().ListByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	infos := make([]model.SessionInfo, 0, len(sessions))
	for i := range sessions {
		modelType, err := sessions[i].Mode.ModelType()
		if err != nil {
			log.Printf("GetUserSessions skip invalid mode: session=%s mode=%s", sessions[i].ID, sessions[i].Mode)
			continue
		}
		infos = append(infos, model.SessionInfo{
			SessionID: sessions[i].ID,
			Title:     sessions[i].Title,
			Mode:      sessions[i].Mode,
			ModelType: modelType,
		})
	}
	return infos, nil
}

func CreateEmptySession(ctx context.Context, userID int64, userName, requestedMode, requestID string) (string, code.Code) {
	created, resultCode := createSession(ctx, userID, userName, "新会话", requestedMode, requestID)
	if resultCode != code.CodeSuccess {
		return "", resultCode
	}
	return created.ID, code.CodeSuccess
}

func createSession(ctx context.Context, userID int64, userName, title, requestedMode, requestID string) (*model.Session, code.Code) {
	mode, err := model.ParseSessionMode(requestedMode)
	if err != nil {
		return nil, code.CodeInvalidParams
	}
	created := &model.Session{
		ID:       uuid.New().String(),
		UserID:   userID,
		UserName: userName,
		Mode:     mode,
		Title:    sessionTitle(title),
	}
	if mode == model.SessionModeAgent {
		key := strings.TrimSpace(requestID)
		if !model.ValidRequestKey(key) {
			return nil, code.CodeInvalidParams
		}
		created.CreationRequestKey = &key
		digest := sha256.Sum256([]byte(title))
		created.CreationInputHash = hex.EncodeToString(digest[:])
	}
	created, _, err = sessiondao.DefaultRepository().CreateOrGet(ctx, created)
	if err != nil {
		log.Println("createSession error:", err)
		if errors.Is(err, sessiondao.ErrSessionRequestConflict) {
			return nil, code.AgentRequestConflict
		}
		if errors.Is(err, sessiondao.ErrSessionLimitReached) {
			return nil, code.SessionLimitReached
		}
		return nil, code.CodeServerBusy
	}
	return created, code.CodeSuccess
}

func sessionTitle(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > 100 {
		return strings.TrimSpace(string(runes[:100]))
	}
	return value
}
