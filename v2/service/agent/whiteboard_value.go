package agent

// The shape of a value, and the questions two board entries may ask about it.
//
// A value is what the learner wrote down: a number counted in a unit, or a
// name. The board compares values, and it does it by what the value is rather
// than by how it is written: "41 页" and "41 页/次" are the same number of
// pages, and "41 分钟" is not.
//
// Two questions are asked here, and neither of them is about what a sentence
// means:
//
//   - does a message carry this value? That is a fact about the message, and
//     it is what decides whether the learner's own words can be kept as a
//     pointer to that message.
//
//   - are two entries about the same thing? A number is not the name of a
//     thing, so an entry that carries the number the board already holds
//     does not decide on its own that it is the same item. The frequency
//     words are read for that question only, and only between two entries:
//     they are not a check on what the learner said.

import (
	"fmt"
	"strconv"
	"strings"

	"GopherAI/model"
)

// whiteboardValuesInText reads every measurement the text carries.
//
// A text may carry more than one number, and each of them is one of the
// values the learner wrote. The order they come in does not matter here:
// what matters is which of them the value names.
func whiteboardValuesInText(text string) []whiteboardValueForm {
	matches := whiteboardValuePattern.FindAllStringSubmatch(text, -1)
	forms := make([]whiteboardValueForm, 0, len(matches))
	for _, match := range matches {
		if len(match) < 3 {
			continue
		}
		number := 0.0
		if _, err := fmt.Sscanf(match[1], "%g", &number); err != nil {
			continue
		}
		forms = append(forms, whiteboardValueForm{number: number, unit: match[2], hasNumber: true})
	}
	return forms
}

// whiteboardQuoteCarriesValue reports whether the quoted words carry the
// value.
//
// The value may be written in more than one way, so the comparison is by
// what the value is rather than by how it is written: "41 页" and "41 页/次"
// are the same number of pages, and "41 分钟" is not.
//
// A value that carries no number is a name, and a name is carried when it is
// written in the words. A name that is not in them is a name the learner did
// not say.
func whiteboardQuoteCarriesValue(quote, value string) bool {
	carries, _ := whiteboardQuoteCarriesValueReason(quote, value)
	return carries
}

// whiteboardQuoteCarriesValueReason is whiteboardQuoteCarriesValue with the
// reason the value is not carried.
//
// The two refusals are different in what they say: a value that is not in
// the quoted words at all is about words the learner did not use, and a
// value whose unit measures something else is about another quantity.
func whiteboardQuoteCarriesValueReason(quote, value string) (bool, string) {
	claimForm, ok := whiteboardValueForms(value)
	if !ok {
		return false, model.SessionWhiteboardAnchorValueNotInQuote
	}
	if !claimForm.hasNumber {
		// A name. The learner has to have written it.
		name := whiteboardPlainText(claimForm.plain)
		if name == "" {
			return false, model.SessionWhiteboardAnchorValueNotInQuote
		}
		if strings.Contains(quote, name) {
			return true, ""
		}
		return false, model.SessionWhiteboardAnchorValueNotInQuote
	}
	// A measurement. Every number in the quoted words is read, and the
	// value has to be one of them with a unit that measures the same thing.
	sameNumber := false
	for _, form := range whiteboardValuesInText(quote) {
		if !form.hasNumber {
			continue
		}
		if form.number != claimForm.number {
			continue
		}
		sameNumber = true
		if whiteboardUnitsCompatible(form.unit, claimForm.unit) {
			return true, ""
		}
	}
	if sameNumber {
		// The number is there and the unit is not the one that measures it.
		return false, model.SessionWhiteboardAnchorValueUnitMismatch
	}
	return false, model.SessionWhiteboardAnchorValueNotInQuote
}

// whiteboardBatchMessage returns the text of one of the learner's messages in
// this batch.
func whiteboardBatchMessage(batch WhiteboardBatch, id uint) (string, bool) {
	if id == 0 {
		return "", false
	}
	for index, candidate := range batch.MessageIDs {
		if candidate != id || index >= len(batch.Messages) {
			continue
		}
		return batch.Messages[index], true
	}
	return "", false
}

// whiteboardEntryNames are the names an entry can be pointed at by.
//
// They are the parts of its value that identify it. A value that carries a
// measurement is identified by that measurement, because "26 pages" and
// "26 minutes" are two different values and the measurement is what says
// which one this is. A value that names something is identified by its name.
//
// A name has to be worth pointing at: a single character is a fragment of a
// word rather than a name, and a part that carries a number is a
// measurement, which the rule above already has.
func whiteboardEntryNames(entry model.SessionWhiteboardEntry) []string {
	value := strings.TrimSpace(entry.Value)
	if value == "" {
		return nil
	}
	if number, unit, ok := parseWhiteboardValue(value); ok {
		return []string{whiteboardPlainText(strconv.FormatFloat(number, 'f', -1, 64) + unit)}
	}
	names := make([]string, 0, 4)
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case ',', '，', '/', '、', '；', ';', ' ', '\t':
			return true
		}
		return false
	}) {
		part = strings.TrimSpace(part)
		if len([]rune(part)) < 2 {
			continue
		}
		if strings.ContainsAny(part, "0123456789") {
			continue
		}
		names = append(names, whiteboardPlainText(part))
	}
	return names
}

// whiteboardEntryUnit is the unit an entry's value is measured in.
//
// An entry with no measurement has no unit: it names something rather than
// measuring it, and an entry that measures something is not the same entry
// as one that names something.
func whiteboardEntryUnit(entry model.SessionWhiteboardEntry) string {
	_, unit, ok := parseWhiteboardValue(entry.Value)
	if !ok {
		return ""
	}
	return unit
}

// whiteboardEntryDescription is what an entry says about itself: its text,
// its key and its value.
//
// It is what the frequency words are read out of when two entries are
// compared with each other.
func whiteboardEntryDescription(entry model.SessionWhiteboardEntry) string {
	return whiteboardPlainText(entry.Text + " " + entry.Key + " " + entry.Value)
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

// whiteboardScopeMarkers are the words that say how often a quantity holds.
//
// The list is short on purpose, and it is read in one place only: between
// two entries of the board, to tell whether they are about the same
// frequency. It is not a check on what the learner said, and the program
// does not decide what a sentence means with it.
//
// A marker that is not in it is not a scope, and a text that names no scope
// constrains nothing: the learner may have said a quantity once and for all,
// and that is a quantity too.
var whiteboardScopeMarkers = []string{
	"每次", "每轮", "每组", "每讲", "每课", "每章", "每节", "每份",
	"每题", "每场", "每个", "每遍", "每回", "每天", "每周", "每月",
	"每星期", "每学期", "每阶段",
}

// whiteboardScopesIn reads the scopes a text names.
//
// The text is read without its spaces, because "每次 23 页" and "每次23页"
// are one text.
func whiteboardScopesIn(text string) []string {
	plain := whiteboardPlainText(text)
	found := make([]string, 0, len(whiteboardScopeMarkers))
	for _, marker := range whiteboardScopeMarkers {
		if strings.Contains(plain, whiteboardPlainText(marker)) {
			found = append(found, marker)
		}
	}
	return found
}

// whiteboardScopesShare reports whether two sets of scopes have one in
// common.
//
// A set that names no scope shares nothing: a clause that says how often the
// quantity holds is not a clause that says nothing about it.
func whiteboardScopesShare(left, right []string) bool {
	for _, one := range left {
		for _, other := range right {
			if one == other {
				return true
			}
		}
	}
	return false
}

// whiteboardScopeMoves reports whether a new entry names a scope the old
// one does not.
//
// The pages a learner reads every session and the pages they read every day
// are two things, and a learner who turns one into the other has said so
// with the frequency itself. A new entry that names a scope the old one
// does not is about another item, not another wording of the same one.
func whiteboardScopeMoves(old model.SessionWhiteboardEntry, updated model.SessionWhiteboardEntry) bool {
	oldScopes := whiteboardScopesIn(whiteboardEntryDescription(old))
	newScopes := whiteboardScopesIn(whiteboardEntryDescription(updated))
	for _, scope := range newScopes {
		if !whiteboardScopesShare([]string{scope}, oldScopes) {
			return true
		}
	}
	return false
}

// whiteboardValueIsTheLearnersOwn reports whether the entry's value is in
// the learner's own words.
//
// It is a fact about the messages and not a reading of them: the value the
// entry wants to put on the board is written in one of the learner's own
// messages of this batch.
//
// It answers one question only - whose words an entry is, when the label the
// model wrote on it and the citations disagree. It never decides whether a
// value may be written: an entry of the learner's is written on what it
// cites, and the value it carries is not compared with the words.
func whiteboardValueIsTheLearnersOwn(ids []uint, entry model.SessionWhiteboardEntry,
	batch WhiteboardBatch) bool {
	value := strings.TrimSpace(entry.Value)
	if value == "" {
		return false
	}
	for _, id := range ids {
		role, known := batch.KnownMessages[id]
		if !known || role != WhiteboardMessageRoleUser {
			continue
		}
		text, ok := whiteboardBatchMessage(batch, id)
		if !ok {
			continue
		}
		if whiteboardQuoteCarriesValue(text, value) {
			return true
		}
	}
	return false
}

// whiteboardSameMeasurement reports whether two values are one measurement.
//
// The same number counted in the same unit: "26 页" and "26 页/次" are one
// measurement, and "26 页" and "26 分钟" are two.
//
// A number is not the name of a thing. The pages a learner planned to read
// and the pages they actually read are two things that may carry the same
// number, and the program cannot tell them apart by the number. So the
// number alone is not evidence that two entries are about the same thing.
func whiteboardSameMeasurement(left, right string) bool {
	leftForm, leftOk := whiteboardValueForms(left)
	rightForm, rightOk := whiteboardValueForms(right)
	if !leftOk || !rightOk {
		return false
	}
	if !leftForm.hasNumber || !rightForm.hasNumber {
		return false
	}
	return leftForm.number == rightForm.number &&
		whiteboardUnitsCompatible(leftForm.unit, rightForm.unit)
}
