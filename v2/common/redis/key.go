package redis

import (
	"GopherAI/config"
	"fmt"
	"strconv"
)

const (
	KnowledgeChunkIndexName       = "gopherai_knowledge_chunks_v3_idx"
	KnowledgeChunkKeyPrefix       = "gopherai:knowledge:chunk:"
	KnowledgeChunkVectorDimension = 1024
)

// key:特定邮箱-> 验证码
func GenerateCaptcha(email string) string {
	return fmt.Sprintf(config.DefaultRedisKeyConfig.CaptchaPrefix, email)
}

func GenerateCaptchaCooldown(email string) string {
	return GenerateCaptcha(email) + ":cooldown"
}

// GenerateKnowledgeChunkKey groups keys by user/document/version while the
// RediSearch index itself remains shared by every user.
func GenerateKnowledgeChunkKey(userID int64, docID string, version int, chunkID string) string {
	return GenerateKnowledgeDocumentChunkPrefix(userID, docID, version) + chunkID
}

func GenerateKnowledgeDocumentChunkPrefix(userID int64, docID string, version int) string {
	return KnowledgeChunkKeyPrefix + strconv.FormatInt(userID, 10) + ":" + docID + ":" + strconv.Itoa(version) + ":"
}
