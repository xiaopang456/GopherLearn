package agent

import (
	"reflect"
	"sort"
	"testing"

	"GopherAI/common/rag"
)

// Both citation forms the product sees must be read: the message_ids= form
// the conversation prompt teaches, and the bracketed [number] form. The
// previous version of this test pinned the mismatch (the taught form was
// invisible); this is the fix regression.

func citationIDsOf(answer string) []int {
	ids := map[int]struct{}{}
	for id := range extractMessageIDCitations(answer) {
		ids[id] = struct{}{}
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([]int, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Ints(out)
	return out
}

func TestTheValidatorReadsTheMessageIDsFormRealAnswersUse(t *testing.T) {
	// The exact shapes seen in the three real-answer batches: single id,
	// multiple ids, parentheses and square brackets, Chinese separators.
	for _, test := range []struct {
		answer string
		want   []int
	}{
		{answer: "…具体怎么排（message_ids=3926）；整份清单…", want: []int{3926}},
		{answer: "…用命令行发请求 [message_ids=3341]，最…", want: []int{3341}},
		{answer: "…你当时没表示同意（message_ids=3931,3967,3991）。", want: []int{3931, 3967, 3991}},
		{answer: "…是「芦青」——之前定下的固定标题（message_ids=[63]）。", want: []int{63}},
		{answer: "…（message_ids=405、406）和（message_ids=407）", want: []int{405, 406, 407}},
		{answer: "答案里没有引用。", want: nil},
	} {
		if got := citationIDsOf(test.answer); !reflect.DeepEqual(got, test.want) {
			t.Errorf("message ids of %q = %v, want %v", test.answer, got, test.want)
		}
	}
}

func TestTheValidatorStillReadsNumericBrackets(t *testing.T) {
	wanted := map[int]struct{}{}
	for _, match := range finalCitationPattern.FindAllStringSubmatch("答案是动作、提问、验收 [405]", -1) {
		wanted[405] = struct{}{}
		_ = match
	}
	if len(wanted) != 1 {
		t.Fatalf("the numeric form did not produce a citation id: %v", wanted)
	}
}

// A conversation message id is authorised by the CONVERSATION grant set, so a
// document citation carrying the same number cannot stand in for it.
func TestADocumentCitationCannotAuthoriseAMessageID(t *testing.T) {
	run := RunContext{UserID: 7, SessionID: "current-session", LockedSessionID: "p2c-04",
		Question: "上次你给我排的那三道题里，第二道练的是什么？"}
	set := NewHistoryGrantSet(run, nil)
	// 405 is authorised for another conversation, not the locked one.
	set.Extend(run, []HistoryMessageGrant{{
		MessageID: 405, SourceKind: HistorySourceSessionSearch, SourceSessionID: "p2c-02",
	}})
	state := &runState{input: run, grantSet: set, lockedSessionID: "p2c-04",
		sources: []rag.Source{{Citation: 405}}}
	// As a document citation it resolves…
	if !state.citationsResolve(map[int]struct{}{405: {}}) {
		t.Fatal("the document citation did not resolve to the shown source")
	}
	// …but as a conversation citation it must not.
	if state.citationsResolveMessageIDs(map[int]struct{}{405: {}}) {
		t.Fatal("a document citation authorised a conversation message id")
	}
	if state.messageCitationAuthorised(405) {
		t.Fatal("a grant from another conversation authorised the id under the lock")
	}
	if state.citationsResolveConversation(map[int]struct{}{405: {}}) {
		t.Fatal("a grant from another conversation passed the numeric citation check under the lock")
	}
	// The same id granted for the locked conversation does authorise it.
	locked := NewHistoryGrantSet(run, nil)
	locked.Extend(run, []HistoryMessageGrant{{
		MessageID: 405, SourceKind: HistorySourceSessionSearch, SourceSessionID: "p2c-04",
	}})
	authorisedState := &runState{input: run, grantSet: locked, lockedSessionID: "p2c-04"}
	if !authorisedState.citationsResolveMessageIDs(map[int]struct{}{405: {}}) {
		t.Fatal("the locked conversation's own grant did not authorise its message id")
	}
}
