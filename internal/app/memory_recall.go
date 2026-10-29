package app

import (
	"context"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/agent"
	"github.com/xiaopang456/GopherLearn/internal/service"
)

// MemoryRecall is the slice of the memory read side this assembly needs. It is spelled
// out as an interface rather than taking *service.MemoryAccess so the wiring stays
// testable, while the production value is that service.
type MemoryRecall interface {
	Recall(ctx context.Context, query service.MemoryQuery) ([]service.MemoryHit, string, error)
}

// memoryReader turns the service's read into what a tool may show a model. The mapping is
// the whole point of this type: everything a memory carries internally - its id, its
// source session, the message ids behind it, its confidence - stops here, and only the
// text, a server-written label, and the citation this read earned cross into the agent
// loop. A model that never sees an identifier cannot cite one it invented.
type memoryReader struct {
	recall MemoryRecall
}

func newMemoryReader(recall MemoryRecall) agent.MemoryReader {
	return memoryReader{recall: recall}
}

// RecallMemories implements agent.MemoryReader.
func (r memoryReader) RecallMemories(ctx context.Context, userID, sessionID, query string) (agent.MemoryReading, error) {
	if r.recall == nil {
		return agent.MemoryReading{}, service.ErrNoMemoryStore
	}
	hits, note, err := r.recall.Recall(ctx, service.MemoryQuery{
		UserID:    userID,
		SessionID: sessionID,
		Query:     query,
	})
	if err != nil {
		return agent.MemoryReading{}, err
	}
	reading := agent.MemoryReading{ScopeNote: strings.TrimSpace(note)}
	for _, hit := range hits {
		reading.Entries = append(reading.Entries, agent.MemoryEntry{
			Content:  hit.Content,
			Label:    hit.Reference.Citation.Title,
			Citation: hit.Reference.Citation,
		})
	}
	return reading, nil
}
