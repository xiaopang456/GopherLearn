package model

import (
	"strings"
	"testing"
	"time"
)

func TestDecodeEpisodeDocumentStrictAndEvidenceBound(t *testing.T) {
	valid := []byte(`{
		"synopsis":"讨论 RPC 拆包边界",
		"topics":[{"key":"rpc-framing","label":"RPC 粘包与拆包"}],
		"decisions":[{"text":"先用长度字段完成拆包","evidence_message_ids":[11]}],
		"open_loops":[{"text":"还未补并发半包测试","evidence_message_ids":[12]}],
		"user_corrections":[],
		"source_handles":["session:s1","message:11"]
	}`)
	document, err := DecodeEpisodeDocument(valid)
	if err != nil {
		t.Fatal(err)
	}
	if document.Topics[0].Key != "rpc-framing" || document.Decisions[0].EvidenceMessageIDs[0] != 11 {
		t.Fatalf("unexpected decoded document: %#v", document)
	}

	for name, payload := range map[string][]byte{
		"unknown field":      []byte(`{"synopsis":"x","topics":[],"decisions":[],"open_loops":[],"user_corrections":[],"source_handles":[],"extra":true}`),
		"trailing json":      []byte(`{"synopsis":"x","topics":[],"decisions":[],"open_loops":[],"user_corrections":[],"source_handles":[]} {}`),
		"missing evidence":   []byte(`{"synopsis":"x","topics":[],"decisions":[{"text":"decision","evidence_message_ids":[]}],"open_loops":[],"user_corrections":[],"source_handles":[]}`),
		"duplicate evidence": []byte(`{"synopsis":"x","topics":[],"decisions":[{"text":"decision","evidence_message_ids":[1,1]}],"open_loops":[],"user_corrections":[],"source_handles":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeEpisodeDocument(payload); err == nil {
				t.Fatalf("DecodeEpisodeDocument(%s) unexpectedly succeeded", payload)
			}
		})
	}
}

func TestEpisodeMemoryValidatesWatermarksAndBounds(t *testing.T) {
	valid := &EpisodeMemory{
		ID: "episode", UserID: 7, SessionID: "session", Title: "RPC framing",
		Document: EpisodeDocument{
			Synopsis: "讨论拆包", Topics: []EpisodeTopic{{Key: "rpc", Label: "RPC"}},
			Decisions:     []EpisodeEvidence{{Text: "使用长度字段", EvidenceMessageIDs: []uint{1}}},
			SourceHandles: []string{"session:session", "message:1"},
		},
		SearchText: "RPC framing 使用长度字段", TopicKeys: StringList{"rpc"},
		SourceSummaryVersion: 2, SourceSummaryWatermark: 10, SourceLastMessageID: 12,
		LastMessageAt: time.Now(), Version: 1, Status: EpisodeStatusActive, IndexStatus: EpisodeIndexPending,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}

	copy := *valid
	copy.SourceSummaryWatermark = 13
	if err := copy.Validate(); err == nil {
		t.Fatal("summary watermark beyond episode watermark unexpectedly passed")
	}
	copy = *valid
	copy.SourceSummaryVersion = 0
	if err := copy.Validate(); err == nil {
		t.Fatal("summary watermark without a summary version unexpectedly passed")
	}
	copy = *valid
	copy.Title = strings.Repeat("长", EpisodeTitleMaxRunes+1)
	if err := copy.Validate(); err == nil {
		t.Fatal("oversized title unexpectedly passed")
	}
	copy = *valid
	copy.TopicKeys = StringList{"other"}
	if err := copy.Validate(); err == nil {
		t.Fatal("topic metadata drift unexpectedly passed")
	}
	copy = *valid
	copy.Document.Decisions = []EpisodeEvidence{{Text: "future", EvidenceMessageIDs: []uint{13}}}
	if err := copy.Validate(); err == nil {
		t.Fatal("evidence beyond source watermark unexpectedly passed")
	}
}

func TestEpisodeProjectionJobRequiresCleanPendingState(t *testing.T) {
	job := &EpisodeProjectionJob{
		ID: "job", UserID: 7, SessionID: "session", DesiredMessageID: 12,
		Status: EpisodeProjectionPending, AvailableAt: time.Now(),
	}
	if err := job.ValidateForCreate(); err != nil {
		t.Fatal(err)
	}
	job.ClaimedMessageID = 12
	if err := job.ValidateForCreate(); err == nil {
		t.Fatal("claimed initial job unexpectedly passed")
	}
}
