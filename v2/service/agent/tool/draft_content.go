package agenttool

import (
	"strings"
	"unicode"
)

// Where a draft's content may come from.
//
// The tool's description says "write into it only what this message states",
// and that used to be the whole of the rule: the model was asked, and whatever
// it wrote became a pending draft with the learner's name on it.
//
// That is enough while the tool is only offered when the learner asked to
// save something, because then the model's attention is on the message it was
// given. It is not enough once the tool is on the table every turn, because a
// model that has just read a passage out of the learner's notes is holding
// exactly the kind of text a draft can be built from - and a draft built from
// a retrieved passage is a false memory that the learner did not say.
//
// So the server checks it. The check is a coverage test rather than an
// equality test, because a model restates: "我最喜欢用 Go 写后端" becomes
// "用户最喜欢用 Go 写后端", and that is the learner's own words with a
// different subject in front of them. What must not happen is a draft that
// says something the learner's message does not say, and that shows up as
// content whose wording is not in the message.
//
// The subject is not checked. It is a label, it is short, and the rule is
// about the fact being remembered rather than about the title on it.

// minDraftContentCoverage is the share of the content's character pairs that
// have to appear in the current user message.
//
// It is a threshold, not a proof. A content that shares almost nothing with
// the message is refused; a content that shares most of it is accepted. The
// band in between is where a restatement and an invention are hardest to tell
// apart, and the threshold sits in the middle of it: a restatement loses only
// its leading subject, while an invention loses the sentence it was built
// from.
const minDraftContentCoverage = 0.6

// draftContentFromCurrentMessage reports whether the draft's content is
// grounded in the learner's current message.
//
// An empty content is refused: a draft with nothing in it remembers nothing,
// and accepting it would let a model create an empty card.
func draftContentFromCurrentMessage(question, content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	wanted := draftContentPairs(content)
	if len(wanted) == 0 {
		return false
	}
	source := draftContentPairs(question)
	covered := 0
	for pair := range wanted {
		if _, ok := source[pair]; ok {
			covered++
		}
	}
	return float64(covered)/float64(len(wanted)) >= minDraftContentCoverage
}

// draftContentPairs is the set of character pairs a text is made of.
//
// Punctuation, spacing and case are folded away first, so that "Go 后端" and
// "go后端" are the same text, and a full stop at the end of a sentence cannot
// hide the sentence it belongs to.
func draftContentPairs(text string) map[string]struct{} {
	folded := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, strings.TrimSpace(text))
	runes := []rune(folded)
	if len(runes) == 0 {
		return nil
	}
	pairs := make(map[string]struct{}, len(runes))
	if len(runes) == 1 {
		pairs[string(runes)] = struct{}{}
		return pairs
	}
	for index := 0; index+1 < len(runes); index++ {
		pairs[string(runes[index:index+2])] = struct{}{}
	}
	return pairs
}

// draftContentRejection is what the model is told when its draft did not come
// from the learner's message.
//
// It names the rule and asks for a rewrite rather than refusing the turn: the
// model can say the right thing on the next call, and the learner gets no
// card that says something they never said.
const draftContentRejection = "The draft was not created: a draft may only record what the learner's current message says. " +
	"Write the content using the learner's own words from that message, without adding anything it did not say, and call the tool again."
