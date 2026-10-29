package model

import "testing"

func TestSessionSummaryDecoderSupportsLegacyAndV2Evidence(t *testing.T) {
	legacy, err := DecodeSessionSummaryDocument(`{"current_goal":"RPC","confirmed_decisions":["使用长度前缀"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.SchemaVersion != 0 || len(legacy.ConfirmedDecisions) != 1 || len(legacy.ConfirmedDecisions[0].EvidenceMessageIDs) != 0 {
		t.Fatalf("legacy=%#v", legacy)
	}
	v2, err := DecodeSessionSummaryDocument(`{"schema_version":2,"current_goal":"RPC","confirmed_decisions":[{"text":"使用长度前缀","evidence_message_ids":[9,3]}],"open_loops":[{"text":"补并发测试","evidence_message_ids":[11]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := v2.EvidenceMessageIDs(); len(got) != 3 || got[0] != 3 || got[1] != 9 || got[2] != 11 {
		t.Fatalf("evidence ids=%v", got)
	}
}

func TestSessionSummaryDecoderRejectsUnsafeEvidence(t *testing.T) {
	for _, payload := range []string{
		`null`,
		`{"schema_version":3,"current_goal":"x"}`,
		`{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[0]}]}`,
		`{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[1,1]}]}`,
		`{"schema_version":2,"confirmed_decisions":[{"text":"x","evidence_message_ids":[1],"extra":true}]}`,
		`{"schema_version":2,"unknown":true}`,
	} {
		if _, err := DecodeSessionSummaryDocument(payload); err == nil {
			t.Fatalf("payload should fail: %s", payload)
		}
	}
}
