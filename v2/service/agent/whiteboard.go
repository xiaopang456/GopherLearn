package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"GopherAI/model"
)

// The whiteboard engine turns one batch of the learner's messages into the
// board's next state.
//
// It is a pure function on purpose. The proposal comes from a model, and
// everything that can go wrong with a model output has to be recoverable
// without a database, a queue or a retry. So the engine never rejects a whole
// proposal: an entry that cites something the batch never contained is
// dropped, an entry that is too long is cut, and an entry the proposal simply
// forgot is kept and reported. What it never leaves behind is a board whose
// contents it cannot explain.
//
// The consequence is that the board is always usable. A board that refuses a
// whole update stops updating, and a board that stopped updating is the
// failure this structure exists to remove.

// WhiteboardBatch is what one batch of learner messages allows a proposal to
// refer to.
type WhiteboardBatch struct {
	// MessageIDs are the learner's messages this batch covered. A citation
	// outside this set points at something the generator never saw.
	MessageIDs []uint
	// Messages are the texts of those messages. They are what a board value
	// is compared against when the engine looks for a value the board may
	// have missed.
	Messages []string
	// FirstMessageID is the oldest message id this batch may cite: everything
	// below it is already covered by the segment before this one.
	FirstMessageID uint
	// LastMessageID is the newest message this batch covered.
	LastMessageID uint
}

// allowed returns the message ids a citation may point at.
func (b WhiteboardBatch) allowed() map[uint]struct{} {
	allowed := make(map[uint]struct{}, len(b.MessageIDs))
	for _, id := range b.MessageIDs {
		if id > 0 {
			allowed[id] = struct{}{}
		}
	}
	return allowed
}

// WhiteboardProposal is what the generator proposed for the board.
//
// It is separate from the stored document because the two are not the same
// thing: the proposal is what one batch asked for, and it may be wrong,
// oversized or unsourced. The engine turns it into a board that can be
// stored.
type WhiteboardProposal struct {
	Goal        *model.SessionWhiteboardGoal
	Constraints []model.SessionWhiteboardEntry
	Plan        []model.SessionWhiteboardEntry
	OpenItems   []model.SessionWhiteboardEntry
}

// WhiteboardDiagnostics counts what the engine had to do to the proposal.
//
// Every field is a count of an intervention, so a run that shows a rising
// number of them is showing a generator that is drifting, not a board that is
// misbehaving.
type WhiteboardDiagnostics struct {
	// AppliedEntries is how many entries the proposal contributed.
	AppliedEntries int
	// ChangedEntries is how many entries ended up different from the old
	// board.
	ChangedEntries int
	// FabricatedEntries is how many entries were dropped because they cited
	// a message this batch never contained.
	FabricatedEntries int
	// SuspiciousDeletes is how many old entries were kept because the
	// proposal left them out without a reason.
	SuspiciousDeletes int
	// MissedUpdates is how many board values look replaced by the learner's
	// own words while the board still carries the old one.
	MissedUpdates int
	// TruncatedTexts is how many entries were cut to fit the length limit.
	TruncatedTexts int
	// DroppedEntries is how many entries were dropped outright.
	DroppedEntries int
	// DroppedDonePlan is how many finished plan entries were dropped
	// first: a completed step should not occupy the board.
	DroppedDonePlan int
}

// WhiteboardResult is the outcome of applying one proposal.
type WhiteboardResult struct {
	// Board is the board to store.
	Board model.SessionWhiteboardDocument
	// Changes are what this batch did to the board, in the order the reader
	// should see them.
	Changes []model.SessionSummarySegmentChange
	// SuspectedMisses are the keys whose current value may be stale.
	SuspectedMisses []string
	// Diagnostics counts the engine's interventions.
	Diagnostics WhiteboardDiagnostics
}

// ApplyWhiteboardProposal merges a proposal into the current board.
//
// The order of the work is the order of the ways a proposal can be wrong: an
// entry is checked for its shape, then for its sources, then the merged board
// is trimmed to its limits, and only what survived is compared against the
// old board to produce the changes.
func ApplyWhiteboardProposal(current model.SessionWhiteboardDocument, proposal WhiteboardProposal, batch WhiteboardBatch) (WhiteboardResult, error) {
	if current.SchemaVersion != model.SessionWhiteboardSchemaV1 {
		return WhiteboardResult{}, fmt.Errorf("session whiteboard schema version must be %d", model.SessionWhiteboardSchemaV1)
	}
	result := WhiteboardResult{
		Board: model.SessionWhiteboardDocument{SchemaVersion: model.SessionWhiteboardSchemaV1},
	}
	allowed := batch.allowed()

	goal, goalChange, dropped, fabricated := applyWhiteboardGoal(current.Goal, proposal.Goal, allowed, batch)
	result.Board.Goal = goal
	result.Diagnostics.DroppedEntries += dropped
	result.Diagnostics.FabricatedEntries += fabricated
	if goalChange != nil {
		result.Changes = append(result.Changes, *goalChange)
	}

	constraints, constraintChanges, constraintDiag := applyWhiteboardGroup(
		current.Constraints, proposal.Constraints, allowed, batch, false)
	result.Board.Constraints = constraints
	result.Changes = append(result.Changes, constraintChanges...)
	result.Diagnostics.add(constraintDiag)

	plan, planChanges, planDiag := applyWhiteboardGroup(
		current.Plan, proposal.Plan, allowed, batch, true)
	result.Board.Plan = plan
	result.Changes = append(result.Changes, planChanges...)
	result.Diagnostics.add(planDiag)

	openItems, openChanges, openDiag := applyWhiteboardGroup(
		current.OpenItems, proposal.OpenItems, allowed, batch, false)
	result.Board.OpenItems = openItems
	result.Changes = append(result.Changes, openChanges...)
	result.Diagnostics.add(openDiag)

	// The board travels with every turn, so its size is a fixed cost that
	// has to stay bounded. Entries are dropped from the oldest end and every
	// drop is recorded, so a value that leaves the board can still be found
	// in the segment that recorded it.
	droppedForTokens := trimWhiteboardToTokenLimit(&result.Board)
	for _, change := range droppedForTokens {
		result.Changes = append(result.Changes, change)
	}
	result.Diagnostics.DroppedEntries += len(droppedForTokens)

	// A value the learner's own words look like they replaced, while the
	// board still carries the old one. The engine cannot know the correct
	// value, so it reports the suspicion and leaves the board alone.
	suspected, missedChanges := detectMissedUpdates(current, result.Board, proposal, batch)
	result.SuspectedMisses = suspected
	result.Changes = append(result.Changes, missedChanges...)
	result.Diagnostics.MissedUpdates = len(suspected)

	if err := result.Board.Validate(); err != nil {
		return WhiteboardResult{}, fmt.Errorf("whiteboard result: %w", err)
	}
	result.Diagnostics.ChangedEntries = countChangedEntries(current, result.Board)
	return result, nil
}

// add folds one group's diagnostics into the total.
func (d *WhiteboardDiagnostics) add(other WhiteboardDiagnostics) {
	d.AppliedEntries += other.AppliedEntries
	d.ChangedEntries += other.ChangedEntries
	d.FabricatedEntries += other.FabricatedEntries
	d.SuspiciousDeletes += other.SuspiciousDeletes
	d.MissedUpdates += other.MissedUpdates
	d.TruncatedTexts += other.TruncatedTexts
	d.DroppedEntries += other.DroppedEntries
	d.DroppedDonePlan += other.DroppedDonePlan
}

// applyWhiteboardGoal decides what the board's goal becomes.
//
// The goal is one entry and it is never trimmed away, so a proposal that
// brings a new goal replaces the old one and a proposal that brings none
// leaves the old one in place. A new goal has to be sourced from this batch
// like any other entry: an unsourced goal is one the generator invented.
func applyWhiteboardGoal(current *model.SessionWhiteboardGoal, proposed *model.SessionWhiteboardGoal,
	allowed map[uint]struct{}, batch WhiteboardBatch) (*model.SessionWhiteboardGoal, *model.SessionSummarySegmentChange, int, int) {
	if proposed == nil {
		return current, nil, 0, 0
	}
	text := strings.TrimSpace(proposed.Text)
	if text == "" {
		return current, nil, 1, 0
	}
	if len([]rune(text)) > model.SessionWhiteboardTextMaxRunes {
		text = string([]rune(text)[:model.SessionWhiteboardTextMaxRunes])
	}
	ids, ok := filterWhiteboardEvidence(proposed.SourceMessageIDs, nil, allowed)
	if !ok {
		// The goal cites something this batch never contained. The old goal
		// stays; a goal the learner never said is worse than a stale one.
		return current, nil, 0, 1
	}
	goal := &model.SessionWhiteboardGoal{
		Text:               text,
		SourceMessageIDs:   ids,
		UpdatedAtMessageID: proposed.UpdatedAtMessageID,
	}
	if goal.UpdatedAtMessageID == 0 {
		goal.UpdatedAtMessageID = batch.LastMessageID
	}
	change := &model.SessionSummarySegmentChange{Key: "goal", NewValue: text, NewMessageIDs: ids}
	if current != nil {
		change.OldValue = current.Text
		change.OldMessageIDs = current.SourceMessageIDs
		if current.Text == text {
			// The goal did not change, so nothing changed.
			return goal, nil, 0, 0
		}
	}
	return goal, change, 0, 0
}

// applyWhiteboardGroup merges one category of entries.
//
// doneFirst marks the plan: a finished step is dropped before an unfinished
// one, because a board that still lists finished work is a board that has
// not been read.
func applyWhiteboardGroup(current []model.SessionWhiteboardEntry, proposed []model.SessionWhiteboardEntry,
	allowed map[uint]struct{}, batch WhiteboardBatch, doneFirst bool) ([]model.SessionWhiteboardEntry, []model.SessionSummarySegmentChange, WhiteboardDiagnostics) {
	var diagnostics WhiteboardDiagnostics
	currentByKey := make(map[string]model.SessionWhiteboardEntry, len(current))
	for _, entry := range current {
		if key := strings.TrimSpace(entry.Key); key != "" {
			currentByKey[key] = entry
		}
	}

	merged := make([]model.SessionWhiteboardEntry, 0, len(current)+len(proposed))
	positions := make(map[string]int, len(current))
	// An entry the proposal did not mention is kept. A proposal that leaves
	// an entry out has not said the entry is gone, and dropping a valid
	// constraint because the generator stopped repeating it would lose the
	// one thing the learner most needs to survive the next turn.
	for _, entry := range current {
		merged = append(merged, entry)
		if key := strings.TrimSpace(entry.Key); key != "" {
			positions[key] = len(merged) - 1
		}
	}

	seenKeys := make(map[string]struct{}, len(proposed))
	changes := make([]model.SessionSummarySegmentChange, 0, len(proposed))
	for _, entry := range proposed {
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			diagnostics.DroppedEntries++
			continue
		}
		if len([]rune(text)) > model.SessionWhiteboardTextMaxRunes {
			text = string([]rune(text)[:model.SessionWhiteboardTextMaxRunes])
			diagnostics.TruncatedTexts++
		}
		key := strings.TrimSpace(entry.Key)
		previous, isExisting := currentByKey[key]
		if key != "" {
			if _, seen := seenKeys[key]; seen {
				// The same key twice in one proposal: the later one wins,
				// because it is the one the generator put last.
				continue
			}
			seenKeys[key] = struct{}{}
		}
		ids, ok := filterWhiteboardEvidence(entry.SourceMessageIDs, previous.SourceMessageIDs, allowed)
		if !ok {
			// The entry cites a message this batch never contained. It is
			// dropped on its own: the rest of the proposal is still usable.
			diagnostics.FabricatedEntries++
			continue
		}
		updated := entry
		updated.Text = text
		updated.Key = key
		updated.SourceMessageIDs = ids
		if updated.UpdatedAtMessageID == 0 {
			if isExisting {
				updated.UpdatedAtMessageID = previous.UpdatedAtMessageID
			} else {
				updated.UpdatedAtMessageID = batch.LastMessageID
			}
		}
		if doneFirst && updated.State == "" && isExisting {
			updated.State = previous.State
		}
		if key != "" {
			if position, exists := positions[key]; exists {
				merged[position] = updated
				diagnostics.AppliedEntries++
				if change := whiteboardEntryChange(key, previous, updated, isExisting); change != nil {
					changes = append(changes, *change)
				}
				continue
			}
			positions[key] = len(merged)
		}
		merged = append(merged, updated)
		diagnostics.AppliedEntries++
		if change := whiteboardEntryChange(key, previous, updated, isExisting); change != nil {
			changes = append(changes, *change)
		}
	}

	// An old entry the proposal did not mention is kept, but the fact that it
	// was left out is reported: a generator that keeps dropping entries is
	// worth knowing about.
	for _, entry := range current {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		if _, mentioned := seenKeys[key]; mentioned {
			continue
		}
		diagnostics.SuspiciousDeletes++
	}

	trimmed, droppedChanges, droppedDone := trimWhiteboardGroup(merged, doneFirst)
	diagnostics.DroppedDonePlan += droppedDone
	diagnostics.DroppedEntries += len(droppedChanges)
	return trimmed, append(changes, droppedChanges...), diagnostics
}

// whiteboardEntryChange describes what one entry's update did.
//
// The change is computed from two boards, not read off the proposal: a
// generator that says it changed something cannot be believed about what the
// board looks like afterwards.
func whiteboardEntryChange(key string, previous, updated model.SessionWhiteboardEntry, existed bool) *model.SessionSummarySegmentChange {
	if !existed {
		return &model.SessionSummarySegmentChange{
			Key: key, NewValue: updated.Value, NewMessageIDs: updated.SourceMessageIDs,
		}
	}
	if previous.Value == updated.Value && previous.Text == updated.Text && previous.State == updated.State {
		return nil
	}
	return &model.SessionSummarySegmentChange{
		Key: key, OldValue: previous.Value, NewValue: updated.Value,
		OldMessageIDs: previous.SourceMessageIDs, NewMessageIDs: updated.SourceMessageIDs,
	}
}

// trimWhiteboardGroup keeps a group inside its entry limit.
//
// The limit is on how many entries a board may carry, and the entries that
// lose their place are the oldest ones: the newest state is the state that
// matters. Every dropped entry is recorded, so the value that left the board
// is still answerable from the segment that recorded it.
func trimWhiteboardGroup(entries []model.SessionWhiteboardEntry, doneFirst bool) ([]model.SessionWhiteboardEntry, []model.SessionSummarySegmentChange, int) {
	if len(entries) <= model.SessionWhiteboardItemLimit {
		return entries, nil, 0
	}
	ordered := make([]model.SessionWhiteboardEntry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(left, right int) bool {
		if doneFirst {
			leftDone := ordered[left].State == "done"
			rightDone := ordered[right].State == "done"
			if leftDone != rightDone {
				// A finished step gives up its place first.
				return !leftDone
			}
		}
		return ordered[left].UpdatedAtMessageID > ordered[right].UpdatedAtMessageID
	})
	kept := ordered[:model.SessionWhiteboardItemLimit]
	dropped := ordered[model.SessionWhiteboardItemLimit:]
	changes := make([]model.SessionSummarySegmentChange, 0, len(dropped))
	droppedDone := 0
	for _, entry := range dropped {
		if entry.State == "done" {
			droppedDone++
		}
		changes = append(changes, model.SessionSummarySegmentChange{
			Key:           strings.TrimSpace(entry.Key),
			OldValue:      entry.Value,
			OldMessageIDs: entry.SourceMessageIDs,
			Removed:       true,
		})
	}
	// The kept entries go back into the order they had, so the board's order
	// does not depend on how the trimming happened to sort them.
	keptSet := make(map[string]struct{}, len(kept))
	for _, entry := range kept {
		keptSet[entryIdentity(entry)] = struct{}{}
	}
	result := make([]model.SessionWhiteboardEntry, 0, len(kept))
	for _, entry := range entries {
		if _, exists := keptSet[entryIdentity(entry)]; exists {
			result = append(result, entry)
		}
	}
	return result, changes, droppedDone
}

// entryIdentity tells two entries apart when the board holds the same text
// under different keys.
func entryIdentity(entry model.SessionWhiteboardEntry) string {
	return strings.TrimSpace(entry.Key) + "\x00" + strings.TrimSpace(entry.Text)
}

// trimWhiteboardToTokenLimit drops the oldest entries until the board fits.
//
// The board is sent on every turn, so its size is a fixed cost. Dropping is
// better than refusing the whole update: a board with fewer entries is still
// a board, and a board that refused updates is not.
func trimWhiteboardToTokenLimit(board *model.SessionWhiteboardDocument) []model.SessionSummarySegmentChange {
	changes := make([]model.SessionSummarySegmentChange, 0)
	for boardTokenEstimate(board) > model.SessionWhiteboardTokenLimit {
		oldest, found := findOldestBoardEntry(board)
		if !found {
			break
		}
		changes = append(changes, model.SessionSummarySegmentChange{
			Key:           strings.TrimSpace(oldest.entry.Key),
			OldValue:      oldest.entry.Value,
			OldMessageIDs: oldest.entry.SourceMessageIDs,
			Removed:       true,
		})
		switch oldest.group {
		case whiteboardGroupConstraints:
			board.Constraints = removeWhiteboardEntry(board.Constraints, oldest.entry)
		case whiteboardGroupPlan:
			board.Plan = removeWhiteboardEntry(board.Plan, oldest.entry)
		case whiteboardGroupOpenItems:
			board.OpenItems = removeWhiteboardEntry(board.OpenItems, oldest.entry)
		}
	}
	return changes
}

// The board's groups, named so the trimming code can say which one an entry
// came from.
const (
	whiteboardGroupConstraints = "constraints"
	whiteboardGroupPlan        = "plan"
	whiteboardGroupOpenItems   = "open_items"
)

type whiteboardEntryLocation struct {
	group string
	entry model.SessionWhiteboardEntry
}

// findOldestBoardEntry returns the entry that should lose its place first.
func findOldestBoardEntry(board *model.SessionWhiteboardDocument) (whiteboardEntryLocation, bool) {
	oldest := whiteboardEntryLocation{}
	found := false
	for _, group := range []struct {
		name    string
		entries []model.SessionWhiteboardEntry
	}{
		{whiteboardGroupConstraints, board.Constraints},
		{whiteboardGroupPlan, board.Plan},
		{whiteboardGroupOpenItems, board.OpenItems},
	} {
		for _, entry := range group.entries {
			if !found || entry.UpdatedAtMessageID < oldest.entry.UpdatedAtMessageID {
				oldest = whiteboardEntryLocation{group: group.name, entry: entry}
				found = true
			}
		}
	}
	return oldest, found
}

// removeWhiteboardEntry takes one entry out of a group.
func removeWhiteboardEntry(entries []model.SessionWhiteboardEntry, target model.SessionWhiteboardEntry) []model.SessionWhiteboardEntry {
	result := make([]model.SessionWhiteboardEntry, 0, len(entries))
	for _, entry := range entries {
		if entryIdentity(entry) == entryIdentity(target) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

// boardTokenEstimate estimates what the board costs to send.
func boardTokenEstimate(board *model.SessionWhiteboardDocument) int {
	total := 0
	if board.Goal != nil {
		total += messageTokenCost(board.Goal.Text)
	}
	for _, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
		for _, entry := range group {
			total += messageTokenCost(entry.Text)
		}
	}
	return total
}

// filterWhiteboardEvidence decides which of an entry's citations survive.
//
// An entry that was already on the board may keep the citations it had: the
// generator did not see those messages this time, so it cannot repeat them,
// and the board must not lose the proof of where a value came from. A
// citation the generator adds has to point at this batch, and a new entry
// has to be sourced entirely from this batch.
//
// The second return value is false when the entry must be dropped.
func filterWhiteboardEvidence(proposed, previous []uint, allowed map[uint]struct{}) ([]uint, bool) {
	if len(proposed) == 0 {
		if len(previous) == 0 {
			// A new entry with no citation at all is an entry nobody said.
			return nil, false
		}
		return append([]uint(nil), previous...), true
	}
	previousSet := make(map[uint]struct{}, len(previous))
	for _, id := range previous {
		previousSet[id] = struct{}{}
	}
	kept := make([]uint, 0, len(proposed))
	seen := make(map[uint]struct{}, len(proposed))
	for _, id := range proposed {
		if id == 0 {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, false
		}
		seen[id] = struct{}{}
		if _, carried := previousSet[id]; carried {
			kept = append(kept, id)
			continue
		}
		if _, permitted := allowed[id]; !permitted {
			return nil, false
		}
		kept = append(kept, id)
	}
	if len(kept) == 0 {
		return nil, false
	}
	return kept, true
}

// countChangedEntries counts the entries whose contents differ from the old
// board.
func countChangedEntries(current, next model.SessionWhiteboardDocument) int {
	changed := 0
	currentEntries := whiteboardEntries(current)
	nextEntries := whiteboardEntries(next)
	for key, entry := range nextEntries {
		previous, exists := currentEntries[key]
		if !exists {
			changed++
			continue
		}
		if previous.Value != entry.Value || previous.Text != entry.Text || previous.State != entry.State {
			changed++
		}
	}
	return changed
}

// allEntries indexes a board's entries by key.
func whiteboardEntries(d model.SessionWhiteboardDocument) map[string]model.SessionWhiteboardEntry {
	entries := make(map[string]model.SessionWhiteboardEntry)
	for _, group := range [][]model.SessionWhiteboardEntry{d.Constraints, d.Plan, d.OpenItems} {
		for _, entry := range group {
			key := strings.TrimSpace(entry.Key)
			if key == "" {
				continue
			}
			entries[key] = entry
		}
	}
	return entries
}

// The value the board holds and the value the learner said, compared for
// sameness. The unit is what makes the comparison meaningful: "48" and "48
// minutes" are the same value, and "48 minutes" and "48 hours" are not.
//
// The unit is bounded so that the sentence around the number does not become
// part of it: "48 分钟吧" is forty-eight minutes, not "48 分钟吧" minutes.
var whiteboardValuePattern = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*(\p{Han}{1,2}|[a-zA-Z]{1,3}|%)`)

// parseWhiteboardValue reads the number and the unit out of a board value.
func parseWhiteboardValue(value string) (float64, string, bool) {
	match := whiteboardValuePattern.FindStringSubmatch(value)
	if len(match) < 3 {
		return 0, "", false
	}
	number := 0.0
	if _, err := fmt.Sscanf(match[1], "%g", &number); err != nil {
		return 0, "", false
	}
	return number, match[2], true
}

// detectMissedUpdates looks for a board value the learner's own words look
// like they replaced.
//
// The engine cannot know the correct value, so it only reports a suspicion.
// Two signals make a suspicion worth reporting: the board's own value appears
// in the learner's words, or the entry's key does. A number with the same
// unit and a different value, next to one of those, is what a missed update
// looks like.
//
// The signal is deliberately weak. A false suspicion costs the next
// generation one sentence of doubt; a missed suspicion costs the learner a
// value that was corrected two turns ago.
func detectMissedUpdates(current, next model.SessionWhiteboardDocument, proposal WhiteboardProposal, batch WhiteboardBatch) ([]string, []model.SessionSummarySegmentChange) {
	if len(batch.Messages) == 0 {
		return nil, nil
	}
	proposedKeys := proposedKeys(proposal)
	nextEntries := whiteboardEntries(next)
	suspects := make([]string, 0)
	changes := make([]model.SessionSummarySegmentChange, 0)
	for key, entry := range whiteboardEntries(current) {
		if key == "" || strings.TrimSpace(entry.Value) == "" {
			continue
		}
		if _, mentioned := proposedKeys[key]; mentioned {
			// The proposal spoke about this key, so the board's value is
			// either updated or deliberately kept.
			continue
		}
		if _, inNext := nextEntries[key]; !inNext {
			continue
		}
		number, unit, ok := parseWhiteboardValue(entry.Value)
		if !ok || unit == "" {
			continue
		}
		if _, found := findReplacementValue(batch.Messages, number, unit); !found {
			continue
		}
		suspects = append(suspects, key)
		changes = append(changes, model.SessionSummarySegmentChange{
			Key:       key,
			OldValue:  entry.Value,
			Suspected: true,
		})
	}
	sort.Strings(suspects)
	return suspects, changes
}

// proposedKeys is the set of keys the proposal spoke about.
func proposedKeys(proposal WhiteboardProposal) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, group := range [][]model.SessionWhiteboardEntry{
		proposal.Constraints, proposal.Plan, proposal.OpenItems,
	} {
		for _, entry := range group {
			if key := strings.TrimSpace(entry.Key); key != "" {
				keys[key] = struct{}{}
			}
		}
	}
	return keys
}

// findReplacementValue looks for a different value of the same unit in the
// learner's messages.
//
// The unit is what makes the comparison meaningful: "48" and "48 minutes" are
// the same value, and "48 minutes" and "48 hours" are not. A value without a
// unit is not compared at all, because "three" and "three days" have nothing
// to do with each other and a bare number in a message is usually not about
// the board's value.
func findReplacementValue(messages []string, number float64, unit string) (float64, bool) {
	for _, message := range messages {
		for _, match := range whiteboardValuePattern.FindAllStringSubmatch(message, -1) {
			if len(match) < 3 || match[2] != unit {
				continue
			}
			candidate := 0.0
			if _, err := fmt.Sscanf(match[1], "%g", &candidate); err != nil {
				continue
			}
			if candidate != number {
				return candidate, true
			}
		}
	}
	return 0, false
}
