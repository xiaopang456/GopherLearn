package agent

import (
	"strings"
	"unicode"

	"GopherAI/common/rag"
)

// Whether a turn has to look at the learner's own material before it can
// answer.
//
// This used to be one predicate - "does this question need grounding" - and it
// answered the wrong question. It said yes to everything except a table of
// greetings and a list of phrases about the current conversation, so the
// server bound the document search as the first tool of nearly every turn and
// the model was never asked whether the question was about the learner's notes
// at all. A question about the capital of China cost a search, and a question
// that the notes did answer cost the same search whether or not the model
// thought it was needed.
//
// The decision now has three parts, and they are three different questions:
//
//  1. Some questions are about this conversation, not about the notes.
//  2. Some questions are about the learner's own past, which the notes are not
//     the source of.
//  3. The rest are the model's decision, unless the learner or the request
//     has already said which source the answer must come from.
//
// The third part is where the server still says no, and it says no for two
// reasons only: the request asked for the knowledge base and nothing else, or
// the question names the learner's own material outright.

// requiresDocumentSearch reports whether this turn must retrieve from the
// learner's uploaded material before it can answer.
//
// A false answer here does not forbid a search. It means the server does not
// bind the search as the first tool: the model is free to search, and free to
// answer without searching. A true answer binds it.
func requiresDocumentSearch(question string, policy rag.KnowledgePolicy) bool {
	// A fact from this conversation is already in the window. Sending it
	// through the notes would answer a different question.
	if isSessionContextQuestion(question) {
		return false
	}
	// Another user's material is refused before any of this; a search would
	// only be a slower way to reach the same refusal.
	if isCrossUserKnowledgeRequest(question) {
		return false
	}
	// A question about the learner's own past is answered from that past.
	// The notes are not a record of what the learner said or decided, and a
	// search there cannot answer it.
	if isHistoryRecallQuestion(question) {
		return false
	}
	// A greeting needs nothing.
	if isDirectGreeting(question) {
		return false
	}
	// "Only from my notes" is an instruction about the source, not a
	// question about relevance, and it is the one instruction the server
	// must not second-guess.
	if policy == rag.KnowledgeOnly {
		return true
	}
	// "Do not look in my notes" is the other instruction about the source,
	// and it is honoured by not offering the notes at all rather than by
	// asking the model to ignore them.
	if isExplicitNoSearchQuestion(question) {
		return false
	}
	// The question names the learner's own material, so the material is the
	// subject of the question rather than a possible source of its answer.
	return mentionsOwnMaterial(question)
}

// requiresForcedDocumentSearch reports whether this turn must retrieve from
// the learner's own material because the server could not look first.
//
// This is the rule that was in force before the pre-retrieval existed, and it
// comes back whenever the pre-retrieval could not run: no reranker to score
// with, a reranker that timed out, an embedder that refused. In those cases
// the window holds no evidence, and a model asked to judge for itself answers
// from its own head - "Redis 的 AOF 有几种策略" is a question the learner's
// notes answer, and a model that answers it from memory answers it
// differently from the notes without ever saying that it did so.
//
// So the two rules are not one rule with the force removed. The pre-retrieval
// replaces the force only when it ran: it either handed over the passages that
// answer the question, or it said that this one search found nothing close.
// Either way the model has something to judge on.
//
// It excludes the questions a document cannot answer, whichever way the
// question is put: a greeting, a question about this conversation, a question
// about the learner's own past, another user's material (which is refused
// before any search), and a learner who said not to look in their notes.
func requiresForcedDocumentSearch(question string) bool {
	if isSessionContextQuestion(question) {
		return false
	}
	if isCrossUserKnowledgeRequest(question) {
		return false
	}
	if isHistoryRecallQuestion(question) {
		return false
	}
	if isDirectGreeting(question) {
		return false
	}
	if isExplicitNoSearchQuestion(question) {
		return false
	}
	return true
}

// ownMaterialMarkers are the phrases that point the question at the learner's
// own uploaded notes, files or documents.
var ownMaterialMarkers = []string{
	"我的笔记", "我的知识库", "我的资料", "我的文档", "我的文件", "我的材料",
	"我的学习记录", "我的学习笔记", "我的记录", "我的学习资料",
	"我上传的", "我上传", "我存的", "我存的文件",
	"笔记里", "笔记中", "知识库里", "知识库中", "资料里", "资料中",
	"文档里", "文档中", "文件里", "文件中", "材料里",
	"学习记录里", "学习记录中",
}

// mentionsOwnMaterial reports whether the question points at the learner's own
// uploaded notes, files or documents.
//
// It is deliberately narrow. A question that merely sounds technical is not
// about the learner's material - that is the model's judgement to make, and
// the server-side pre-retrieval is what gives it the material to judge with.
// What this predicate catches is the question that says outright that the
// answer is in there: "我的笔记", "我上传的文件", "我的资料里有讲 X 的吗".
//
// The phrases are matched on a normalized question, so punctuation and spacing
// cannot hide a match, and the possessive forms are matched with a pattern so
// that "我上传的那份笔记" is caught as readily as "我的笔记".
func mentionsOwnMaterial(question string) bool {
	normalized := normalizeSearchDecisionText(question)
	if normalized == "" {
		return false
	}
	for _, marker := range ownMaterialMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	// The possessive forms the phrase list cannot cover: "我的那份笔记",
	// "我上传的这份文档", "我存下的资料".
	return mentionsPossessiveMaterial(normalized)
}

// mentionsPossessiveMaterial matches a first-person possessive followed by a
// material noun, with anything short in between.
//
// The gap is short on purpose. "我的笔记" is one word apart and must match;
// "我最近在读的一本关于 Go 的书" must not, and it does not because there is no
// material noun in it.
func mentionsPossessiveMaterial(normalized string) bool {
	possessives := []string{"我的", "我上传的", "我存的", "我存下的", "我这份", "我那份"}
	nouns := []string{"笔记", "知识库", "资料", "文档", "文件", "材料", "上传", "学习记录", "学习笔记"}
	for _, possessive := range possessives {
		index := 0
		for {
			found := strings.Index(normalized[index:], possessive)
			if found < 0 {
				break
			}
			start := index + found + len(possessive)
			index = start
			if start+6 > len(normalized) {
				break
			}
			gap := normalized[start:minInt(start+6, len(normalized))]
			for _, noun := range nouns {
				if strings.Contains(gap, noun) {
					return true
				}
			}
		}
	}
	return false
}

// normalizeSearchDecisionText folds a question into the form the decision
// predicates compare: lower case, no spaces, no punctuation, no symbols.
//
// It is the same normalization the old grounding predicate used, and it is
// here for the same reason: a phrase table that a full stop can hide is a
// phrase table that does not say what it means.
func normalizeSearchDecisionText(question string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(question))
}

// isDirectGreeting reports whether the question is one of the short greetings
// and identity questions that need nothing at all.
func isDirectGreeting(question string) bool {
	normalized := normalizeSearchDecisionText(question)
	if normalized == "" {
		return false
	}
	_, direct := directConversationQuestions[normalized]
	return direct
}

// noSearchMarkers are the ways a learner says "do not go and look in my
// notes".
//
// This is the mirror image of requiresKnowledgeOnly: one set of phrases says
// "answer from my notes and nothing else", the other says "do not touch my
// notes". Both are instructions about the source rather than questions about
// relevance, and both are the learner's decision to make.
//
// The list is about instructions, not topics. A question about a subject the
// notes happen to cover is not an instruction, and the pre-retrieval is what
// decides that case.
var noSearchMarkers = []string{
	"不用查", "不要查", "别查", "不查我的", "别查我的", "不要查我的",
	"不用搜", "不要搜", "别搜", "不搜我的", "别搜我的", "不要搜我的",
	"不用检索", "不要检索", "别检索", "不用搜索", "不要搜索", "别搜索",
	"不用引用", "不要引用", "别引用",
	"不用翻", "不要翻", "别翻", "别去翻", "不用去翻", "不要去翻",
	"别动我的", "不用动我的", "不要动我的",
	"别用我的", "不用我的", "不要用我的",
	"凭你自己的常识", "用自己的常识", "用你自己的常识",
	"一般性说法", "一般知识", "通用知识",
	"不查资料", "不用查资料", "不要查资料", "别查资料",
	"不查我的笔记", "不用我的笔记", "不要用我的笔记",
}

// isExplicitNoSearchQuestion reports whether the learner told the product not
// to look at their material.
//
// It is honoured by removing the document tools from this turn's table
// altogether, rather than by asking the model not to use them. A model told
// not to use a tool may still use it; a model that cannot see it cannot.
func isExplicitNoSearchQuestion(question string) bool {
	normalized := normalizeSearchDecisionText(question)
	if normalized == "" {
		return false
	}
	for _, marker := range noSearchMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}
