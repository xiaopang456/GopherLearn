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
// an unconfirmed statement: the message they came from can be read back, the
// reader can see what the learner actually said, and a value the board shows
// next to them is marked as contested rather than as the one true value.
//
// What an unconfirmed statement is not: it is not evidence, it is not a
// fallback fact, and it is not a way to let any message be read. It exists
// only for a message of this batch, of this session, that the learner wrote
// and that really carries the value the refused entry wanted to put up.

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
	reasons := make(map[string]int, len(diagnostics.ClaimCandidates))
	for _, candidate := range diagnostics.ClaimCandidates {
		if candidate.Reason == "" {
			continue
		}
		reasons[candidate.Reason]++
	}
	if len(reasons) > 0 {
		observability.Memory.WhiteboardClaimRejections(reasons)
	}
	if diagnostics.UnconfirmedStatements > 0 {
		observability.Memory.WhiteboardUnconfirmedStatements(diagnostics.UnconfirmedStatements)
	}
}

// The labels a verdict is written under in a diagnostic.
//
// They are a fixed set, so a diagnostic never carries the learner's own words
// and never becomes free text in a metric.
const (
	whiteboardClaimVerdictLabelRejected  = "rejected"
	whiteboardClaimVerdictLabelUnsettled = "unsettled"
	whiteboardClaimVerdictLabelAmbiguous = "ambiguous"
)

// whiteboardClaimVerdictLabel is the label a verdict is written under.
func whiteboardClaimVerdictLabel(verdict whiteboardClaimVerdict) string {
	switch verdict {
	case whiteboardClaimRejected:
		return whiteboardClaimVerdictLabelRejected
	case whiteboardClaimUnsettled:
		return whiteboardClaimVerdictLabelUnsettled
	}
	return whiteboardClaimVerdictLabelAmbiguous
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
func whiteboardAppendAnchor(anchors []model.SessionWhiteboardAnchor, entry model.SessionWhiteboardEntry,
	claim WhiteboardLearnerClaim, hasClaim bool, batch WhiteboardBatch,
	reason string) []model.SessionWhiteboardAnchor {
	anchor, ok := whiteboardAnchorFor(entry, claim, hasClaim, batch, reason)
	if !ok {
		return anchors
	}
	for _, existing := range anchors {
		if existing.MessageID == anchor.MessageID && existing.Value == anchor.Value {
			return anchors
		}
	}
	return append(anchors, anchor)
}

// whiteboardAnchorFor builds the unconfirmed statement of a refused entry.
//
// The words it points at are the learner's own, in this batch: the message the
// claim rests on, and the messages the entry cites. A message is only used
// when it really carries the value the entry wanted to put on the board, so a
// statement is never built out of a message that does not mention it.
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
			Quote:              whiteboardAnchorQuote(text, value, claim, hasClaim),
			Reason:             reason,
			UpdatedAtMessageID: batch.LastMessageID,
		}
		if hasClaim {
			anchor.Subject = whiteboardPlainText(claim.Subject)
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

// whiteboardAnchorQuote is the learner's own words the statement points at.
//
// The model's quote is used when it really is in the message: it is the part
// of the message the model read, and it is the shortest honest answer. When
// it is not, the sentence that carries the value is used instead, because a
// statement has to show the words it is about.
func whiteboardAnchorQuote(text, value string, claim WhiteboardLearnerClaim, hasClaim bool) string {
	if hasClaim {
		quote := strings.TrimSpace(claim.Quote)
		if quote != "" && strings.Contains(whiteboardPlainText(text), whiteboardPlainText(quote)) {
			// The words are kept as they were written: a reader is shown
			// the learner's own sentence, not a tidied one.
			return whiteboardTrimRunes(quote, model.SessionWhiteboardAnchorTextMaxRunes)
		}
	}
	for _, sentence := range whiteboardSentences(text) {
		if whiteboardQuoteCarriesValue(sentence, value) {
			return whiteboardTrimRunes(sentence, model.SessionWhiteboardAnchorTextMaxRunes)
		}
	}
	return whiteboardTrimRunes(strings.TrimSpace(text), model.SessionWhiteboardAnchorTextMaxRunes)
}

// whiteboardSentences splits a message into the sentences a reader would
// read on their own.
//
// The split is on the marks a learner writes between thoughts. A message that
// carries no such mark is one sentence.
func whiteboardSentences(text string) []string {
	parts := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '。', '！', '？', '；', '\n', '!', '?', ';':
			return true
		}
		return false
	})
	sentences := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		sentences = append(sentences, part)
	}
	if len(sentences) == 0 {
		return []string{strings.TrimSpace(text)}
	}
	return sentences
}

// whiteboardTrimRunes shortens a text to a limit without splitting a rune.
func whiteboardTrimRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// whiteboardMergeAnchors carries the unconfirmed statements forward and adds
// the new ones.
//
// A statement is resolved when the board now holds what it pointed at: either
// an entry cites the message the words came from, or an entry under the same
// key carries the value the words were about. A statement that has been
// resolved is dropped, because what it said is now on the board.
//
// The newest statements are the ones kept. The board travels with every turn,
// so its size is a fixed cost, and a statement from many turns ago has had
// every chance to be confirmed since.
func whiteboardMergeAnchors(current model.SessionWhiteboardDocument,
	board model.SessionWhiteboardDocument, added []model.SessionWhiteboardAnchor) []model.SessionWhiteboardAnchor {
	kept := make([]model.SessionWhiteboardAnchor, 0, len(current.Unverified)+len(added))
	for _, anchor := range current.Unverified {
		if whiteboardAnchorResolved(anchor, board) {
			continue
		}
		kept = append(kept, anchor)
	}
	for _, anchor := range added {
		if anchor.MessageID == 0 || strings.TrimSpace(anchor.Quote) == "" {
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
	if len(kept) > model.SessionWhiteboardAnchorLimit {
		kept = kept[:model.SessionWhiteboardAnchorLimit]
	}
	return kept
}

// whiteboardAnchorResolved reports whether the board now holds what the
// statement pointed at.
func whiteboardAnchorResolved(anchor model.SessionWhiteboardAnchor,
	board model.SessionWhiteboardDocument) bool {
	key := strings.TrimSpace(anchor.Key)
	for _, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
		for _, entry := range group {
			for _, id := range entry.SourceMessageIDs {
				if id == anchor.MessageID {
					return true
				}
			}
			if key != "" && strings.TrimSpace(entry.Key) == key &&
				strings.TrimSpace(entry.Value) == strings.TrimSpace(anchor.Value) {
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

// whiteboardContestedEntries are the board entries whose value an
// unconfirmed statement is about.
//
// The link is the statement's subject: it is the thing the learner spoke
// about, and it is only used when exactly one entry names it. A statement
// whose subject names no entry, or more than one, contests nothing: the
// program cannot place it, and it says so by leaving the board alone.
//
// The value has to differ, or the statement is about the value the board
// already holds and there is nothing to contest.
func whiteboardContestedEntries(board model.SessionWhiteboardDocument) map[string]string {
	contested := make(map[string]string)
	for _, anchor := range board.Unverified {
		subject := strings.TrimSpace(anchor.Subject)
		if subject == "" {
			continue
		}
		if anchor.Reason == model.SessionWhiteboardAnchorUnsettled {
			// The learner did not settle the value: they ruled it out or
			// asked about it. There is no conflict to mark, because the
			// board still holds what they kept.
			continue
		}
		value := strings.TrimSpace(anchor.Value)
		matched := ""
		count := 0
		for _, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
			for _, entry := range group {
				if !strings.Contains(whiteboardEntryDescription(entry), subject) {
					continue
				}
				count++
				if matched == "" {
					matched = strings.TrimSpace(entry.Key)
				}
			}
		}
		if count != 1 || matched == "" {
			continue
		}
		if strings.TrimSpace(currentWhiteboardValue(board, matched)) == value {
			continue
		}
		contested[matched] = value
	}
	return contested
}

// currentWhiteboardValue is the value the board holds under a key.
func currentWhiteboardValue(board model.SessionWhiteboardDocument, key string) string {
	for _, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
		for _, entry := range group {
			if strings.TrimSpace(entry.Key) == key {
				return entry.Value
			}
		}
	}
	return ""
}
