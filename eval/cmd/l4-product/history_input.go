package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
)

// bytesReader wraps the raw input for the strict decoder.
func bytesReader(raw json.RawMessage) *bytes.Reader { return bytes.NewReader(raw) }

// readHistories reads the conversations of a prepare phase's input.
//
// The input is either a flat turn array - one conversation, whose id comes
// from the run - or an object that names the conversations:
//
//	{"sessions": [{"session_id": "l4-a", "turns": [...]}, ...]}
//
// The second form is what a comparison of two cross-session memory paths
// needs: the retrieval has to have several conversations to choose between,
// and a database that holds only the conversation the question is about
// measures nothing about the choosing.
func readHistories(raw json.RawMessage) ([]conversation.History, error) {
	var flat []conversation.Turn
	if err := json.Unmarshal(raw, &flat); err == nil {
		if len(flat) < 1 || len(flat) > 256 {
			return nil, fmt.Errorf("turn count must be 1 through 256")
		}
		return nil, fmt.Errorf("the turn file has to name its conversations")
	}
	var named struct {
		Sessions []struct {
			SessionID string              `json:"session_id"`
			Turns     []conversation.Turn `json:"turns"`
		} `json:"sessions"`
	}
	decoder := json.NewDecoder(bytesReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&named); err != nil {
		return nil, fmt.Errorf("the turn file is neither a turn array nor a set of conversations: %w", err)
	}
	if len(named.Sessions) < 1 {
		return nil, fmt.Errorf("the turn file names no conversation")
	}
	seen := map[string]struct{}{}
	out := make([]conversation.History, 0, len(named.Sessions))
	for _, session := range named.Sessions {
		if session.SessionID == "" {
			return nil, fmt.Errorf("a conversation has no id")
		}
		if _, exists := seen[session.SessionID]; exists {
			return nil, fmt.Errorf("the conversation %s is named twice", session.SessionID)
		}
		seen[session.SessionID] = struct{}{}
		if len(session.Turns) < 1 || len(session.Turns) > 256 {
			return nil, fmt.Errorf("the turn count of %s must be 1 through 256", session.SessionID)
		}
		out = append(out, conversation.History{SessionID: session.SessionID, Turns: session.Turns})
	}
	return out, nil
}
