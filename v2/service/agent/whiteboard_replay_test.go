package agent

// The offline replay of a recorded run.
//
// A run leaves its raw answers behind: what the model wrote for every batch,
// what the engine made of every entry, and the board before and after. The
// replay takes those answers and puts them through the engine as it is now,
// so that the change can be measured against the proposals a model really
// made instead of against proposals written for a test.
//
// The replay calls no model. It proves what the engine does with a recorded
// answer; it does not prove what a model would answer today, and it does not
// prove anything about the answers a run gives.
//
// The directory comes from GOPHER_WHITEBOARD_REPLAY_DIR and the run's own
// layout is expected:
//
//   <dir>/fixtures/<scenario>.history.json
//   <dir>/out/<scenario>/rep< n>/evidence/batch-< n>-attempt-< n>.json
//   <dir>/out/<scenario>/rep< n>/evidence/batch-< n>-attempt-< n>-call-< n>.json
//
// Without the directory the replay is skipped: the evidence is a run's own
// record, and it is not part of the repository.
//
// What the replay can rebuild and what it cannot:
//
//   - The board before the batch is the one the run recorded, and it is
//     rebuilt from the snapshot the run left. A snapshot does not carry the
//     text of an entry, so the text is taken from the raw answers of the same
//     run where the key matches, and the entries whose text could not be
//     found are counted and reported.
//   - The messages of the batch are rebuilt from the conversation the run
//     was given. Their roles are the conversation's own.
//   - The messages the previous segment cited are added to what the engine
//     knows about the batch, exactly as the generator adds them.
//
// What cannot be rebuilt is the generator's own splitting: a batch that was
// answered by more than one call is replayed from its first answer, so a
// proposal the later calls made is not part of the replay.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"GopherAI/model"
)

// replayTurn is one exchange of the conversation a run was given.
type replayTurn struct {
	User      *string `json:"user"`
	Assistant *string `json:"assistant"`
}

// replayMessage is one message of that conversation.
type replayMessage struct {
	id      uint
	isUser  bool
	content string
}

// replayBoardEntry is one entry of a board snapshot.
type replayBoardEntry struct {
	Key              string `json:"key"`
	Text             string `json:"text"`
	Value            string `json:"value"`
	Origin           string `json:"origin"`
	SourceMessageIDs []uint `json:"source_message_ids"`
}

// replayBoardAnchor is one unconfirmed statement of a board snapshot.
type replayBoardAnchor struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	MessageID uint   `json:"message_id"`
	Reason    string `json:"reason"`
}

// replayBoard is a board as a run recorded it.
type replayBoard struct {
	SchemaVersion int                 `json:"schema_version"`
	Constraints   []replayBoardEntry  `json:"constraints"`
	Plan          []replayBoardEntry  `json:"plan"`
	OpenItems     []replayBoardEntry  `json:"open_items"`
	Anchors       []replayBoardAnchor `json:"anchors"`
}

// replayEntry is what a run recorded about one proposed entry.
type replayEntry struct {
	Key             string `json:"key"`
	CallIndex       int    `json:"call_index"`
	ClaimVerdict    string `json:"claim_verdict"`
	ClaimReason     string `json:"claim_reason"`
	Outcome         string `json:"outcome"`
	OutcomeReason   string `json:"outcome_reason"`
	AnchorMessageID uint   `json:"anchor_message_id"`
	AnchorReason    string `json:"anchor_reason"`
}

// replayCall is one model call a run made.
type replayCall struct {
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	File  string `json:"file"`
}

// replayEvidence is one attempt of one batch.
type replayEvidence struct {
	Batch            int           `json:"batch"`
	Attempt          int           `json:"attempt"`
	Outcome          string        `json:"outcome"`
	StartMessageID   uint          `json:"start_message_id"`
	EndMessageID     uint          `json:"end_message_id"`
	SegmentBody      string        `json:"segment_body"`
	Calls            []replayCall  `json:"calls"`
	Entries          []replayEntry `json:"entries"`
	WhiteboardBefore *replayBoard  `json:"whiteboard_before"`
	WhiteboardAfter  *replayBoard  `json:"whiteboard_after"`
}

// replayCallFile is the answer of one model call.
type replayCallFile struct {
	RawOutput string `json:"raw_output"`
}

// replayHistory is the conversation of a scenario, read from its turns.
func replayHistory(t *testing.T, path string) map[uint]replayMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the conversation: %v", err)
	}
	var turns []replayTurn
	if err := json.Unmarshal(data, &turns); err != nil {
		t.Fatalf("read the conversation: %v", err)
	}
	messages := make(map[uint]replayMessage, len(turns)*2)
	id := uint(1)
	for _, turn := range turns {
		if turn.User != nil {
			messages[id] = replayMessage{id: id, isUser: true, content: *turn.User}
			id++
		}
		if turn.Assistant != nil {
			messages[id] = replayMessage{id: id, isUser: false, content: *turn.Assistant}
			id++
		}
	}
	return messages
}

// replayMessagesInRange returns the messages a batch covered, oldest first.
func replayMessagesInRange(messages map[uint]replayMessage, first, last uint) []model.Message {
	ids := make([]uint, 0, len(messages))
	for id := range messages {
		if id < first || id > last {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	out := make([]model.Message, 0, len(ids))
	for _, id := range ids {
		message := messages[id]
		out = append(out, model.Message{ID: message.id, IsUser: message.isUser, Content: message.content})
	}
	return out
}

// replayDocument rebuilds the board a run started the batch from.
//
// The snapshot carries the value and the citations of an entry but not its
// text, so the text is taken from the run's own answers where the key
// matches. The entries whose text could not be found are counted: a board
// that is partly rebuilt is a board the replay has to be careful with.
func replayDocument(board *replayBoard, texts map[string]string) (model.SessionWhiteboardDocument, int) {
	document := model.SessionWhiteboardDocument{SchemaVersion: board.SchemaVersion}
	missing := 0
	convert := func(entries []replayBoardEntry) []model.SessionWhiteboardEntry {
		out := make([]model.SessionWhiteboardEntry, 0, len(entries))
		for _, entry := range entries {
			text := entry.Text
			if text == "" {
				text = texts[entry.Key]
			}
			if text == "" {
				missing++
			}
			out = append(out, model.SessionWhiteboardEntry{
				Key: entry.Key, Text: text, Value: entry.Value, Origin: entry.Origin,
				SourceMessageIDs: entry.SourceMessageIDs,
			})
		}
		return out
	}
	document.Constraints = convert(board.Constraints)
	document.Plan = convert(board.Plan)
	document.OpenItems = convert(board.OpenItems)
	for _, anchor := range board.Anchors {
		document.Unverified = append(document.Unverified, model.SessionWhiteboardAnchor{
			Key: anchor.Key, Value: anchor.Value, MessageID: anchor.MessageID, Reason: anchor.Reason,
		})
	}
	return document, missing
}

// replayProposalEntries are the entries a proposal brought.
func replayProposalEntries(proposal WhiteboardProposal) []model.SessionWhiteboardEntry {
	entries := make([]model.SessionWhiteboardEntry, 0,
		len(proposal.Constraints)+len(proposal.Plan)+len(proposal.OpenItems))
	entries = append(entries, proposal.Constraints...)
	entries = append(entries, proposal.Plan...)
	entries = append(entries, proposal.OpenItems...)
	return entries
}

// replayTexts collects the text of every entry the run's answers proposed.
//
// The text is what a reader sees, and it is what says which subject an entry
// is about. A board that is rebuilt without it is a board whose entries say
// nothing about themselves.
func replayTexts(raws []string) map[string]string {
	texts := make(map[string]string)
	for _, raw := range raws {
		document, err := decodeSegmentGenerationDocument(raw)
		if err != nil {
			continue
		}
		for _, entry := range replayProposalEntries(whiteboardProposalFor(document)) {
			key := strings.TrimSpace(entry.Key)
			if key == "" || entry.Text == "" {
				continue
			}
			texts[key] = entry.Text
		}
	}
	return texts
}

// replayClaimReasons are the reasons only the claim layer could produce.
//
// They are the old value's fingerprint: an entry the claim refused because
// its subject, attribute, stance or update did not match the learner's words
// is an entry the claim layer refused, and the claim layer is gone.
var replayClaimReasons = map[string]bool{
	model.SessionWhiteboardAnchorStanceUnknown:       true,
	model.SessionWhiteboardAnchorUpdateUnknown:       true,
	model.SessionWhiteboardAnchorMessageNotLearner:   true,
	model.SessionWhiteboardAnchorQuoteMissing:        true,
	model.SessionWhiteboardAnchorQuoteNotInMessage:   true,
	model.SessionWhiteboardAnchorValueNotInQuote:     true,
	model.SessionWhiteboardAnchorValueUnitMismatch:   true,
	model.SessionWhiteboardAnchorValueMismatch:       true,
	model.SessionWhiteboardAnchorSubjectNotInQuote:   true,
	model.SessionWhiteboardAnchorSubjectNotInEntry:   true,
	model.SessionWhiteboardAnchorAttributeNotInQuote: true,
	model.SessionWhiteboardAnchorAttributeNotInEntry: true,
	model.SessionWhiteboardAnchorScopeConflict:       true,
	model.SessionWhiteboardAnchorUnsettled:           true,
	model.SessionWhiteboardAnchorUnbackedChange:      true,
	model.SessionWhiteboardAnchorKeyAmbiguous:        true,
}

// replayPolicy is the write path the replay uses.
//
// The recorded run says which one it used: a run whose board carries origins
// is a run that recorded the assistant's products, and a run whose board does
// not is a run that did not. GOPHER_WHITEBOARD_REPLAY_POLICY says so
// explicitly when the run's own board is not enough to tell.
func replayPolicy(version int) WhiteboardPolicy {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOPHER_WHITEBOARD_REPLAY_POLICY"))) {
	case "off", "learner", "learner-only":
		return learnerOnlyPolicy(version)
	}
	return assistantProductsPolicy(version)
}

// replayContractReasons are the reasons the new contract has for an entry
// the old one kept.
//
// An entry the model labelled as the assistant's product is one of them: the
// old contract let a claim overrule that label, and the new one has no claim
// to overrule it with.
var replayContractReasons = map[string]bool{
	whiteboardEntryOutcomeDropped + "/" + whiteboardEntryReasonDuplicateKey:            true,
	whiteboardEntryOutcomeRefused + "/" + whiteboardEntryReasonAssistantProduct:        true,
	whiteboardEntryOutcomeRefused + "/" + whiteboardEntryReasonUnprovenAcceptance:      true,
	whiteboardEntryOutcomeRefused + "/" + model.SessionWhiteboardAnchorCitationInvalid: true,
	whiteboardEntryOutcomeRefused + "/" + model.SessionWhiteboardAnchorSameNumber:      true,
}

// TestTheRecordedProposalsReplayThroughTheEngineAsItIsNow replays the
// proposals a recorded run really made.
//
// The test says two things. The claim reasons are gone: no entry is refused
// for one of them any more. And the entries the claim layer refused are the
// ones the learner's own facts were lost on, so the replay counts how many of
// them the engine now keeps.
func TestTheRecordedProposalsReplayThroughTheEngineAsItIsNow(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("GOPHER_WHITEBOARD_REPLAY_DIR"))
	if dir == "" {
		t.Skip("the replay needs the evidence of a recorded run: set GOPHER_WHITEBOARD_REPLAY_DIR")
	}
	scenarios, err := filepath.Glob(filepath.Join(dir, "out", "*"))
	if err != nil || len(scenarios) == 0 {
		t.Fatalf("no recorded run under %s: %v", dir, err)
	}
	kept, refusedBefore, refusedAfter, appliedBefore := 0, 0, 0, 0
	missingTexts := 0
	regressed := 0
	afterByReason := map[string]int{}
	regressedByReason := map[string]int{}
	for _, scenarioDir := range scenarios {
		scenario := filepath.Base(scenarioDir)
		messages := replayHistory(t, filepath.Join(dir, "fixtures", scenario+".history.json"))
		reps, err := filepath.Glob(filepath.Join(scenarioDir, "rep*"))
		if err != nil {
			t.Fatalf("the runs of %s: %v", scenario, err)
		}
		for _, rep := range reps {
			evidenceFiles, err := filepath.Glob(filepath.Join(rep, "evidence", "batch-*-attempt-*.json"))
			if err != nil {
				t.Fatalf("the evidence of %s: %v", rep, err)
			}
			var raws []string
			var records []replayEvidence
			for _, path := range evidenceFiles {
				if strings.Contains(filepath.Base(path), "-call-") {
					continue
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read the evidence: %v", err)
				}
				var record replayEvidence
				if err := json.Unmarshal(data, &record); err != nil {
					t.Fatalf("read the evidence: %v", err)
				}
				records = append(records, record)
				for _, call := range record.Calls {
					if call.Kind != "summary" {
						continue
					}
					answer, err := os.ReadFile(filepath.Join(filepath.Dir(path), call.File))
					if err != nil {
						continue
					}
					var file replayCallFile
					if err := json.Unmarshal(answer, &file); err != nil {
						continue
					}
					raws = append(raws, file.RawOutput)
				}
			}
			texts := replayTexts(raws)
			previous := ""
			for _, record := range records {
				if record.WhiteboardBefore == nil {
					continue
				}
				document, missing := replayDocument(record.WhiteboardBefore, texts)
				missingTexts += missing
				input := SegmentGenerationInput{
					Messages:        replayMessagesInRange(messages, record.StartMessageID, record.EndMessageID),
					FirstMessageID:  record.StartMessageID,
					LastMessageID:   record.EndMessageID,
					Whiteboard:      "",
					PreviousSegment: previous,
				}
				batch := whiteboardBatchFor(input, true)
				var raw string
				for _, call := range record.Calls {
					if call.Kind != "summary" {
						continue
					}
					answer, err := os.ReadFile(filepath.Join(rep, "evidence", call.File))
					if err != nil {
						continue
					}
					var file replayCallFile
					if err := json.Unmarshal(answer, &file); err != nil {
						continue
					}
					raw = file.RawOutput
					break
				}
				if raw == "" {
					continue
				}
				proposal, err := decodeSegmentGenerationDocument(raw)
				if err != nil {
					continue
				}
				policy := replayPolicy(document.SchemaVersion)
				applied, err := ApplyWhiteboardProposal(document, whiteboardProposalFor(proposal), batch, policy)
				if err != nil {
					continue
				}
				before := map[string]replayEntry{}
				for _, entry := range record.Entries {
					before[entry.Key] = entry
				}
				for _, entry := range applied.Diagnostics.Entries {
					was, known := before[entry.Key]
					if !known {
						continue
					}
					if was.Outcome == whiteboardEntryOutcomeApplied {
						appliedBefore++
						if entry.Outcome != whiteboardEntryOutcomeApplied {
							regressed++
							regressedByReason[entry.Outcome+"/"+entry.OutcomeReason]++
						}
					}
					if was.Outcome == whiteboardEntryOutcomeRefused && replayClaimReasons[was.OutcomeReason] {
						refusedBefore++
						afterByReason[entry.Outcome+"/"+entry.OutcomeReason]++
					}
					if entry.Outcome == whiteboardEntryOutcomeRefused && replayClaimReasons[entry.OutcomeReason] {
						refusedAfter++
						t.Errorf("an entry is refused for a reason only the claim layer had: %+v", entry)
					}
					if was.Outcome != whiteboardEntryOutcomeApplied && entry.Outcome == whiteboardEntryOutcomeApplied {
						kept++
					}
				}
				previous = record.SegmentBody
			}
		}
	}
	reasons := make([]string, 0, len(afterByReason))
	for reason := range afterByReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		t.Logf("REPLAY claim_refused_now=%s %d", reason, afterByReason[reason])
	}
	// The entries the run kept and the engine does not are the ones the
	// model labelled as the assistant's products: the old contract let a
	// claim overrule that label, and the new one has no claim to overrule
	// it with. They stay in the summary segment, which is where a product
	// of the assistant's belongs.
	otherReasons := make([]string, 0, len(regressedByReason))
	for reason := range regressedByReason {
		otherReasons = append(otherReasons, reason)
	}
	sort.Strings(otherReasons)
	for _, reason := range otherReasons {
		t.Logf("REPLAY kept_before_now_not=%s %d", reason, regressedByReason[reason])
	}
	t.Logf("REPLAY applied_before=%d kept_now=%d regressed=%d refused_by_claim_before=%d refused_by_claim_after=%d texts_missing=%d",
		appliedBefore, kept, regressed, refusedBefore, refusedAfter, missingTexts)
	if refusedBefore == 0 {
		t.Skip("the recorded run had no entry the claim layer refused")
	}
	if kept == 0 {
		t.Fatalf("none of the %d entries the claim layer refused is kept now", refusedBefore)
	}
	for reason := range regressedByReason {
		if !replayContractReasons[reason] {
			t.Errorf("an entry the run kept is refused for a reason the new contract does not have: %s", reason)
		}
	}
}

// TestTheReplayNeedsItsEvidence says what the replay is and is not.
//
// The replay reads a run's own record. It is not a test of a model, and a
// deployment that has no recorded run has nothing to replay.
func TestTheReplayNeedsItsEvidence(t *testing.T) {
	dir := strings.TrimSpace(os.Getenv("GOPHER_WHITEBOARD_REPLAY_DIR"))
	if dir != "" {
		t.Skip("the replay's evidence is here: " + dir)
	}
	if _, err := filepath.Glob(filepath.Join(os.Getenv("TEMP"), "gopherlearn-no-such-replay")); err != nil {
		t.Fatalf("the replay directory: %v", err)
	}
	fmt.Fprintln(os.Stderr, "the replay is skipped without GOPHER_WHITEBOARD_REPLAY_DIR")
}
