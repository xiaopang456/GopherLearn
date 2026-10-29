package utils

import (
	"GopherAI/model"
	cryptorand "crypto/rand"
	"fmt"
	"math/big"

	"github.com/cloudwego/eino/schema"
)

func SecureRandomDigits(length int) (string, error) {
	if length <= 0 || length > 128 {
		return "", fmt.Errorf("invalid random digit length %d", length)
	}
	result := make([]byte, length)
	for i := range result {
		digit, err := cryptorand.Int(cryptorand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		result[i] = byte('0' + digit.Int64())
	}
	return string(result), nil
}

// 将 schema 消息转换为数据库可存储的格式
func ConvertToModelMessage(sessionID string, userName string, msg *schema.Message) *model.Message {
	return &model.Message{
		SessionID: sessionID,
		UserName:  userName,
		Content:   msg.Content,
	}
}

// 将数据库消息转换为 schema 消息（供 AI 使用）
func ConvertToSchemaMessages(msgs []*model.Message) []*schema.Message {
	schemaMsgs := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		role := schema.Assistant
		if m.IsUser {
			role = schema.User
		}
		schemaMsgs = append(schemaMsgs, &schema.Message{
			Role:    role,
			Content: m.Content,
		})
	}
	return schemaMsgs
}
