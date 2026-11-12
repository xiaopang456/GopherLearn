package agent

// What the learner said, and whether it was about the thing on the board.
//
// The engine cannot ask what a learner meant. It can only read what they
// wrote and ask one narrow question: did they state this value, about this
// thing, as the state it is in now?
//
// The answer is not "the value appears in the message". A learner who writes
// "do not change it to 41 pages, keep 26" has written 41 and has said the
// opposite. A learner who writes "what would happen if it were 41 pages" has
// written 41 and has decided nothing. A learner who writes "the other book is
// 41 pages" has written 41 about another thing. A learner who writes "give me
// three exercises" has written three and asked for them.
//
// So a message is read as a list of statements - the clauses between the
// punctuation - and each statement is read for two things: the value it
// carries, and the qualifiers that turn it into something other than a plain
// statement about the current value.
//
// The rule that falls out of it: the last plain statement that carries a
// value is what the learner says now. A qualified statement - negated,
// hypothetical, asked about, asked for, about another thing, or about the
// past - is not a statement about the current value at all and is skipped. A
// statement that carries another value of the same quantity cancels the one
// before it: the learner who wrote both has not said which of the two is
// current, and the engine does not guess.
//
// The unit is what makes two values comparable. "41 pages" and "41 minutes"
// are two values, and a learner who wrote one of them has not said the
// other.

import (
	"strconv"
	"strings"

	"GopherAI/model"
)

// whiteboardStatementSeparators are the marks that end a statement.
//
// A period and a comma are not among them: a decimal point and a thousands
// separator belong to a number, and a sentence in another language is rarer
// here than a number with two decimals in it.
//
// A question mark is not among them either. It says that the clause it ends
// is a question, and a clause that ends in one is not a statement - so it
// has to stay inside the clause for the reader to see it.
const whiteboardStatementSeparators = "，。；、\n\r！"

// whiteboardQualifier is what a statement carries that makes it something
// other than a plain statement about the current value.
//
// The kinds are kept apart because they are read apart: a negated statement
// is not a statement, a hypothetical one is not a decision, a question is not
// an answer, a request is not a decision, a statement about another thing is
// about another thing, and a statement about the past is about a value that
// is no longer in force.
type whiteboardQualifier string

const (
	whiteboardQualifierNegation   whiteboardQualifier = "negation"
	whiteboardQualifierHypothesis whiteboardQualifier = "hypothesis"
	whiteboardQualifierQuestion   whiteboardQualifier = "question"
	whiteboardQualifierRequest    whiteboardQualifier = "request"
	whiteboardQualifierOtherThing whiteboardQualifier = "other-thing"
	whiteboardQualifierPast       whiteboardQualifier = "past"
)

// whiteboardQualifierMarkers are the words that carry each qualifier.
//
// They are a closed list, and each of them is read inside one statement
// only: a qualifier in one clause is about the value in that clause, not
// about every value in the message.
//
// The list is a list of words and not of rules, so it is a heuristic with
// edges. A clause that carries a word nobody thought of is read as plain,
// and a clause that carries one of these words for another reason is read as
// qualified. Both ways the engine stays on the safe side: a value it cannot
// read as stated is not made the learner's current one.
var whiteboardQualifierMarkers = map[whiteboardQualifier][]string{
	whiteboardQualifierNegation: {
		"不要", "不用", "不必", "无需", "不再", "不许", "不能", "不可以", "禁止",
		"先不", "暂时不", "不算", "不作数", "不是", "没必要", "别改", "别弄", "别用", "别再",
	},
	whiteboardQualifierHypothesis: {
		"如果", "假如", "假使", "假设", "万一", "倘若", "若是", "会怎样", "怎么样", "好不好",
	},
	whiteboardQualifierQuestion: {
		"吗", "呢", "行吗", "可不可以", "？", "?",
	},
	whiteboardQualifierRequest: {
		"给我", "给个", "来个", "来几", "来点", "推荐", "想要", "要几", "需要几",
		"写几", "出几", "挑几",
	},
	whiteboardQualifierOtherThing: {
		"另一", "别的", "其他",
	},
	whiteboardQualifierPast: {
		"原来", "原先", "之前", "以前", "上次", "先前", "那版", "上一版", "早先", "当初",
	},
}

// whiteboardNumber is a number and the unit it is measured in.
type whiteboardNumber struct {
	value float64
	unit  string
}

// whiteboardStatement is one clause of a learner's message.
type whiteboardStatement struct {
	text    string
	numbers []whiteboardNumber
	// qualifiers are the kinds of qualifier the clause carries.
	qualifiers map[whiteboardQualifier]bool
}

// whiteboardValueForm is a value as the learner can say it.
//
// A value that carries a number is said by that number and its unit, because
// that is how a learner says one. A value that carries no number is said as
// itself, because a name is said as a name.
type whiteboardValueForm struct {
	number    float64
	unit      string
	plain     string
	hasNumber bool
}

// whiteboardValueVerdict is what a statement says about a value.
type whiteboardValueVerdict int

const (
	// whiteboardValueSilent says nothing about the quantity.
	whiteboardValueSilent whiteboardValueVerdict = iota
	// whiteboardValueAgrees states the value.
	whiteboardValueAgrees
	// whiteboardValueDiffers states another value of the same quantity.
	whiteboardValueDiffers
)

// whiteboardValueForms reads the ways a value can be said.
func whiteboardValueForms(value string) (whiteboardValueForm, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return whiteboardValueForm{}, false
	}
	form := whiteboardValueForm{plain: whiteboardPlainText(trimmed)}
	if number, unit, ok := parseWhiteboardValue(trimmed); ok {
		form.number = number
		form.unit = unit
		form.hasNumber = true
	}
	return form, true
}

// whiteboardNumbersIn reads the numbers a text carries.
func whiteboardNumbersIn(text string) []whiteboardNumber {
	matches := whiteboardValuePattern.FindAllStringSubmatch(text, -1)
	numbers := make([]whiteboardNumber, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		number, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		numbers = append(numbers, whiteboardNumber{value: number, unit: match[2]})
	}
	return numbers
}

// whiteboardStatementUnits reads a learner's message as its statements.
func whiteboardStatementUnits(text string) []whiteboardStatement {
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return strings.ContainsRune(whiteboardStatementSeparators, r)
	})
	statements := make([]whiteboardStatement, 0, len(parts))
	for _, part := range parts {
		clause := strings.TrimSpace(part)
		if clause == "" {
			continue
		}
		statements = append(statements, whiteboardStatement{
			text:       clause,
			numbers:    whiteboardNumbersIn(clause),
			qualifiers: whiteboardQualifiersIn(clause),
		})
	}
	return statements
}

// whiteboardQualifiersIn reads the qualifiers a clause carries.
func whiteboardQualifiersIn(text string) map[whiteboardQualifier]bool {
	found := map[whiteboardQualifier]bool{}
	for qualifier, markers := range whiteboardQualifierMarkers {
		for _, marker := range markers {
			if strings.Contains(text, marker) {
				found[qualifier] = true
				break
			}
		}
	}
	return found
}

// whiteboardStatementIsPlain reports whether a clause is a plain statement
// about the current value.
//
// A clause that carries a qualifier is not one. It is negated, or
// hypothetical, or a question, or a request, or about another thing, or
// about the past, and none of those says what the value is now.
func whiteboardStatementIsPlain(statement whiteboardStatement) bool {
	return len(statement.qualifiers) == 0
}

// whiteboardUnitsCompatible reports whether two units measure the same thing.
//
// The unit is read out of the text around the number, and the text around a
// number is not always the unit alone: "5 天交" is five days, and the "交"
// is the verb. So one unit is taken to be the other when one of them starts
// the other - "天" and "天交" are one unit, and "页" and "分钟" are two.
//
// A unit that is empty is not compatible with anything: an entry that
// measures something and an entry that names something are two entries.
func whiteboardUnitsCompatible(left, right string) bool {
	if left == right {
		return true
	}
	if left == "" || right == "" {
		return false
	}
	return strings.HasPrefix(left, right) || strings.HasPrefix(right, left)
}

// whiteboardStatementVerdict reads what a clause says about a value.
//
// The unit is what makes the comparison meaningful: "48" and "48 minutes"
// are the same value, and "48 minutes" and "48 hours" are not. A clause that
// carries another value of the same quantity says something about the
// quantity, and what it says is not this value.
func whiteboardStatementVerdict(statement whiteboardStatement, form whiteboardValueForm) whiteboardValueVerdict {
	if !form.hasNumber {
		// A name cannot be compared with another name: the engine does
		// not know what else in the message is a name.
		if form.plain == "" {
			return whiteboardValueSilent
		}
		if strings.Contains(whiteboardPlainText(statement.text), form.plain) {
			return whiteboardValueAgrees
		}
		return whiteboardValueSilent
	}
	verdict := whiteboardValueSilent
	for _, number := range statement.numbers {
		if !whiteboardUnitsCompatible(number.unit, form.unit) {
			continue
		}
		if number.value == form.number {
			verdict = whiteboardValueAgrees
		} else {
			verdict = whiteboardValueDiffers
		}
	}
	return verdict
}

// whiteboardMessageVerdict reads what a learner's message says about a
// value.
//
// The last plain statement that carries a value is the one that counts: a
// learner who says one thing and then another has said the later one.
func whiteboardMessageVerdict(text string, form whiteboardValueForm) whiteboardValueVerdict {
	verdict := whiteboardValueSilent
	for _, statement := range whiteboardStatementUnits(text) {
		if !whiteboardStatementIsPlain(statement) {
			continue
		}
		if said := whiteboardStatementVerdict(statement, form); said != whiteboardValueSilent {
			verdict = said
		}
	}
	return verdict
}

// whiteboardLearnerStatesValue reports whether one of the learner's own
// messages in this batch states the value, and which message said it.
//
// The messages that are read are the learner's own messages that the entry
// cites: the entry's citations are the chain of evidence the model offered,
// and a value the learner's words in this batch back up is the learner's
// own. A message the entry does not cite is not evidence for it - that is
// what keeps a stale proposal of the assistant's from putting an old value
// back on a board the learner has already moved on from.
//
// The messages are read in the order they were written, and the last one
// that says anything about this quantity is the one that counts. A message
// that says another value of the same quantity leaves the learner with two
// values and no way to tell which one is current, so the engine says
// nothing at all.
func whiteboardLearnerStatesValue(value string, ids []uint, batch WhiteboardBatch) (uint, bool) {
	form, ok := whiteboardValueForms(value)
	if !ok {
		return 0, false
	}
	messageID := uint(0)
	stated := false
	for index := range batch.MessageIDs {
		if index >= len(batch.Messages) {
			break
		}
		if !whiteboardCitesID(ids, batch.MessageIDs[index]) {
			continue
		}
		switch whiteboardMessageVerdict(batch.Messages[index], form) {
		case whiteboardValueAgrees:
			messageID = batch.MessageIDs[index]
			stated = true
		case whiteboardValueDiffers:
			messageID = 0
			stated = false
		}
	}
	return messageID, stated
}

// whiteboardLearnerOwnEntry reports whether the entry is the learner's own
// fact, and the message of the learner's that says it.
//
// It is the learner's own fact when the learner's message in this batch
// states the value and no assistant message the entry cites comes before
// it. The label the model wrote on the entry does not decide it: a model
// that calls the learner's own new value the assistant's is wrong about the
// label and right about the words.
//
// The entry does not have to replace a value the board already holds. A
// board that is empty because the last batch failed to generate is exactly
// the case where the learner's own words are the only thing there is.
func whiteboardLearnerOwnEntry(entry model.SessionWhiteboardEntry, batch WhiteboardBatch) (uint, bool) {
	messageID, stated := whiteboardLearnerStatesValue(entry.Value, entry.SourceMessageIDs, batch)
	if !stated {
		return 0, false
	}
	if whiteboardCitesAssistantBefore(entry.SourceMessageIDs, messageID, batch) {
		return 0, false
	}
	return messageID, true
}
