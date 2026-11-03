package calls

import (
	"strings"
	"testing"
	"time"
)

func at(text string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestCallsAreAttributedByIdentityAndNotByNeighbouringRuns(t *testing.T) {
	runs := []Run{
		{Arm: "on", QID: "q1", Rep: 0, RunID: "run-1", SessionID: "s1", AnsweredAt: at("2026-11-02T11:00:10Z")},
		{Arm: "on", QID: "q2", Rep: 0, RunID: "run-2", SessionID: "s2", AnsweredAt: at("2026-11-02T11:00:20Z")},
	}
	calls := []Call{
		// The first run's own call, and the background call it caused. Both
		// carry the first run's identity, even though the second one happened
		// while the second run was already in flight.
		{At: at("2026-11-02T11:00:05Z"), Lane: "chat", RunID: "run-1", SessionID: "s1"},
		{At: at("2026-11-02T11:00:12Z"), Lane: "chat", RunID: "run-1", SessionID: "s1"},
		{At: at("2026-11-02T11:00:15Z"), Lane: "chat", RunID: "run-2", SessionID: "s2"},
	}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	first := result.Runs[0]
	if first.Answer.Calls != 1 || first.AfterAnswer.Calls != 1 {
		t.Fatalf("run 1: answer calls %d, after-answer calls %d; want 1 and 1", first.Answer.Calls, first.AfterAnswer.Calls)
	}
	second := result.Runs[1]
	if second.Answer.Calls != 1 || second.AfterAnswer.Calls != 0 {
		t.Fatalf("run 2: answer calls %d, after-answer calls %d; want 1 and 0", second.Answer.Calls, second.AfterAnswer.Calls)
	}
	if result.UnattributedCalls != 0 {
		t.Fatalf("%d calls were left unattributed; want 0", result.UnattributedCalls)
	}
}

func TestACallWithoutIdentityIsCountedApartAndNeverFoldedIntoARun(t *testing.T) {
	runs := []Run{{Arm: "on", QID: "q1", RunID: "run-1", SessionID: "s1", AnsweredAt: at("2026-11-02T11:00:10Z")}}
	calls := []Call{{At: at("2026-11-02T11:00:05Z"), Lane: "chat"}}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if result.Runs[0].Answer.Calls != 0 || result.Runs[0].AfterAnswer.Calls != 0 {
		t.Fatalf("a call without identity was put into a run: %+v", result.Runs[0])
	}
	if result.UnattributedCalls != 1 || result.UnknownIdentityCalls != 1 {
		t.Fatalf("unattributed %d (unknown %d); want 1 and 1", result.UnattributedCalls, result.UnknownIdentityCalls)
	}
}

func TestACallNamingAnUnknownRunIsNotGuessedIntoTheRunBeforeIt(t *testing.T) {
	runs := []Run{{Arm: "on", QID: "q1", RunID: "run-1", SessionID: "s1", AnsweredAt: at("2026-11-02T11:00:10Z")}}
	calls := []Call{{At: at("2026-11-02T11:00:05Z"), Lane: "chat", RunID: "run-that-never-existed"}}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if result.Runs[0].Answer.Calls != 0 {
		t.Fatalf("the call was put into the only run: %+v", result.Runs[0])
	}
	if result.UnattributedCalls != 1 {
		t.Fatalf("unattributed %d; want 1", result.UnattributedCalls)
	}
}

func TestASessionWithSeveralRunsDoesNotTakeAnUnnamedCall(t *testing.T) {
	runs := []Run{
		{Arm: "on", QID: "q1", RunID: "run-1", SessionID: "s1", AnsweredAt: at("2026-11-02T11:00:10Z")},
		{Arm: "on", QID: "q2", RunID: "run-2", SessionID: "s1", AnsweredAt: at("2026-11-02T11:00:20Z")},
	}
	calls := []Call{{At: at("2026-11-02T11:00:12Z"), Lane: "chat", SessionID: "s1"}}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if result.Runs[0].Answer.Calls != 0 || result.Runs[1].Answer.Calls != 0 {
		t.Fatalf("an ambiguous call was put into a run: %+v", result.Runs)
	}
	if result.AmbiguousCalls != 1 {
		t.Fatalf("ambiguous %d; want 1", result.AmbiguousCalls)
	}
}

func TestUsageThatWasNotReportedStaysUnknown(t *testing.T) {
	runs := []Run{{Arm: "on", QID: "q1", RunID: "run-1", AnsweredAt: at("2026-11-02T11:00:10Z")}}
	prompt, completion := int64(100), int64(20)
	calls := []Call{
		{At: at("2026-11-02T11:00:05Z"), Lane: "chat", RunID: "run-1", Usage: &Usage{Prompt: &prompt, Completion: &completion}},
		{At: at("2026-11-02T11:00:06Z"), Lane: "chat", RunID: "run-1"},
	}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	answer := result.Runs[0].Answer
	if answer.Prompt != 100 || answer.Completion != 20 {
		t.Fatalf("reported usage was not summed: %+v", answer)
	}
	if answer.UnmeteredCalls != 1 {
		t.Fatalf("unmetered calls %d; want 1", answer.UnmeteredCalls)
	}
}

func TestARunThatNeverAnsweredHasNoAfterAnswerSide(t *testing.T) {
	runs := []Run{{Arm: "on", QID: "q1", RunID: "run-1"}}
	calls := []Call{{At: at("2026-11-02T11:00:05Z"), Lane: "chat", RunID: "run-1"}}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if result.Runs[0].Answer.Calls != 1 || result.Runs[0].AfterAnswer.Calls != 0 {
		t.Fatalf("a run that never answered was split: %+v", result.Runs[0])
	}
}

func TestTheLedgerPhaseIsOnlyAClaim(t *testing.T) {
	runs := []Run{{Arm: "on", QID: "q1", RunID: "run-1", AnsweredAt: at("2026-11-02T11:00:10Z")}}
	calls := []Call{{At: at("2026-11-02T11:00:12Z"), Lane: "chat", RunID: "run-1", Phase: PhaseAnswer}}
	result, err := Attribute(runs, calls)
	if err != nil {
		t.Fatalf("attribute: %v", err)
	}
	if result.Runs[0].AfterAnswer.Calls != 1 {
		t.Fatalf("the claimed phase was taken instead of the run's answer boundary: %+v", result.Runs[0])
	}
	if result.Runs[0].ClaimedPhaseMismatch != 1 {
		t.Fatalf("the disagreement was not counted: %+v", result.Runs[0])
	}
}

func TestRunLedgerIsReadBackIntoRuns(t *testing.T) {
	ledger := strings.Join([]string{
		`{"at":"2026-11-02T11:00:00Z","phase":"run","arm":"on","qid":"q1","rep":0,"sessionId":"s1"}`,
		`{"at":"2026-11-02T11:00:10Z","phase":"result","arm":"on","qid":"q1","rep":0,"http":200,"statusCode":1000,"modelCalls":2,"toolCalls":1,"runId":"run-1","answeredAt":"2026-11-02T11:00:10Z"}`,
		`{"at":"2026-11-02T11:00:11Z","phase":"steps","arm":"on","qid":"q1","rep":0,"runId":"run-1"}`,
		`{"at":"2026-11-02T11:00:12Z","phase":"cleanup","arm":"on","qid":"q1","rep":0,"sessionId":"s1","http":200}`,
	}, "\n")
	runs, err := ReadRuns(strings.NewReader(ledger))
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs; want 1", len(runs))
	}
	run := runs[0]
	if run.RunID != "run-1" || run.SessionID != "s1" {
		t.Fatalf("identity: %+v", run)
	}
	if run.AnsweredAt != at("2026-11-02T11:00:10Z") {
		t.Fatalf("answer boundary: %s", run.AnsweredAt)
	}
	if run.ReportedModelCalls == nil || *run.ReportedModelCalls != 2 {
		t.Fatalf("reported calls: %+v", run.ReportedModelCalls)
	}
}

func TestARunWhoseResultIsMissingKeepsItsIdentity(t *testing.T) {
	ledger := strings.Join([]string{
		`{"at":"2026-11-02T11:00:00Z","phase":"run","arm":"on","qid":"q1","rep":0,"sessionId":"s1"}`,
		`{"at":"2026-11-02T11:00:11Z","phase":"steps","arm":"on","qid":"q1","rep":0,"runId":"run-1"}`,
	}, "\n")
	runs, err := ReadRuns(strings.NewReader(ledger))
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-1" {
		t.Fatalf("runs: %+v", runs)
	}
	if !runs[0].AnsweredAt.IsZero() {
		t.Fatalf("a run without a result got an answer boundary: %s", runs[0].AnsweredAt)
	}
}

func TestCallLedgerIsReadAndRefusedWhenItsShapeChanged(t *testing.T) {
	ledger := `{"schema":"call.v1","at":"2026-11-02T11:00:05Z","lane":"chat","model":"deepseek-flash","stream":true,"run":"run-1","session":"s1","phase":"answer","usage":{"prompt":1003,"completion":78,"total":1081,"cached":768,"reasoning":null}}` + "\n"
	list, err := ReadCalls(strings.NewReader(ledger))
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("%d calls; want 1", len(list))
	}
	call := list[0]
	if call.RunID != "run-1" || call.SessionID != "s1" || call.Lane != "chat" || !call.Stream {
		t.Fatalf("call: %+v", call)
	}
	if call.Usage == nil || call.Usage.Prompt == nil || *call.Usage.Prompt != 1003 {
		t.Fatalf("usage: %+v", call.Usage)
	}
	if call.Usage.Reasoning != nil {
		t.Fatalf("a usage field that was not reported became a number: %+v", call.Usage)
	}
	if _, err := ReadCalls(strings.NewReader(`{"schema":"call.v2","at":"2026-11-02T11:00:05Z"}` + "\n")); err == nil {
		t.Fatal("a call ledger with another schema was read")
	}
	if _, err := ReadCalls(strings.NewReader(`{"schema":"call.v1","at":"2026-11-02T11:00:05Z","surprise":1}` + "\n")); err == nil {
		t.Fatal("a call ledger with an unknown field was read")
	}
}

func TestCallLedgerRoundTrip(t *testing.T) {
	prompt := int64(7)
	list := []Call{{At: at("2026-11-02T11:00:05.123456Z"), Lane: "chat", Model: "m", RunID: "run-1", SessionID: "s1", Phase: PhaseAnswer, Usage: &Usage{Prompt: &prompt}}}
	var buffer strings.Builder
	if err := WriteCalls(&buffer, list); err != nil {
		t.Fatalf("write calls: %v", err)
	}
	again, err := ReadCalls(strings.NewReader(buffer.String()))
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	if len(again) != 1 || again[0].RunID != "run-1" || again[0].Usage == nil || *again[0].Usage.Prompt != 7 {
		t.Fatalf("round trip: %+v", again)
	}
	if again[0].At != list[0].At {
		t.Fatalf("the time changed: %s vs %s", again[0].At, list[0].At)
	}
}
