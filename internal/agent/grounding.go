package agent

import (
	"strings"
	"unicode"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// This file carries the grounding boundary the harness enforces: which
// questions must be answered from the learner's own documents, which are
// answered from the conversation in front of the model, and which must be
// refused. The classifiers below are a verbatim restoration of the reference
// project's, because the refusal rules, the evidence reserve and the tool
// offers all hang off the same predicates and a reworded marker list here
// would silently move all three.

// documentKnowledgeReachable reports whether knowledge_search and document_read
// can emit any evidence in this turn. It is the single definition behind three
// decisions that must agree: the window reserves evidence tokens only when it
// returns true, the tool offers exclude the two tools when it returns false,
// and the final citation check excuses a document-less answer on the questions
// it refuses for. TestDocumentKnowledgeReserveMatchesToolRegistration guards
// the pairing.
func documentKnowledgeReachable(question string) bool {
	return !isSessionContextQuestion(question)
}

// isSessionContextQuestion identifies explicit requests to write or recall
// facts from the current conversation. These facts are already grounded by the
// recent window and the session summary, and must not be forced through the
// personal document knowledge base.
func isSessionContextQuestion(question string) bool {
	normalized := strings.ToLower(strings.TrimSpace(question))
	if normalized == "" {
		return false
	}
	markers := []string{
		"请记住", "帮我记住", "记一下", "记住这个", "你还记得", "还记得我",
		"刚才", "之前我们", "前面我们", "本次对话", "当前对话", "本会话", "当前会话",
		"会话事实", "会话记忆", "记忆检查", "recent messages", "session summary",
		"不需要知识库", "无需知识库", "不要检索知识库", "只使用 recent", "只使用 session",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// isHistoryRecallQuestion recognises a question about what the learner
// themselves said or experienced. It decides which refusals and fallbacks
// apply, and it is kept beside the two classifiers above because the three
// marker lists share phrases and must not drift apart silently.
func isHistoryRecallQuestion(question string) bool {
	normalized := strings.ToLower(strings.TrimSpace(question))
	markers := []string{
		"你还记得", "还记得我", "我当时", "我之前", "我上次", "之前我", "上次我",
		"我的原话", "我说了什么", "我怎么说", "当时怎么说", "一字不差", "逐字", "原文",
		"之前我们", "前面我们", "本次对话", "当前对话", "本会话", "当前会话",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// requiresExactHistoryEvidence marks the questions whose only honest answer
// quotes the learner verbatim. A paraphrase is not an answer here, so a turn
// that could not read the words back is refused rather than let approximate.
func requiresExactHistoryEvidence(question string) bool {
	normalized := strings.ToLower(strings.TrimSpace(question))
	markers := []string{"原话", "一字不差", "逐字", "原文", "我说了什么", "我怎么说", "当时怎么说"}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// isCrossUserKnowledgeRequest recognizes only explicit requests for another
// user's private resources. It never looks at a file name or user identifier:
// authorization remains server-owned, and the harness simply avoids a
// pointless current-user search before returning the stable refusal.
func isCrossUserKnowledgeRequest(question string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(question), ""))
	if normalized == "" {
		return false
	}
	otherUserMarkers := []string{
		"其他用户", "其它用户", "另一个用户", "另一位用户", "另一名用户", "别的用户", "他人用户",
		"其他账号", "其它账号", "另一个账号", "别的账号", "他人账号",
		"别人的", "他人的",
	}
	resourceMarkers := []string{
		"笔记", "知识库", "文档", "文件", "上传", "聊天记录", "对话记录", "会话", "记忆",
	}
	hasOtherUser := false
	for _, marker := range otherUserMarkers {
		if strings.Contains(normalized, marker) {
			hasOtherUser = true
			break
		}
	}
	if !hasOtherUser {
		return false
	}
	for _, marker := range resourceMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

// requiresGrounding reports whether a question must rest on document evidence
// rather than on whatever the model happens to know. Only explicit
// session-context requests and ordinary greetings are exempt; the narrow
// exemptions are what keep an ordinary technical question from being answered
// out of the model's own head.
func requiresGrounding(question string) bool {
	if isSessionContextQuestion(question) {
		return false
	}
	normalized := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(question))
	if normalized == "" {
		return false
	}
	_, direct := directConversationQuestions[normalized]
	return !direct
}

// requiresKnowledgeOnly can only tighten a server/request policy. The narrow
// phrases avoid treating ordinary mentions of notes as a refusal to use a
// clearly labelled general-knowledge fallback; the UI enum remains primary.
func requiresKnowledgeOnly(question string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(question), ""))
	markers := []string{
		"只根据我的笔记", "只能根据我的笔记", "仅根据我的笔记",
		"只根据我的知识库", "只能根据我的知识库", "仅根据我的知识库",
		"只用我的笔记", "只能用我的笔记", "只用知识库", "只能用知识库",
	}
	for _, marker := range markers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

var directConversationQuestions = map[string]struct{}{
	"你好": {}, "您好": {}, "嗨": {}, "哈喽": {}, "hello": {}, "hi": {}, "hey": {}, "在吗": {},
	"早上好": {}, "下午好": {}, "晚上好": {}, "谢谢": {}, "谢谢你": {}, "感谢": {}, "多谢": {},
	"不客气": {}, "再见": {}, "拜拜": {}, "你是谁": {}, "你叫什么": {}, "你叫什么名字": {},
	"介绍一下自己": {}, "请介绍一下自己": {}, "你能做什么": {}, "你会做什么": {}, "请问你能做什么": {},
	"help": {}, "帮助": {},
}

// withoutDocumentKnowledgeTools drops the two document-backed tool offers for
// a turn that cannot ground from documents anyway. The offers, not the
// registry, are filtered: a model that asks for one of them anyway is answered
// by the run-scoped registry the same way it would answer an unknown tool,
// which is the same shape the reference project produces.
func withoutDocumentKnowledgeTools(definitions []model.ToolDefinition) []model.ToolDefinition {
	kept := make([]model.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if isDocumentKnowledgeTool(definition.Name) {
			continue
		}
		kept = append(kept, definition)
	}
	return kept
}

// isDocumentKnowledgeTool names the tools that ground answers in uploaded
// documents. documentKnowledgeReachable's false branch must remove exactly
// this set and no other. DocumentReadName is declared here rather than beside
// the tool so this predicate cannot be compiled against an incomplete set.
const DocumentReadName = "document_read"

func isDocumentKnowledgeTool(name string) bool {
	switch strings.TrimSpace(name) {
	case KnowledgeSearchName, DocumentReadName:
		return true
	default:
		return false
	}
}
