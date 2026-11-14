package agent

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"GopherAI/common/rag"
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

// WhiteboardMessageRole says what a cited message was.
//
// It is a role rather than a boolean because the engine has to tell three
// things apart: a message the learner wrote, a message the assistant wrote,
// and a message whose role is not known - and an unknown role proves
// nothing.
type WhiteboardMessageRole string

const (
	// WhiteboardMessageRoleUser is a message the learner wrote.
	WhiteboardMessageRoleUser WhiteboardMessageRole = "user"
	// WhiteboardMessageRoleAssistant is a message the assistant wrote.
	WhiteboardMessageRoleAssistant WhiteboardMessageRole = "assistant"
)

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
	// AssistantMessageIDs are the assistant's messages this batch covered.
	//
	// They are here for one reason: a proposal of the assistant's that the
	// learner took on may reach the board, and it has to cite the message
	// the proposal was made in as well as the message that took it on.
	AssistantMessageIDs []uint
	// KnownMessages says what every message id the citations may point at
	// was.
	//
	// It holds the messages of this batch and the ones of an earlier batch
	// that the previous segment still cites. The engine cannot ask the
	// database what a message was, so an acceptance that straddles two
	// batches can only be checked if the earlier message is described here.
	KnownMessages map[uint]WhiteboardMessageRole
	// FirstMessageID is the oldest message id this batch may cite: everything
	// below it is already covered by the segment before this one.
	FirstMessageID uint
	// LastMessageID is the newest message this batch covered.
	LastMessageID uint
}

// learnerMessage reports whether an id is one of the learner's messages in
// this batch.
func (b WhiteboardBatch) learnerMessage(id uint) (WhiteboardMessageRole, bool) {
	for _, candidate := range b.MessageIDs {
		if candidate == id {
			return WhiteboardMessageRoleUser, true
		}
	}
	return "", false
}

// allowed returns the message ids a citation of an entry with this origin
// may point at.
//
// The origin decides the set: an entry of the learner's may only cite the
// learner's messages, and an entry that is an accepted proposal may also
// cite the assistant's message the proposal was made in.
//
// The learner's own set is the one the engine always had. Widening it for
// the other origin is what lets a proposal reach the board without letting
// an entry of the learner's cite something the learner did not say.
func (b WhiteboardBatch) allowed(origin string) map[uint]struct{} {
	allowed := make(map[uint]struct{}, len(b.MessageIDs))
	for _, id := range b.MessageIDs {
		if id > 0 {
			allowed[id] = struct{}{}
		}
	}
	if origin != model.SessionWhiteboardOriginAssistant &&
		origin != model.SessionWhiteboardOriginAssistantAccepted {
		return allowed
	}
	for _, id := range b.AssistantMessageIDs {
		if id > 0 {
			allowed[id] = struct{}{}
		}
	}
	// A proposal the assistant made in an earlier batch. The previous
	// segment still cites it, so the engine knows it was the assistant's;
	// this batch never saw the message itself.
	for id, role := range b.KnownMessages {
		if role != WhiteboardMessageRoleAssistant {
			continue
		}
		if _, inBatch := allowed[id]; inBatch {
			continue
		}
		allowed[id] = struct{}{}
	}
	return allowed
}

// acceptedAssistantProposal reports whether the citations prove that the
// learner took an assistant proposal on.
//
// The proof is a pair: the assistant's message that made the proposal, and a
// learner's message after it that accepted it. One without the other is not
// acceptance - the assistant's message alone is a proposal nobody answered,
// and the learner's message alone is a statement with no proposal to accept.
//
// The order is the message ids' order, which is the order the messages were
// written in.
func acceptedAssistantProposal(ids []uint, batch WhiteboardBatch) bool {
	proposals := make([]uint, 0, len(ids))
	for _, id := range ids {
		if batch.KnownMessages[id] == WhiteboardMessageRoleAssistant {
			proposals = append(proposals, id)
		}
	}
	if len(proposals) == 0 {
		return false
	}
	for _, id := range ids {
		if batch.KnownMessages[id] != WhiteboardMessageRoleUser {
			continue
		}
		for _, proposal := range proposals {
			if id > proposal {
				return true
			}
		}
	}
	return false
}

// citesAssistantMessage reports whether the citations name a message of the
// assistant's.
//
// It is the question that decides what a claim is worth: an entry that claims
// to be the assistant's and cites nothing of the assistant's is not the
// assistant's - whatever it says, it was drawn from the learner's own words.
func citesAssistantMessage(ids []uint, batch WhiteboardBatch) bool {
	for _, id := range ids {
		if batch.KnownMessages[id] == WhiteboardMessageRoleAssistant {
			return true
		}
	}
	return false
}

// whiteboardPlainText is a text with its spaces taken out.
//
// Two texts that differ only in spacing say the same thing, and a learner
// writes "41 页" where the board holds "41 页/次".
func whiteboardPlainText(text string) string {
	var builder strings.Builder
	for _, r := range text {
		switch r {
		case ' ', '\t', '\n', '\r', '\u3000':
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// whiteboardCitesID reports whether the citations name the message.
func whiteboardCitesID(ids []uint, id uint) bool {
	for _, cited := range ids {
		if cited == id {
			return true
		}
	}
	return false
}

// whiteboardCitesAssistantBefore reports whether the entry cites a message of
// the assistant's that is older than the learner's message.
//
// It is the question that separates the learner's own statement from the
// assistant's proposal the learner took on. When the assistant's message comes
// first, the value came from the assistant and the learner's message is the
// acceptance. When the learner's message comes first, the value is the
// learner's and the assistant only reacted to it - which is not an acceptance
// of anything and must not be read as one.
func whiteboardCitesAssistantBefore(ids []uint, learnerMessageID uint, batch WhiteboardBatch) bool {
	for _, id := range ids {
		if id >= learnerMessageID {
			continue
		}
		if batch.KnownMessages[id] == WhiteboardMessageRoleAssistant {
			return true
		}
	}
	return false
}

// whiteboardOriginForProposal is the origin a proposed entry is read under.
//
// The empty origin is the learner's, and the case of the label does not
// change what it means: "User" and "user" are the same claim.
//
// A board that cannot name an assistant's origin cannot hold one. The mode
// that does not record the assistant's products never asked for an origin, so
// a label naming one there is the model writing something it was not asked
// for, and it is refused the way it always was.
func whiteboardOriginForProposal(origin string, schemaVersion int) (string, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(origin))
	switch trimmed {
	case "", model.SessionWhiteboardOriginUser:
		return model.SessionWhiteboardOriginUser, true
	case model.SessionWhiteboardOriginAssistant:
		if schemaVersion == model.SessionWhiteboardSchemaV1 {
			return "", false
		}
		return model.SessionWhiteboardOriginAssistant, true
	case model.SessionWhiteboardOriginAssistantAccepted:
		return model.SessionWhiteboardOriginAssistantAccepted, true
	}
	return "", false
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
	// Claims are the learner's own words behind the entries, one per entry
	// that has one.
	//
	// They are what a correction rests on: an entry the model labelled as
	// the assistant's may still be the learner's own statement, and the
	// only way to know is the learner's own words. A claim is matched to
	// its entry by the entry's key, and a claim that cannot be placed is
	// not used.
	Claims []WhiteboardLearnerClaim
}

// WhiteboardDiagnostics counts what the engine had to do to the proposal.
//
// Every field is a count of an intervention, so a run that shows a rising
// number of them is showing a generator that is drifting, not a board that is
// misbehaving.
type whiteboardRemoval struct{ key, value, reason, group string }
type anchorRemoval struct {
	messageID          uint
	key, value, reason string
}
type WhiteboardDiagnostics struct {
	removedEntries []whiteboardRemoval
	removedAnchors []anchorRemoval
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
	// DroppedPoints is how many segment points were dropped before the
	// segment could be stored. The whiteboard engine does not set it; the
	// generator does, because the points belong to the segment and the
	// counters are reported together.
	DroppedPoints int
	// UnprovenAcceptances is how many entries were dropped because they
	// claimed the learner took an assistant proposal on and their citations
	// did not show it.
	//
	// It is a count of its own because it is the one intervention that says
	// something about the model: a generator that keeps claiming acceptances
	// nobody gave is a generator that has learned the wrong rule.
	UnprovenAcceptances int
	// UnknownOrigins is how many entries were dropped because they named an
	// origin the engine does not know.
	UnknownOrigins int
	// MislabeledOrigins is how many entries claimed an assistant origin and
	// cited nothing of the assistant's, and were kept as the learner's own.
	//
	// It is a count of its own because it is the one intervention that says
	// something about the model: a generator that keeps labelling the
	// learner's own words as somebody else's has learned the wrong rule, and
	// the count is what shows it.
	MislabeledOrigins int
	// AssistantProposals is how many entries were the assistant's own
	// proposal, which the learner did not take on.
	//
	// They do not reach the board: a proposal nobody agreed to is not a
	// current fact. They go to the summary segment instead, labelled as the
	// assistant's, which is where a reader can still find them.
	AssistantProposals int
	// VerifiedClaims is how many entries were confirmed by the learner's
	// own words - a quote that is really in the message, a value that is
	// the entry's value, and a subject and an attribute that are what the
	// entry is about.
	//
	// It is a count of its own because it is the one that says the
	// evidence is being used: a generator that keeps leaving the board
	// without it is a generator whose corrections are being refused.
	VerifiedClaims int
	// RejectedClaims is how many claims did not hold.
	//
	// A claim that does not hold is not evidence, and the entry it was
	// written for keeps the label the model gave it. A rising number here
	// is a generator that writes evidence it did not read.
	RejectedClaims int
	// UnsettledClaims is how many claims held but said the learner did
	// not settle the value.
	//
	// A value nobody settled is not a current fact, so an entry that says
	// it is the learner's own does not go on the board.
	UnsettledClaims int
	// UnbackedChanges is how many entries were refused because they
	// replaced a value the board already held and the learner's own words
	// were not behind them.
	//
	// A value that is replaced by a value nobody said is a value that is
	// gone, so the old one stays until something backs the new one.
	UnbackedChanges int
	// UnconfirmedStatements is how many unconfirmed learner statements the
	// batch left behind.
	//
	// A statement is the learner's own words that could not be confirmed as
	// a current fact. It is not a fact and it never becomes one: it is the
	// words, kept so that a reader can find them and ask for the message
	// they came from.
	UnconfirmedStatements int
	// ResolvedUnconfirmedStatements is how many statements the board caught
	// up with: the board now holds the same value under the same key.
	//
	// It is counted apart from the expired ones, because the two mean
	// different things: a resolved statement was dealt with, an expired one
	// was crowded out.
	ResolvedUnconfirmedStatements int
	// ExpiredUnconfirmedStatements is how many statements were crowded out
	// by the capacity or by the board's budget.
	//
	// They are not resolved. The learner's words are still unconfirmed; they
	// are simply no longer carried, and the count says so.
	ExpiredUnconfirmedStatements int

	// Entries are what became of every entry the proposal brought, one row
	// each, and the three layers of the decision are kept apart.
	//
	// The counters above say how many claims did not hold. They cannot say
	// which entry was refused or which check refused it, so a refusal was a
	// number and not a place. These are the place: the key is the model's
	// own name for the entry, the claim says what the engine made of the
	// model's own account of the learner's words, the outcome says what
	// became of the entry itself, and the anchor says whether the learner's
	// words stayed findable.
	//
	// The three layers are apart because they are three different facts. A
	// claim that holds is not an entry on the board, and an entry that was
	// refused may still have left the learner's words findable.
	//
	// The learner's own words are not here. They are in the model's answer,
	// and a count that carried them would put the learner's text into a
	// counter.
	Entries []WhiteboardEntryOutcome
}

// WhiteboardEntryOutcome is what became of one proposed entry.
//
// The three layers are kept apart on purpose. The claim is the model's own
// account of the learner's words and what the engine made of it; the outcome
// is what became of the entry itself; the anchor is whether the learner's
// words were kept findable, and under which message.
//
// A claim that holds is not an entry on the board, so a diagnostic that
// reported only the claim could not tell a reader that the value never
// landed. And an entry that was refused is not a lost turn: the learner's
// words may still be readable under an anchor, which is a different thing
// again from the value being confirmed.
type WhiteboardEntryOutcome struct {
	// Key is the model's own name for the entry.
	Key            string
	CallIndex      int
	candidateEntry *model.SessionWhiteboardEntry
	candidateGroup string
	// ClaimVerdict is what the engine made of the claim: settled,
	// unsettled, rejected, ambiguous or absent.
	//
	// Every verdict has its own name. A verdict that is not named falls into
	// "absent", which says nothing was asserted - and a claim that holds
	// used to be indistinguishable from that.
	ClaimVerdict string
	// ClaimReason is why the claim did not hold. It is empty when the claim
	// held.
	//
	// It is one of the fixed reasons in the model package, so it never
	// carries the learner's own words.
	ClaimReason string
	// ClaimMessageID is the learner's message the claim rested on. It is
	// zero when the entry carried no claim.
	ClaimMessageID uint
	// Outcome is what became of the entry: applied, refused or dropped.
	Outcome string
	// OutcomeReason is why. It is empty when the entry was applied.
	OutcomeReason string
	// AnchorMessageID is the message the learner's own words were kept
	// under. It is zero when no anchor was made.
	AnchorMessageID uint
	// AnchorReason is the reason the anchor carries.
	AnchorReason string
	// EntryRetention and AnchorRetention describe the generated board after all limits.
	// These are not persistence or display claims.
	EntryRetention        string
	EntryRetentionReason  string
	AnchorRetention       string
	AnchorRetentionReason string
	candidateText         string
	candidateValue        string
}

// What became of a proposed entry.
//
// The names are a fixed set, so a diagnostic never carries the learner's own
// words and never becomes free text in a metric.
const (
	// whiteboardEntryOutcomeApplied says the entry went onto the board.
	whiteboardEntryOutcomeApplied = "applied"
	// whiteboardEntryOutcomeRefused says a check refused the entry: the
	// value was not confirmed, or the entry could not be sourced.
	whiteboardEntryOutcomeRefused = "refused"
	// whiteboardEntryOutcomeDropped says the entry never reached a check -
	// it had no text, or the proposal repeated a key.
	whiteboardEntryOutcomeDropped = "dropped"
)

// Why an entry did not go on the board.
//
// The reasons that are about the learner's own words are the ones in the
// model package, because they are the same reasons the unconfirmed
// statements carry. The rest are about the shape of the proposal.
const (
	// whiteboardEntryReasonEmptyText says the entry had no text.
	whiteboardEntryReasonEmptyText = "empty_text"
	// whiteboardEntryReasonDuplicateKey says the proposal brought the same
	// key twice and the later one won.
	whiteboardEntryReasonDuplicateKey = "duplicate_key"
	// whiteboardEntryReasonOriginUnknown says the entry named an origin
	// the program does not know.
	whiteboardEntryReasonOriginUnknown = "origin_unknown"
	// whiteboardEntryReasonAssistantProduct says the entry was the
	// assistant's own proposal, which the learner did not take on.
	whiteboardEntryReasonAssistantProduct = "assistant_product"
	// whiteboardEntryReasonUnprovenAcceptance says the entry said the
	// learner took a proposal on and the citations did not show it.
	whiteboardEntryReasonUnprovenAcceptance = "unproven_acceptance"
)

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

// whiteboardKnownSchemaVersion reports whether a version is one the engine
// knows.
func whiteboardKnownSchemaVersion(version int) bool {
	switch version {
	case model.SessionWhiteboardSchemaV1, model.SessionWhiteboardSchemaV2,
		model.SessionWhiteboardSchemaV3:
		return true
	}
	return false
}

// The versions of the board, and what each of them can hold.
//
// The engine's behaviour is decided by the version, because the version is
// the format the document is written in: a version 1 board could only hold
// what the learner said, a version 2 board says where each entry came from
// and is checked against the learner's own words, and a version 3 board can
// also carry the learner's words behind a change that was refused.
//
// A document written before the statements existed - version 1 or version 2 -
// is read and written as it was: it has no statements, and the engine never
// puts any there. The statements are a version 3 field and a version 3
// document says so in its version, so a deployment that rolls back can see
// that the documents it will meet are not the ones it knows.
func whiteboardCarriesOrigins(schemaVersion int) bool {
	return schemaVersion >= model.SessionWhiteboardSchemaV2
}

// whiteboardCarriesStatements reports whether the version can hold the
// learner's own words behind a refused change.
func whiteboardCarriesStatements(schemaVersion int) bool {
	return schemaVersion >= model.SessionWhiteboardSchemaV3
}

// ApplyWhiteboardProposal merges a proposal into the current board.
//
// The order of the work is the order of the ways a proposal can be wrong: an
// entry is checked for its shape, then for its sources, then the merged board
// is trimmed to its limits, and only what survived is compared against the
// old board to produce the changes.
//
// The schema version is the one the board is written in, and it is the
// caller's decision: the mode that does not record what the assistant
// produced writes version 1 and the mode that does writes version 2. The
// version decides two things - what the document looks like on disk, and what
// may be in it, because a version 1 board could only hold what the learner
// said.
func ApplyWhiteboardProposal(current model.SessionWhiteboardDocument, proposal WhiteboardProposal,
	batch WhiteboardBatch, schemaVersion int) (WhiteboardResult, error) {
	if !whiteboardKnownSchemaVersion(current.SchemaVersion) {
		return WhiteboardResult{}, fmt.Errorf("session whiteboard schema version must be %d, %d or %d",
			model.SessionWhiteboardSchemaV1, model.SessionWhiteboardSchemaV2,
			model.SessionWhiteboardSchemaV3)
	}
	if !whiteboardKnownSchemaVersion(schemaVersion) {
		return WhiteboardResult{}, fmt.Errorf("session whiteboard schema version must be %d, %d or %d",
			model.SessionWhiteboardSchemaV1, model.SessionWhiteboardSchemaV2,
			model.SessionWhiteboardSchemaV3)
	}
	// The version the document is written in never goes backwards.
	//
	// A session whose board was written in version 3 keeps version 3 when
	// the mode is turned off: the statements in it are the learner's own
	// words, and a version 1 document written over it would drop them
	// without trace. A deployment that never turned the mode on has a
	// version 1 board and keeps writing version 1, exactly as it always
	// did.
	//
	// The behaviour of the engine is a separate thing from the format of
	// the document: the mode decides what is checked, and the format
	// decides what can be stored. Turning the mode off stops the checks;
	// it does not rewrite the document.
	written := schemaVersion
	if current.SchemaVersion == model.SessionWhiteboardSchemaV3 {
		written = model.SessionWhiteboardSchemaV3
	}
	result := WhiteboardResult{
		Board: model.SessionWhiteboardDocument{SchemaVersion: written},
	}
	anchors := make([]model.SessionWhiteboardAnchor, 0, 2)
	// The mode that does not record the assistant's products keeps the board it
	// has always written: the rules below belong to the mode that carries
	// origins, and the other one must stay what it was.
	if whiteboardCarriesOrigins(schemaVersion) {
		proposal = whiteboardReuseSupersededKeys(current, proposal, batch)
	}
	// The goal is always the learner's, so it may only cite the learner's
	// own messages however the rest of the proposal is made up.
	allowed := batch.allowed(model.SessionWhiteboardOriginUser)

	goal, goalChange, dropped, fabricated := applyWhiteboardGoal(current.Goal, proposal.Goal, allowed, batch)
	result.Board.Goal = goal
	result.Diagnostics.DroppedEntries += dropped
	result.Diagnostics.FabricatedEntries += fabricated
	if goalChange != nil {
		result.Changes = append(result.Changes, *goalChange)
	}

	// The messages the board already carries. They are the session's own
	// record of what was said, and a proposal that cites one of them names
	// something that really happened.
	carried := whiteboardBoardEvidenceIDs(current)

	constraints, constraintChanges, constraintDiag, constraintAnchors := applyWhiteboardGroup(
		current.Constraints, proposal.Constraints, batch, false, schemaVersion, proposal, carried, "constraints")
	result.Board.Constraints = constraints
	result.Changes = append(result.Changes, constraintChanges...)
	result.Diagnostics.add(constraintDiag)
	anchors = append(anchors, constraintAnchors...)

	plan, planChanges, planDiag, planAnchors := applyWhiteboardGroup(
		current.Plan, proposal.Plan, batch, true, schemaVersion, proposal, carried, "plan")
	result.Board.Plan = plan
	result.Changes = append(result.Changes, planChanges...)
	result.Diagnostics.add(planDiag)
	anchors = append(anchors, planAnchors...)

	openItems, openChanges, openDiag, openAnchors := applyWhiteboardGroup(
		current.OpenItems, proposal.OpenItems, batch, false, schemaVersion, proposal, carried, "open_items")
	result.Board.OpenItems = openItems
	result.Changes = append(result.Changes, openChanges...)
	result.Diagnostics.add(openDiag)
	anchors = append(anchors, openAnchors...)

	// The statements the refused entries left behind travel with the board.
	//
	// They are carried forward and cleared, not accumulated: a statement the
	// board has caught up with is dropped, and the newest ones are the ones
	// kept.
	//
	// They travel with the document, not with the mode. A board that
	// already carries them keeps carrying them after the mode is turned
	// off, because the learner's own words are not the mode's business to
	// throw away.
	if whiteboardCarriesStatements(written) {
		kept, resolved, expired := whiteboardMergeAnchors(current, result.Board, anchors)
		for _, prior := range current.Unverified {
			found := false
			for _, now := range kept {
				if sameAnchorIdentity(prior, now) {
					found = true
					break
				}
			}
			if !found {
				reason := "anchor_capacity"
				if whiteboardAnchorResolved(prior, result.Board) {
					reason = "anchor_resolved"
				}
				result.Diagnostics.removedAnchors = append(result.Diagnostics.removedAnchors, anchorRemoval{prior.MessageID, prior.Key, prior.Value, reason})
			}
		}
		result.Board.Unverified = kept
		result.Diagnostics.ResolvedUnconfirmedStatements += resolved
		result.Diagnostics.ExpiredUnconfirmedStatements += expired
	}

	reconcileAnchorRetention(&result.Diagnostics, result.Board, "anchor_capacity")

	// One subject does not stay in two places.
	//
	// The key is the model's name for an entry and the group is where the
	// model put it, and a model moves an entry from one group to another
	// when the entry changes shape. The subject does not move with it, so
	// two copies of one subject would show the reader two values for it and
	// leave the question of which one is current open.
	//
	// The mode that does not record the assistant's products keeps the board
	// it has always written: this rule belongs to the mode that carries
	// origins, and the other one must stay byte for byte what it was.
	if whiteboardCarriesOrigins(schemaVersion) {
		result.Board = whiteboardDropMovedCopies(result.Board, proposal)
	}

	// The board travels with every turn, so its size is a fixed cost that
	// has to stay bounded. Entries are dropped from the oldest end and every
	// drop is recorded, so a value that leaves the board can still be found
	// in the segment that recorded it.
	reconcileEntryRetention(&result.Diagnostics, result.Board, "moved_or_replaced")
	anchorsBeforeTrim := append([]model.SessionWhiteboardAnchor(nil), result.Board.Unverified...)
	droppedForTokens, expiredStatements := trimWhiteboardToTokenLimit(&result.Board)
	for _, prior := range anchorsBeforeTrim {
		found := false
		for _, now := range result.Board.Unverified {
			if sameAnchorIdentity(prior, now) {
				found = true
				break
			}
		}
		if !found {
			result.Diagnostics.removedAnchors = append(result.Diagnostics.removedAnchors, anchorRemoval{prior.MessageID, prior.Key, prior.Value, "render_budget"})
		}
	}
	markRemovedEntries(&result.Diagnostics, droppedForTokens, "render_budget")
	reconcileAnchorRetention(&result.Diagnostics, result.Board, "render_budget")
	for _, change := range droppedForTokens {
		result.Changes = append(result.Changes, change)
	}
	result.Diagnostics.DroppedEntries += len(droppedForTokens)
	result.Diagnostics.ExpiredUnconfirmedStatements += expiredStatements

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
	reconcileEntryRetention(&result.Diagnostics, result.Board, "not_retained")
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
	d.UnprovenAcceptances += other.UnprovenAcceptances
	d.UnknownOrigins += other.UnknownOrigins
	d.MislabeledOrigins += other.MislabeledOrigins
	d.AssistantProposals += other.AssistantProposals
	d.VerifiedClaims += other.VerifiedClaims
	d.RejectedClaims += other.RejectedClaims
	d.UnsettledClaims += other.UnsettledClaims
	d.UnbackedChanges += other.UnbackedChanges
	d.UnconfirmedStatements += other.UnconfirmedStatements
	d.ResolvedUnconfirmedStatements += other.ResolvedUnconfirmedStatements
	d.ExpiredUnconfirmedStatements += other.ExpiredUnconfirmedStatements

	d.removedEntries = append(d.removedEntries, other.removedEntries...)
	d.removedAnchors = append(d.removedAnchors, other.removedAnchors...)
	d.Entries = append(d.Entries, other.Entries...)
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

// whiteboardOriginForVersion is the origin an entry is written with.
//
// The field is written only when it says something other than the learner:
// the learner's own entry is the default, and an entry that does not say so
// is the document the mode that does not record the assistant's products
// always wrote.
//
// A version 1 board could only hold what the learner said, so a proposal the
// learner took on is written there as the learner's own. That is not a
// guess: "assistant_accepted" says the learner took it on, and the version
// simply cannot say who proposed it.
func whiteboardOriginForVersion(origin string, schemaVersion int) string {
	if origin != model.SessionWhiteboardOriginAssistantAccepted {
		return ""
	}
	if schemaVersion == model.SessionWhiteboardSchemaV1 {
		return ""
	}
	return model.SessionWhiteboardOriginAssistantAccepted
}

// whiteboardEntryForVersion is an entry as the board's version may hold it.
//
// An entry the proposal did not mention is carried over from the old board,
// and it may name a proposal the learner took on. The same rule as for a
// proposed entry applies to it.
func whiteboardEntryForVersion(entry model.SessionWhiteboardEntry, schemaVersion int) model.SessionWhiteboardEntry {
	entry.Origin = whiteboardOriginForVersion(entry.EntryOrigin(), schemaVersion)
	return entry
}

// applyWhiteboardGroup merges one category of entries.
//
// doneFirst marks the plan: a finished step is dropped before an unfinished
// one, because a board that still lists finished work is a board that has
// not been read.
func applyWhiteboardGroup(current []model.SessionWhiteboardEntry, proposed []model.SessionWhiteboardEntry,
	batch WhiteboardBatch, doneFirst bool, schemaVersion int, proposal WhiteboardProposal,
	carried map[uint]struct{}, groupName string) ([]model.SessionWhiteboardEntry, []model.SessionSummarySegmentChange, WhiteboardDiagnostics, []model.SessionWhiteboardAnchor) {
	var diagnostics WhiteboardDiagnostics
	anchors := make([]model.SessionWhiteboardAnchor, 0, 2)
	keyCounts := whiteboardProposalKeyCounts(proposal)
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
		merged = append(merged, whiteboardEntryForVersion(entry, schemaVersion))
		if key := strings.TrimSpace(entry.Key); key != "" {
			positions[key] = len(merged) - 1
		}
	}

	seenKeys := make(map[string]struct{}, len(proposed))
	changes := make([]model.SessionSummarySegmentChange, 0, len(proposed))
	for _, entry := range proposed {
		// What became of this entry is recorded in three layers, and the
		// record is written before the loop moves on: the entries that were
		// refused are exactly the ones a reader of a run has to find.
		outcome := WhiteboardEntryOutcome{
			Key:           strings.TrimSpace(entry.Key),
			candidateText: strings.TrimSpace(entry.Text), candidateValue: strings.TrimSpace(entry.Value),
			ClaimVerdict: whiteboardClaimVerdictLabel(whiteboardClaimAbsent),
		}
		record := func() {
			diagnostics.Entries = append(diagnostics.Entries, outcome)
		}
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			diagnostics.DroppedEntries++
			outcome.Outcome = whiteboardEntryOutcomeDropped
			outcome.OutcomeReason = whiteboardEntryReasonEmptyText
			record()
			continue
		}
		if len([]rune(text)) > model.SessionWhiteboardTextMaxRunes {
			text = string([]rune(text)[:model.SessionWhiteboardTextMaxRunes])
			diagnostics.TruncatedTexts++
		}
		origin, originKnown := whiteboardOriginForProposal(entry.Origin, schemaVersion)
		if !originKnown {
			// The model named an origin the program does not know. It is an
			// origin it cannot check, and an entry it cannot check does not
			// go on the board.
			//
			// A label the program cannot read is not a reason to lose the
			// learner's words, though. The anchor keeps them findable under
			// a message of this batch that really carries the value, and it
			// says nothing about the value being the current one.
			diagnostics.UnknownOrigins++
			anchors, outcome.AnchorMessageID, outcome.AnchorReason =
				whiteboardAppendAnchor(anchors, entry, WhiteboardLearnerClaim{}, false, batch,
					model.SessionWhiteboardAnchorNoClaim)
			outcome.Outcome = whiteboardEntryOutcomeRefused
			outcome.OutcomeReason = whiteboardEntryReasonOriginUnknown
			record()
			continue
		}
		key := strings.TrimSpace(entry.Key)
		previous, isExisting := currentByKey[key]
		if key != "" {
			if _, seen := seenKeys[key]; seen {
				// The same key twice in one proposal: the later one wins,
				// because it is the one the generator put last.
				outcome.Outcome = whiteboardEntryOutcomeDropped
				outcome.OutcomeReason = whiteboardEntryReasonDuplicateKey
				record()
				continue
			}
			seenKeys[key] = struct{}{}
		}

		// The learner's own words decide whose entry this is.
		//
		// A model that labels the learner's own new value as the assistant's
		// - which happens when the assistant only confirmed what the learner
		// said - must not lose the learner's current value over the label.
		// The learner's words are the evidence, and every part of them is
		// checked: the quote has to be in the message, the value has to be
		// the entry's value, and the subject and the attribute have to be
		// what the entry is about.
		//
		// The entry does not have to replace a value the board already
		// holds. A board that is empty - because the last batch failed to
		// generate - is exactly where the learner's own words are the only
		// thing there is.
		learnerMessageID := uint(0)
		learnerStated := false
		claimVerdict := whiteboardClaimAbsent
		claimReason := ""
		var claim WhiteboardLearnerClaim
		hasClaim := false
		if whiteboardCarriesOrigins(schemaVersion) {
			claim, hasClaim = whiteboardClaimFor(proposal.Claims, key, keyCounts)
			if hasClaim {
				claimVerdict, claimReason = whiteboardVerifyClaim(claim, entry, batch)
				switch claimVerdict {
				case whiteboardClaimSettled:
					learnerMessageID = claim.MessageID
					learnerStated = true
					diagnostics.VerifiedClaims++
				case whiteboardClaimUnsettled:
					diagnostics.UnsettledClaims++
				case whiteboardClaimRejected:
					diagnostics.RejectedClaims++
				}
				outcome.ClaimVerdict = whiteboardClaimVerdictLabel(claimVerdict)
				outcome.ClaimReason = claimReason
				outcome.ClaimMessageID = claim.MessageID
			} else if whiteboardClaimsNameKey(proposal.Claims, key) {
				// The claim could not be placed under this entry: two entries
				// carry the key, or two claims name it. A claim that cannot
				// be placed is not evidence, and the entry is then judged
				// without it - which is a refusal of its own and is recorded
				// as one.
				outcome.ClaimVerdict = whiteboardClaimVerdictLabelAmbiguous
				outcome.ClaimReason = model.SessionWhiteboardAnchorKeyAmbiguous
				claimReason = model.SessionWhiteboardAnchorKeyAmbiguous
			}
		}
		if claimVerdict == whiteboardClaimRejected {
			// The model offered evidence for this entry and the evidence
			// does not hold.
			//
			// A rejected claim is not the same as no claim. No claim says
			// nothing, and the entry is then judged by the label the model
			// wrote on it. A rejected claim says the model's own account of
			// the learner's words cannot be confirmed, and an entry whose
			// account of the learner's words is wrong is not one to write.
			//
			// So the entry is refused here, before any of the paths that
			// would put it on the board: it does not become the learner's
			// own, it does not go on a board that is empty, and it does not
			// replace a value the board already holds. The rest of the
			// proposal is unaffected.
			//
			// The learner's own words do not go with it. They are kept as
			// an unconfirmed statement, so that a reader can find them and
			// ask for the message they came from. The value stays refused:
			// it never becomes the current state.
			anchors, outcome.AnchorMessageID, outcome.AnchorReason =
				whiteboardAppendAnchor(anchors, entry, claim, hasClaim, batch, claimReason)
			diagnostics.DroppedEntries++
			outcome.Outcome = whiteboardEntryOutcomeRefused
			outcome.OutcomeReason = claimReason
			record()
			continue
		}
		if claimVerdict == whiteboardClaimUnsettled && origin == model.SessionWhiteboardOriginUser {
			// The entry says it is the learner's own, and the learner's own
			// words say the value was not settled: proposed, asked about,
			// ruled out, or not sure. A value nobody settled is not a
			// current fact, so the entry does not go on the board.
			//
			// The words stay findable all the same: a learner who asked
			// what would happen if the value were 41 pages is worth being
			// able to read back.
			anchors, outcome.AnchorMessageID, outcome.AnchorReason =
				whiteboardAppendAnchor(anchors, entry, claim, hasClaim, batch, claimReason)
			diagnostics.DroppedEntries++
			outcome.Outcome = whiteboardEntryOutcomeRefused
			outcome.OutcomeReason = claimReason
			record()
			continue
		}
		if whiteboardCarriesOrigins(schemaVersion) &&
			origin == model.SessionWhiteboardOriginUser && isExisting &&
			previous.Value != entry.Value && claimVerdict != whiteboardClaimSettled {
			// The entry replaces a value the board already holds, and the
			// learner's own words are not behind it.
			//
			// The board holds what is true now, and a value that is
			// replaced by a value nobody said is a value that is gone. The
			// program cannot read a decision out of a sentence, so the
			// model has to show the words it read and the program checks
			// them. An entry that cannot show them does not replace what
			// is there.
			anchors, outcome.AnchorMessageID, outcome.AnchorReason =
				whiteboardAppendAnchor(anchors, entry, claim, hasClaim, batch,
					model.SessionWhiteboardAnchorUnbackedChange)
			diagnostics.UnbackedChanges++
			diagnostics.DroppedEntries++
			outcome.Outcome = whiteboardEntryOutcomeRefused
			outcome.OutcomeReason = model.SessionWhiteboardAnchorUnbackedChange
			record()
			continue
		}

		if learnerStated && origin != model.SessionWhiteboardOriginUser {
			// The label was wrong; the words were the learner's. The entry
			// is the learner's own, and it replaces the value it changed.
			origin = model.SessionWhiteboardOriginUser
			diagnostics.MislabeledOrigins++
		}

		// The evidence of an entry the learner's own words settled is
		// collected the same way, whichever label the model wrote on it.
		//
		// A model that calls the learner's own new value the assistant's
		// and a model that calls it the learner's own have said the same
		// thing once the claim holds, and the engine has to treat them
		// the same way. The claim is the evidence; the label is only the
		// model's guess about which side of the conversation the entry
		// belongs to, and a guess that the evidence contradicts is not a
		// reason to lose the value.
		var ids []uint
		var ok bool
		if learnerStated {
			ids, ok = filterWhiteboardVerifiedEvidence(entry.SourceMessageIDs, learnerMessageID,
				previous.SourceMessageIDs, batch, carried)
		} else {
			ids, ok = filterWhiteboardEvidence(entry.SourceMessageIDs, previous.SourceMessageIDs, batch.allowed(origin))
		}
		if !ok {
			// The entry cites a message this batch never contained. It is
			// dropped on its own: the rest of the proposal is still usable.
			//
			// The learner's own words do not go with it. They are kept as
			// an unconfirmed statement, so that a reader can find them and
			// ask for the message they came from. The value stays refused:
			// it never becomes the current state.
			//
			// The anchor is navigation and nothing else. It says the words
			// are in a message of this batch, and it does not say the value
			// is the learner's current one.
			diagnostics.FabricatedEntries++
			anchors, outcome.AnchorMessageID, outcome.AnchorReason =
				whiteboardAppendAnchor(anchors, entry, claim, hasClaim, batch,
					model.SessionWhiteboardAnchorCitationInvalid)
			outcome.Outcome = whiteboardEntryOutcomeRefused
			outcome.OutcomeReason = model.SessionWhiteboardAnchorCitationInvalid
			record()
			continue
		}
		if origin != model.SessionWhiteboardOriginUser {
			if !whiteboardCarriesOrigins(schemaVersion) {
				// The board that cannot name an assistant's origin. The
				// acceptance is checked the way it always was, and an
				// entry that cannot show it does not go on the board.
				if origin == model.SessionWhiteboardOriginAssistantAccepted &&
					!acceptedAssistantProposal(ids, batch) {
					diagnostics.UnprovenAcceptances++
					outcome.Outcome = whiteboardEntryOutcomeRefused
					outcome.OutcomeReason = whiteboardEntryReasonUnprovenAcceptance
					record()
					continue
				}
			} else {
				// The citations decide what the entry is, not the label the
				// model wrote on it. A label the citations do not support
				// is the model's mistake, and the entry is not thrown away
				// with it.
				//
				// The one thing the citations cannot decide is whether the
				// learner settled the value, and that is what a claim is
				// for. An entry whose own evidence says the learner did
				// settle it is the learner's own; one whose evidence says
				// they did not is not raised by this branch, however the
				// model labelled it.
				switch {
				case claimVerdict == whiteboardClaimUnsettled:
					// The learner's own words are the evidence and they
					// do not settle the value. The entry does not become
					// the learner's current fact, and it does not become a
					// product the learner took on either: what the label
					// says the learner accepted is not what the words
					// behind the entry say.
					//
					// The words stay findable: the learner asked about the
					// value and is worth being able to read back.
					anchors, outcome.AnchorMessageID, outcome.AnchorReason =
						whiteboardAppendAnchor(anchors, entry, claim, hasClaim, batch, claimReason)
					diagnostics.DroppedEntries++
					outcome.Outcome = whiteboardEntryOutcomeRefused
					outcome.OutcomeReason = claimReason
					record()
					continue
				case !citesAssistantMessage(ids, batch) &&
					origin == model.SessionWhiteboardOriginAssistantAccepted:
					// The entry says the learner took a proposal on, and
					// what it cites is the learner's own words about it.
					// The label was wrong; the words were the learner's,
					// and they stay on the board as the learner's.
					origin = model.SessionWhiteboardOriginUser
					diagnostics.MislabeledOrigins++
				case !citesAssistantMessage(ids, batch):
					// The entry claims to be the assistant's and cites
					// nothing of the assistant's. It cannot be placed,
					// and a proposal that cannot be placed is not a
					// product.
					//
					// The value here is the assistant's own, so there is
					// no anchor: the assistant's product cannot be the
					// learner's evidence.
					diagnostics.AssistantProposals++
					outcome.Outcome = whiteboardEntryOutcomeRefused
					outcome.OutcomeReason = whiteboardEntryReasonAssistantProduct
					record()
					continue
				case origin == model.SessionWhiteboardOriginAssistantAccepted &&
					!acceptedAssistantProposal(ids, batch):
					// The entry says the learner took a proposal on, and
					// the citations do not show it. A proposal nobody
					// accepted is not a current fact, so it stays in the
					// summary segment as one of the assistant's products
					// and never reaches the board.
					diagnostics.UnprovenAcceptances++
					outcome.Outcome = whiteboardEntryOutcomeRefused
					outcome.OutcomeReason = whiteboardEntryReasonUnprovenAcceptance
					record()
					continue
				case origin == model.SessionWhiteboardOriginAssistant:
					// The assistant's own proposal, which the learner did
					// not take on. It is the same place as the one above:
					// the segment carries it as a product, the board does
					// not.
					diagnostics.AssistantProposals++
					outcome.Outcome = whiteboardEntryOutcomeRefused
					outcome.OutcomeReason = whiteboardEntryReasonAssistantProduct
					record()
					continue
				}
			}
		}
		outcome.candidateText = text
		updated := entry
		updated.Text = text
		updated.Key = key
		updated.Origin = whiteboardOriginForVersion(origin, schemaVersion)
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
		snapshot := updated
		snapshot.SourceMessageIDs = append([]uint(nil), updated.SourceMessageIDs...)
		outcome.candidateEntry = &snapshot
		outcome.candidateGroup = groupName
		if key != "" {
			if position, exists := positions[key]; exists {
				merged[position] = updated
				diagnostics.AppliedEntries++
				if change := whiteboardEntryChange(key, previous, updated, isExisting); change != nil {
					changes = append(changes, *change)
				}
				outcome.Outcome = whiteboardEntryOutcomeApplied
				record()
				continue
			}
			positions[key] = len(merged)
		}
		merged = append(merged, updated)
		diagnostics.AppliedEntries++
		if change := whiteboardEntryChange(key, previous, updated, isExisting); change != nil {
			changes = append(changes, *change)
		}
		outcome.Outcome = whiteboardEntryOutcomeApplied
		record()
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
	markRemovedEntries(&diagnostics, droppedChanges, "item_capacity")
	for i := range diagnostics.removedEntries {
		diagnostics.removedEntries[i].group = groupName
	}
	diagnostics.DroppedDonePlan += droppedDone
	diagnostics.DroppedEntries += len(droppedChanges)
	diagnostics.UnconfirmedStatements = len(anchors)
	return trimmed, append(changes, droppedChanges...), diagnostics, anchors
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

// trimWhiteboardToTokenLimit drops entries until the board fits what the
// window sends.
//
// The board is sent on every turn, so its size is a fixed cost. Dropping is
// better than refusing the whole update: a board with fewer entries is still
// a board, and a board that refused updates is not. Every dropped entry is
// recorded as a change, so the value that left the board is still answerable
// from the segment that recorded it.
//
// The measure is the rendered block, not the stored body. The stored body is
// JSON, and on a real board it runs to about a fifth more than the block the
// model reads; measuring it dropped entries the window would have carried and
// still let the block overrun the budget it was supposed to fit.
func trimWhiteboardToTokenLimit(board *model.SessionWhiteboardDocument) ([]model.SessionSummarySegmentChange, int) {
	changes := make([]model.SessionSummarySegmentChange, 0)
	expired := 0
	for RenderedWhiteboardTokens(*board) > model.SessionWhiteboardTokenLimit {
		if len(board.Unverified) > 0 {
			// The words of the oldest statement go first, and the statement
			// itself goes only if that was not enough.
			//
			// The words are the expensive part and the message id is not:
			// a statement without its words is still a pointer to the
			// learner's own message, and a reader can still read it back. A
			// board entry is the current state, so it is given up last.
			oldest := board.Unverified[len(board.Unverified)-1]
			if oldest.Complete {
				board.Unverified[len(board.Unverified)-1] = whiteboardAnchorNavigation(oldest)
				continue
			}
			board.Unverified = board.Unverified[:len(board.Unverified)-1]
			expired++
			continue
		}
		victim, found := nextBoardEntryToDrop(*board)
		if !found {
			break
		}
		changes = append(changes, model.SessionSummarySegmentChange{
			Key:           strings.TrimSpace(victim.entry.Key),
			OldValue:      victim.entry.Value,
			OldMessageIDs: victim.entry.SourceMessageIDs,
			Removed:       true,
		})
		switch victim.group {
		case whiteboardGroupConstraints:
			board.Constraints = removeWhiteboardEntry(board.Constraints, victim.entry)
		case whiteboardGroupPlan:
			board.Plan = removeWhiteboardEntry(board.Plan, victim.entry)
		case whiteboardGroupOpenItems:
			board.OpenItems = removeWhiteboardEntry(board.OpenItems, victim.entry)
		}
	}
	return changes, expired
}

// trimWhiteboardToOutputLimit drops entries until the stored body fits the
// output budget.
//
// It exists as a floor under the delta rewrite: the delta keeps the update
// small, but a board that was already at the limit when the batch started
// still has to be stored inside the budget, and a batch that cannot be stored
// is a batch that wrote nothing at all. The order is the same one the window
// budget uses, so a board that is trimmed twice gives up the same entries in
// the same order.
func trimWhiteboardToOutputLimit(board *model.SessionWhiteboardDocument, limit int) []model.SessionSummarySegmentChange {
	changes := make([]model.SessionSummarySegmentChange, 0)
	for limit > 0 && storedWhiteboardTokens(*board) > limit {
		victim, found := nextBoardEntryToDrop(*board)
		if !found {
			break
		}
		changes = append(changes, model.SessionSummarySegmentChange{
			Key:           strings.TrimSpace(victim.entry.Key),
			OldValue:      victim.entry.Value,
			OldMessageIDs: victim.entry.SourceMessageIDs,
			Removed:       true,
		})
		switch victim.group {
		case whiteboardGroupConstraints:
			board.Constraints = removeWhiteboardEntry(board.Constraints, victim.entry)
		case whiteboardGroupPlan:
			board.Plan = removeWhiteboardEntry(board.Plan, victim.entry)
		case whiteboardGroupOpenItems:
			board.OpenItems = removeWhiteboardEntry(board.OpenItems, victim.entry)
		}
	}
	return changes
}

// storedWhiteboardTokens is what the board costs as the body it is stored in.
func storedWhiteboardTokens(board model.SessionWhiteboardDocument) int {
	content, err := model.MarshalSessionWhiteboardDocument(board)
	if err != nil {
		return 0
	}
	return rag.EstimateTokens(content)
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

// nextBoardEntryToDrop returns the entry that should lose its place first.
//
// The entries are not worth the same. A finished step is worth nothing: it
// already happened, and the segment that recorded it still says so. A value
// and a plan step that nobody has mentioned since are worth little. A
// constraint is worth the most, because it is what the learner asked the
// answers to obey, and an answer that breaks one is wrong in a way the
// learner cannot see.
//
// Inside a rank the oldest loses first: the newest is the one the current
// conversation is about.
func nextBoardEntryToDrop(board model.SessionWhiteboardDocument) (whiteboardEntryLocation, bool) {
	// The rank a board entry gives up its place in.
	const (
		rankDonePlan = iota
		rankValuesAndPlan
		rankConstraints
	)
	rankOf := func(group string, entry model.SessionWhiteboardEntry) int {
		switch {
		case group == whiteboardGroupPlan && entry.State == "done":
			return rankDonePlan
		case group == whiteboardGroupConstraints:
			return rankConstraints
		default:
			return rankValuesAndPlan
		}
	}
	best := whiteboardEntryLocation{}
	bestRank := 0
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
			rank := rankOf(group.name, entry)
			switch {
			case !found || rank < bestRank:
			case rank == bestRank && entry.UpdatedAtMessageID < best.entry.UpdatedAtMessageID:
			default:
				continue
			}
			best = whiteboardEntryLocation{group: group.name, entry: entry}
			bestRank = rank
			found = true
		}
	}
	return best, found
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

// whiteboardReuseSupersededKeys gives a proposal's entry the key the board
// already gave the entry it replaces.
//
// The key is the model's name for an entry, and a model renames it when the
// entry changes shape: the same subject arrives under a new name and in a new
// group. The board's key is the engine's to keep. An entry that replaces
// another keeps the key the board already gave that subject, so the value
// that was replaced is recorded under a key the reader has seen, and the mark
// that says "this was changed later" has a value to point at.
//
// Only an entry the learner's own words justify takes a key from another
// entry, and the words have to be evidence: the learner's message has to say
// the value, about that subject, and the entry has to be about the same
// subject the old entry is about. An entry the program cannot place leaves
// the board as it was, because a value that takes over the wrong entry's key
// loses a fact the learner still has.
func whiteboardReuseSupersededKeys(current model.SessionWhiteboardDocument,
	proposal WhiteboardProposal, batch WhiteboardBatch) WhiteboardProposal {
	groups := map[string][]model.SessionWhiteboardEntry{
		"constraints": proposal.Constraints,
		"plan":        proposal.Plan,
		"open_items":  proposal.OpenItems,
	}
	incoming := map[string][]model.SessionWhiteboardEntry{
		"constraints": nil, "plan": nil, "open_items": nil,
	}
	keyCounts := whiteboardProposalKeyCounts(proposal)
	for name, entries := range groups {
		kept := make([]model.SessionWhiteboardEntry, 0, len(entries))
		for _, entry := range entries {
			key := strings.TrimSpace(entry.Key)
			learnerMessageID, own := uint(0), false
			var claim *WhiteboardLearnerClaim
			if key != "" {
				if found, has := whiteboardClaimFor(proposal.Claims, key, keyCounts); has {
					if verdict, _ := whiteboardVerifyClaim(found, entry, batch); verdict == whiteboardClaimSettled {
						learnerMessageID = found.MessageID
						own = true
						claim = &found
					}
				}
			}
			superseded, group, found := "", "", false
			if own && !whiteboardHasKey(current, key) {
				// A key the board already holds is replaced where it is.
				// The key is only taken from another entry when the model
				// renamed the entry, which is the one case the board
				// cannot place on its own.
				superseded, group, found = whiteboardSupersedingEntry(current, entry, key, learnerMessageID, claim)
			}
			if !found {
				kept = append(kept, entry)
				continue
			}
			entry.Key = superseded
			// The claim follows the entry. The evidence was written for
			// the entry the model proposed, and the entry is the same one
			// under the key the board already gave it.
			whiteboardMoveClaim(proposal.Claims, key, superseded)
			incoming[group] = append(incoming[group], entry)
		}
		groups[name] = kept
	}
	for name, entries := range groups {
		// An entry that took another entry's key goes first: a proposal
		// that still carries the old value under that key must not have the
		// old value win.
		groups[name] = append(incoming[name], entries...)
	}
	proposal.Constraints = groups["constraints"]
	proposal.Plan = groups["plan"]
	proposal.OpenItems = groups["open_items"]
	return proposal
}

// whiteboardSupersedingEntry finds the entry the board holds about the
// subject the learner's message changed.
//
// The two entries are about one subject when the new entry names the old
// one, when they measure the same thing, and when the old entry is the
// earlier version: it was written before the message that changed it.
//
// The claim adds a second way to say "the same subject": the subject the
// learner named is the subject the old entry is about. One of the two is
// enough, and both are checked against what the entries say rather than
// against the words they happen to share.
//
// There has to be exactly one such entry. Two of them means the engine
// cannot tell which one the new entry replaces, and a value that takes over
// the wrong entry's key loses a fact the learner still has. So a case the
// engine cannot place is left alone: the new entry keeps its own key, and
// the board keeps what it had.
func whiteboardSupersedingEntry(current model.SessionWhiteboardDocument,
	entry model.SessionWhiteboardEntry, key string, learnerMessageID uint,
	claim *WhiteboardLearnerClaim) (string, string, bool) {
	type candidate struct {
		group string
		key   string
		entry model.SessionWhiteboardEntry
	}
	candidates := make([]candidate, 0, len(current.Constraints)+len(current.Plan)+len(current.OpenItems))
	for _, item := range current.Constraints {
		candidates = append(candidates, candidate{"constraints", strings.TrimSpace(item.Key), item})
	}
	for _, item := range current.Plan {
		candidates = append(candidates, candidate{"plan", strings.TrimSpace(item.Key), item})
	}
	for _, item := range current.OpenItems {
		candidates = append(candidates, candidate{"open_items", strings.TrimSpace(item.Key), item})
	}
	found := candidate{}
	matches := 0
	for _, item := range candidates {
		if item.key == "" || item.key == key {
			continue
		}
		if !whiteboardEntryIsOlder(item.entry, learnerMessageID) {
			continue
		}
		if !whiteboardEntrySupersedes(item.entry, entry) &&
			!whiteboardClaimNamesSubject(claim, item.entry) {
			continue
		}
		matches++
		if matches > 1 {
			// Two entries the new entry could be replacing. The engine
			// does not know which one, so it replaces neither.
			return "", "", false
		}
		found = item
	}
	if matches == 0 {
		return "", "", false
	}
	return found.key, found.group, true
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

// whiteboardClaimNamesSubject reports whether the claim's subject is the
// subject the old entry is about.
//
// The subject is the learner's own word for the thing, and the old entry has
// to be about the same thing: a claim about the reading plan and an entry
// about the venue are two subjects however many words they share. One
// subject carries many attributes - the pages of a book and the minutes a
// book takes are two - and the measurement is what tells those apart, so the
// two entries have to measure the same thing as well.
func whiteboardClaimNamesSubject(claim *WhiteboardLearnerClaim, old model.SessionWhiteboardEntry) bool {
	if claim == nil {
		return false
	}
	subject := whiteboardPlainText(claim.Subject)
	if subject == "" {
		return false
	}
	_, claimUnit, hasNumber := parseWhiteboardValue(claim.Value)
	if hasNumber && !whiteboardUnitsCompatible(whiteboardEntryUnit(old), claimUnit) {
		return false
	}
	return strings.Contains(whiteboardEntryDescription(old), subject)
}

// whiteboardEntrySupersedes reports whether a new entry is the same entry as
// an old one under another key.
//
// Three things have to hold, and all three of them are about what the two
// entries say rather than about the words they share:
//
//   - they measure the same thing: two entries that measure different units
//     are two entries, whatever words they share;
//   - the new entry names the old one: an entry that replaces another says
//     what it replaces.
//
// "Reading pages" and "reading time" share "reading" and measure different
// units, and they are two entries. A model that renames one of them has to
// keep both.
//
// What the two entries share in words is not evidence of anything. "Reading"
// is in both "reading pages" and "reading time", and the word says only that
// both are about reading.
func whiteboardEntrySupersedes(old model.SessionWhiteboardEntry, updated model.SessionWhiteboardEntry) bool {
	if !whiteboardUnitsCompatible(whiteboardEntryUnit(old), whiteboardEntryUnit(updated)) {
		return false
	}
	names := whiteboardEntryNames(old)
	if len(names) == 0 {
		return false
	}
	plain := whiteboardPlainText(updated.Text + " " + updated.Value)
	for _, name := range names {
		if name == "" {
			continue
		}
		if strings.Contains(plain, name) {
			return true
		}
	}
	return false
}

// whiteboardRemoveAt drops the entry at the index from the group.
func whiteboardRemoveAt(entries []model.SessionWhiteboardEntry, index int) []model.SessionWhiteboardEntry {
	kept := make([]model.SessionWhiteboardEntry, 0, len(entries))
	kept = append(kept, entries[:index]...)
	kept = append(kept, entries[index+1:]...)
	return kept
}

// whiteboardHasKey reports whether the board holds an entry under the key.
func whiteboardHasKey(document model.SessionWhiteboardDocument, key string) bool {
	if key == "" {
		return false
	}
	for _, group := range [][]model.SessionWhiteboardEntry{
		document.Constraints, document.Plan, document.OpenItems,
	} {
		for _, entry := range group {
			if strings.TrimSpace(entry.Key) == key {
				return true
			}
		}
	}
	return false
}

// whiteboardEntryIsOlder reports whether the entry was written before the
// learner's message.
func whiteboardEntryIsOlder(entry model.SessionWhiteboardEntry, messageID uint) bool {
	for _, id := range entry.SourceMessageIDs {
		if id >= messageID {
			return false
		}
	}
	return len(entry.SourceMessageIDs) > 0
}

// whiteboardDropMovedCopies removes the copy of an entry the board still
// holds in a group the proposal did not speak about.
//
// An entry the proposal spoke about in one group and did not speak about in
// another was moved: the copy in the other group is the same subject, and
// two copies of one subject are two values for one thing. The copy is only
// removed when the proposal's own entry landed, so a proposal that named the
// entry and then failed does not leave the board without it.
func whiteboardDropMovedCopies(board model.SessionWhiteboardDocument,
	proposal WhiteboardProposal) model.SessionWhiteboardDocument {
	groups := map[string][]model.SessionWhiteboardEntry{
		"constraints": board.Constraints,
		"plan":        board.Plan,
		"open_items":  board.OpenItems,
	}
	mentioned := map[string]map[string]bool{}
	for name, entries := range map[string][]model.SessionWhiteboardEntry{
		"constraints": proposal.Constraints,
		"plan":        proposal.Plan,
		"open_items":  proposal.OpenItems,
	} {
		for _, entry := range entries {
			key := strings.TrimSpace(entry.Key)
			if key == "" {
				continue
			}
			if mentioned[key] == nil {
				mentioned[key] = map[string]bool{}
			}
			mentioned[key][name] = true
		}
	}
	for key, names := range mentioned {
		if len(names) != 1 {
			continue
		}
		keep := ""
		for name := range names {
			keep = name
		}
		if !whiteboardGroupHasKey(groups[keep], key) {
			// The proposal's own entry did not land. The board keeps the
			// copy it had, because a value that is on the board is better
			// than no value at all.
			continue
		}
		for name, entries := range groups {
			if name == keep {
				continue
			}
			groups[name] = whiteboardRemoveKey(entries, key)
		}
	}
	board.Constraints = groups["constraints"]
	board.Plan = groups["plan"]
	board.OpenItems = groups["open_items"]
	return board
}

// whiteboardGroupHasKey reports whether the group holds an entry under the key.
func whiteboardGroupHasKey(entries []model.SessionWhiteboardEntry, key string) bool {
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) == key {
			return true
		}
	}
	return false
}

// whiteboardRemoveKey drops the entry under the key from the group.
func whiteboardRemoveKey(entries []model.SessionWhiteboardEntry, key string) []model.SessionWhiteboardEntry {
	kept := make([]model.SessionWhiteboardEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) == key {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// filterWhiteboardVerifiedEvidence keeps the citations of an entry the
// learner's own words settled.
//
// It is the one evidence path of a verified entry, and it does not care which
// label the model wrote on the entry. An entry the model called the
// assistant's and an entry it called the learner's own have said the same
// thing once the claim holds, and the engine has to treat them the same way:
// the claim is the evidence, and the label is the model's guess about which
// side of the conversation the entry belongs to.
//
// The verified message comes first and always stays. It is the message the
// learner's own words are in, and it is the proof - whether or not the model
// cited it, because a model that wrote the wrong label on the entry may well
// have cited the wrong message with it.
//
// The other citations the model added are sorted into four kinds.
//
// The learner's own messages of this batch are kept: they are the learner's
// own words about the same value.
//
// The citations the board already carries for this very entry are kept: they
// are the board's own sources for the entry the proposal replaces.
//
// The assistant's messages are tolerated but not kept. A model that cites the
// assistant's confirmation next to the learner's own words has not made the
// entry wrong - the assistant confirming something is not the learner saying
// it, so the confirmation is not evidence of the learner's fact and it is left
// out. It is left out rather than being a reason to lose the entry: an extra
// citation of something that really happened is not a fabrication.
//
// The messages the board carries elsewhere are tolerated and left out as well.
// A model that cites the message the old value came from is saying where the
// value changed, not what the value is, and the board has already vouched for
// that message.
//
// The citations that name nothing the session can account for are refused,
// and the entry with them. An id that does not exist, that belongs to another
// session, or that belongs to a batch this one cannot see is not a citation
// the engine can drop and carry on: a proposal that cites messages nobody can
// check is a proposal the engine has to refuse. Filtering them out instead
// would let a proposal that invented its sources keep its entry, and the
// check would be a formality.
func filterWhiteboardVerifiedEvidence(cited []uint, stated uint, previous []uint,
	batch WhiteboardBatch, carried map[uint]struct{}) ([]uint, bool) {
	if stated == 0 {
		return nil, false
	}
	if _, ok := batch.learnerMessage(stated); !ok {
		// The verified message is not the learner's own message of this
		// batch, so there is no proof to build the evidence on.
		return nil, false
	}
	ids := make([]uint, 0, len(cited)+1)
	seen := map[uint]struct{}{stated: {}}
	ids = append(ids, stated)
	previousSet := make(map[uint]struct{}, len(previous))
	for _, id := range previous {
		previousSet[id] = struct{}{}
	}
	for _, id := range cited {
		if id == 0 {
			return nil, false
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if _, inBatch := batch.learnerMessage(id); inBatch {
			ids = append(ids, id)
			continue
		}
		if _, own := previousSet[id]; own {
			ids = append(ids, id)
			continue
		}
		if batch.KnownMessages[id] == WhiteboardMessageRoleAssistant {
			// The assistant's confirmation of the learner's own words. It
			// is a real message, and it does not make the entry wrong, but
			// it is not the learner's evidence - so it is left out of the
			// evidence rather than being a reason to lose the entry.
			continue
		}
		if _, known := carried[id]; known {
			// A message the board already carries. It is a real message of
			// this session, so the citation is not a fabrication, but it is
			// not evidence of this value either.
			continue
		}
		return nil, false
	}
	if len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// whiteboardBoardEvidenceIDs are the messages the board already cites.
//
// It is the set of messages the session can account for: every id an entry
// or an unconfirmed statement carries. A citation of one of them names
// something that really happened, so it is not a fabrication - and it is not
// evidence of a new value either, because the board already knows what it
// was for.
func whiteboardBoardEvidenceIDs(document model.SessionWhiteboardDocument) map[uint]struct{} {
	ids := make(map[uint]struct{})
	for _, id := range document.EvidenceMessageIDs() {
		if id != 0 {
			ids[id] = struct{}{}
		}
	}
	return ids
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

// These diagnostics describe the generated board, not a committed row or a rendered window.
func markRemovedEntries(d *WhiteboardDiagnostics, changes []model.SessionSummarySegmentChange, reason string) {
	for _, c := range changes {
		if c.Removed {
			d.removedEntries = append(d.removedEntries, whiteboardRemoval{key: c.Key, value: strings.TrimSpace(c.OldValue), reason: reason})
		}
	}
	for i := range d.Entries {
		e := &d.Entries[i]
		if e.Outcome != whiteboardEntryOutcomeApplied {
			continue
		}
		for _, c := range changes {
			if c.Removed && c.Key == e.Key && strings.TrimSpace(c.OldValue) == e.candidateValue {
				e.EntryRetention = "removed"
				e.EntryRetentionReason = reason
			}
		}
	}
}
func reconcileEntryRetention(d *WhiteboardDiagnostics, board model.SessionWhiteboardDocument, missing string) {
	for i := range d.Entries {
		e := &d.Entries[i]
		if e.Outcome != whiteboardEntryOutcomeApplied {
			e.EntryRetention = "not_applied"
			continue
		}
		if e.EntryRetention == "removed" {
			continue
		}
		found := false
		for gi, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
			for _, item := range group {
				if e.candidateGroup == []string{"constraints", "plan", "open_items"}[gi] && e.candidateEntry != nil && reflect.DeepEqual(item, *e.candidateEntry) {
					found = true
				}
			}
		}
		if found {
			e.EntryRetention = "retained"
			e.EntryRetentionReason = ""
		} else {
			e.EntryRetention = "removed"
			e.EntryRetentionReason = missing
		}
	}
}
func reconcileAnchorRetention(d *WhiteboardDiagnostics, board model.SessionWhiteboardDocument, missing string) {
	for i := range d.Entries {
		e := &d.Entries[i]
		if e.AnchorMessageID == 0 {
			e.AnchorRetention = "not_created"
			continue
		}
		if e.AnchorRetention == "removed" {
			continue
		}
		found := false
		for _, a := range board.Unverified {
			if a.MessageID == e.AnchorMessageID && strings.TrimSpace(a.Value) == e.candidateValue {
				found = true
				break
			}
		}
		if found {
			e.AnchorRetention = "retained"
			e.AnchorRetentionReason = ""
		} else {
			e.AnchorRetention = "removed"
			e.AnchorRetentionReason = missing
		}
	}
}

func sameAnchorIdentity(a, b model.SessionWhiteboardAnchor) bool {
	return a.MessageID == b.MessageID && a.Key == b.Key && a.Value == b.Value && a.Reason == b.Reason
}

// reconcilePriorCall marks each earlier version as soon as it leaves the board.
// A later re-addition must not resurrect that earlier observation.
func reconcilePriorCall(prior *WhiteboardDiagnostics, later WhiteboardDiagnostics, board model.SessionWhiteboardDocument) {
	for i := range prior.Entries {
		e := &prior.Entries[i]
		if e.Outcome == whiteboardEntryOutcomeApplied && e.EntryRetention != "removed" {
			replaced := false
			for _, next := range later.Entries {
				if e.Key != "" && next.Outcome == whiteboardEntryOutcomeApplied && next.Key == e.Key && next.candidateGroup == e.candidateGroup {
					replaced = true
					break
				}
			}
			found := false
			if e.candidateEntry != nil {
				for gi, g := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
					for _, item := range g {
						if e.candidateGroup == []string{"constraints", "plan", "open_items"}[gi] && reflect.DeepEqual(item, *e.candidateEntry) {
							found = true
						}
					}
				}
			}
			if !found || replaced {
				e.EntryRetention = "removed"
				e.EntryRetentionReason = "updated_or_replaced"
				for _, event := range later.removedEntries {
					if event.key == e.Key && event.value == e.candidateValue && (event.group == "" || event.group == e.candidateGroup) {
						e.EntryRetentionReason = event.reason
						break
					}
				}
			}
		}
		if e.AnchorMessageID != 0 && e.AnchorRetention != "removed" {
			found := false
			for _, a := range board.Unverified {
				if a.MessageID == e.AnchorMessageID && a.Key == e.Key && strings.TrimSpace(a.Value) == e.candidateValue && a.Reason == e.AnchorReason {
					found = true
					break
				}
			}
			if !found {
				e.AnchorRetention = "removed"
				e.AnchorRetentionReason = "updated_or_replaced"
				for _, event := range later.removedAnchors {
					if event.messageID == e.AnchorMessageID && event.key == e.Key && strings.TrimSpace(event.value) == e.candidateValue {
						e.AnchorRetentionReason = event.reason
						break
					}
				}
			}
		}
	}
}
