package agenttool

import (
	"fmt"
	"sync"

	serviceagent "GopherAI/service/agent"
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

// Seed adopts the citations a server-side pre-retrieval already handed out.
//
// The pre-retrieval and the search tool are two doors into the same evidence,
// and a model that saw a chunk under [1] before the tool returned it must not
// see a different chunk under [1] afterwards. So the pre-retrieval's numbers
// are adopted here: the chunks keep their numbers, the documents they came
// from become eligible for a follow-up read, their text is marked as already
// shown so a later search does not spend the context budget on it twice, and
// the next citation continues where the pre-retrieval stopped.
//
// A nil outcome means there was no pre-retrieval, and an outcome without
// available evidence means it found nothing to hand over. Both leave the set
// exactly as a run without a pre-retrieval would have it.
func (e *EvidenceSet) Seed(outcome *serviceagent.RelevanceOutcome) {
	if outcome == nil || !outcome.Available() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, chunk := range outcome.Chunks {
		if chunk.Citation <= 0 || chunk.Identity == "" {
			continue
		}
		e.citations[chunk.Identity] = chunk.Citation
		if chunk.Citation > e.nextCitation {
			e.nextCitation = chunk.Citation
		}
		if chunk.DocID != "" && chunk.DocVersion > 0 {
			e.allowedDocuments[documentVersionKey(chunk.DocID, chunk.DocVersion)] = struct{}{}
		}
		// The chunk's text was already shown to the model, so a later search
		// that returns it must not spend the context budget showing it again.
		e.contentEmitted[chunk.Identity] = true
	}
	if outcome.NextCitation > e.nextCitation {
		e.nextCitation = outcome.NextCitation
	}
}
