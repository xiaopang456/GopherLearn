package agent

// The unconfirmed learner statement.
//
// A refused entry takes two things with it: the value it wanted to put on the
// board, and the learner's own words behind it. The value is refused on
// purpose - the program could not confirm it, so it is not the current fact.
// The words are a different matter: they are the learner's own words, they
// are still true, and before this they went with the entry. A board that said
// the old value and no sign that the learner had said something else is a
// board that lost a turn of the conversation.
//
// So the two are separated. The value stays refused: it never becomes the
// current state, and the board never presents it as one. The words are kept as
// an unconfirmed statement: the message they came from can be read back, and
// the reader can see what the learner actually said.
//
// Three rules keep the statement from becoming a second kind of fact.
//
// The first is that it does not say what it is about. A refused claim is a
// claim the program could not confirm, and everything in it is therefore
// unconfirmed - including the name of the item it spoke about. A statement
// that hung itself on an entry because the rejected claim named the same
// subject would be hanging itself on a field that was refused for a reason.
// So the statement says where the words are (which message) and what was
// refused (the reason), and it says nothing about which entry they were
// about.
//
// The second is that the words are whole or not shown. A message that was cut
// in half would hide the end of the sentence, and the end of the sentence is
// where a condition or a negation lives: "41 pages, unless it gets busy"
// reads as a decision when its last three words are missing. So a message
// that is too long to be carried whole is carried as its id alone, and the
// reader is told to read it back.
//
// The third is that a statement is cleared only when something verifiable says
// it has been dealt with - the board now holds the same value under the same
// key. Another entry citing the same message is not that: it says the message
// was used for something, not that the words behind it were confirmed. A
// statement that is not cleared that way stays until it is crowded out, and
// being crowded out is reported as what it is.

import (
	"strings"

	"GopherAI/common/observability"
	"GopherAI/model"
)

// whiteboardReportClaimRefusals reports why the batch refused its candidates.
//
// The counters say how many claims did not hold. This says which check they
// did not hold, so a generator that keeps failing the same one is visible
// without reading the model's own answer.
func whiteboardReportClaimRefusals(diagnostics WhiteboardDiagnostics) {
	reasons := make(map[string]int, len(diagnostics.Entries))
	for _, entry := range diagnostics.Entries {
		if entry.ClaimReason == "" {
			continue
		}
		reasons[entry.ClaimReason]++
	}
	if len(reasons) > 0 {
		observability.Memory.WhiteboardClaimRejections(reasons)
	}
	if diagnostics.UnconfirmedStatements > 0 {
		observability.Memory.WhiteboardUnconfirmedStatements(diagnostics.UnconfirmedStatements)
	}
	if diagnostics.ResolvedUnconfirmedStatements > 0 {
		observability.Memory.WhiteboardUnconfirmedStatementsResolved(diagnostics.ResolvedUnconfirmedStatements)
	}
	if diagnostics.ExpiredUnconfirmedStatements > 0 {
		observability.Memory.WhiteboardUnconfirmedStatementsExpired(diagnostics.ExpiredUnconfirmedStatements)
	}

}

// The labels a verdict is written under in a diagnostic.
//
// They are a fixed set, so a diagnostic never carries the learner's own words
// and never becomes free text in a metric.
//
// Every verdict has its own label. A verdict that is not named here used to
// fall into "ambiguous", which put two different things under one name: a
// claim that holds and a claim that was never made. A reader of the evidence
// could not tell "the model said it and the words hold" from "the model said
// nothing about it", and a claim that holds is not the same thing as an entry
// on the board.
const (
	whiteboardClaimVerdictLabelRejected  = "rejected"
	whiteboardClaimVerdictLabelUnsettled = "unsettled"
	whiteboardClaimVerdictLabelAmbiguous = "ambiguous"
	whiteboardClaimVerdictLabelSettled   = "settled"
	whiteboardClaimVerdictLabelAbsent    = "absent"
)

// whiteboardClaimVerdictLabel is the label a verdict is written under.
//
// Every verdict is named. The default is the label of the verdict that says
// nothing was asserted, because that is the one a caller that does not know
// the verdict has to assume: a claim that was never checked is not evidence.
func whiteboardClaimVerdictLabel(verdict whiteboardClaimVerdict) string {
	switch verdict {
	case whiteboardClaimRejected:
		return whiteboardClaimVerdictLabelRejected
	case whiteboardClaimUnsettled:
		return whiteboardClaimVerdictLabelUnsettled
	case whiteboardClaimSettled:
		return whiteboardClaimVerdictLabelSettled
	case whiteboardClaimAbsent:
		return whiteboardClaimVerdictLabelAbsent
	}
	return whiteboardClaimVerdictLabelAbsent
}

// whiteboardClaimsNameKey reports whether any claim names the key.
//
// A claim that cannot be placed under its entry is not evidence, and the
// entry is then judged without it. That is a refusal of its own, so it is
// recorded as one rather than left to look like an entry nobody wrote.
func whiteboardClaimsNameKey(claims []WhiteboardLearnerClaim, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, claim := range claims {
		if strings.TrimSpace(claim.Key) == key {
			return true
		}
	}
	return false
}

// whiteboardAppendAnchor adds the unconfirmed statement of a refused entry.
//
// The statement is only built when the learner's own words really carry the
// value the entry wanted to put up: a statement that points at a message that
// does not mention the value would send a reader looking for words that are
// not there.
//
// It reports the message the statement ended up under and the reason it
// carries, because a reader of a run has to be able to tell an entry whose
// words stayed findable from one whose words were lost. Both are zero and
// empty when no statement was built.
func whiteboardAppendAnchor(anchors []model.SessionWhiteboardAnchor, entry model.SessionWhiteboardEntry,
	claim WhiteboardLearnerClaim, hasClaim bool, batch WhiteboardBatch,
	reason string) ([]model.SessionWhiteboardAnchor, uint, string) {
	anchor, ok := whiteboardAnchorFor(entry, claim, hasClaim, batch, reason)
	if !ok {
		return anchors, 0, ""
	}
	for _, existing := range anchors {
		if existing.MessageID == anchor.MessageID && existing.Value == anchor.Value {
			// The statement is already there. The entry's words are still
			// findable, under the message they were already found under.
			return anchors, anchor.MessageID, anchor.Reason
		}
	}
	return append(anchors, anchor), anchor.MessageID, anchor.Reason
}

// whiteboardAnchorFor builds the unconfirmed statement of a refused entry.
//
// The words it points at are the learner's own, in this batch: the message the
// claim rests on, and the messages the entry cites. A message is only used
// when it really carries the value the entry wanted to put on the board, so a
// statement is never built out of a message that does not mention it.
//
// The message is kept whole or not at all. A message that is too long to be
// carried whole becomes a pointer: the reader is told to read it back rather
// than shown half of it.
//
// The reason says why the value was not confirmed. It is one of a fixed set,
// so it never carries the learner's own words.
func whiteboardAnchorFor(entry model.SessionWhiteboardEntry, claim WhiteboardLearnerClaim,
	hasClaim bool, batch WhiteboardBatch, reason string) (model.SessionWhiteboardAnchor, bool) {
	if reason == "" {
		return model.SessionWhiteboardAnchor{}, false
	}
	value := strings.TrimSpace(entry.Value)
	if value == "" {
		// An entry without a value is not a candidate value, so there is
		// nothing a reader could be looking for.
		return model.SessionWhiteboardAnchor{}, false
	}
	for _, id := range whiteboardAnchorCandidates(entry, claim, hasClaim, batch) {
		text, ok := whiteboardBatchMessage(batch, id)
		if !ok {
			continue
		}
		if !whiteboardQuoteCarriesValue(text, value) {
			continue
		}
		anchor := model.SessionWhiteboardAnchor{
			Key:                strings.TrimSpace(entry.Key),
			Value:              value,
			MessageID:          id,
			Reason:             reason,
			UpdatedAtMessageID: batch.LastMessageID,
		}
		if len([]rune(text)) <= model.SessionWhiteboardAnchorMessageMaxRunes {
			anchor.Text = strings.TrimSpace(text)
			anchor.Complete = true
		}
		return anchor, true
	}
	return model.SessionWhiteboardAnchor{}, false
}

// whiteboardAnchorCandidates are the messages an unconfirmed statement may be
// built from.
//
// The claim's own message comes first: it is the message the model said the
// words came from. The messages the entry cites come after, because an entry
// that cites the learner's message is a claim about that message whether or
// not the claim was written well.
//
// Every candidate is a learner message of this batch. An assistant message is
// never one: the assistant confirming something is not the learner saying it.
func whiteboardAnchorCandidates(entry model.SessionWhiteboardEntry, claim WhiteboardLearnerClaim,
	hasClaim bool, batch WhiteboardBatch) []uint {
	candidates := make([]uint, 0, len(entry.SourceMessageIDs)+1)
	seen := make(map[uint]struct{}, len(entry.SourceMessageIDs)+1)
	add := func(id uint) {
		if id == 0 {
			return
		}
		if _, done := seen[id]; done {
			return
		}
		if _, ok := whiteboardBatchMessage(batch, id); !ok {
			return
		}
		seen[id] = struct{}{}
		candidates = append(candidates, id)
	}
	if hasClaim {
		add(claim.MessageID)
	}
	for _, id := range entry.SourceMessageIDs {
		add(id)
	}
	return candidates
}

// whiteboardAnchorNavigation is the statement without its words.
//
// It is what a statement becomes when the board has no room for the words:
// the message is still there and still readable, and a reader is told to read
// it rather than shown a part of it.
func whiteboardAnchorNavigation(anchor model.SessionWhiteboardAnchor) model.SessionWhiteboardAnchor {
	anchor.Text = ""
	anchor.Complete = false
	return anchor
}

// whiteboardMergeAnchors carries the unconfirmed statements forward and adds
// the new ones.
//
// A statement is cleared only when the board now holds what it pointed at: an
// entry under the same key carries the same value. That is the one relation
// that can be checked and that says the words have been dealt with. Another
// entry citing the same message is not that - it says the message was used,
// not that the words behind it were confirmed - so a statement that only has
// that stays.
//
// A statement that is neither confirmed nor cleared is crowded out by the
// capacity or by the board's budget, and that is counted as what it is: an
// expired statement, not a resolved one.
//
// The newest statements are the ones kept. The board travels with every turn,
// so its size is a fixed cost, and a statement from many turns ago has had
// every chance to be confirmed since.
func whiteboardMergeAnchors(current model.SessionWhiteboardDocument,
	board model.SessionWhiteboardDocument,
	added []model.SessionWhiteboardAnchor) ([]model.SessionWhiteboardAnchor, int, int) {
	kept := make([]model.SessionWhiteboardAnchor, 0, len(current.Unverified)+len(added))
	resolved := 0
	for _, anchor := range current.Unverified {
		if whiteboardAnchorResolved(anchor, board) {
			resolved++
			continue
		}
		kept = append(kept, anchor)
	}
	for _, anchor := range added {
		if anchor.MessageID == 0 {
			continue
		}
		duplicate := false
		for _, existing := range kept {
			if existing.MessageID == anchor.MessageID && existing.Value == anchor.Value {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		kept = append(kept, anchor)
	}
	// The newest first, so that the limit keeps the statements a reader is
	// most likely to need.
	sortWhiteboardAnchors(kept)
	expired := 0
	if len(kept) > model.SessionWhiteboardAnchorLimit {
		expired = len(kept) - model.SessionWhiteboardAnchorLimit
		kept = kept[:model.SessionWhiteboardAnchorLimit]
	}
	return kept, resolved, expired
}

// whiteboardAnchorResolved reports whether the board now holds what the
// statement pointed at.
//
// The relation is the key and the value together: the board has an entry
// under the statement's key and that entry carries the statement's value.
// Anything weaker - another entry citing the same message, an entry with the
// same key and a different value - is not a resolution, because none of them
// says the words behind the statement were confirmed.
func whiteboardAnchorResolved(anchor model.SessionWhiteboardAnchor,
	board model.SessionWhiteboardDocument) bool {
	key := strings.TrimSpace(anchor.Key)
	value := strings.TrimSpace(anchor.Value)
	if key == "" || value == "" {
		return false
	}
	for _, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
		for _, entry := range group {
			if strings.TrimSpace(entry.Key) != key {
				continue
			}
			if strings.TrimSpace(entry.Value) == value {
				return true
			}
		}
	}
	return false
}

// sortWhiteboardAnchors orders the statements newest first.
func sortWhiteboardAnchors(anchors []model.SessionWhiteboardAnchor) {
	for i := 1; i < len(anchors); i++ {
		for j := i; j > 0 && whiteboardAnchorNewer(anchors[j], anchors[j-1]); j-- {
			anchors[j], anchors[j-1] = anchors[j-1], anchors[j]
		}
	}
}

// whiteboardAnchorNewer reports whether the first statement is the newer one.
func whiteboardAnchorNewer(left, right model.SessionWhiteboardAnchor) bool {
	if left.UpdatedAtMessageID != right.UpdatedAtMessageID {
		return left.UpdatedAtMessageID > right.UpdatedAtMessageID
	}
	return left.MessageID > right.MessageID
}
