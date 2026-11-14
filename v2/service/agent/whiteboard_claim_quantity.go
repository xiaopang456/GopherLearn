package agent

// The attribute of a claim whose value is a measurement.
//
// A claim says which message it read, quotes it word for word, names the
// subject and the attribute the value belongs to, and gives the value. Three
// of those are facts about the learner's words and one of them is a name.
//
// The subject is a thing the learner spoke about and the value is a thing
// the learner wrote, so both are looked for in the words. The attribute is
// usually a third fact - "按章节记进度" is what the learner said - and it is
// looked for in the words as well.
//
// A measurement is different. "23 页" has three aspects and no more: the
// thing it is about, the unit it is counted in, and the scope it holds in.
// A model that names that aspect writes it as a summary - "每次阅读页数" -
// and a summary is not a sentence the learner said. Asking for the summary
// to be in the quote throws away an update the learner stated plainly, and
// the three refusals of the run of 2026-11-14 were exactly that.
//
// So a measurement is checked by the three things it does have: the subject
// (against the quote and against the entry, as before), the unit it is
// counted in, and the scope it holds in. A name keeps the old check.
//
// The scope is read out of the words, and it is read out of the clause the
// value stands in. "还是每次看 12 页吧，别改成 23 页" is two clauses: the
// 每次 belongs to the twelve pages, and the twenty-three pages stand in a
// clause that has no scope of its own. A value the learner ruled out is
// therefore not a value of a clause that says how often, which is what keeps
// a ruled-out value from being settled by a summary.
//
// Nothing here reads what a sentence means. The scope is a word in the
// sentence, the unit is a word next to the number, and the subject is the
// thing the entry is about. What the words mean is still the model's
// judgement, and it is still asked for separately.

import (
	"strings"

	"GopherAI/model"
)

// whiteboardScopeMarkers are the words that say how often a quantity holds.
//
// The list is short on purpose. A marker that is not in it is not a scope,
// and a text that names no scope constrains nothing: the learner may have
// said a quantity once and for all, and that is a quantity too.
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

// whiteboardScopesMeet reports whether two sets of scopes agree.
//
// A set that names no scope agrees with every other set: it says nothing
// about the scope, so it neither agrees nor disagrees. The attribute of a
// claim and the entry it is written for are compared this way, because an
// attribute that names no scope and an entry that names one are not in
// conflict - one of them simply does not say.
func whiteboardScopesMeet(left, right []string) bool {
	if len(left) == 0 || len(right) == 0 {
		return true
	}
	return whiteboardScopesShare(left, right)
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

// whiteboardClauses splits a text into the clauses it is made of.
//
// A clause is what a sentence says between two marks of punctuation. It is
// where a scope belongs: "别改成 23 页" is a clause of its own, and the
// 每次 of the clause before it is not its own.
//
// A full stop between two digits is not a mark of punctuation, so "41.5 页"
// stays one clause.
func whiteboardClauses(text string) []string {
	parts := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '，', '。', '！', '？', '；', '、', '：',
			',', '!', '?', ';', ':', '\n', '\r', '\t':
			return true
		}
		return false
	})
	clauses := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			clauses = append(clauses, part)
		}
	}
	return clauses
}

// whiteboardClausesCarryingValue returns the clauses that carry the value.
//
// A clause carries a measurement when it counts that number in a unit that
// measures the same thing: "9 道题" and "9 道" are one quantity, and "23 页"
// and "23 分钟" are two.
func whiteboardClausesCarryingValue(text string, form whiteboardValueForm) []string {
	clauses := make([]string, 0, 2)
	for _, clause := range whiteboardClauses(text) {
		for _, candidate := range whiteboardValuesInText(clause) {
			if !candidate.hasNumber || candidate.number != form.number {
				continue
			}
			if !whiteboardUnitsCompatible(candidate.unit, form.unit) {
				continue
			}
			clauses = append(clauses, clause)
			break
		}
	}
	return clauses
}

// whiteboardQuantityAttributeHolds reports whether the attribute of a claim
// about a measurement is what the learner's words support.
//
// The subject has been checked already: it is in the quote, and the entry is
// about it. What is left is the unit and the scope.
//
// The unit: the clause that carries the value has to count it in the unit
// the value names. A claim about twenty-three minutes is not supported by a
// sentence about twenty-three pages, however close the numbers are.
//
// The scope: the scope the attribute names has to be the scope of the clause
// the value stands in, and the scope the entry names has to meet it. Two
// entries about the same subject in different scopes - a page count per
// session and a page count per week - are two entries, and a claim for one
// of them proves nothing about the other.
func whiteboardQuantityAttributeHolds(claim WhiteboardLearnerClaim,
	entry model.SessionWhiteboardEntry, quote string) (bool, string) {
	form, ok := whiteboardValueForms(claim.Value)
	if !ok || !form.hasNumber {
		return false, model.SessionWhiteboardAnchorAttributeNotInQuote
	}
	clauses := whiteboardClausesCarryingValue(quote, form)
	if len(clauses) == 0 {
		// The value is not in the quote at all. The check that says so has
		// run before this one, and this is the same fact read a second way.
		return false, model.SessionWhiteboardAnchorAttributeNotInQuote
	}
	attributeScopes := whiteboardScopesIn(claim.Attribute)
	entryScopes := whiteboardScopesIn(whiteboardEntryDescription(entry))
	if len(attributeScopes) > 0 && len(entryScopes) > 0 &&
		!whiteboardScopesMeet(attributeScopes, entryScopes) {
		// The attribute is about a scope the entry is not about.
		return false, model.SessionWhiteboardAnchorAttributeNotInEntry
	}
	expected := attributeScopes
	if len(expected) == 0 {
		// The attribute names no scope, so the entry names it: the entry is
		// what says whether this is a count per session or a count per week.
		expected = entryScopes
	}
	if len(expected) > 0 {
		carries := false
		for _, clause := range clauses {
			if whiteboardScopesShare(whiteboardScopesIn(clause), expected) {
				carries = true
				break
			}
		}
		if !carries {
			// The learner did not say how often, in the clause where the
			// value stands.
			return false, model.SessionWhiteboardAnchorAttributeNotInQuote
		}
	}
	return true, ""
}

// whiteboardMeasurementClaim reports whether the value of a claim is a
// measurement.
//
// A name is a thing the learner wrote and a measurement is a number counted
// in a unit, and the two are checked in two different ways.
func whiteboardMeasurementClaim(claim WhiteboardLearnerClaim) bool {
	form, ok := whiteboardValueForms(claim.Value)
	return ok && form.hasNumber
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

// whiteboardClaimSettledness asks the two questions about the words that a
// value has to pass before it is a current fact.
//
// The stance says what kind of statement the words were, and the update says
// what they do to the item. A model that reads a negation as a plain
// statement usually still answers "no" to the second question, and a model
// that answers "yes" to it while calling the words a hypothetical has
// contradicted itself. Either way the value does not go on the board.
func whiteboardClaimSettledness(claim WhiteboardLearnerClaim) (whiteboardClaimVerdict, string) {
	if !strings.EqualFold(strings.TrimSpace(claim.Stance), whiteboardClaimStanceDecided) {
		return whiteboardClaimUnsettled, model.SessionWhiteboardAnchorUnsettled
	}
	if !strings.EqualFold(strings.TrimSpace(claim.Update), whiteboardClaimUpdateYes) {
		return whiteboardClaimUnsettled, model.SessionWhiteboardAnchorUnsettled
	}
	return whiteboardClaimSettled, ""
}
