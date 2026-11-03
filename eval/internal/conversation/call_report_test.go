package conversation

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCallReportSeparatesBackgroundAndUnknownUsage(t *testing.T) {
	answer := time.Now()
	calls := AttributeCalls([]ModelCall{
		{Role: "answer", Started: answer.Add(-time.Second), RawUsageSamples: []json.RawMessage{json.RawMessage(`{"prompt_tokens":8,"completion_tokens":1}`), json.RawMessage(`{"prompt_tokens":8,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":2}}`)}},
		{Role: "summary", Started: answer.Add(-time.Second)},
		{Role: "embedding", Started: answer.Add(time.Second), RawUsageSamples: []json.RawMessage{json.RawMessage(`{"prompt_tokens":5}`)}},
	}, answer)
	if calls[2].Role != "background" || calls[2].Purpose != "embedding" {
		t.Fatal(calls)
	}
	totals, err := SummarizeCalls(calls)
	if err != nil {
		t.Fatal(err)
	}
	if totals["answer"].ReportedInput != 8 || totals["answer"].ReportedOutput != 3 || totals["answer"].ReportedCachedInput != 2 {
		t.Fatal("cumulative snapshots double counted", totals)
	}
	if totals["summary"].UnmeteredInput != 1 || totals["summary"].UnmeteredOutput != 1 || totals["background"].UnmeteredOutput != 1 || totals["background"].ReportedInput != 5 || totals["background"].UnmeteredCache != 1 {
		t.Fatal("unknown usage lost", totals)
	}
	if _, err := SummarizeCalls([]ModelCall{{Role: "unattributed"}}); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestEveryCallSaysWhichRunItBelongsToAndWhenItWasSent(t *testing.T) {
	answer := time.Now()
	calls := BindRun(AttributeCalls([]ModelCall{
		{Role: "answer", Method: "stream", Started: answer.Add(-time.Second)},
		{Role: "background", Method: "generate", Started: answer.Add(time.Second)},
	}, answer), "session-1", "run-1")
	if calls[0].RunID != "run-1" || calls[0].SessionID != "session-1" {
		t.Fatalf("the identity is missing: %+v", calls[0])
	}
	if calls[0].Phase != "answer" || calls[1].Phase != "after_answer" {
		t.Fatalf("the phase is wrong: %+v", calls)
	}
	ledger := CallLedger(calls)
	if len(ledger) != 2 {
		t.Fatalf("%d calls in the ledger; want 2", len(ledger))
	}
	if ledger[0].RunID != "run-1" || ledger[0].SessionID != "session-1" || ledger[0].Lane != "chat" {
		t.Fatalf("the ledger lost the identity: %+v", ledger[0])
	}
	if ledger[1].Phase != "after_answer" {
		t.Fatalf("the ledger lost the phase: %+v", ledger[1])
	}
	if ledger[0].At != calls[0].Started {
		t.Fatalf("the ledger changed the time: %s vs %s", ledger[0].At, calls[0].Started)
	}
}

func TestTheLedgerKeepsUsageThatWasNotReportedUnknown(t *testing.T) {
	answer := time.Now()
	calls := BindRun(AttributeCalls([]ModelCall{
		{Role: "answer", Started: answer.Add(-time.Second)},
	}, answer), "session-1", "run-1")
	ledger := CallLedger(calls)
	if ledger[0].Usage != nil {
		t.Fatalf("a call without usage got usage: %+v", ledger[0].Usage)
	}
}
