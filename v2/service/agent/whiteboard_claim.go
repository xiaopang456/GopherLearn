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
// attribute the value belongs to, and reports what kind of statement it was.
// The program then checks every part of that statement against the messages
// of the batch: the message is one of the learner's own, the quote really is
// in it, the value is the entry's value, and the subject and the attribute
// are what the entry is about.
//
// What the checks cannot do is decide what the quote means. "改成 41 页" and
// "不要改成 41 页" are both a learner who wrote a number, and only the model
// can tell them apart. The stance it reports is the one part of this that is
// a judgement rather than a fact, so the program bounds how far that
// judgement reaches: a stance that says "negated", "hypothetical",
// "proposed" or "uncertain" never becomes a current fact, and a stance that
// says "decided" becomes one only when the quote, the value, the subject and
// the attribute all check out. A claim that does not check out is not
// evidence, and an entry without evidence keeps the label the model gave it.
//
// The claim is not stored. The board keeps what it has always kept: the
// entry, its value and the messages it came from. The claim is evidence for
// a decision that is made while the proposal is applied, and it is gone when
// that decision has been made.

import (
	"encoding/json"
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

// whiteboardClaimVerdict is what the program made of one claim.
type whiteboardClaimVerdict int

const (
	// whiteboardClaimAbsent says the entry carried no claim at all.
	whiteboardClaimAbsent whiteboardClaimVerdict = iota
	// whiteboardClaimFalse says the claim does not hold: the quote is not
	// in the message, the value is another one, or the subject and the
	// attribute are not what the entry is about.
	whiteboardClaimFalse
	// whiteboardClaimSettled says the claim holds and the learner settled
	// the value.
	whiteboardClaimSettled
	// whiteboardClaimUnsettled says the claim holds and the learner did
	// not settle the value: they proposed it, asked about it, ruled it out
	// or were not sure.
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
func whiteboardVerifyClaim(claim WhiteboardLearnerClaim, entry model.SessionWhiteboardEntry,
	batch WhiteboardBatch) whiteboardClaimVerdict {
	if !whiteboardClaimStanceKnown(claim.Stance) {
		return whiteboardClaimFalse
	}
	message, found := whiteboardBatchMessage(batch, claim.MessageID)
	if !found {
		// The claim rests on a message the learner did not write in this
		// batch. It may be the assistant's message, a message of another
		// batch, or a message that does not exist at all. None of them is
		// the learner's own words, and the assistant confirming something
		// is not the learner accepting it.
		return whiteboardClaimFalse
	}
	quote := whiteboardPlainText(claim.Quote)
	if quote == "" || !strings.Contains(whiteboardPlainText(message), quote) {
		// The quote is not the learner's own wording.
		return whiteboardClaimFalse
	}
	if !whiteboardClaimValueMatches(claim.Value, entry.Value) {
		// The value of the claim is not the value of the entry. A claim
		// about another value of the same quantity proves nothing about
		// this one.
		return whiteboardClaimFalse
	}
	subject := whiteboardPlainText(claim.Subject)
	if subject == "" || !strings.Contains(quote, subject) ||
		!strings.Contains(whiteboardEntryDescription(entry), subject) {
		// The subject is not what the learner spoke about, or the entry is
		// not about that subject. Two subjects that share a number are two
		// subjects.
		return whiteboardClaimFalse
	}
	attribute := whiteboardPlainText(claim.Attribute)
	if attribute == "" || !strings.Contains(quote, attribute) ||
		!strings.Contains(whiteboardEntryDescription(entry), attribute) {
		// The attribute is not what the learner spoke about, or the entry
		// is not about that attribute. One subject carries many
		// attributes, and they are not each other's values.
		return whiteboardClaimFalse
	}
	if !strings.EqualFold(strings.TrimSpace(claim.Stance), whiteboardClaimStanceDecided) {
		return whiteboardClaimUnsettled
	}
	return whiteboardClaimSettled
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
