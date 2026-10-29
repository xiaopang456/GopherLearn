package agent

import (
	"GopherAI/common/topicmatch"
	learningdao "GopherAI/dao/learning"
	"context"
	"regexp"
	"strings"
)

// A topic's name is a poor description of it.
//
// Measured on forty-six hand-labelled cases, matching a chat question against
// topic names alone reached 0.765: eight of the thirty-four questions that
// should have found their topic did not, and seven of those eight failed the
// same way — the learner named a sub-concept the topic name never contained.
// "惰性删除和定期删除的区别" does not share a character with "Redis TTL";
// "autoAck 为什么危险" shares nothing with "RabbitMQ 消息确认".
//
// Those words are not missing from the system. They are in the questions the
// learner was asked and in the points the grader recorded as missing, both of
// which are already stored and already shown on the quiz screen. This file
// makes them searchable.
//
// The danger in widening the target is collision: a dozen question texts offer
// far more surface for an accidental match than one short name, and a wrong
// topic is worse than none because it tells the model the learner is weak at
// something they did not ask about. Distinctiveness is what holds that back —
// a term is evidence only if it belongs to one topic and not the others.

// TopicTextReader supplies the learner's recent graded attempts across topics.
type TopicTextReader interface {
	ListRecentAttemptTexts(ctx context.Context, userID int64, limit int) ([]learningdao.TopicAttemptRecord, error)
}

// topicTerms holds, per topic key, the terms that appear in that topic's quiz
// history and nowhere else in it.
type topicTerms struct {
	distinctive map[string]map[string]struct{}
}

var termPattern = regexp.MustCompile(`[a-z][a-z0-9_]{2,}`)

// genericIdentifiers are the ASCII counterpart of genericTerms below.
//
// A single ASCII hit is enough to claim a topic, on the reasoning that nobody
// writes autoAck or PacketBuffer by accident. That reasoning holds for names and
// fails for vocabulary: `map`, `type`, `error` and `value` appear in any
// discussion of any Go subject. Observed in production: "帮我讲讲 Go 的 map
// 实现" was handed the Codec 注册表 assessment, because that topic's quiz text
// describes a Type→Factory map and no other topic's did, which made `map` look
// like a distinguishing name.
//
// The list holds language keywords, builtin types and functions, and nouns that
// describe code rather than name anything in it. Library and API names are
// deliberately absent: those are exactly the evidence this path exists to use.
var genericIdentifiers = map[string]struct{}{
	"map": {}, "func": {}, "type": {}, "var": {}, "const": {}, "struct": {},
	"interface": {}, "chan": {}, "string": {}, "int": {}, "int64": {}, "bool": {},
	"byte": {}, "rune": {}, "float": {}, "err": {}, "error": {}, "nil": {},
	"true": {}, "false": {}, "return": {}, "range": {}, "len": {}, "cap": {},
	"make": {}, "new": {}, "package": {}, "import": {}, "for": {}, "if": {},
	"else": {}, "switch": {}, "case": {}, "code": {}, "data": {}, "name": {},
	"key": {}, "value": {}, "list": {}, "size": {}, "time": {}, "line": {},
	"file": {}, "test": {}, "item": {}, "part": {}, "mode": {}, "step": {},
	"text": {}, "word": {}, "info": {}, "field": {}, "method": {}, "object": {},
	"class": {}, "result": {}, "number": {}, "count": {}, "index": {},
}

// functionRunes are the characters Chinese uses to build sentences rather than
// to name things.
//
// The first attempt filtered whole bigrams by a list, and writing tests for it
// showed why that cannot work: the noise is not in the words a person writes
// but in the pairs that straddle them. "崩溃了会怎样" yielded 崩溃, 会怎 and
// 怎样, and two of those three are fragments of 会怎样 — a phrase that says
// nothing about any subject. Enumerating such pairs is hopeless; there are as
// many as there are sentences.
//
// Enumerating the characters is not hopeless, because the set is closed. A
// bigram containing one of these is grammar, not vocabulary, and is dropped
// whichever side it falls on.
var functionRunes = map[rune]struct{}{
	'的': {}, '了': {}, '是': {}, '会': {}, '怎': {}, '样': {}, '什': {}, '么': {},
	'有': {}, '在': {}, '和': {}, '与': {}, '就': {}, '都': {}, '也': {}, '个': {},
	'下': {}, '上': {}, '中': {}, '要': {}, '用': {}, '说': {}, '请': {}, '还': {},
	'能': {}, '可': {}, '不': {}, '这': {}, '那': {}, '为': {}, '被': {}, '把': {},
	'吗': {}, '呢': {}, '吧': {}, '啊': {}, '如': {}, '果': {}, '时': {}, '候': {},
	'些': {}, '哪': {}, '我': {}, '你': {}, '他': {}, '它': {}, '们': {}, '自': {},
	'己': {}, '过': {}, '来': {}, '去': {}, '到': {}, '给': {}, '让': {}, '做': {},
	'需': {}, '于': {}, '及': {}, '或': {}, '而': {}, '但': {}, '所': {}, '以': {},
}

// genericTerms are real words that name no subject.
//
// Function characters catch fragments that straddle word boundaries; these are
// the other kind of noise — whole words a technical question produces whatever
// it is about. Both filters are needed, and neither substitutes for the other:
// after function characters removed 会怎 and 怎样, "这个需要使用什么，什么情况
// 下的问题" still yielded 情况 and 问题, which is two bigrams and would have
// fired.
var genericTerms = map[string]struct{}{
	"情况": {}, "问题": {}, "方法": {}, "方式": {}, "原因": {}, "内容": {}, "结果": {},
	"影响": {}, "作用": {}, "意思": {}, "东西": {}, "地方": {}, "部分": {}, "过程": {},
	"区别": {}, "关系": {}, "含义": {}, "概念": {}, "情形": {}, "场景": {},
}

// isFunctionBigram reports whether a bigram is grammar rather than subject
// matter. A pair is kept only when neither half is a function character.
func isFunctionBigram(bigram string) bool {
	if _, generic := genericTerms[bigram]; generic {
		return true
	}
	for _, r := range bigram {
		if _, exists := functionRunes[r]; exists {
			return true
		}
	}
	return false
}

// minDistinctiveTopics is the number of topics below which a term being unique
// to one of them says nothing about that topic.
const minDistinctiveTopics = 3

// buildTopicTerms indexes the attempt texts and keeps only the terms unique to
// one topic. A term shared by two topics cannot decide between them, and a term
// shared by many is noise.
func buildTopicTerms(records []learningdao.TopicAttemptRecord) *topicTerms {
	perTopic := map[string]map[string]struct{}{}
	frequency := map[string]int{}
	for _, record := range records {
		key := strings.TrimSpace(record.TopicKey)
		if key == "" {
			continue
		}
		if perTopic[key] == nil {
			perTopic[key] = map[string]struct{}{}
		}
		text := record.QuestionText + " " + strings.Join(record.MissingPoints, " ")
		for term := range extractTerms(text) {
			if _, seen := perTopic[key][term]; seen {
				continue
			}
			perTopic[key][term] = struct{}{}
			frequency[term]++
		}
	}
	distinctive := make(map[string]map[string]struct{}, len(perTopic))
	for key, terms := range perTopic {
		kept := map[string]struct{}{}
		for term := range terms {
			// "Used by exactly one topic" is only evidence when there are other
			// topics it could have been used by. With two, a term appearing in
			// one and not the other is unremarkable; the claim needs at least
			// two alternatives it actually excludes.
			//
			// This is a floor, not a tuned value. The 0.964 measurement behind
			// this path came from a ten-topic fixture, so the small-vocabulary
			// regime it now guards was never covered by that evaluation — the
			// first real account to reach production had two topics and was
			// mismatched immediately.
			if frequency[term] == 1 && len(perTopic) >= minDistinctiveTopics {
				kept[term] = struct{}{}
			}
		}
		distinctive[key] = kept
	}
	return &topicTerms{distinctive: distinctive}
}

// hits reports whether the question reproduces enough of a topic's distinctive
// vocabulary to count as evidence, and how strongly.
//
// The two kinds of term are not worth the same, which the first version of this
// file got wrong and paid for: counting them alike took correct rejection from
// 0.917 down to 0.417 on the same forty-six cases. Seven unrelated questions
// were handed a topic because a single common word — 服务, 分组, 处理, 并发 —
// happened to appear in one topic's quiz history and no other. Across ten
// topics, "used by exactly one" is not rarity; it is coincidence.
//
// An ASCII identifier does not have that problem. Nobody writes autoAck or
// goroutine by accident, so one is enough. A Han bigram is cheap, so two are
// required, and they must come from the same topic.
func (t *topicTerms) hits(topicKey, question string, minBigrams int) int {
	if t == nil {
		return 0
	}
	terms, ok := t.distinctive[topicKey]
	if !ok || len(terms) == 0 {
		return 0
	}
	identifiers, bigrams := 0, 0
	for term := range extractTerms(question) {
		if _, exists := terms[term]; !exists {
			continue
		}
		if topicmatch.IsASCIISegment(term) {
			identifiers++
			continue
		}
		bigrams++
	}
	if identifiers > 0 {
		return identifiers + bigrams
	}
	if bigrams >= minBigrams {
		return bigrams
	}
	return 0
}

// minDistinctiveBigrams is how many Han bigrams must coincide before the quiz
// history is treated as naming the same subject as the question.
const minDistinctiveBigrams = 2

// extractTerms yields ASCII words of three characters or more and Han bigrams,
// minus the ones built out of function characters. Han is cut into bigrams because there is no segmenter
// available and a bigram is the shortest run that carries meaning.
func extractTerms(text string) map[string]struct{} {
	lowered := strings.ToLower(text)
	out := map[string]struct{}{}
	for _, word := range termPattern.FindAllString(lowered, -1) {
		if _, generic := genericIdentifiers[word]; generic {
			continue
		}
		out[word] = struct{}{}
	}
	var run []rune
	emit := func() {
		for i := 0; i+1 < len(run); i++ {
			if bigram := string(run[i : i+2]); !isFunctionBigram(bigram) {
				out[bigram] = struct{}{}
			}
		}
		run = run[:0]
	}
	for _, r := range lowered {
		if r > 127 {
			run = append(run, r)
			continue
		}
		emit()
	}
	emit()
	return out
}
