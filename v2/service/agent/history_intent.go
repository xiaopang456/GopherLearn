package agent

import (
	"regexp"
	"strings"
)

// Why the history rule reads one request at a time.
//
// The rule used to be a phrase table over the whole question. If the question
// contained 原话, 一字不差, 逐字, 原文, 我说了什么, 我怎么说 or 当时怎么说,
// then the turn was taken to be a request for the learner's own words, and the
// server required a read-back of this conversation's history before it would
// answer.
//
// A phrase table cannot tell a request from a description, and it cannot tell
// the learner's words from the learner's notes.
//
//   - "原话用来核对证据" is a description. The learner was explaining what a
//     whiteboard, a summary segment and a transcript are for. The table read it
//     as a request, so the turn was forced to read a history message it did not
//     need, and with no history to read the answer was refused with
//     "未能读取到可验证的历史原话". A conceptual question was answered with a
//     refusal.
//   - "请引用《03-layered-summary.md》里白板定义的原文" asks for a document's
//     wording. The table read "原文" as a request for a message of this
//     conversation and sent a document question to the history reader.
//
// One question can hold two requests at once, and they do not belong together:
//
//   - "我上次提到的 03-layered-summary.md 里的原话是什么？" asks for the
//     learner's own words. The file name is the topic those words were about,
//     not the thing being quoted.
//   - "请引用《分层摘要》里白板定义的原文" asks for a document's wording. The
//     title is the thing being quoted.
//
// So the two questions - does this request quote at all, and whose words does
// it quote - have to be asked of the same request. Reading them off the whole
// question makes one of them answer about the other.
//
// The rule therefore works one request at a time, and each request is read in
// four parts:
//
//  1. Does it quote? ("原话", "逐字", "一字不差", "原文", ...) or does it ask for
//     words to be produced? ("贴出", "复述", "照抄", ...) A sentence that only
//     talks about quoting - "逐字复述是用来核对证据的" - is none of that: the
//     verb there is a noun, and a subject being discussed is not a request.
//
//  2. What is the object? The words being quoted belong either to this
//     conversation ("我上次说的口诀", "我刚才那段话") or to a document
//     ("我上次上传的文档里白板定义的原文", "《分层摘要》里的原文"). A time phrase
//     that points at a file ("上次上传") points at the document, not at the
//     conversation, and the object is what the evidence check hangs on.
//
//  3. Is it a request? A question about the quoted words ("原话是什么？"), an
//     instruction addressed to the assistant ("请逐字贴出…"), or an action to
//     produce the words.
//
//  4. Which object does the request have? The conversation, the document, or
//     none - and a modifier that follows it belongs to it ("请贴出我刚才那段话，
//     逐字" is one request).
//
// A request whose object is a document keeps the document's own evidence
// requirement: the answer has to rest on the document, and a turn that cannot
// cite it is refused for that reason. A request whose object is this
// conversation keeps the history requirement: the learner's words have to be
// read back, and a turn with nothing to read back says so.

// historyQuoteMarkers are the phrases that ask for wording to be reproduced
// exactly.
var historyQuoteMarkers = []string{
	"原话", "一字不差", "逐字", "原文", "我说了什么", "我怎么说", "当时怎么说",
}

// historyRecallAnchors point at this conversation's own earlier messages.
//
// They also decide who the quoted words belong to when the same request names
// a document: "我上次提到的 note.md 里的原话" is about the learner's words
// about note.md, not about note.md itself.
var historyRecallAnchors = []string{
	"你还记得", "还记得我", "我当时", "我之前", "我上次", "之前我", "上次我",
	"我的原话", "我说了什么", "我怎么说", "当时怎么说",
	"我刚才", "刚才那段", "刚说的",
	"之前我们", "前面我们", "上次我们", "本次对话", "当前对话", "本会话", "当前会话",
}

// historyActionMarkers are the ways a learner asks the assistant to produce
// words.
var historyActionMarkers = []string{
	"贴出", "贴出来", "贴一下", "复述", "照抄", "原封不动", "再说一遍", "再发一次",
	"念一遍", "写出来", "打出来", "抄出来", "重复一遍", "重复一次", "说出来",
	"发我", "贴给我", "说一遍",
}

// nominalAfterAction are the words that turn an action into a noun.
//
// "逐字复述" is a thing being discussed; "请复述我刚才那段话" is a request.
// What follows the verb tells them apart: a definition ("是用…", "指…", "的意思")
// makes it a subject rather than something to do.
var nominalAfterAction = []string{"是", "的", "指", "用", "这个", "那个", "到底", "什么", "意思"}

// documentProvenanceMarkers are the words that make a time phrase point at a
// file rather than at the conversation.
//
// "我上次上传的文档" is the document the learner uploaded last time, not a
// message of this conversation; "我上次说的口诀" is a message of it.
var documentProvenanceMarkers = []string{"上传", "下载", "导入", "保存", "存"}

// requestPolitenessMarkers are the words that address a request to the
// assistant.
var requestPolitenessMarkers = []string{"请", "帮我", "给我", "替我", "麻烦"}

// documentContainerMarkers are the containers a quoted passage can live in.
//
// A "原文" inside one of them is a document's wording, not a message of this
// conversation. "我上次贴的那段原文" is deliberately not here: that segment was
// pasted into the conversation, so it is a message of this conversation.
var documentContainerMarkers = []string{
	"文档里", "文档中", "文档内", "文档的",
	"笔记里", "笔记中", "笔记内", "笔记的",
	"文件里", "文件中", "文件内", "文件的",
	"资料里", "资料中", "资料内", "资料的",
	"文章里", "文章中", "文章内", "文章的",
	"知识库里", "知识库中", "知识库的",
	"章节里", "章节中", "章节的", "段落里", "段落中", "段落的",
}

// documentFileNamePattern matches a file name with a known extension, which is
// how a learner names a document of their own: "03-layered-summary.md".
var documentFileNamePattern = regexp.MustCompile(
	`(?i)(?:[\w\-]|[\p{Han}]{1,8})\.(?:md|markdown|mdx|txt|pdf|doc|docx|ppt|pptx|xlsx|csv|html?|json|ya?ml)\b`)

// clauseSeparators are the punctuation marks that end one request.
//
// The title marks 《》 are not among them: the title they wrap is part of the
// request that quotes it, and splitting there would cut the document away from
// the word that points at it.
const clauseSeparators = "。！？；;!?\n，,、（）()"

// quoteIntent is what one request asks for.
type quoteIntent int

const (
	// quoteIntentNone: the request does not ask for quoted wording.
	quoteIntentNone quoteIntent = iota
	// quoteIntentHistory: the request asks for the learner's own words from
	// this conversation.
	quoteIntentHistory
	// quoteIntentDocument: the request asks for a document's wording.
	quoteIntentDocument
)

// quoteObject is whose words a request asks for.
type quoteObject int

const (
	// quoteObjectNone: the request names no object.
	quoteObjectNone quoteObject = iota
	// quoteObjectConversation: the words belong to this conversation.
	quoteObjectConversation
	// quoteObjectDocument: the words belong to a document.
	quoteObjectDocument
)

// requiresExactHistoryEvidence reports whether this turn may only answer after
// reading the learner's own words back from this conversation.
//
// It is the single definition behind the two places that decide: the first
// tool choice binds read_history_message because of it, and the final answer
// check refuses a turn that has no such evidence because of it. One predicate,
// two call sites, and they cannot answer differently.
//
// It fires on a request, not on a phrase, and it reads one request at a time.
// "请逐字贴出我上次说的口诀" is a request. "原话用来核对证据" is a sentence about
// what a transcript is for. "请引用《03-layered-summary.md》里白板定义的原文"
// asks for a document.
func requiresExactHistoryEvidence(question string) bool {
	for _, request := range historyRequests(question) {
		if clauseQuoteIntent(request) == quoteIntentHistory {
			return true
		}
	}
	return false
}

// isHistoryRecallQuestion reports whether the question asks about this
// learner's past in this conversation.
//
// It is the wider of the two predicates: a question about the learner's own
// past is answered from that past, whether it asks for the words verbatim or
// only asks what they were. A question that quotes a document is not a
// question about the learner's past, and an incidental mention of the past in
// another request of a document question does not turn it into one.
//
// See history_intent.go for why the rule reads the request and not the
// vocabulary.
func isHistoryRecallQuestion(question string) bool {
	anchored := false
	document := false
	for _, request := range historyRequests(question) {
		switch clauseQuoteIntent(request) {
		case quoteIntentHistory:
			// A request for the learner's own words is a request about their
			// past, wherever in the question it sits.
			return true
		case quoteIntentDocument:
			document = true
		default:
			if clauseHasConversationAnchor(request) {
				anchored = true
			}
		}
	}
	// The turn quotes a document. A mention of the past in another request does
	// not make it a question about the past.
	if document {
		return false
	}
	return anchored
}

// historyRequests splits a question into its requests.
//
// A modifier stays with the request it modifies. "请贴出我刚才那段话，逐字" is
// one request: the "逐字" after the comma says how the message is to be
// reproduced, and the object it modifies is the message to the left. Judging
// the two halves apart reads the second half as a request that names no
// object, and a request for the learner's words that never reads them back.
//
// A question about the quoted words stands apart, though: "《分层摘要》是一份
// 笔记，原话是什么？" is two requests, because the first half says what the
// document is and the second half asks for the learner's own words.
func historyRequests(question string) []string {
	clauses := historyClauses(question)
	requests := make([]string, 0, len(clauses))
	for _, clause := range clauses {
		if len(requests) > 0 && clauseJoinsPreviousRequest(clause) && clauseHasObject(requests[len(requests)-1]) {
			requests[len(requests)-1] += clause
			continue
		}
		requests = append(requests, clause)
	}
	return requests
}

// clauseJoinsPreviousRequest reports whether the clause modifies the request
// to its left rather than being a request of its own.
//
// It names no object of its own, and it is not a question about the quoted
// words - those two things make it a modifier. A clause that names an object
// is a request about that object, and a question about the quoted words asks
// for something the request to its left did not.
func clauseJoinsPreviousRequest(clause string) bool {
	folded := normalizeSearchDecisionText(clause)
	if folded == "" {
		return false
	}
	if clauseHasObject(clause) {
		return false
	}
	return !asksAboutQuotedWords(folded)
}

// clauseQuoteIntent classifies one request.
//
// The order is the order of the questions in the file comment: the request has
// to quote or ask for words before anything else can be said about it, the
// object decides whose words they are, and the object of this conversation
// wins over the object of a document, because a file named in the same breath
// as "我上次说的" is the topic of those words.
func clauseQuoteIntent(clause string) quoteIntent {
	folded := normalizeSearchDecisionText(clause)
	if folded == "" {
		return quoteIntentNone
	}
	marker := containsAnyFolded(folded, historyQuoteMarkers)
	action := clauseHasAction(folded)
	if !marker && !action {
		return quoteIntentNone
	}
	ask := clauseHasAsk(folded)
	switch clauseQuoteObject(clause) {
	case quoteObjectDocument:
		return quoteIntentDocument
	case quoteObjectConversation:
		if action || ask {
			return quoteIntentHistory
		}
		return quoteIntentNone
	default:
		// No object named. Only a question about the quoted words can still be
		// a request for them: the words a learner asks for without naming an
		// object are their own.
		if marker && ask {
			return quoteIntentHistory
		}
		return quoteIntentNone
	}
}

// clauseQuoteObject reports whose words the request asks for.
func clauseQuoteObject(clause string) quoteObject {
	if clauseHasConversationAnchor(clause) {
		return quoteObjectConversation
	}
	if namesOwnDocument(clause) {
		return quoteObjectDocument
	}
	return quoteObjectNone
}

// clauseHasObject reports whether the request says whose words are being
// quoted: this conversation's, or a document's.
//
// The object is what the evidence check is bound to. A request that quotes but
// names no object asks for nothing in particular yet, and a request that names
// a document has bound the evidence check to that document.
func clauseHasObject(clause string) bool {
	return clauseQuoteObject(clause) != quoteObjectNone
}

// clauseHasConversationAnchor reports whether the request points at this
// conversation's own earlier messages.
//
// A time phrase that points at a file does not: "我上次上传的文档" is a
// document the learner uploaded, not a message of this conversation, and the
// words to be quoted there are the document's.
func clauseHasConversationAnchor(clause string) bool {
	folded := normalizeSearchDecisionText(clause)
	if folded == "" {
		return false
	}
	for _, anchor := range historyRecallAnchors {
		index := 0
		for {
			found := strings.Index(folded[index:], anchor)
			if found < 0 {
				break
			}
			start := index + found
			index = start + len(anchor)
			after := []rune(folded[index:])
			if len(after) > 6 {
				after = after[:6]
			}
			provenance := false
			for _, marker := range documentProvenanceMarkers {
				if strings.HasPrefix(string(after), marker) {
					provenance = true
					break
				}
			}
			if !provenance {
				return true
			}
		}
	}
	return false
}

// clauseHasAction reports whether the request asks the assistant to produce
// words.
//
// The verb has to be a verb. "逐字复述是用来核对证据的" uses it as the subject
// of a sentence about quoting, and that is not a request for anything.
func clauseHasAction(folded string) bool {
	for _, action := range historyActionMarkers {
		index := 0
		for {
			found := strings.Index(folded[index:], action)
			if found < 0 {
				break
			}
			start := index + found
			index = start + len(action)
			after := []rune(folded[index:])
			if len(after) > 4 {
				after = after[:4]
			}
			nominal := false
			for _, marker := range nominalAfterAction {
				if strings.HasPrefix(string(after), marker) {
					nominal = true
					break
				}
			}
			if !nominal {
				return true
			}
		}
	}
	return false
}

// clauseHasAsk reports whether the request asks for the quoted words.
//
// Three ways to ask: a question about the wording ("原话是什么？"), an
// instruction addressed to the assistant that quotes ("请逐字贴出…"), or an
// action to produce the words.
func clauseHasAsk(folded string) bool {
	if asksAboutQuotedWords(folded) {
		return true
	}
	return containsAnyFolded(folded, historyQuoteMarkers) &&
		containsAnyFolded(folded, requestPolitenessMarkers)
}

// asksAboutQuotedWords reports whether the question asks what the quoted words
// were, on either side of the marker: "原话是什么", "什么原话".
//
// The window after the marker is short on purpose. "原话用来核对证据" says
// nothing about what the words were, and the marker there is the noun of a
// sentence about evidence rather than a question about wording.
func asksAboutQuotedWords(folded string) bool {
	for _, marker := range historyQuoteMarkers {
		index := 0
		for {
			found := strings.Index(folded[index:], marker)
			if found < 0 {
				break
			}
			start := index + found
			index = start + len(marker)
			// The window is cut on runes, not on bytes: a byte cut lands
			// inside a Chinese character, and the question word after the
			// marker would no longer be the question word.
			after := []rune(folded[index:])
			if len(after) > 8 {
				after = after[:8]
			}
			for _, form := range quoteQuestionForms {
				if strings.HasPrefix(string(after), form) {
					return true
				}
			}
			before := []rune(folded[:start])
			if len(before) > 6 {
				before = before[len(before)-6:]
			}
			for _, form := range leadingQuoteQuestionForms {
				if strings.HasSuffix(string(before), form) {
					return true
				}
			}
		}
	}
	return false
}

// quoteQuestionForms are the question words that turn a quoted noun into a
// question about the wording itself: "原话是什么", "原话是怎么说的".
var quoteQuestionForms = []string{
	"是什么", "是怎么", "怎么说", "怎么讲", "叫什么", "是哪", "有哪些", "哪些", "什么样",
}

// leadingQuoteQuestionForms are the question words that stand in front of the
// quoted noun: "什么原话", "哪句原话".
var leadingQuoteQuestionForms = []string{"什么", "哪句", "哪些", "哪个", "哪种"}

// historyClauses splits a question into the pieces it is made of.
//
// A question can ask for two different things in two different clauses - "我上次
// 说的那句，请逐字贴出来" - and those two requests do not belong together: the
// first one is about the learner's own words, the second one asks for them
// verbatim. The separators are the punctuation that ends a request.
func historyClauses(question string) []string {
	clauses := make([]string, 0, 4)
	current := strings.Builder{}
	for _, r := range question {
		if strings.ContainsRune(clauseSeparators, r) {
			if current.Len() > 0 {
				clauses = append(clauses, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		clauses = append(clauses, current.String())
	}
	return clauses
}

// namesOwnDocument reports whether the request points at a document rather
// than at a message of this conversation.
//
// Three shapes count: a file name with a known extension, a title between 《》,
// and a container word ("笔记里", "文档中的"). A title counts whatever script it
// is written in - "《分层摘要》" is a document of the learner's just as much as
// "《03-layered-summary.md》" is, and a rule that only recognised the ASCII
// half of them read the Chinese one as a message of this conversation.
func namesOwnDocument(clause string) bool {
	if documentFileNamePattern.MatchString(clause) {
		return true
	}
	if inner, ok := titleMarkedText(clause); ok && strings.TrimSpace(inner) != "" {
		return true
	}
	folded := normalizeSearchDecisionText(clause)
	if folded == "" {
		return false
	}
	return containsAnyFolded(folded, documentContainerMarkers)
}

// titleMarkedText returns the text between the first 《 and the first 》 after
// it.
func titleMarkedText(question string) (string, bool) {
	open := strings.Index(question, "《")
	if open < 0 {
		return "", false
	}
	rest := question[open+len("《"):]
	close := strings.Index(rest, "》")
	if close < 0 {
		return "", false
	}
	return rest[:close], true
}

// containsAnyFolded reports whether the folded text contains any of the
// markers.
func containsAnyFolded(folded string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(folded, marker) {
			return true
		}
	}
	return false
}
