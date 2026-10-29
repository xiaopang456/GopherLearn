package memory

import (
	"GopherAI/model"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var queryTagAliases = map[string][]string{
	"go":          {"golang", "go语言", "go 语言"},
	"gin":         {"gin"},
	"gorm":        {"gorm"},
	"mysql":       {"mysql"},
	"redis":       {"redis"},
	"rabbitmq":    {"rabbitmq", "rabbit mq", "消息队列"},
	"etcd":        {"etcd"},
	"watch":       {"watch", "监听机制"},
	"rpc":         {"rpc", "远程过程调用"},
	"protocol":    {"协议", "codec", "编解码", "packetbuffer", "粘包", "拆包"},
	"docker":      {"docker", "容器"},
	"gopherai":    {"gopherai", "gopher ai"},
	"rag":         {"rag", "rrf", "bm25", "reciprocal rank fusion", "倒数排名融合", "混合召回", "知识库", "检索增强"},
	"embedding":   {"embedding", "vector", "vector search", "向量", "向量化", "嵌入"},
	"agent":       {"agent", "session", "会话", "智能体"},
	"memory":      {"memory", "dream", "dream run", "memory candidate", "长期记忆", "记忆系统", "候选记忆"},
	"context":     {"context", "session title", "会话标题", "长标题", "上下文", "窗口", "摘要"},
	"learning":    {"学习", "复习", "掌握", "薄弱点"},
	"testing":     {"测试", "测评", "验收", "test"},
	"deployment":  {"部署", "deploy", "上线"},
	"concurrency": {"并发", "goroutine", "channel", "mutex", "协程"},
	"network":     {"网络", "tcp", "http", "sse", "socket"},
	"security":    {"安全", "jwt", "鉴权", "认证", "权限"},
	"debugging":   {"bug", "debug", "调试", "报错", "错误", "故障"},
	"performance": {"性能", "压测", "benchmark", "延迟", "吞吐"},
	"api":         {"api", "接口"},
	"database":    {"数据库", "database", "sql"},
	"backend":     {"后端", "backend", "服务端"},
	"preference":  {"偏好", "习惯", "喜欢", "倾向", "先讲", "请先", "回答顺序", "讲解顺序", "请记住"},
	"project":     {"项目", "project", "边界", "方案", "目标", "设计"},
}

var asciiWordPattern = regexp.MustCompile(`[a-z0-9_+#.-]+`)

// NormalizeMemoryTags is the single fixed-vocabulary boundary shared by
// extraction and retrieval. Unknown/free-form values are deliberately dropped.
func NormalizeMemoryTags(values []string) model.StringList {
	return normalizeFixedTags(values, 5)
}

// normalizeFixedTags sorts before truncating, which is correct only when the
// caller has no preference between tags — GenerateQueryTags is one such case,
// because it iterates a map and its input order is not deterministic anyway.
func normalizeFixedTags(values []string, limit int) model.StringList {
	result := dedupeValidTags(values)
	sort.Strings(result)
	if len(result) > limit {
		result = result[:limit]
	}
	return result
}

// normalizeRankedTags truncates in the caller's order and sorts afterwards, so
// that a deliberate priority survives the cut while storage order stays stable.
//
// BUG-AGENT-023: the old code did this the other way round, dropping the most
// discriminating tags purely because of how they were spelled.
func normalizeRankedTags(values []string, limit int) model.StringList {
	result := dedupeValidTags(values)
	if len(result) > limit {
		result = result[:limit]
	}
	sort.Strings(result)
	return result
}

func dedupeValidTags(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if !model.ValidMemoryTag(value) || utf8.RuneCountInString(value) > 40 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func GenerateQueryTags(question, topic string) model.StringList {
	text := strings.ToLower(strings.TrimSpace(question + "\n" + topic))
	if text == "" {
		return nil
	}
	words := map[string]struct{}{}
	for _, word := range asciiWordPattern.FindAllString(text, -1) {
		words[word] = struct{}{}
	}
	matched := make([]string, 0, 5)
	for tag, aliases := range queryTagAliases {
		if _, exactTag := words[tag]; exactTag {
			matched = append(matched, tag)
			continue
		}
		for _, alias := range aliases {
			alias = strings.ToLower(alias)
			_, exactASCII := words[alias]
			containsText := strings.Contains(alias, " ") || !isASCII(alias)
			if exactASCII || (containsText && strings.Contains(text, alias)) {
				matched = append(matched, tag)
				break
			}
		}
	}
	return normalizeFixedTags(matched, 12)
}

// DeriveCandidateTags keeps the closed indexing vocabulary on the Go side.
// The LLM decides semantic content; it cannot invent storage or index metadata.
//
// Two AGENT-018 corrections are visible here.
//
// The evidence body, the subject and the content are weighted separately.
// Previously they were concatenated and the result truncated alphabetically,
// so a memory matching {agent, api, backend, context, memory, rag} kept the
// first four by spelling and dropped `memory` and `rag` — the two tags that
// actually identified it (BUG-AGENT-023).
//
// The per-type fallback tag is gone. Every project_fact used to be stamped
// with `project`, whose query aliases include 设计, 方案 and 目标, so any
// question containing one of those matched all of them at once and the tag
// stopped discriminating (BUG-AGENT-024). A memory that yields no tag now
// simply has fewer tags, and is reached through its topic key instead.
func DeriveCandidateTags(evidence Evidence, subject, content string) model.StringList {
	// Subject and content are the memory itself; the surrounding evidence is
	// context that may discuss several unrelated things. Both groups are
	// individually sorted by GenerateQueryTags, so the only ordering that
	// matters here is that the memory's own tags survive the cut first.
	ordered := append([]string(nil), GenerateQueryTags(strings.Join([]string{subject, content}, "\n"), "")...)
	ordered = append(ordered, GenerateQueryTags(evidence.Content, "")...)
	return normalizeRankedTags(ordered, 5)
}

func isASCII(value string) bool {
	for _, r := range value {
		if r > 127 {
			return false
		}
	}
	return true
}
