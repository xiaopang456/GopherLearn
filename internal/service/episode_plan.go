package service

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The episodic read plan. A question can say three independent things about the
// learner's history - that it is about their past at all, how it is bounded in
// time, and which kind of structured fact it asks for. The original project
// replaced a single-intent router with this plan because the router's first
// match won the whole question: "上次关于缓冲区复用的决定" collapsed to
// "last session" and returned whichever session happened to be newest. Time
// range, topic constraint and evidence type stay independent fields here, so
// none of them can silently delete another.

// EpisodeTemporalMode is how the plan wants history bounded in time.
type EpisodeTemporalMode string

const (
	// EpisodeTemporalNone asks for no time bound at all: relevance decides.
	EpisodeTemporalNone EpisodeTemporalMode = "none"
	// EpisodeTemporalLatestAny is the literal "what did we talk about last
	// time", which carries no topic once its shell words are removed.
	EpisodeTemporalLatestAny EpisodeTemporalMode = "latest_any"
	// EpisodeTemporalLatestMatching is "last time, about X": recency ranks the
	// relevant candidates, it does not replace relevance.
	EpisodeTemporalLatestMatching EpisodeTemporalMode = "latest_matching"
	// EpisodeTemporalRange is an explicit date or period.
	EpisodeTemporalRange EpisodeTemporalMode = "time_range"
)

// EpisodeEvidenceMode is which structured projection the question asks for. It
// is applied after the store reload against document fields, not as a search
// term: "决定" is a word that appears in plenty of episodes that recorded none.
type EpisodeEvidenceMode string

const (
	EpisodeEvidenceAny        EpisodeEvidenceMode = "any"
	EpisodeEvidenceDecision   EpisodeEvidenceMode = "decision"
	EpisodeEvidenceCorrection EpisodeEvidenceMode = "correction"
	EpisodeEvidenceOpenLoop   EpisodeEvidenceMode = "open_loop"
)

// EpisodeReadPlan is the deterministic result of reading one question.
type EpisodeReadPlan struct {
	NeedEpisode bool

	TemporalMode EpisodeTemporalMode
	From, To     time.Time

	EvidenceMode EpisodeEvidenceMode

	// TopicTerms are the lexical terms left after the history, evidence and
	// discourse shell has been removed. They drive the lexical channel and
	// decide whether "上次" still has a topic attached to it.
	TopicTerms []string
	// ExactTopicKeys are normalized topic-key candidates read straight off the
	// question. They are preferred, never required.
	ExactTopicKeys []string
	// ExactIdentifiers are strong code-shaped tokens, which embeddings handle
	// badly and exact search handles well.
	ExactIdentifiers []string
	// RequireExactTopic records that the question named a topic explicitly with
	// 关于/围绕. It raises the bar for what counts as relevant; it no longer
	// terminates the read when the exact key misses.
	RequireExactTopic bool

	ReasonCodes []string
}

// Reason is the primary reason code.
func (p EpisodeReadPlan) Reason() string {
	if len(p.ReasonCodes) == 0 {
		return ""
	}
	return p.ReasonCodes[0]
}

// HasTopicConstraint reports whether anything in the question still names a
// subject after the shell words are gone.
func (p EpisodeReadPlan) HasTopicConstraint() bool {
	return len(p.TopicTerms) > 0 || len(p.ExactIdentifiers) > 0 || len(p.ExactTopicKeys) > 0
}

// The marker vocabularies below are organised by the grammatical role a word
// plays, not by the questions that happened to fail. A list that grows one
// sentence at a time only ever covers the sentences already seen; these lists
// are meant to be read as "history pointer", "conversation reference",
// "evidence type" and "discourse shell", and extended by role.

// episodeLatestMarkers point at the most recent session rather than at a subject.
var episodeLatestMarkers = []string{
	"上一次会话", "上一次聊天", "上一轮会话", "上一轮聊天", "最近一次会话", "最近一次聊天",
	"上一个会话", "前一个会话", "前一轮会话", "上个会话",
	"上一次", "上一轮", "上一回", "最近一次", "前一次", "上次",
	"那一次", "那一轮", "那次", "那轮",
}

// episodePastMarkers place the question in the past without naming which session.
var episodePastMarkers = []string{
	"之前", "以前", "先前", "早先", "曾经", "当初", "当时", "后来", "过去", "历史", "原来", "原先", "最初", "一开始",
}

// episodeConversationMarkers name the act of having talked about something.
var episodeConversationMarkers = []string{
	"聊过", "聊到", "聊了", "聊完", "聊天", "讨论过", "讨论了", "讨论完", "讨论", "谈过", "谈完", "会话", "对话",
}

// episodeDecisionMarkers map to the decisions slot.
var episodeDecisionMarkers = []string{
	"决定", "定下来", "定下", "定了", "定的", "结论", "选择", "选了", "选的", "采用", "取舍", "拍板", "敲定", "为什么选",
}

// episodeCorrectionMarkers stay narrow on purpose. "改" alone is how a decision
// is described just as often as a correction, and a wrong evidence mode is a
// hard filter that removes the right episode.
var episodeCorrectionMarkers = []string{
	"纠正", "更正", "改掉", "改正", "推翻", "修正", "误解", "搞错", "想错",
}

var episodeOpenLoopMarkers = []string{
	"遗留问题", "遗留", "待定项", "待定", "未完成", "没做完", "还欠着", "还欠", "欠着", "开放问题", "悬而未决", "还缺", "还差", "缺的",
}

// episodeFirstPersonMarkers and episodeProcessInterrogatives implement the
// second route into "this is a question about my own history": a first person
// subject with a past decision or correction predicate that is being asked
// *about*. The interrogative requirement is what keeps "我给 X 定的策略是什么"
// out: that sentence uses the same verb attributively, asking for a current
// state rather than a past event.
var episodeFirstPersonMarkers = []string{"我们", "咱们", "我"}

var episodeProcessInterrogatives = []string{"怎么", "如何", "为什么", "为何", "哪条", "哪一条", "哪种", "哪一种", "起因", "理由", "依据"}

// episodeShellMarkers are removed before the remaining topic is read. They are
// the history pointers, the conversation nouns, the evidence vocabulary and the
// discourse frame a learner wraps a question in - everything that describes how
// the question is being asked rather than what it is about.
var episodeShellMarkers = concatEpisodeMarkers(
	episodeLatestMarkers, episodePastMarkers, episodeConversationMarkers,
	episodeDecisionMarkers, episodeCorrectionMarkers, episodeOpenLoopMarkers,
	[]string{
		// reference and person
		"这件事", "那件事", "这个", "那个", "这块", "那块", "这边", "那边", "这些", "那些", "那点", "这点", "自己",
		"我们", "咱们", "我", "你们", "你", "他们", "她们",
		// interrogatives and particles
		"是什么", "有什么", "为什么", "为何", "怎么样", "怎么", "如何", "哪一步", "哪一条", "哪一种", "哪条", "哪种", "哪些", "哪个",
		"什么", "多少", "是否", "能不能", "可不可以", "到什么程度", "来着", "吗", "呢",
		// speech-act frame. Single characters are deliberately absent here: "用"
		// and "对" are shell in isolation and the middle of a word everywhere
		// else, and removing them turns 缓冲区复用 into 缓冲区复 - a topic the
		// lexical channel can no longer find.
		"帮我", "给我", "说明", "解释", "讲清楚", "讲一讲", "讲讲", "说说", "回顾", "重讲", "一遍", "排一下", "一下",
		"结合", "配合", "考虑", "交代", "按照", "按我", "用我",
		// generic nouns that describe the answer rather than the subject
		"起因", "原因", "理由", "依据", "程度", "走向", "结果", "进展", "安排", "主题", "话题", "内容",
		"方式", "方案", "做法", "判断", "立场", "态度", "看法", "想法", "理解", "疑问", "疑惑", "困惑", "打算",
		// time and modality
		"现在", "目前", "平时", "一般", "通常", "常用", "一直", "最后", "最终", "下一步", "接下来", "等下",
		"之后", "以后", "那天", "当天",
	},
)

// episodeGenericTermStopWords remove terms that survive stripping but name no
// subject. They only affect whether a topic constraint exists, never what is
// searched.
var episodeGenericTermStopWords = map[string]struct{}{
	"事情": {}, "东西": {}, "地方": {}, "情况": {}, "时候": {}, "部分": {}, "问题": {},
	"要求": {}, "习惯": {}, "偏好": {}, "次序": {}, "顺序": {}, "详略": {}, "程度": {},
	"比方": {}, "说法": {}, "类比": {}, "改变": {}, "过来": {}, "得上": {}, "解决": {}, "推进": {},
	// Function-word pairs that survive stripping because neither half is shell
	// on its own. They name no subject, so they must not turn "上次聊了什么"
	// into a topic-constrained read.
	"在做": {}, "做的": {}, "有的": {}, "没有": {}, "这样": {}, "那样": {}, "怎样": {},
	"一个": {}, "一些": {}, "可以": {}, "需要": {}, "应该": {}, "知道": {}, "觉得": {},
}

func concatEpisodeMarkers(groups ...[]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 128)
	for _, group := range groups {
		for _, marker := range group {
			if _, duplicate := seen[marker]; duplicate {
				continue
			}
			seen[marker] = struct{}{}
			out = append(out, marker)
		}
	}
	// Longest first, so a phrase is removed before one of its own substrings
	// can cut it in half.
	sort.SliceStable(out, func(i, j int) bool {
		left, right := len([]rune(out[i])), len([]rune(out[j]))
		if left != right {
			return left > right
		}
		return out[i] < out[j]
	})
	return out
}

// EpisodeRouteRequest carries what the router may know about the question.
type EpisodeRouteRequest struct {
	Question string
	Now      time.Time
	// CurrentSessionHasHistory decides the "继续上次" branch, whose correct
	// answer depends on whether this session already has stored conversation.
	CurrentSessionHasHistory bool
}

var episodeDatePattern = regexp.MustCompile(`\b(20\d{2})-(\d{2})-(\d{2})\b`)

// episodeExplicitTopicTailPatterns remove only question predicates anchored at
// the end of the text after 关于/围绕. Topic names may themselves contain words
// such as 方案 or 选择, so those nouns are never stripped on their own.
var episodeExplicitTopicTailPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?:最终|最后|当时)?(?:选择|选|采用|采取)(?:的|了|的是|了的是)?(?:什么|哪(?:个|种)?)(?:方案|做法|方式)?$`),
	regexp.MustCompile(`(?:最终|最后|当时)?(?:定下|确定)(?:的|了|的是|了的是)?(?:什么|哪(?:个|种)?)(?:方案|做法|方式)?$`),
	regexp.MustCompile(`(?:最终|最后|当时)?(?:怎么|如何)(?:选择|决定|取舍|处理)(?:的)?$`),
	regexp.MustCompile(`(?:当时|最后|最终)?(?:是)?怎么做$`),
	regexp.MustCompile(`(?:当时|最后|最终)?的?取舍(?:是)?(?:什么|如何|怎样)?$`),
	regexp.MustCompile(`(?:聊|讨论)了什么$`),
	regexp.MustCompile(`(?:是)?什么$`),
	regexp.MustCompile(`怎么样$`),
	regexp.MustCompile(`的(?:决定|结论|选择|方案|取舍)(?:和|及|以及)(?:未完成项|开放问题)$`),
	regexp.MustCompile(`的(?:决定|结论|选择|方案|取舍|未完成项|开放问题)$`),
	regexp.MustCompile(`(?:决定|结论|取舍)$`),
	regexp.MustCompile(`(?:最终|最后|当时)?定(?:下来|下|了|的)?$`),
	regexp.MustCompile(`(?:讲清楚|讲一讲|讲讲|说说|说明|解释|回顾|重讲|复述|梳理)(?:一下|一遍)?$`),
}

// episodeExplicitTopicBoundaries end the topic named after 关于/围绕. A topic
// phrase ends where the sentence turns back to the speaker, to the past, or to
// punctuation.
var episodeExplicitTopicBoundaries = concatEpisodeMarkers(
	episodeLatestMarkers, episodePastMarkers, episodeConversationMarkers,
	[]string{
		"我们", "咱们", "我", "你们", "你", "他们", "她们", "他", "她",
		"是否", "还有", "还剩", "还", "该", "现在", "目前", "到底",
		"，", ",", "、", "。", "？", "?", "！", "!", "；", ";", "：", ":",
	},
)

// BuildEpisodeReadPlan reads one question into a plan. It is pure: same
// question and same clock, same plan, no provider and no database.
func BuildEpisodeReadPlan(request EpisodeRouteRequest) EpisodeReadPlan {
	question := strings.TrimSpace(request.Question)
	now := request.Now
	if now.IsZero() {
		now = time.Now()
	}
	plan := EpisodeReadPlan{TemporalMode: EpisodeTemporalNone, EvidenceMode: EpisodeEvidenceAny}
	if question == "" {
		plan.ReasonCodes = []string{"empty_query"}
		return plan
	}

	explicitTopic, hasExplicitTopic := episodeExplicitTopicKey(question)
	plan.RequireExactTopic = hasExplicitTopic
	core := episodeTopicCore(question)
	plan.TopicTerms = episodeTopicTerms(core)
	plan.ExactIdentifiers = episodeStrongIdentifiers(question)
	// Keys are read off the core only when the core still carries a subject.
	// Without that guard the leftovers of a purely temporal question
	// normalize into a key-shaped string, and a date query that the range alone
	// had answered correctly acquires a topic constraint it never expressed.
	if len(plan.TopicTerms) > 0 {
		plan.ExactTopicKeys = episodeTopicKeyCandidates(core)
	}
	if hasExplicitTopic {
		// A learner who wrote 关于 named the topic themselves. Keys guessed from
		// the rest of the sentence can only widen the exact channel with values
		// nothing carries, so the named one stands alone.
		plan.ExactTopicKeys = []string{explicitTopic}
	}
	plan.EvidenceMode = episodeEvidenceModeOf(question)

	// "继续上次" is a resumption, not a lookup. It keeps its own branch because
	// the correct answer depends on whether this session already has history,
	// which no other route consults.
	if containsAny(question,
		"继续上次", "接着上次", "继续上一次", "接着上一次", "继续上一轮", "接着上一轮",
		"继续最近一次", "接着最近一次", "继续前一个会话", "接着前一个会话",
		"继续之前", "接着之前", "上次继续",
	) {
		if request.CurrentSessionHasHistory {
			plan.ReasonCodes = []string{"current_session_has_history"}
			return plan
		}
		plan.NeedEpisode, plan.TemporalMode = true, EpisodeTemporalLatestAny
		plan.ReasonCodes = []string{"continue_empty_session"}
		return plan
	}

	if from, to, ok := episodeParseTimeRange(question, now); ok {
		plan.NeedEpisode, plan.TemporalMode, plan.From, plan.To = true, EpisodeTemporalRange, from, to
		plan.ReasonCodes = []string{"time_range"}
		if plan.HasTopicConstraint() {
			plan.ReasonCodes = append(plan.ReasonCodes, "time_range_with_topic")
		}
		return plan
	}

	historyReason, isHistory := episodeHistoryIntent(question)
	if !isHistory {
		plan.ReasonCodes = []string{"no_personal_history_intent"}
		return plan
	}
	plan.NeedEpisode = true
	plan.ReasonCodes = []string{historyReason}
	if containsAny(question, episodeLatestMarkers...) {
		if plan.HasTopicConstraint() {
			plan.TemporalMode = EpisodeTemporalLatestMatching
			plan.ReasonCodes = []string{"latest_matching", historyReason}
		} else {
			plan.TemporalMode = EpisodeTemporalLatestAny
			plan.ReasonCodes = []string{"latest_any", historyReason}
		}
		return plan
	}
	plan.TemporalMode = EpisodeTemporalNone
	return plan
}

// episodeHistoryIntent answers the first of the two questions the old router
// conflated: is this about the learner's own past at all?
func episodeHistoryIntent(question string) (string, bool) {
	switch {
	case containsAny(question, episodeLatestMarkers...):
		return "history_session_pointer", true
	case containsAny(question, episodePastMarkers...):
		return "history_past_pointer", true
	case containsAny(question, episodeConversationMarkers...):
		return "history_conversation_pointer", true
	case containsAny(question, episodeOpenLoopMarkers...) && containsAny(question, "这", "那", "还"):
		// An open loop is a reference to a state a past session established.
		// The demonstrative requirement is what separates "那个遗留问题" from a
		// textbook question about what a design is still missing.
		return "implicit_history_open_loop", true
	case episodeFirstPersonPastPredicate(question):
		return "implicit_history_decision", true
	}
	return "", false
}

func episodeFirstPersonPastPredicate(question string) bool {
	if !containsAny(question, episodeFirstPersonMarkers...) {
		return false
	}
	if !containsAny(question, episodeDecisionMarkers...) && !containsAny(question, episodeCorrectionMarkers...) {
		return false
	}
	if containsAny(question, "现在", "目前", "平时", "一般", "通常", "常用", "接下来", "下次", "以后") {
		return false
	}
	return containsAny(question, episodeProcessInterrogatives...)
}

func episodeEvidenceModeOf(question string) EpisodeEvidenceMode {
	switch {
	case containsAny(question, episodeCorrectionMarkers...):
		return EpisodeEvidenceCorrection
	case containsAny(question, episodeOpenLoopMarkers...):
		return EpisodeEvidenceOpenLoop
	case containsAny(question, episodeDecisionMarkers...):
		return EpisodeEvidenceDecision
	}
	return EpisodeEvidenceAny
}

// episodeTopicCore strips the shell so that what is left is the subject, if any.
func episodeTopicCore(question string) string {
	value := episodeDatePattern.ReplaceAllString(question, " ")
	for _, marker := range []string{"关于", "围绕", "昨天", "前天", "上周", "上一周", "本周", "这周", "上个月", "那天"} {
		value = strings.ReplaceAll(value, marker, " ")
	}
	for _, marker := range episodeShellMarkers {
		value = strings.ReplaceAll(value, marker, " ")
	}
	return value
}

func episodeTopicTerms(core string) []string {
	terms := extractEpisodeLexicalTerms(core)
	kept := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, generic := episodeGenericTermStopWords[term]; generic {
			continue
		}
		kept = append(kept, term)
	}
	return kept
}

// episodeTopicKeyCandidates proposes normalized keys from the stripped core.
// The whole core comes first because a learner naming a topic usually names it
// whole. A topic key is a name, not a clause: anything longer than a short
// name is a fragment of the question that survived stripping, and it can only
// widen the exact channel with a value nothing will ever carry.
func episodeTopicKeyCandidates(core string) []string {
	keys, seen := make([]string, 0, 4), map[string]struct{}{}
	add := func(value string) {
		key := domain.NormalizeEpisodeTopicKey(value)
		if len([]rune(key)) < 2 {
			return
		}
		if _, generic := episodeGenericTermStopWords[key]; generic {
			return
		}
		if len([]rune(key)) > 16 {
			return
		}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	fields := strings.Fields(core)
	add(strings.Join(fields, " "))
	for _, field := range fields {
		add(field)
	}
	if len(keys) > 4 {
		keys = keys[:4]
	}
	return keys
}

func episodeExplicitTopicKey(q string) (string, bool) {
	start := -1
	markerSize := 0
	for _, marker := range []string{"关于", "围绕"} {
		if index := strings.Index(q, marker); index >= 0 && (start < 0 || index < start) {
			start, markerSize = index, len(marker)
		}
	}
	if start < 0 {
		return "", false
	}
	value := episodeCutAtTopicBoundary(q[start+markerSize:])
	value = episodeTrimExplicitTopicQuestion(value)
	key := domain.NormalizeEpisodeTopicKey(value)
	return key, key != ""
}

func episodeCutAtTopicBoundary(value string) string {
	cut := len(value)
	for _, boundary := range episodeExplicitTopicBoundaries {
		if index := strings.Index(value, boundary); index >= 0 && index < cut {
			cut = index
		}
	}
	return strings.TrimRight(strings.TrimSpace(value[:cut]), " \t\r\n的了")
}

func episodeTrimExplicitTopicQuestion(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), " \t\r\n？?。！!，,；;")
	for {
		trimmed := false
		for _, pattern := range episodeExplicitTopicTailPatterns {
			location := pattern.FindStringIndex(value)
			if location == nil || location[1] != len(value) || location[0] == 0 {
				continue
			}
			value = strings.TrimRight(strings.TrimSpace(value[:location[0]]), " \t\r\n，,；;")
			trimmed = true
			break
		}
		if !trimmed {
			return value
		}
	}
}

func episodeParseTimeRange(q string, now time.Time) (time.Time, time.Time, bool) {
	loc := now.Location()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	switch {
	case strings.Contains(q, "昨天"):
		return day.AddDate(0, 0, -1), day, true
	case strings.Contains(q, "前天"):
		return day.AddDate(0, 0, -2), day.AddDate(0, 0, -1), true
	case strings.Contains(q, "上周") || strings.Contains(q, "上一周"):
		weekday := (int(day.Weekday()) + 6) % 7
		thisMonday := day.AddDate(0, 0, -weekday)
		return thisMonday.AddDate(0, 0, -7), thisMonday, true
	case strings.Contains(q, "本周") || strings.Contains(q, "这周"):
		weekday := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -weekday), day.AddDate(0, 0, 1), true
	case strings.Contains(q, "上个月"):
		thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
		return thisMonth.AddDate(0, -1, 0), thisMonth, true
	}
	if match := episodeDatePattern.FindStringSubmatch(q); len(match) == 4 {
		if parsed, err := time.ParseInLocation("2006-01-02", match[0], loc); err == nil {
			return parsed, parsed.AddDate(0, 0, 1), true
		}
	}
	return time.Time{}, time.Time{}, false
}
