package memory

import (
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	"context"
	"fmt"
	"sort"
	"time"
)

// TopicMemoryRow is one topic semantic memory read by exact key, with the
// server-side identity still attached.
//
// The injected path has always rendered memories to []string and thrown the
// rest away, which was fine while nothing downstream had to say which record a
// line stood for. STABILIZE-039 has two things that do: the evidence registry
// issues an S# that must resolve to a real row, and memory_search has to
// recognise the same row if a later search returns it. Neither can be done from
// a rendered sentence.
type TopicMemoryRow struct {
	ID       string
	Version  uint64
	Owner    int64
	Status   string
	Scope    string
	TopicKey string
	Subject  string
	Content  string
	// Line is the same rendering the injected path has always used, so the two
	// modes show the model the same shape of text.
	Line string
}

// TopicMemorySnapshot is the v4 exact-key candidate set. EligibleCount is
// measured before the tool's MaxResults and token limits so callers can
// distinguish five total records from five shown out of eight. Rows contains
// every eligible identity up to CandidateLimit; a larger set is rejected
// rather than reported with an inexact already-in-context count.
type TopicMemorySnapshot struct {
	Rows          []TopicMemoryRow
	EligibleCount int
}

type topicWindowRepository interface {
	ListTopicWindow(context.Context, int64, model.MemoryScope, []string, float64, time.Time, int) (memorydao.TopicMemoryWindow, error)
}

// TopicMemoriesByKeys reads topic semantic memory for keys the server itself
// established, and for nothing else.
//
// It is a separate entry point rather than a flag on RelevantMemories because
// RelevantMemories does three things this caller must not do: it reads the
// active learning session again, it runs the lexical topic matcher (or the
// resolver) over the question, and it merges global memory into the result. In
// agent_memory_tool_v2 the keys are already decided by server state, and
// letting any of those three run again would quietly reopen the very sources
// the trust boundary excludes.
//
// There is no embedding and no vector search on this path, by construction: the
// only reads are ListByTopicKey, which is one indexed SQL predicate per key.
func (r *Retriever) TopicMemoriesByKeys(ctx context.Context, userID int64, keys []string) ([]TopicMemoryRow, error) {
	if r == nil || r.repository == nil {
		return nil, fmt.Errorf("memory recall repository is required")
	}
	if userID <= 0 {
		return nil, fmt.Errorf("memory recall identity is required")
	}
	normalized := normalizeStrongTopicKeys(keys)
	if len(normalized) == 0 {
		return nil, nil
	}
	now := r.now()
	rows := make([]TopicMemoryRow, 0, r.config.MaxResults)
	seen := make(map[string]struct{}, r.config.MaxResults)
	for _, topicKey := range normalized {
		if len(rows) == r.config.MaxResults {
			break
		}
		memories, err := r.repository.ListByTopicKey(ctx, userID, model.MemoryScopeTopic, topicKey,
			r.config.MinConfidence, r.config.CandidateLimit)
		if err != nil {
			// Fail the read rather than return a partial one. The caller drops
			// the whole block on error; a half-read block would look to the
			// model exactly like a complete one that happened to be short.
			return nil, fmt.Errorf("load topic memories: %w", err)
		}
		// The same ordering RelevantMemories applies, so the two paths cannot
		// disagree about which five of a large topic the learner sees.
		sort.SliceStable(memories, func(i, j int) bool {
			if memories[i].UserConfirmed != memories[j].UserConfirmed {
				return memories[i].UserConfirmed
			}
			if memories[i].Durability != memories[j].Durability {
				return memories[i].Durability == model.MemoryDurabilityStable
			}
			if memories[i].Importance != memories[j].Importance {
				return memories[i].Importance > memories[j].Importance
			}
			if !memories[i].UpdatedAt.Equal(memories[j].UpdatedAt) {
				return memories[i].UpdatedAt.After(memories[j].UpdatedAt)
			}
			return memories[i].ID < memories[j].ID
		})
		for _, memory := range memories {
			if len(rows) == r.config.MaxResults {
				break
			}
			if _, duplicate := seen[memory.ID]; duplicate {
				continue
			}
			// The SQL already filtered on all five, and they are re-checked
			// here for the same reason the recall path re-checks them: a
			// lifecycle or ownership mistake on this path puts another
			// account's private notes in front of a model with a citable
			// label, and that is not a failure worth trusting one layer with.
			if !eligibleMemory(memory, userID, model.MemoryScopeTopic, r.config.MinConfidence, now) {
				continue
			}
			// Exact equality only. No prefix, no alias, no normalisation of the
			// stored value: the key came from server state, and a topic the
			// learner never opened must not be reachable by being spelled
			// nearly the same.
			if memory.TopicKey != topicKey {
				continue
			}
			seen[memory.ID] = struct{}{}
			rows = append(rows, TopicMemoryRow{
				ID: memory.ID, Version: model.SemanticMemoryVersion(memory), Owner: memory.UserID,
				Status: string(memory.Status), Scope: string(memory.Scope), TopicKey: memory.TopicKey,
				Subject: memory.Subject, Content: memory.Content, Line: renderMemory(memory),
			})
		}
	}
	return rows, nil
}

// TopicMemorySnapshotByKeys is the v4 counterpart of TopicMemoriesByKeys. It
// requires a repository that can return rows and their pre-limit count from one
// database snapshot; silently falling back to the old capped API would make
// complete=true unverifiable.
func (r *Retriever) TopicMemorySnapshotByKeys(ctx context.Context, userID int64,
	keys []string) (TopicMemorySnapshot, error) {
	if r == nil || r.repository == nil {
		return TopicMemorySnapshot{}, fmt.Errorf("memory recall repository is required")
	}
	if userID <= 0 {
		return TopicMemorySnapshot{}, fmt.Errorf("memory recall identity is required")
	}
	normalized := normalizeStrongTopicKeys(keys)
	if len(normalized) == 0 {
		return TopicMemorySnapshot{Rows: []TopicMemoryRow{}}, nil
	}
	repository, ok := r.repository.(topicWindowRepository)
	if !ok {
		return TopicMemorySnapshot{}, fmt.Errorf("memory repository does not support an exact topic snapshot")
	}
	now := r.now()
	window, err := repository.ListTopicWindow(ctx, userID, model.MemoryScopeTopic, normalized,
		r.config.MinConfidence, now, r.config.CandidateLimit)
	if err != nil {
		return TopicMemorySnapshot{}, fmt.Errorf("load topic memory snapshot: %w", err)
	}
	if window.EligibleCount != int64(len(window.Memories)) || window.EligibleCount > int64(^uint(0)>>1) {
		return TopicMemorySnapshot{}, fmt.Errorf("topic memory snapshot count is invalid")
	}
	allowed := make(map[string]struct{}, len(normalized))
	for _, key := range normalized {
		allowed[key] = struct{}{}
	}
	rows := make([]TopicMemoryRow, 0, len(window.Memories))
	seen := make(map[string]struct{}, len(window.Memories))
	for _, memory := range window.Memories {
		if !eligibleMemory(memory, userID, model.MemoryScopeTopic, r.config.MinConfidence, now) {
			return TopicMemorySnapshot{}, fmt.Errorf("topic memory snapshot returned an ineligible row")
		}
		if _, exists := allowed[memory.TopicKey]; !exists {
			return TopicMemorySnapshot{}, fmt.Errorf("topic memory snapshot returned a row outside the exact keys")
		}
		if _, duplicate := seen[memory.ID]; duplicate {
			return TopicMemorySnapshot{}, fmt.Errorf("topic memory snapshot returned a duplicate row")
		}
		seen[memory.ID] = struct{}{}
		rows = append(rows, TopicMemoryRow{
			ID: memory.ID, Version: model.SemanticMemoryVersion(memory), Owner: memory.UserID,
			Status: string(memory.Status), Scope: string(memory.Scope), TopicKey: memory.TopicKey,
			Subject: memory.Subject, Content: memory.Content, Line: renderMemory(memory),
		})
	}
	return TopicMemorySnapshot{Rows: rows, EligibleCount: int(window.EligibleCount)}, nil
}
