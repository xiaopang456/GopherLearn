package episode

import (
	"GopherAI/common/observability"
	"GopherAI/common/rag"
	episodedao "GopherAI/dao/episode"
	"GopherAI/model"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// This file is the pre-STABILIZE-030 Episode read path, kept as its own code
// rather than as a configuration of the new one.
//
// STABILIZE-031 requires `legacy_v2` to be *proved* equivalent to the frozen v2
// artefact, not argued equivalent (design 3.4). Candidate ordering and the exact
// `reason` strings are internal details of the old router; reproducing them by
// configuring the new router would be an argument, and every difference would
// then need a defence. Keeping the old path makes the equivalence structural.
//
// Nothing here may be "improved". Fixing a bug in this file changes what
// `legacy_v2` means, which is the one thing it is not allowed to do. Bugs found
// here belong to a v3 mode.

// LegacyRetriever is the pre-030 Episode retriever.
//
// It deliberately has no request-scoped Cycle and no resolved topic keys: those
// are v3 capabilities, and a mode that does not offer them must not be able to
// construct them.
type LegacyRetriever struct {
	search   EpisodeSearcher
	loader   EpisodeLoader
	embedder EpisodeQueryEmbedder
	config   RetrieverConfig
}

func NewLegacyRetriever(search EpisodeSearcher, loader EpisodeLoader, embedder EpisodeQueryEmbedder, config RetrieverConfig) (*LegacyRetriever, error) {
	if search == nil || loader == nil || config.CandidateDepth < config.MaxResults || config.MaxResults < 1 || config.MaxResults > 5 {
		return nil, fmt.Errorf("invalid legacy episode retriever configuration")
	}
	return &LegacyRetriever{search: search, loader: loader, embedder: embedder, config: config}, nil
}

// Recall reproduces the pre-030 behaviour exactly.
//
// ProposedTopicKeys and Cycle on the request are ignored on purpose: under
// `legacy_v2` nothing fills them, and ignoring them here means a future caller
// that starts filling them cannot silently change what this mode does.
func (r *LegacyRetriever) Recall(ctx context.Context, request RecallRequest) (result RecallResult, err error) {
	started := time.Now()
	route := legacyRoute(RouteRequest{Question: request.Question, Now: request.Now, CurrentSessionHasHistory: request.CurrentSessionHasHistory})
	result = RecallResult{Episodes: []model.EpisodeMemory{}, Trace: ShadowTrace{Intent: route.Intent, Reason: route.Reason}}
	defer func() {
		duration := time.Since(started)
		result.Trace.ElapsedMS = duration.Milliseconds()
		for _, episode := range result.Episodes {
			result.Trace.EstimatedTokens += rag.EstimateTokens(episode.SearchText)
		}
		channels := make([]string, 0, len(result.Trace.Candidates))
		for _, candidate := range result.Trace.Candidates {
			channels = append(channels, candidate.Channels)
		}
		reason := result.Trace.Reason
		if err != nil {
			reason = "provider_error"
		}
		observability.Memory.Retrieval(string(result.Trace.Intent), channels, len(result.Episodes), reason, duration)
	}()
	if request.UserID <= 0 || request.SessionID == "" {
		return result, fmt.Errorf("episode recall identity required")
	}
	if !route.NeedRecall {
		result.Trace.Abstained = true
		return result, nil
	}
	if route.Intent == IntentLastSession || route.Intent == IntentTimeRange {
		from, to := route.From, route.To
		limit := r.config.MaxResults
		if route.Intent == IntentLastSession {
			limit = 1
		} else if len(route.ExactTopicKeys) > 0 {
			limit = r.config.CandidateDepth
		}
		rows, loadErr := r.loader.ListByTime(ctx, request.UserID, request.SessionID, from, to, limit)
		if loadErr != nil {
			return result, loadErr
		}
		if route.Intent == IntentTimeRange && len(route.ExactTopicKeys) > 0 {
			filtered := rows[:0]
			for _, row := range rows {
				if episodeMatchesTopic(row, route.ExactTopicKeys) {
					filtered = append(filtered, row)
				}
			}
			rows = filtered
			if len(rows) > r.config.MaxResults {
				rows = rows[:r.config.MaxResults]
			}
		}
		result.Episodes = rows
		result.Trace.Abstained = len(rows) == 0
		for _, row := range rows {
			result.Trace.Candidates = append(result.Trace.Candidates, CandidateTrace{EpisodeID: row.ID, Channels: "mysql_time"})
		}
		return result, nil
	}
	exact, exactErr := r.search.SearchExactTopics(ctx, request.UserID, request.SessionID, route.ExactTopicKeys, r.config.CandidateDepth)
	if len(route.ExactIdentifiers) > 0 {
		identifierHits, identifierErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, route.ExactIdentifiers, r.config.CandidateDepth)
		exact = mergeSearchResults(exact, identifierHits)
		if exactErr == nil {
			exactErr = identifierErr
		}
	}
	if route.RequireExactTopic {
		topicTerms := rag.ExtractLexicalTerms(strings.ReplaceAll(route.ExactTopicKeys[0], "-", " "))
		topicLexical, topicLexicalErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, topicTerms, r.config.CandidateDepth)
		if exactErr != nil && topicLexicalErr != nil {
			result.Trace.Abstained = true
			result.Trace.Reason = "exact_topic_unavailable"
			return result, nil
		}
		candidates := mergeSearchResults(exact, topicLexical)
		if len(candidates) == 0 {
			result.Trace.Abstained = true
			result.Trace.Reason = "exact_topic_not_found"
			return result, nil
		}
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.EpisodeID)
		}
		rows, loadErr := r.loader.LoadCandidates(ctx, request.UserID, request.SessionID, ids)
		if loadErr != nil {
			return result, loadErr
		}
		for _, row := range rows {
			if legacyEpisodeContainsAllTerms(row, topicTerms) && legacyEpisodeEligibleForIntent(row, route.Intent) {
				result.Episodes = append(result.Episodes, row)
				result.Trace.Candidates = append(result.Trace.Candidates, CandidateTrace{EpisodeID: row.ID, Channels: "explicit_topic"})
				if len(result.Episodes) == r.config.MaxResults {
					break
				}
			}
		}
		result.Trace.Abstained = len(result.Episodes) == 0
		if result.Trace.Abstained {
			result.Trace.Reason = "exact_topic_not_found"
		}
		return result, nil
	}
	lexical, lexicalErr := r.search.SearchByText(ctx, request.UserID, request.SessionID, route.Terms, r.config.CandidateDepth)
	if exactErr != nil && lexicalErr != nil {
		result.Trace.Abstained = true
		result.Trace.Reason = "redis_unavailable"
		return result, nil
	}
	var vectors []episodedao.EpisodeSearchResult
	if (route.Intent == IntentDecisionHistory || route.Intent == IntentTopicHistory) && r.embedder != nil {
		if vector, embedErr := r.embedder.EmbedQuery(ctx, request.Question); embedErr == nil {
			vectors, _ = r.search.SearchByVector(ctx, request.UserID, request.SessionID, vector, r.config.CandidateDepth)
		}
	}
	ranked, traces := legacyEligibleRRF(exact, lexical, vectors, r.config.MaxResults)
	result.Trace.Candidates = traces
	if len(ranked) == 0 {
		result.Trace.Abstained = true
		result.Trace.Reason = "no_eligible_candidate"
		return result, nil
	}
	rows, loadErr := r.loader.LoadCandidates(ctx, request.UserID, request.SessionID, ranked)
	if loadErr != nil {
		return result, loadErr
	}
	for _, row := range rows {
		if legacyEpisodeEligibleForIntent(row, route.Intent) {
			result.Episodes = append(result.Episodes, row)
		}
	}
	result.Trace.Abstained = len(result.Episodes) == 0
	return result, nil
}

// legacyExplicitTopicTailPatterns is the pre-030 list. It is a copy rather than
// a reference because STABILIZE-030 added two patterns to the live one, and
// `legacy_v2` must keep producing the old keys.
var legacyExplicitTopicTailPatterns = []*regexp.Regexp{
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
}

// legacyRoute is the pre-030 single mutually exclusive Intent router.
func legacyRoute(request RouteRequest) QueryRoute {
	q := strings.TrimSpace(request.Question)
	now := request.Now
	if now.IsZero() {
		now = time.Now()
	}
	explicitTopic, requireExactTopic := legacyExplicitTopicKey(q)
	result := QueryRoute{Intent: IntentNone, Terms: rag.ExtractLexicalTerms(q), ExactTopicKeys: legacyExactTopicKeys(q), ExactIdentifiers: exactIdentifiers(q), RequireExactTopic: requireExactTopic}
	if requireExactTopic {
		result.ExactTopicKeys = []string{explicitTopic}
	}
	if q == "" {
		result.Reason = "empty_query"
		return result
	}
	continueLast := containsAny(q,
		"继续上次", "接着上次", "继续上一次", "接着上一次", "继续上一轮", "接着上一轮",
		"继续最近一次", "接着最近一次", "继续前一个会话", "接着前一个会话",
		"继续之前", "接着之前", "上次继续",
	)
	if continueLast {
		if request.CurrentSessionHasHistory {
			result.Reason = "current_session_has_history"
			return result
		}
		result.Intent, result.NeedRecall, result.Reason = IntentLastSession, true, "continue_empty_session"
		return result
	}
	if from, to, ok := parseTimeRange(q, now); ok {
		if !containsAny(q, "关于", "围绕") {
			result.ExactTopicKeys = nil
		}
		result.Intent, result.NeedRecall, result.From, result.To, result.Reason = IntentTimeRange, true, from, to, "explicit_time_range"
		return result
	}
	if containsAny(q,
		"上次", "上一回", "上个会话", "上一次会话", "上一次聊天", "上一轮会话", "上一轮聊天",
		"最近一次会话", "最近一次聊天", "前一个会话", "前一轮会话",
	) {
		result.Intent, result.NeedRecall, result.Reason = IntentLastSession, true, "last_session_marker"
		return result
	}
	if !containsAny(q, "之前", "以前", "历史", "曾经", "聊过", "讨论过", "过去") {
		result.Reason = "no_history_intent"
		return result
	}
	if containsAny(q, "决定", "结论", "选择", "方案", "取舍", "为什么选", "当时怎么") {
		result.Intent, result.NeedRecall, result.Reason = IntentDecisionHistory, true, "history_decision_marker"
		return result
	}
	result.Intent, result.NeedRecall, result.Reason = IntentTopicHistory, true, "history_topic_marker"
	return result
}

func legacyExplicitTopicKey(q string) (string, bool) {
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
	value := legacyTrimExplicitTopicQuestion(q[start+markerSize:])
	key := model.NormalizeLearningTopicKey(value)
	return key, key != ""
}

func legacyTrimExplicitTopicQuestion(value string) string {
	value = strings.TrimRight(strings.TrimSpace(value), " \t\r\n？?。！!，,；;")
	for {
		trimmed := false
		for _, pattern := range legacyExplicitTopicTailPatterns {
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

func legacyExactTopicKeys(q string) []string {
	value := q
	for _, prefix := range []string{"关于", "围绕", "讨论过", "聊过", "之前", "以前", "历史", "上周", "上一周", "本周", "这周", "昨天", "前天", "上个月"} {
		value = strings.ReplaceAll(value, prefix, " ")
	}
	for _, suffix := range []string{"怎么决定的", "怎么决定", "的决定", "的结论", "聊了什么", "讨论了什么", "是什么", "怎么样", "继续", "吗", "呢", "？", "?"} {
		value = strings.ReplaceAll(value, suffix, " ")
	}
	keys, seen := make([]string, 0, 4), map[string]struct{}{}
	for _, part := range strings.Fields(value) {
		key := model.NormalizeLearningTopicKey(part)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	whole := model.NormalizeLearningTopicKey(strings.Join(strings.Fields(value), " "))
	if whole != "" {
		if _, ok := seen[whole]; !ok {
			keys = append([]string{whole}, keys...)
		}
	}
	if len(keys) > 4 {
		keys = keys[:4]
	}
	return keys
}

func legacyEpisodeEligibleForIntent(episode model.EpisodeMemory, intent QueryIntent) bool {
	if intent == IntentDecisionHistory {
		return len(episode.Document.Decisions) > 0
	}
	return true
}

func legacyEpisodeContainsAllTerms(episode model.EpisodeMemory, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	haystack := strings.ToLower(episode.Title + " " + episode.SearchText + " " + strings.Join(episode.TopicKeys, " "))
	for _, term := range terms {
		if !strings.Contains(haystack, strings.ToLower(strings.TrimSpace(term))) {
			return false
		}
	}
	return true
}

// legacyEligibleRRF is the pre-030 fusion. It has no corroboration-depth bound:
// that guard is a v3 change (D-156) and adding it here would silently move what
// `legacy_v2` means.
func legacyEligibleRRF(exact, lexical, vector []episodedao.EpisodeSearchResult, max int) ([]string, []CandidateTrace) {
	byID := map[string]*fusedEpisode{}
	add := func(items []episodedao.EpisodeSearchResult, channel string) {
		for idx, item := range items {
			if item.EmbeddingVersion != episodedao.CurrentEpisodeEmbeddingVersion {
				continue
			}
			entry := byID[item.EpisodeID]
			if entry == nil {
				entry = &fusedEpisode{id: item.EpisodeID}
				byID[item.EpisodeID] = entry
			}
			rank := idx + 1
			entry.score += 1 / float64(60+rank)
			switch channel {
			case "exact":
				entry.exact = rank
			case "lexical":
				entry.lexical = rank
			case "vector":
				entry.vector = rank
			}
		}
	}
	add(exact, "exact")
	add(lexical, "lexical")
	add(vector, "vector")
	items := make([]fusedEpisode, 0, len(byID))
	for _, entry := range byID {
		if entry.exact > 0 || (entry.lexical > 0 && entry.vector > 0) {
			items = append(items, *entry)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].exact > 0 && items[j].exact == 0 {
			return true
		}
		if items[j].exact > 0 && items[i].exact == 0 {
			return false
		}
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].id < items[j].id
	})
	if len(items) > max {
		items = items[:max]
	}
	ids := make([]string, 0, len(items))
	traces := make([]CandidateTrace, 0, len(items))
	for _, item := range items {
		channels := ""
		if item.exact > 0 {
			channels = "exact"
		}
		if item.lexical > 0 {
			if channels != "" {
				channels += "+"
			}
			channels += "lexical"
		}
		if item.vector > 0 {
			if channels != "" {
				channels += "+"
			}
			channels += "vector"
		}
		ids = append(ids, item.id)
		traces = append(traces, CandidateTrace{EpisodeID: item.id, Channels: channels, ExactRank: item.exact, LexicalRank: item.lexical, VectorRank: item.vector})
	}
	return ids, traces
}
