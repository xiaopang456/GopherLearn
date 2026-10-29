package episode

import (
	"GopherAI/common/rag"
	"GopherAI/model"
	"sort"
	"strings"
	"time"
)

// STABILIZE-030 replaces the single mutually exclusive Intent with a plan that
// keeps the three things a question can say about history apart.
//
// The old router answered "which one query do I run" with one enum, so the
// first marker it recognised won the whole question. A question carrying a
// time reference, a topic and an evidence type kept only the time reference:
// "上次关于缓冲区复用的决定" became last_session and returned whichever session
// happened to be newest. Measured on the frozen EXP-006 recall artefacts that
// was 14 of the 20 mixed cases; six more were rejected outright because their
// phrasing was absent from a fixed word list.
//
// A MemoryReadPlan says what the question needs, not which SQL to run. Time
// range, topic constraint and evidence type are independent fields, so none of
// them can silently delete another.

// TemporalMode is how the plan wants history bounded in time.
type TemporalMode string

const (
	// TemporalNone asks for no time bound at all: relevance decides.
	TemporalNone TemporalMode = "none"
	// TemporalLatestAny is the literal "what did we talk about last time",
	// which carries no topic once its shell words are removed.
	TemporalLatestAny TemporalMode = "latest_any"
	// TemporalLatestMatching is "last time, about X": recency ranks the
	// relevant candidates, it does not replace relevance.
	TemporalLatestMatching TemporalMode = "latest_matching"
	// TemporalRange is an explicit date or period.
	TemporalRange TemporalMode = "time_range"
)

// EvidenceMode is which structured projection the question asks for. It is
// applied after the MySQL reload against Document fields, not as a search term:
// "决定" is a word that appears in plenty of episodes that recorded none.
type EvidenceMode string

const (
	EvidenceAny        EvidenceMode = "any"
	EvidenceDecision   EvidenceMode = "decision"
	EvidenceCorrection EvidenceMode = "correction"
	EvidenceOpenLoop   EvidenceMode = "open_loop"
)

// MemoryReadPlan is the deterministic result of reading one question.
type MemoryReadPlan struct {
	NeedEpisode bool

	TemporalMode TemporalMode
	From, To     time.Time

	EvidenceMode EvidenceMode

	// TopicTerms are the lexical terms left after the history, evidence and
	// discourse shell has been removed. They drive the lexical channel and
	// decide whether "上次" still has a topic attached to it.
	TopicTerms []string
	// ExactTopicKeys are normalised TopicKey candidates read straight off the
	// question. They are preferred, never required.
	ExactTopicKeys []string
	// ExactIdentifiers are strong code-shaped tokens, which embeddings handle
	// badly and exact search handles well.
	ExactIdentifiers []string
	// ProposedTopicKeys are keys a resolver confirmed for this user. They are
	// filled by the caller, not by parsing, and they are one of the three ways
	// a candidate becomes eligible.
	ProposedTopicKeys []string
	// RequireExactTopic records that the question named a topic explicitly with
	// 关于/围绕. It raises the bar for what counts as relevant; it no longer
	// terminates the read when the exact key misses.
	RequireExactTopic bool

	ReasonCodes []string
}

// Reason is the primary reason code, kept first for the existing trace field.
func (p MemoryReadPlan) Reason() string {
	if len(p.ReasonCodes) == 0 {
		return ""
	}
	return p.ReasonCodes[0]
}

// HasTopicConstraint reports whether anything in the question still names a
// subject after the shell words are gone.
func (p MemoryReadPlan) HasTopicConstraint() bool {
	return len(p.TopicTerms) > 0 || len(p.ExactIdentifiers) > 0 || len(p.ExactTopicKeys) > 0 || len(p.ProposedTopicKeys) > 0
}

// The marker vocabularies below are organised by the grammatical role a word
// plays, not by the questions that happened to fail. A list that grows one
// sentence at a time only ever covers the sentences already seen; these lists
// are meant to be read as "history pointer", "conversation reference",
// "evidence type" and "discourse shell", and extended by role.

// latestMarkers point at the most recent session rather than at a subject.
var latestMarkers = []string{
	"上一次会话", "上一次聊天", "上一轮会话", "上一轮聊天", "最近一次会话", "最近一次聊天",
	"上一个会话", "前一个会话", "前一轮会话", "上个会话",
	"上一次", "上一轮", "上一回", "最近一次", "前一次", "上次",
	"那一次", "那一轮", "那次", "那轮",
}

// pastMarkers place the question in the past without naming which session.
var pastMarkers = []string{
	"之前", "以前", "先前", "早先", "曾经", "当初", "当时", "后来", "过去", "历史", "原来", "原先", "最初", "一开始",
}

// conversationMarkers name the act of having talked about something.
var conversationMarkers = []string{
	"聊过", "聊到", "聊了", "聊完", "聊天", "讨论过", "讨论了", "讨论完", "讨论", "谈过", "谈完", "会话", "对话",
}

// Evidence vocabularies. Each one maps to a structured Document field, so a
// word only belongs here when its presence really means "the answer must come
// from that field".
var decisionMarkers = []string{
	"决定", "定下来", "定下", "定了", "定的", "结论", "选择", "选了", "选的", "采用", "取舍", "拍板", "敲定", "为什么选",
}

// correctionMarkers stay narrow on purpose. "改" alone is how a decision is
// described just as often as a correction, and a wrong evidence mode is a hard
// filter that removes the right episode.
var correctionMarkers = []string{
	"纠正", "更正", "改掉", "改正", "推翻", "修正", "误解", "搞错", "想错",
}

var openLoopMarkers = []string{
	"遗留问题", "遗留", "待定项", "待定", "未完成", "没做完", "还欠着", "还欠", "欠着", "开放问题", "悬而未决", "还缺", "还差", "缺的",
}

// firstPersonMarkers and pastPredicateInterrogatives implement the second route
// into "this is a question about my own history": a first person subject with a
// past decision or correction predicate that is being asked *about*.
//
// The interrogative requirement is what keeps "我给 X 定的策略是什么" out. That
// sentence uses the same verb, but attributively: it asks for a current state,
// not for a past event. Without the requirement every "我...定的" turns into a
// history lookup, and the no-memory stratum is full of them.
var firstPersonMarkers = []string{"我们", "咱们", "我"}

var processInterrogatives = []string{"怎么", "如何", "为什么", "为何", "哪条", "哪一条", "哪种", "哪一种", "起因", "理由", "依据"}

// shellMarkers are removed before the remaining topic is read. They are the
// history pointers, the conversation nouns, the evidence vocabulary and the
// discourse frame a learner wraps a question in — everything that describes
// how the question is being asked rather than what it is about.
var shellMarkers = concatMarkers(
	latestMarkers, pastMarkers, conversationMarkers,
	decisionMarkers, correctionMarkers, openLoopMarkers,
	[]string{
		// reference and person
		"这件事", "那件事", "这个", "那个", "这块", "那块", "这边", "那边", "这些", "那些", "那点", "这点", "自己",
		"我们", "咱们", "我", "你们", "你", "他们", "她们",
		// interrogatives and particles
		"是什么", "有什么", "为什么", "为何", "怎么样", "怎么", "如何", "哪一步", "哪一条", "哪一种", "哪条", "哪种", "哪些", "哪个",
		"什么", "多少", "是否", "能不能", "可不可以", "到什么程度", "来着", "吗", "呢",
		// speech-act frame
		//
		// Single characters are deliberately absent here. "用" and "对" are
		// shell in isolation and the middle of a word everywhere else, and
		// removing them turns 缓冲区复用 into 缓冲区复 — a topic the lexical
		// channel can no longer find.
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

// genericTermStopWords remove terms that survive stripping but name no subject.
// They only affect whether a topic constraint exists, never what is searched.
var genericTermStopWords = map[string]struct{}{
	"事情": {}, "东西": {}, "地方": {}, "情况": {}, "时候": {}, "部分": {}, "问题": {},
	"要求": {}, "习惯": {}, "偏好": {}, "次序": {}, "顺序": {}, "详略": {}, "程度": {},
	"比方": {}, "说法": {}, "类比": {}, "改变": {}, "过来": {}, "得上": {}, "解决": {}, "推进": {},
	// Function-word pairs that survive stripping because neither half is shell
	// on its own. They name no subject, so they must not turn "上次聊了什么"
	// into a topic-constrained read.
	"在做": {}, "做的": {}, "有的": {}, "没有": {}, "这样": {}, "那样": {}, "怎样": {},
	"一个": {}, "一些": {}, "可以": {}, "需要": {}, "应该": {}, "知道": {}, "觉得": {},
}

func concatMarkers(groups ...[]string) []string {
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

// BuildReadPlan reads one question into a plan. It is pure: same question and
// same clock, same plan, no provider and no database.
func BuildReadPlan(request RouteRequest) MemoryReadPlan {
	question := strings.TrimSpace(request.Question)
	now := request.Now
	if now.IsZero() {
		now = time.Now()
	}
	plan := MemoryReadPlan{TemporalMode: TemporalNone, EvidenceMode: EvidenceAny}
	if question == "" {
		plan.ReasonCodes = []string{"empty_query"}
		return plan
	}

	explicitTopic, hasExplicitTopic := explicitTopicKey(question)
	plan.RequireExactTopic = hasExplicitTopic
	core := topicCore(question)
	plan.TopicTerms = topicTerms(core)
	plan.ExactIdentifiers = exactIdentifiers(question)
	// Keys are read off the core only when the core still carries a subject.
	// Without that guard the leftovers of a purely temporal question ("那天最后
	// 是怎么收的") normalise into a key-shaped string, and a date query that the
	// range alone had answered correctly acquires a topic constraint it never
	// expressed.
	if len(plan.TopicTerms) > 0 {
		plan.ExactTopicKeys = topicKeyCandidates(core)
	}
	if hasExplicitTopic {
		// A learner who wrote 关于 named the topic themselves. Keys guessed from
		// the rest of the sentence can only widen the exact channel with values
		// nothing carries, so the named one stands alone.
		plan.ExactTopicKeys = []string{explicitTopic}
	}
	plan.EvidenceMode = evidenceModeOf(question)

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
		plan.NeedEpisode, plan.TemporalMode = true, TemporalLatestAny
		plan.ReasonCodes = []string{"continue_empty_session"}
		return plan
	}

	if from, to, ok := parseTimeRange(question, now); ok {
		plan.NeedEpisode, plan.TemporalMode, plan.From, plan.To = true, TemporalRange, from, to
		plan.ReasonCodes = []string{"time_range"}
		if plan.HasTopicConstraint() {
			plan.ReasonCodes = append(plan.ReasonCodes, "time_range_with_topic")
		}
		return plan
	}

	historyReason, isHistory := historyIntent(question)
	if !isHistory {
		plan.ReasonCodes = []string{"no_personal_history_intent"}
		return plan
	}
	plan.NeedEpisode = true
	plan.ReasonCodes = []string{historyReason}
	if containsAny(question, latestMarkers...) {
		if plan.HasTopicConstraint() {
			plan.TemporalMode = TemporalLatestMatching
			plan.ReasonCodes = []string{"latest_matching", historyReason}
		} else {
			plan.TemporalMode = TemporalLatestAny
			plan.ReasonCodes = []string{"latest_any", historyReason}
		}
		return plan
	}
	plan.TemporalMode = TemporalNone
	return plan
}

// historyIntent answers the first of the two questions the old router conflated:
// is this about the learner's own past at all?
func historyIntent(question string) (string, bool) {
	switch {
	case containsAny(question, latestMarkers...):
		return "history_session_pointer", true
	case containsAny(question, pastMarkers...):
		return "history_past_pointer", true
	case containsAny(question, conversationMarkers...):
		return "history_conversation_pointer", true
	case containsAny(question, openLoopMarkers...) && containsAny(question, "这", "那", "还"):
		// An open loop is a reference to a state a past session established.
		// The demonstrative requirement is what separates "那个遗留问题" from a
		// textbook question about what a design is still missing.
		return "implicit_history_open_loop", true
	case firstPersonPastPredicate(question):
		return "implicit_history_decision", true
	}
	return "", false
}

// firstPersonPastPredicate is the "我 + 过去决定/纠正" route. All three parts
// have to be present: the person, the decision or correction predicate, and an
// interrogative asking about that predicate rather than about its object.
func firstPersonPastPredicate(question string) bool {
	if !containsAny(question, firstPersonMarkers...) {
		return false
	}
	if !containsAny(question, decisionMarkers...) && !containsAny(question, correctionMarkers...) {
		return false
	}
	if containsAny(question, "现在", "目前", "平时", "一般", "通常", "常用", "接下来", "下次", "以后") {
		return false
	}
	return containsAny(question, processInterrogatives...)
}

func evidenceModeOf(question string) EvidenceMode {
	switch {
	case containsAny(question, correctionMarkers...):
		return EvidenceCorrection
	case containsAny(question, openLoopMarkers...):
		return EvidenceOpenLoop
	case containsAny(question, decisionMarkers...):
		return EvidenceDecision
	}
	return EvidenceAny
}

// topicCore strips the shell so that what is left is the subject, if any.
func topicCore(question string) string {
	value := datePattern.ReplaceAllString(question, " ")
	for _, marker := range []string{"关于", "围绕", "昨天", "前天", "上周", "上一周", "本周", "这周", "上个月", "那天"} {
		value = strings.ReplaceAll(value, marker, " ")
	}
	for _, marker := range shellMarkers {
		value = strings.ReplaceAll(value, marker, " ")
	}
	return value
}

func topicTerms(core string) []string {
	terms := rag.ExtractLexicalTerms(core)
	kept := make([]string, 0, len(terms))
	for _, term := range terms {
		if _, generic := genericTermStopWords[term]; generic {
			continue
		}
		kept = append(kept, term)
	}
	return kept
}

// topicKeyCandidates proposes normalised keys from the stripped core. The whole
// core comes first because a learner naming a topic usually names it whole.
func topicKeyCandidates(core string) []string {
	keys, seen := make([]string, 0, 4), map[string]struct{}{}
	add := func(value string) {
		key := model.NormalizeLearningTopicKey(value)
		if len([]rune(key)) < 2 {
			return
		}
		if _, generic := genericTermStopWords[key]; generic {
			return
		}
		// A topic key is a name, not a clause. Anything longer than this is a
		// fragment of the question that survived stripping, and it can only
		// widen the exact channel with a value nothing will ever carry.
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

// TopicSignals is what the read plan derived from a question that a topic
// resolver needs but must not re-derive.
//
// The de-shelling rule and the identifier bar both live here; a resolver that
// re-implemented either would eventually disagree with the plan about the same
// question, and the disagreement would be silent.
type TopicSignals struct {
	// TopicTerms are the lexical terms left after the history, evidence and
	// discourse shell is removed. Empty means the question named no subject.
	TopicTerms []string
	// Identifiers are the strong, code-shaped tokens the learner spelled out.
	Identifiers []string
}

// TopicSignalsFor reads one question into the signals a resolver may use. It is
// pure: no clock dependency, no provider, no database.
func TopicSignalsFor(question string) TopicSignals {
	plan := BuildReadPlan(RouteRequest{Question: question})
	return TopicSignals{TopicTerms: plan.TopicTerms, Identifiers: plan.ExactIdentifiers}
}
