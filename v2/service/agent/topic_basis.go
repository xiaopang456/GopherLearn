package agent

import (
	"regexp"
	"strings"
	"unicode"

	"GopherAI/common/rag"
)

// Why the answer basis hangs on the object the learner asked about.
//
// The learner's own statement about a layered summary - "原话用来核对证据" - was
// read as a request for the learner's own words. The rule behind it was a
// phrase table: 原话, 一字不差, 逐字, 原文, 我说了什么, 我怎么说, 当时怎么说,
// and a hit made the turn a request for a verbatim quotation.
//
// A phrase table cannot tell a request from a description, and it cannot tell
// the learner's words from the learner's notes. Two questions have to be asked
// of the same request instead:
//
//  1. Does this request quote at all?
//  2. Whose words does it quote?
//
// The second question is what the basis hangs on. A confirmed topic note is the
// learner's own material too, and an answer that rests on it is grounded - but
// grounding in a note is not grounding in the uploaded documents, and the two
// must not be recorded as one.
//
// So the rule reads one request at a time, and a request is read in four parts:
//
//  1. does it quote at all? ("原话", "逐字", "一字不差", "原文", "贴出", "复述")
//  2. whose words does it quote? this conversation's messages, or a document?
//  3. is it a request? a question about the quoted words, an instruction
//     addressed to the assistant, or an action to produce them
//  4. which object does the request have? the conversation, the document, or -
//     when the learner confirmed a topic - the topic note
//
// The receipt - "本回答使用了：分层摘要" - is not evidence of anything. It says
// the notes reached the model's context. Only the answer itself says what it
// rests on.

// confirmedTopicSourcePrefix is the Source prefix a confirmed topic note
// carries in the evidence registry.
//
// The Source names the topic the note belongs to, which is what makes "whose
// words is this answer about" answerable from a reference alone. A note the
// server bootstrapped, or one a tool read under the current topic, carries a
// different prefix: those are the server's own reads, not the learner's
// choice, and the learner chose nothing there.
const confirmedTopicSourcePrefix = "user_confirmed_topic:"

// topicMemoryRefPattern matches one topic-semantic reference.
//
// It is the same shape as the registry's own memory reference pattern, and it
// is narrowed to the topic letter on purpose: a reference to the conversation
// (C), to an episode (E) or to the profile (G) is not a topic note, and an
// answer that cites those is not a topic answer.
var topicMemoryRefPattern = regexp.MustCompile(`\[S([0-9]{1,3})\]`)

// topicNoteFragmentRunes is how many characters of a note have to appear in an
// answer before the server says the answer rests on that note.
//
// The note's own words, not a paraphrase of them. The learner asked for a
// mnemonic "as written" and the answer has it as written, and the server can
// see that without guessing: a run of this many characters of the note, in
// the answer, in the same order.
//
// Eight characters is a long way from accidental in Chinese text, and it is
// long enough to carry a phrase rather than a word: "现值看白板，旧值翻档案"
// is the learner's own construction, and no unrelated answer happens to
// contain it.
const topicNoteFragmentRunes = 8

// topicNoteMinRunes is the shortest note that can carry the words signal at
// all.
//
// A note of a few characters ("缓存") is a title, not a statement, and an
// answer that happens to contain the title is not an answer built on the
// note. A note this short can still be attributed through its reference,
// which is the model's own statement about what it used.
const topicNoteMinRunes = 8

// ConfirmedTopicNote is one note the learner confirmed for this question.
//
// It is the note as the window rendered it: the reference it was given, the
// topic it belongs to, and the learner's own words. The text is here because
// the server has to be able to check that the answer rests on the note, and
// the reference alone only says the model was shown the note.
type ConfirmedTopicNote struct {
	// Ref is the reference the note was given in the window, e.g. "S1".
	Ref string
	// TopicKey is the topic the note belongs to.
	TopicKey string
	// Subject is the note's own subject line.
	Subject string
	// Text is the learner's own wording, as rendered into the window.
	Text string
}

// topicAttribution is what one answer rests on, when it rests on the learner's
// own confirmed topic notes.
type topicAttribution struct {
	// Keys are the topic keys whose confirmed notes the answer used, in the
	// order the evidence first appeared.
	Keys []string
	// Names are the display names of those topics, in the same order.
	//
	// A topic with no display name falls back to its own key rather than
	// dropping out of the list, because the learner recognises a subject by
	// its name and a list of names has to line up with the list of keys.
	Names []string
}

// Empty reports whether the answer could be attributed to any topic note.
func (t topicAttribution) Empty() bool { return len(t.Keys) == 0 }

// attributeToTopicNotes decides whether this answer rests on the learner's own
// confirmed notes, and on which subject's notes.
//
// Two signals say it does, and both of them are about the answer rather than
// about the window:
//
//  1. The answer cites a note's reference. The registry resolves it, and only
//     a note the learner confirmed carries the source prefix that says the
//     learner chose it.
//  2. The answer repeats the note's own words. A learner who asked for a
//     mnemonic "as written" gets it back as written, and the server can see
//     the note's text inside the answer.
//
// This is the check the review asked for. The receipt is not enough: a topic
// the learner confirmed says the notes reached the context, and only the
// answer itself says the answer rests on them.
func attributeToTopicNotes(registry *MemoryEvidenceRegistry, notes []ConfirmedTopicNote,
	answer string, topicNames map[string]string) topicAttribution {
	if strings.TrimSpace(answer) == "" {
		return topicAttribution{}
	}
	attribution := topicAttribution{}
	seenKeys := map[string]struct{}{}
	appendKey := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, duplicate := seenKeys[key]; duplicate {
			return
		}
		seenKeys[key] = struct{}{}
		attribution.Keys = append(attribution.Keys, key)
		name := strings.TrimSpace(topicNames[key])
		if name == "" {
			// A topic with no display name is still a subject the learner
			// confirmed, and the basis has to name it. The key is what the
			// learner saw in the selection card when there was no other name.
			name = key
		}
		attribution.Names = append(attribution.Names, name)
	}
	// The reference first: it is the model's own statement about what it used.
	for _, match := range topicMemoryRefPattern.FindAllStringSubmatch(answer, -1) {
		if registry == nil {
			break
		}
		entry, err := registry.Lookup("S" + match[1])
		if err != nil || entry.Kind != MemoryEvidenceTopic {
			continue
		}
		if !strings.HasPrefix(entry.Source, confirmedTopicSourcePrefix) {
			continue
		}
		appendKey(strings.TrimPrefix(entry.Source, confirmedTopicSourcePrefix))
	}
	// The note's own words second: a learner who asked for a mnemonic as
	// written gets it as written, and a reference is not always written.
	for _, note := range notes {
		if !strings.HasPrefix(note.Ref, "S") {
			continue
		}
		if !answerRepeatsNote(answer, note.Text) {
			continue
		}
		appendKey(note.TopicKey)
	}
	return attribution
}

// answerRepeatsNote reports whether the answer carries the note's own words.
//
// It is a run of the note's text, long enough to be a phrase rather than a
// word, appearing in the answer in the same order. Whitespace and the
// emphasis marks of markdown are ignored on both sides, because the learner
// asked for a sentence and the answer formatted it.
func answerRepeatsNote(answer, note string) bool {
	noteRunes := []rune(foldTopicText(note))
	if len(noteRunes) < topicNoteMinRunes {
		return false
	}
	answerRunes := []rune(foldTopicText(answer))
	if len(answerRunes) == 0 {
		return false
	}
	run := topicNoteFragmentRunes
	if len(noteRunes) < run {
		run = len(noteRunes)
	}
	for start := 0; start+run <= len(noteRunes); start++ {
		if containsRunes(answerRunes, noteRunes[start:start+run]) {
			return true
		}
	}
	return false
}

// containsRunes reports whether the haystack carries the needle in order.
func containsRunes(haystack, needle []rune) bool {
	if len(needle) == 0 || len(needle) > len(haystack) {
		return false
	}
	for start := 0; start+len(needle) <= len(haystack); start++ {
		if string(haystack[start:start+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}

// foldTopicText removes the characters that carry no wording: whitespace, the
// emphasis marks of markdown and the quote marks around a phrase.
//
// The wording is what has to match. Everything else is how the answer chose to
// lay the same words out.
func foldTopicText(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			continue
		case strings.ContainsRune("*_`#>~《》\"'“”‘’", r):
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// topicDisplayNames maps topic keys to the names the learner knows them by.
//
// It is read from the same place the receipt is built from, so the name in the
// answer basis is the name the learner saw in the selection card.
func topicDisplayNames(states []ConfirmedTopicLearningState, records []TopicMemoryRecord) map[string]string {
	names := map[string]string{}
	for _, state := range states {
		key := strings.TrimSpace(state.TopicKey)
		name := strings.TrimSpace(state.DisplayName)
		if key == "" || name == "" {
			continue
		}
		if _, exists := names[key]; !exists {
			names[key] = name
		}
	}
	for _, record := range records {
		key := strings.TrimSpace(record.TopicKey)
		if key == "" {
			continue
		}
		if _, exists := names[key]; exists {
			continue
		}
		if name := strings.TrimSpace(record.Subject); name != "" {
			names[key] = name
		}
	}
	return names
}

// questionDemandsDocument reports whether this question asks the learner to
// quote an uploaded document.
//
// Two ways to ask, and they are the two the product already knows:
//
//   - the question names the learner's own uploaded material ("我的笔记里…",
//     "我上传的文档…"). That is a request for the documents.
//   - the question quotes a document by name or by title ("请引用《03-layered-summary.md》…").
//
// A file name on its own is not enough: "我上次提到的 03-layered-summary.md 里的原话是什么？"
// is a question about the learner's own words, and the file is only what they
// were talking about.
//
// This is the question the topic branch asks before it takes an answer away
// from the document rules: a question that asks for a document keeps its
// document rules, and only a question that asks for something else may be
// answered from a confirmed topic note.
func questionDemandsDocument(question string) bool {
	if mentionsOwnMaterial(question) {
		return true
	}
	if !namesOwnDocument(question) {
		return false
	}
	return !pointsAtConversation(question)
}

// pointsAtConversation reports whether the question asks about what was said
// in this conversation.
//
// It is the same anchor list the history rule reads: "我上次", "我刚才", "我的原话"
// and the rest. A question that points here is asking about the learner's own
// words, whatever files it mentions.
func pointsAtConversation(question string) bool {
	folded := normalizeSearchDecisionText(question)
	if folded == "" {
		return false
	}
	for _, anchor := range historyRecallAnchors {
		if strings.Contains(folded, anchor) {
			return true
		}
	}
	return false
}

// topicAttributionFor decides whether this answer rests on one of the notes the
// learner confirmed for this question.
//
// The caller holds the state lock.
func (s *runState) topicAttributionFor(answer string) topicAttribution {
	if len(s.confirmedTopicNotes) == 0 && s.memoryTrace == nil {
		return topicAttribution{}
	}
	return attributeToTopicNotes(s.memoryTrace, s.confirmedTopicNotes, answer, s.usedTopicNames)
}

// documentsAreDemanded reports whether the learner's question put the uploaded
// documents in charge of this turn.
//
// A question that names the learner's own material, or quotes a document by
// name, is answered from the documents or not at all - and the document rules
// keep their refusals there. A question that asks for a topic note is a
// different question, and the note is the source it asked for.
//
// The caller holds the state lock.
func (s *runState) documentsAreDemanded() bool {
	if s.knowledgePolicy == rag.KnowledgeOnly {
		return true
	}
	return questionDemandsDocument(s.input.Question)
}

// copyStringMap copies a name lookup so that a later change to the window
// cannot change what a finished run recorded.
func copyStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
