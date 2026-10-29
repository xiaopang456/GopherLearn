package session

import (
	"GopherAI/common/code"
	"GopherAI/model"
	"context"
)

// CreateForAssembly exposes the existing title-aware creation implementation.
// Its quotas, mode rules and Agent request-key requirements remain original.
func CreateForAssembly(ctx context.Context, userID int64, userName, title, mode, requestID string) (*model.Session, code.Code) {
	return createSession(ctx, userID, userName, title, mode, requestID)
}
