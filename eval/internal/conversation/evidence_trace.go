package conversation

// The retrieval trace of an answer, and where the injected memory came from.
//
// Two facts a comparison needs and that the answer text cannot carry: what
// the model asked the retrieval for and what it got back, and which
// conversations the memory that reached the model was written from.
//
// Both are read out of the product's own records. The trace comes from the
// tool rounds the product persisted - the product's own summary of a search
// names the status, the reason and the records it handed over - and the
// provenance comes from the rows themselves, because the injected block names
// the messages it was written from and the messages say which conversation
// they were in.
//
// What the harness CANNOT see is the server-side pre-retrieval: the decision
// the product made before the model was asked, with its intent and its
// abstention. That decision is not persisted, so it is registered as a
// wiring gap rather than guessed at.

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"GopherAI/model"
	"gorm.io/gorm"
)

// SearchTrace is one memory search the answer made and what it brought back.
//
// The arguments are the intent the model expressed and the result is the
// tool's own verdict, so a reader can tell "the search ran and found
// nothing" from "the search never ran" - which are different findings about
// the retrieval, and different findings about the arm.
type SearchTrace struct {
	// Tool is the tool the model called.
	Tool string `json:"tool"`
	// Query is what the model asked for.
	Query string `json:"query,omitempty"`
	// Kinds is what it restricted the search to.
	Kinds []string `json:"kinds,omitempty"`
	// TopK is how many cards it asked for.
	TopK int `json:"top_k,omitempty"`
	// Status is the tool's own verdict: "ok", "no_evidence", "unavailable",
	// "invalid_request" or "call_limit".
	Status string `json:"status,omitempty"`
	// Reason is why: "none", "duplicate_search", "call_limit_reached",
	// "query_invalid" and the rest.
	Reason string `json:"reason,omitempty"`
	// Cards is how many evidence cards the search brought back.
	Cards int `json:"cards,omitempty"`
	// Refs are the records the search handed to the model.
	Refs []string `json:"refs,omitempty"`
	// Truncated says the product's own record of the call was cut off, so the
	// numbers above may be short.
	Truncated bool `json:"truncated,omitempty"`
}

// The product's own summary of a memory search.
//
// It is one line, written by the product, of the form
//
//	memory_search status=ok reason=none cards=2 refs=E1,E2 semantic=0 episodic=2 tokens=128 degraded=false truncated=false
//
// The harness parses the product's line rather than the tool's raw JSON: the
// line is what the product chose to keep, and it is the same line in every
// deployment.
var (
	searchSummaryPattern   = regexp.MustCompile(`status=(\S+)\s+reason=(\S+)\s+cards=(\d+)\s+refs=(\S*)`)
	searchTruncatedPattern = regexp.MustCompile(`truncated=(true|false)`)
)

// searchToolNames are the tools whose record is a retrieval.
//
// The cross-session search returns its own JSON and the memory search returns
// the summary line; the harness reads both. The list is explicit because
// other tools answer with JSON as well, and a read-back that happened to
// carry a "status" field is not a retrieval.
var searchToolNames = map[string]bool{
	"memory_search":          true,
	"search_past_sessions":   true,
	"search_session_history": true,
}

// searchArguments reads the intent out of a search call's arguments.
//
// The arguments are the model's own words, so they are read as JSON rather
// than guessed at: a call whose arguments cannot be read still produces a
// trace row, with the intent empty and the verdict present.
type searchArguments struct {
	Query string   `json:"query"`
	Kinds []string `json:"kinds"`
	TopK  int      `json:"top_k"`
}

// searchSessionOutput is the cross-session search's own answer shape.
//
// It is the tool's JSON: the status and the reason are its verdict, and the
// matches are the messages it brought back. The harness reads it because the
// product's summary line is only written for the memory search, and a
// harness that read only that line would report a cross-session search that
// really ran as a search that never happened.
type searchSessionOutput struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Matches []struct {
		MessageID uint `json:"message_id"`
	} `json:"matches"`
	Truncated bool `json:"truncated"`
}

// ReadSearchTrace reads the retrieval trace out of the answer's tool rounds.
//
// Only the searches are traced. A browse or a read-back is not a retrieval:
// it reads something the retrieval already chose, and mixing the two would
// make "the retrieval found nothing" unreadable.
func ReadSearchTrace(steps []model.AgentStep) []SearchTrace {
	var out []SearchTrace
	for _, step := range steps {
		if step.Type != model.AgentStepTool || !searchToolNames[step.ToolName] {
			continue
		}
		trace, ok := readSearchTrace(step)
		if !ok {
			continue
		}
		out = append(out, trace)
	}
	return out
}

// readSearchTrace reads one search call's record.
func readSearchTrace(step model.AgentStep) (SearchTrace, bool) {
	trace := SearchTrace{Tool: step.ToolName}
	var arguments searchArguments
	if err := json.Unmarshal([]byte(step.ArgumentsSummary), &arguments); err == nil {
		trace.Query = arguments.Query
		trace.Kinds = arguments.Kinds
		trace.TopK = arguments.TopK
	}
	// The memory search's own line.
	if match := searchSummaryPattern.FindStringSubmatch(step.ResultSummary); len(match) == 5 {
		trace.Status = match[1]
		trace.Reason = match[2]
		if cards, err := strconv.Atoi(match[3]); err == nil {
			trace.Cards = cards
		}
		for _, ref := range strings.Split(match[4], ",") {
			if ref = strings.TrimSpace(ref); ref != "" {
				trace.Refs = append(trace.Refs, ref)
			}
		}
		trace.Truncated = summaryTruncated(step.ResultSummary)
		return trace, true
	}
	// The cross-session search's own JSON.
	var payload searchSessionOutput
	if err := json.Unmarshal([]byte(step.ResultSummary), &payload); err != nil || payload.Status == "" {
		return trace, false
	}
	trace.Status = payload.Status
	trace.Reason = payload.Reason
	trace.Cards = len(payload.Matches)
	for _, match := range payload.Matches {
		trace.Refs = append(trace.Refs, strconv.FormatUint(uint64(match.MessageID), 10))
	}
	trace.Truncated = payload.Truncated
	return trace, true
}

// summaryTruncated says whether the product's own record said the call was
// cut short.
//
// The line carries the flag, so it is read from there rather than guessed
// from the length of the record: the product truncates by runes and the
// harness would be counting bytes.
func summaryTruncated(summary string) bool {
	if match := searchTruncatedPattern.FindStringSubmatch(summary); len(match) == 2 {
		return match[1] == "true"
	}
	return len(summary) >= toolRecordLimit
}

// SessionsForMessageIDs reads the conversations the given messages are in.
//
// The injected block cites the messages it was written from, and the messages
// say which conversation they were in. So the block's own citations are
// enough to say where the memory came from, without the harness guessing
// which session a memory row belonged to.
//
// The conversations are the ones this learner owns: a message in somebody
// else's conversation is not a source this learner was granted, so it is not
// a source at all.
func SessionsForMessageIDs(ctx context.Context, db *gorm.DB, userID int64, ids []uint) ([]string, error) {
	if db == nil || userID <= 0 || len(ids) == 0 {
		return nil, nil
	}
	var rows []model.Message
	if err := db.WithContext(ctx).
		Where("id IN ?", ids).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	candidates := map[string]struct{}{}
	for _, row := range rows {
		if strings.TrimSpace(row.SessionID) == "" {
			continue
		}
		candidates[row.SessionID] = struct{}{}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	ids2 := make([]string, 0, len(candidates))
	for session := range candidates {
		ids2 = append(ids2, session)
	}
	var owned []model.Session
	if err := db.WithContext(ctx).
		Where("id IN ? AND user_id = ?", ids2, userID).
		Find(&owned).Error; err != nil {
		return nil, err
	}
	var sessions []string
	for _, row := range owned {
		if _, exists := candidates[row.ID]; !exists {
			continue
		}
		sessions = append(sessions, row.ID)
	}
	sort.Strings(sessions)
	return sessions, nil
}

// The states of the injected memory's authorisation.
//
// They are three because the findings are three: a memory the learner was
// shown without a grant, a memory that came from their own conversations, and
// nothing to check at all. A boolean would say the first and the third the
// same way, and a reader would take "the learner was shown something they
// were not granted" out of a run that simply injected nothing.
const (
	// InjectedSourceWithin says every cited message belongs to one of the
	// arm's own source conversations.
	InjectedSourceWithin = "within"
	// InjectedSourceOutside says at least one cited message belongs to a
	// conversation the arm was never given.
	InjectedSourceOutside = "outside"
	// InjectedSourceUnknown says there was nothing to check: no memory was
	// injected, or the block cited no message the harness could resolve.
	InjectedSourceUnknown = "unknown"
)

// WithinSessions says whether every one of the given conversations is one of
// the allowed ones.
//
// It is the authorisation check of the injected memory: a block that cited a
// message from a conversation the arm was never given would be a memory the
// learner was shown without a grant, and the comparison has to be able to
// say that it did not happen.
func WithinSessions(found, allowed []string) bool {
	if len(found) == 0 {
		return true
	}
	set := map[string]struct{}{}
	for _, session := range allowed {
		set[session] = struct{}{}
	}
	for _, session := range found {
		if _, exists := set[session]; !exists {
			return false
		}
	}
	return true
}
