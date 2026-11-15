package agent

// The helpers the tests of the board share.
//
// The write path is told what to do by a policy, and the tests say it
// explicitly: a deployment that records the assistant's products and one that
// does not write the same rules for the learner's own facts, and they differ
// in what they may hold.

import (
	"strings"

	"GopherAI/model"
)

// learnerOnlyPolicy is the write path of a deployment that does not record
// what the assistant produced.
func learnerOnlyPolicy(version int) WhiteboardPolicy {
	return WhiteboardPolicy{SchemaVersion: version}
}

// assistantProductsPolicy is the write path of a deployment that records it.
func assistantProductsPolicy(version int) WhiteboardPolicy {
	return WhiteboardPolicy{SchemaVersion: version, RecordAssistantProducts: true}
}

// boardValues is what a board holds, by key.
func boardValues(board model.SessionWhiteboardDocument) map[string]string {
	values := make(map[string]string)
	for _, group := range [][]model.SessionWhiteboardEntry{board.Constraints, board.Plan, board.OpenItems} {
		for _, entry := range group {
			values[strings.TrimSpace(entry.Key)] = strings.TrimSpace(entry.Value)
		}
	}
	return values
}
