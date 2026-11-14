package agent

// The learner's own words behind a whiteboard entry.
//
// A board entry says what is true now, and only the learner can say what is
// true about their own work. The program cannot read that out of a sentence
// with a list of words: a negation nobody thought of is read as a plain
// statement, and a plain statement that was meant as a negation is written on
// the board as the learner's current value. A closed list of qualifiers only
// decides which of the two mistakes is made.
//
// So the evidence is stated rather than read. The model says which message it
// read, quotes that message word for word, names the subject and the
// attribute the value belongs to, and reports what kind of statement it was
// and whether that statement updates the item. The program then checks every
// part of that statement against the messages of the batch: the message is
// one of the learner's own, the quote really is in it, the value really is
// in the quoted words, the value is the entry's value, and the subject and
// the attribute are what the entry is about.
//
// The attribute is the one part that is not always a sentence of the
// learner. When the value is a measurement, the attribute is a name for the
// aspect of the subject the value belongs to, and the three refusals of the
// run of 2026-11-14 were claims that were dropped because a summary was not
// in the words. A measurement is therefore checked by what it has - the
// subject, the unit and the scope - and the words of the learner are read
// for those.
//
// The checks are facts about the messages, not about the meaning of a
// sentence. What a quote means is still the model's judgement, so the model
// is asked two questions about it rather than one: what kind of statement it
// is, and whether it updates the item. A model that misreads a sentence
// usually misreads one of the two, and the program refuses the entry when
// the two answers do not agree. What the program cannot do is prove that a
// model which answers both questions the same wrong way is wrong, and the
// report of this change says so.
//
// The claim is not stored. The board keeps what it has always kept: the
// entry, its value and the messages it came from. The claim is evidence for
// a decision that is made while the proposal is applied, and it is gone when
// that decision has been made.

import (
	"encoding/json"
	"fmt"
	"strings"

	"GopherAI/model"
)

// WhiteboardLearnerClaim is what the learner said behind one entry.
//
// It is the evidence a correction rests on, and every field of it is checked
// before it is used: a claim is only as good as the part of it that holds up.
type WhiteboardLearnerClaim struct {
	// Key names the entry the claim belongs to.
	Key string `json:"key"`
	// MessageID is the learner's message the quote was taken from.
	MessageID uint `json:"message_id"`
	// Quote is the part of that message the claim rests on, word for word.
	Quote string `json:"quote"`
	// Subject is the thing the learner spoke about.
	Subject string `json:"subject"`
	// Attribute is the part of the subject the value belongs to.
	Attribute string `json:"attribute"`
	// Value is the value as the learner wrote it.
	Value string `json:"value"`
	// Stance says what kind of statement it was.
	Stance string `json:"stance"`
	// Update says whether that statement updates the item to this value.
	//
	// It is a second question about the same words, asked apart from the
	// stance, and the program needs both answers before it lets a claim
	// change what the board holds. A model that reads "do not change it to
	// 41 pages" as a plain statement usually still answers "no" here.
	Update string `json:"update"`
}

// UnmarshalJSON reads a claim and ignores the fields the program does not know.
//
// The document around the claim is read strictly - an unknown field there
// means the model's answer changed shape underneath the code that reads it.
// The claim is the exception, because the claim is evidence and evidence is
// optional: a claim that arrives with a field the program does not read is
// still a claim, and the fields the program does read are the ones it
// checks. A claim whose own fields are missing simply supports nothing.
func (c *WhiteboardLearnerClaim) UnmarshalJSON(data []byte) error {
	type claimFields struct {
		Key       string `json:"key"`
		MessageID uint   `json:"message_id"`
		Quote     string `json:"quote"`
		Subject   string `json:"subject"`
		Attribute string `json:"attribute"`
		Value     string `json:"value"`
		Stance    string `json:"stance"`
		Update    string `json:"update"`
	}
	var fields claimFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*c = WhiteboardLearnerClaim{
		Key:       fields.Key,
		MessageID: fields.MessageID,
		Quote:     fields.Quote,
		Subject:   fields.Subject,
		Attribute: fields.Attribute,
		Value:     fields.Value,
		Stance:    fields.Stance,
		Update:    fields.Update,
	}
	return nil
}

// The stances a claim may report.
//
// They are the model's judgement of what the learner did with the value, and
// the program reads them as they are reported: only "decided" says the
// learner settled it.
const (
	// whiteboardClaimStanceDecided says the learner settled the value.
	whiteboardClaimStanceDecided = "decided"
	// whiteboardClaimStanceProposed says the learner proposed the value.
	whiteboardClaimStanceProposed = "proposed"
	// whiteboardClaimStanceHypothetical says the learner asked what would
	// happen if the value were so.
	whiteboardClaimStanceHypothetical = "hypothetical"
	// whiteboardClaimStanceNegated says the learner ruled the value out.
	whiteboardClaimStanceNegated = "negated"
	// whiteboardClaimStanceUncertain says the learner was not sure.
	whiteboardClaimStanceUncertain = "uncertain"
)

// whiteboardClaimStanceKnown reports whether a stance is one the program
// knows.
//
// An unknown stance is not a stance the program may read as a decision: the
// model wrote something the program does not understand, and what it does
// not understand it does not act on.
func whiteboardClaimStanceKnown(stance string) bool {
	switch strings.ToLower(strings.TrimSpace(stance)) {
	case whiteboardClaimStanceDecided, whiteboardClaimStanceProposed,
		whiteboardClaimStanceHypothetical, whiteboardClaimStanceNegated,
		whiteboardClaimStanceUncertain:
		return true
	}
	return false
}

// The answers a claim may give to the second question: does this statement
// update the item to this value.
//
// This is asked apart from the stance on purpose. The stance says what kind
// of sentence the words are; the update says what they do to the item. A
// model that reads a negation as a plain statement usually still answers
// "no" here, and a model that answers "yes" here while calling the words a
// hypothetical has contradicted itself. Either way the program refuses the
// entry, so a single misreading does not reach the board.
const (
	// whiteboardClaimUpdateYes says the words update the item to this value.
	whiteboardClaimUpdateYes = "yes"
	// whiteboardClaimUpdateNo says the words do not update the item.
	whiteboardClaimUpdateNo = "no"
	// whiteboardClaimUpdateUnsure says the model cannot tell.
	whiteboardClaimUpdateUnsure = "unsure"
)

// whiteboardClaimUpdateKnown reports whether an update answer is one the
// program knows.
func whiteboardClaimUpdateKnown(update string) bool {
	switch strings.ToLower(strings.TrimSpace(update)) {
	case whiteboardClaimUpdateYes, whiteboardClaimUpdateNo, whiteboardClaimUpdateUnsure:
		return true
	}
	return false
}

// whiteboardClaimVerdict is what the program made of one claim.
//
// The three states are kept apart because they mean three different things
// to the engine: a claim that was never offered says nothing about the
// entry, a claim that holds says the learner's own words are behind it, and
// a claim that does not hold says the model asserted something the program
// could not confirm. The last one is a refusal, and a refusal is not the
// same as silence.
type whiteboardClaimVerdict int

const (
	// whiteboardClaimAbsent says the entry carried no claim at all.
	//
	// Nothing was asserted, so nothing is checked. The entry is judged the
	// way it was judged before claims existed.
	whiteboardClaimAbsent whiteboardClaimVerdict = iota
	// whiteboardClaimRejected says the claim was offered and does not hold:
	// the quote is not in the message, the value is not in the quoted
	// words, the value is another one, or the subject and the attribute are
	// not what the entry is about.
	//
	// A rejected claim is not evidence and it is not silence either. The
	// entry it was written for is refused: it does not become the
	// learner's own, it does not take over another entry's key, and it does
	// not replace a value the board already holds.
	whiteboardClaimRejected
	// whiteboardClaimSettled says the claim holds and the learner settled
	// the value.
	whiteboardClaimSettled
	// whiteboardClaimUnsettled says the claim holds but the learner did not
	// settle the value: they proposed it, asked about it, ruled it out, were
	// not sure, or the model's own two answers disagree about it.
	whiteboardClaimUnsettled
)

// whiteboardVerifyClaim reads one claim against the batch and the entry it
// belongs to.
//
// The order of the checks is the order of the ways a claim can be wrong, and
// each one is a fact about the messages rather than about the meaning of a
// sentence: the message has to be the learner's, the quote has to be in it
// word for word, the value has to be the entry's value, and the subject and
// the attribute have to be in the learner's words and in what the entry
// says about itself.
// whiteboardVerifyClaim reads one claim against the batch and the entry it
// belongs to.
//
// The order of the checks is the order of the ways a claim can be wrong, and
// each one is a fact about the messages rather than about the meaning of a
// sentence: the message has to be the learner's, the quote has to be in it
// word for word, the value has to be the entry's value, and the subject and
// the attribute have to be in the learner's words and in what the entry says
// about itself.
//
// The second value it returns is why the claim does not hold, and it is one of
// a fixed set of reasons. The verdict alone says whether the entry may go on
// the board; the reason says which check refused it, which is what makes a
// refused entry locatable instead of a count.
func whiteboardVerifyClaim(claim WhiteboardLearnerClaim, entry model.SessionWhiteboardEntry,
	batch WhiteboardBatch) (whiteboardClaimVerdict, string) {
	if strings.TrimSpace(claim.Stance) == "" || !whiteboardClaimStanceKnown(claim.Stance) {
		// The model did not say what kind of statement the learner's words
		// were, or it said it in a way the program does not read. Either
		// way there is no decision to act on.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorStanceUnknown
	}
	if strings.TrimSpace(claim.Update) == "" || !whiteboardClaimUpdateKnown(claim.Update) {
		// The model did not answer the second question, so the program has
		// one answer where it needs two. An unanswered question is not an
		// answer that says yes.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorUpdateUnknown
	}
	message, found := whiteboardBatchMessage(batch, claim.MessageID)
	if !found {
		// The claim rests on a message the learner did not write in this
		// batch. It may be the assistant's message, a message of another
		// batch, or a message that does not exist at all. None of them is
		// the learner's own words, and the assistant confirming something
		// is not the learner accepting it.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorMessageNotLearner
	}
	quote := whiteboardPlainText(claim.Quote)
	if quote == "" {
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorQuoteMissing
	}
	if !strings.Contains(whiteboardPlainText(message), quote) {
		// The quote is not the learner's own wording.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorQuoteNotInMessage
	}
	carries, valueReason := whiteboardQuoteCarriesValueReason(quote, claim.Value)
	if !carries {
		// The value the claim names is not in the words the claim quotes.
		//
		// This is the check that keeps the claim from being two model
		// fields agreeing with each other and nothing else. The learner
		// never wrote the number, so there is nothing behind the entry
		// whatever the entry and the claim say about each other.
		return whiteboardClaimRejected, valueReason
	}
	if !whiteboardClaimValueMatches(claim.Value, entry.Value) {
		// The value of the claim is not the value of the entry. A claim
		// about another value of the same quantity proves nothing about
		// this one.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorValueMismatch
	}
	subject := whiteboardPlainText(claim.Subject)
	if subject == "" || !strings.Contains(quote, subject) {
		// The subject is not what the learner spoke about.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorSubjectNotInQuote
	}
	if !strings.Contains(whiteboardEntryDescription(entry), subject) {
		// The entry is not about that subject. Two subjects that share a
		// number are two subjects.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorSubjectNotInEntry
	}
	attribute := whiteboardPlainText(claim.Attribute)
	if attribute == "" {
		// The claim does not say what the value is about.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorAttributeNotInQuote
	}
	if whiteboardMeasurementClaim(claim) {
		// A measurement is checked by the three things it has: the thing it
		// is about (the subject, checked above), the unit it is counted in,
		// and the scope it holds in. Its attribute is a name for that aspect
		// and not a sentence the learner said, so it is not looked for in
		// the quote.
		//
		// The model is asked what kind of statement the words were before
		// the attribute is read. A value the learner only asked about is not
		// a current fact whatever the attribute is called, and that answer
		// does not depend on the attribute at all.
		if verdict, reason := whiteboardClaimSettledness(claim); verdict != whiteboardClaimSettled {
			return verdict, reason
		}
		if holds, reason := whiteboardQuantityAttributeHolds(claim, entry, quote); !holds {
			return whiteboardClaimRejected, reason
		}
	} else {
		// A name. The attribute is a fact about the learner's words, and it
		// has to be in them.
		if !strings.Contains(quote, attribute) {
			// The attribute is not what the learner spoke about.
			return whiteboardClaimRejected, model.SessionWhiteboardAnchorAttributeNotInQuote
		}
		if !strings.Contains(whiteboardEntryDescription(entry), attribute) {
			// One subject carries many attributes, and they are not each
			// other's values.
			return whiteboardClaimRejected, model.SessionWhiteboardAnchorAttributeNotInEntry
		}
		if verdict, reason := whiteboardClaimSettledness(claim); verdict != whiteboardClaimSettled {
			// The model called the words a decision and then said they do not
			// update the item. The two answers disagree, so the program acts
			// on neither: what it cannot make out, it does not write.
			return verdict, reason
		}
	}
	if !whiteboardScopeHolds(message, claim.Value) {
		// The model says the learner settled this value, but the learner's
		// own words left a different value of the same quantity standing
		// after it.
		//
		// This is the check that does not depend on the model having read
		// the words correctly. "Do not change it to 41 pages, keep 26" says
		// 41 and leaves 26 standing, and no reading of the sentence makes
		// 41 the current value.
		return whiteboardClaimRejected, model.SessionWhiteboardAnchorScopeConflict
	}
	return whiteboardClaimSettled, ""
}

// whiteboardValuesInText reads every measurement the text carries.
//
// A text may carry more than one number, and each of them is one of the
// values the learner wrote. The order they come in does not matter here:
// what matters is which of them the claim names.
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

// whiteboardScopeHolds reports whether the claimed value is the one the
// learner's own message leaves standing.
//
// A message that carries two values of the same quantity says the learner
// wrote both, and the one that comes last is the one they left standing.
// This is a fact about the order of the words rather than about their
// meaning, and it is what tells "do not change it to 41 pages, keep 26" from
// "change it to 41 pages": in the first the learner ends on 26.
//
// A value that is a name has no order, and a message that carries no other
// value of the same quantity says nothing against it. Both are left to the
// model's own reading.
func whiteboardScopeHolds(message, value string) bool {
	form, ok := whiteboardValueForms(value)
	if !ok || !form.hasNumber {
		// A name, or a bare number with no unit to compare: there is no
		// order to read.
		return true
	}
	last, found := whiteboardLastMeasurement(message, form.unit)
	if !found {
		return true
	}
	return last.number == form.number
}

// whiteboardLastMeasurement returns the last measurement of the unit in the
// text.
//
// The unit is compared the way the board compares units - one is taken to be
// the other when one of them starts the other - so "5 天" and "5 天交" are
// the same dimension.
func whiteboardLastMeasurement(text, unit string) (whiteboardValueForm, bool) {
	last := whiteboardValueForm{}
	found := false
	for _, form := range whiteboardValuesInText(text) {
		if !whiteboardUnitsCompatible(form.unit, unit) {
			continue
		}
		last = form
		found = true
	}
	return last, found
}

// whiteboardQuoteCarriesValue reports whether the quoted words carry the
// value the claim names.
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
// The two refusals are different in what they say about the model: a value
// that is not in the quoted words at all is a claim about words the learner
// did not use, and a value whose unit measures something else is a claim
// about another quantity. A reader that has to act on the refusal - and a
// generator that has to learn from it - needs to tell them apart.
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

// whiteboardEntryDescription is what an entry says about itself: its text,
// its key and its value.
//
// A claim's subject and attribute have to be found in it, because an entry
// that does not name the subject it is about cannot be checked against a
// claim about that subject.
func whiteboardEntryDescription(entry model.SessionWhiteboardEntry) string {
	return whiteboardPlainText(entry.Text + " " + entry.Key + " " + entry.Value)
}

// whiteboardClaimValueMatches reports whether the value of a claim and the
// value of an entry are the same value.
//
// A value that carries a measurement is the same value when the numbers are
// equal and the units measure the same thing: "41 页" and "41 页/次" are one
// value, and "41 页" and "41 分钟" are two. A value that names something is
// the same value when one of them says the other.
func whiteboardClaimValueMatches(claimValue, entryValue string) bool {
	claimForm, claimOK := whiteboardValueForms(claimValue)
	entryForm, entryOK := whiteboardValueForms(entryValue)
	if !claimOK || !entryOK {
		return false
	}
	if claimForm.hasNumber || entryForm.hasNumber {
		if !claimForm.hasNumber || !entryForm.hasNumber {
			return false
		}
		return claimForm.number == entryForm.number &&
			whiteboardUnitsCompatible(claimForm.unit, entryForm.unit)
	}
	if claimForm.plain == entryForm.plain {
		return true
	}
	return strings.Contains(claimForm.plain, entryForm.plain) ||
		strings.Contains(entryForm.plain, claimForm.plain)
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

// whiteboardClaimFor returns the claim that belongs to an entry.
//
// The claims are matched by the entry's key, because the key is the model's
// name for the entry and the claim is written for that name. A key that more
// than one entry of the proposal carries is ambiguous - the program cannot
// tell which of the two the claim was written for - and so is a key that
// more than one claim names. Neither is used: a claim that cannot be placed
// is not evidence.
func whiteboardClaimFor(claims []WhiteboardLearnerClaim, key string,
	keyCounts map[string]int) (WhiteboardLearnerClaim, bool) {
	key = strings.TrimSpace(key)
	if key == "" || keyCounts[key] > 1 {
		return WhiteboardLearnerClaim{}, false
	}
	found := WhiteboardLearnerClaim{}
	matches := 0
	for _, claim := range claims {
		if strings.TrimSpace(claim.Key) != key {
			continue
		}
		matches++
		found = claim
	}
	if matches != 1 {
		return WhiteboardLearnerClaim{}, false
	}
	return found, true
}

// whiteboardMoveClaim moves the claim of one key to another.
//
// The key of an entry is the board's to keep, and the claim was written for
// the entry rather than for the key, so it goes with it.
func whiteboardMoveClaim(claims []WhiteboardLearnerClaim, from, to string) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return
	}
	for index := range claims {
		if strings.TrimSpace(claims[index].Key) != from {
			continue
		}
		claims[index].Key = to
	}
}

// whiteboardProposalKeyCounts counts how many entries of the proposal carry
// each key.
//
// The count is what makes a claim ambiguous: a key that two entries carry is
// a key the claim cannot be placed under.
func whiteboardProposalKeyCounts(proposal WhiteboardProposal) map[string]int {
	counts := make(map[string]int)
	for _, group := range [][]model.SessionWhiteboardEntry{
		proposal.Constraints, proposal.Plan, proposal.OpenItems,
	} {
		for _, entry := range group {
			key := strings.TrimSpace(entry.Key)
			if key == "" {
				continue
			}
			counts[key]++
		}
	}
	return counts
}
