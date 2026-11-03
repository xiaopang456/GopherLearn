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
