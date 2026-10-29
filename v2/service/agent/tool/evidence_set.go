package agenttool

import (
	"fmt"
	"sync"
)

// EvidenceSet is the request-scoped evidence authority shared by all tools in
// one AgentRun. It assigns stable citations, records which document versions
// search made eligible for follow-up reads, prevents repeated content from
// consuming context, and owns the total evidence-token budget.
type EvidenceSet struct {
	mu               sync.Mutex
	maxTokens        int
	usedTokens       int
	nextCitation     int
	citations        map[string]int
	contentEmitted   map[string]bool
	allowedDocuments map[string]struct{}
}

func newEvidenceSet(maxTokens int) *EvidenceSet {
	return &EvidenceSet{
		maxTokens: maxTokens, citations: make(map[string]int),
		contentEmitted: make(map[string]bool), allowedDocuments: make(map[string]struct{}),
	}
}

func documentVersionKey(documentID string, version int) string {
	return fmt.Sprintf("%s:v%d", boundedText(documentID, 128), version)
}

func (e *EvidenceSet) documentAllowed(documentID string, version int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, exists := e.allowedDocuments[documentVersionKey(documentID, version)]
	return exists
}
