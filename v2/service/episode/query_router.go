package episode

import (
	"GopherAI/common/rag"
	"GopherAI/common/topicmatch"
	"GopherAI/model"
	"regexp"
	"strings"
	"time"
)

type QueryIntent string

const (
	IntentNone            QueryIntent = "none"
	IntentLastSession     QueryIntent = "last_session"
	IntentTimeRange       QueryIntent = "time_range"
	IntentTopicHistory    QueryIntent = "topic_history"
	IntentDecisionHistory QueryIntent = "decision_history"
)

type RouteRequest struct {
	Question                 string
	Now                      time.Time
	CurrentSessionHasHistory bool
	// ProposedTopicKeys are TopicKeys a resolver has already confirmed belong
	// to this user. The router does not produce them; it carries them into the
	// plan so the retriever can treat them as an eligibility route.
	ProposedTopicKeys []string
}

type QueryRoute struct {
	Intent            QueryIntent
	NeedRecall        bool
	From, To          time.Time
	Terms             []string
	ExactTopicKeys    []string
	ExactIdentifiers  []string
	RequireExactTopic bool
	Reason            string
}

type QueryRouter struct{}

func NewQueryRouter() *QueryRouter { return &QueryRouter{} }

var datePattern = regexp.MustCompile(`\b(20\d{2})-(\d{2})-(\d{2})\b`)

// explicitTopicTailPatterns remove only question predicates anchored at the
// end of the text after 关于/围绕.  Topic names may themselves contain words
// such as 方案 or 选择, so those nouns are never stripped on their own.
var explicitTopicTailPatterns = []*regexp.Regexp{
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
	// A bare past-tense "settled on" left after the interrogative was trimmed.
	regexp.MustCompile(`(?:最终|最后|当时)?定(?:下来|下|了|的)?$`),
	// Speech-act verbs the learner appends to the topic they just named.
	regexp.MustCompile(`(?:讲清楚|讲一讲|讲讲|说说|说明|解释|回顾|重讲|复述|梳理)(?:一下|一遍)?$`),
}

// explicitTopicBoundaries end the topic named after 关于/围绕.
//
// The previous rule took everything to the end of the sentence and then trimmed
// a fixed list of tail predicates, so "关于 分块切片 我之前聊到哪一步了" produced
// the key `分块切片-我之前聊到哪一步了`. Nothing matched it and the read stopped
// with exact_topic_not_found — five of the twenty episodic cases failed exactly
// there. A topic phrase ends where the sentence turns back to the speaker, to
// the past, or to punctuation, and those boundaries are short and stable.
var explicitTopicBoundaries = concatMarkers(
	latestMarkers, pastMarkers, conversationMarkers,
	[]string{
		"我们", "咱们", "我", "你们", "你", "他们", "她们", "他", "她",
		"是否", "还有", "还剩", "还", "该", "现在", "目前", "到底",
		"，", ",", "、", "。", "？", "?", "！", "!", "；", ";", "：", ":",
	},
)

// Route is the pre-STABILIZE-030 entry point, kept only for the diagnostic
// command and the tests that were written against it.
//
// It is a lossy projection and must not be used to drive recall: TemporalMode,
// EvidenceMode and the topic constraint all collapse back into one enum here,
// which is the defect the plan exists to remove. Production reads Plan.
func (r *QueryRouter) Route(request RouteRequest) QueryRoute {
	plan := r.Plan(request)
	route := QueryRoute{
		Intent: IntentNone, NeedRecall: plan.NeedEpisode,
		From: plan.From, To: plan.To,
		Terms:            rag.ExtractLexicalTerms(strings.TrimSpace(request.Question)),
		ExactTopicKeys:   plan.ExactTopicKeys,
		ExactIdentifiers: plan.ExactIdentifiers,
		// The legacy field meant "abandon the read when the key misses". The
		// plan no longer works that way, so the compatibility view reports the
		// preference without the termination it used to imply.
		RequireExactTopic: plan.RequireExactTopic,
		Reason:            plan.Reason(),
	}
	if !plan.NeedEpisode {
		return route
	}
	switch {
	case plan.TemporalMode == TemporalLatestAny:
		route.Intent = IntentLastSession
	case plan.TemporalMode == TemporalRange:
		route.Intent = IntentTimeRange
	case plan.EvidenceMode == EvidenceDecision:
		route.Intent = IntentDecisionHistory
	default:
		route.Intent = IntentTopicHistory
	}
	return route
}

// Plan is the production entry point. It returns every axis the question
// expressed instead of the single one that happened to be matched first.
func (r *QueryRouter) Plan(request RouteRequest) MemoryReadPlan {
	plan := BuildReadPlan(request)
	plan.ProposedTopicKeys = normalizeTopicKeys(request.ProposedTopicKeys)
	return plan
}

func normalizeTopicKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		normalized := model.NormalizeLearningTopicKey(key)
		if normalized == "" {
			continue
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func explicitTopicKey(q string) (string, bool) {
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
	value := cutAtTopicBoundary(q[start+markerSize:])
	value = trimExplicitTopicQuestion(value)
	key := model.NormalizeLearningTopicKey(value)
	return key, key != ""
}

func cutAtTopicBoundary(value string) string {
	cut := len(value)
	for _, boundary := range explicitTopicBoundaries {
		if index := strings.Index(value, boundary); index >= 0 && index < cut {
			cut = index
		}
	}
	return strings.TrimRight(strings.TrimSpace(value[:cut]), " \t\r\n的了")
}

func trimExplicitTopicQuestion(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), " \t\r\n？?。！!，,；;")
	for {
		trimmed := false
		for _, pattern := range explicitTopicTailPatterns {
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

func parseTimeRange(q string, now time.Time) (time.Time, time.Time, bool) {
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
	if match := datePattern.FindStringSubmatch(q); len(match) == 4 {
		if parsed, err := time.ParseInLocation("2006-01-02", match[0], loc); err == nil {
			return parsed, parsed.AddDate(0, 0, 1), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// exactIdentifiers and strongIdentifier now delegate to the single definition
// in common/topicmatch. The rule is unchanged; it simply has one home, because
// the semantic topic gate needs the same answer and two copies would drift.
func exactIdentifiers(q string) []string { return topicmatch.StrongIdentifiers(q) }

func strongIdentifier(value string) bool { return topicmatch.IsStrongIdentifier(value) }

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
