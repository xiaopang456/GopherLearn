package memory

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"GopherAI/model"
)

// TopicRelation classifies one proposed topic note against the active notes of
// the same topic. It mirrors GlobalRelation for the topic scope, where the
// conflict query alone (same topic key or same content hash) cannot tell an
// echo apart from a new nuance.
type TopicRelation string

const (
	TopicRelationIndependent   TopicRelation = "independent"
	TopicRelationDuplicate     TopicRelation = "duplicate"
	TopicRelationReplacement   TopicRelation = "replacement"
	TopicRelationComplementary TopicRelation = "complementary"
)

func (r TopicRelation) Valid() bool {
	switch r {
	case TopicRelationIndependent, TopicRelationDuplicate, TopicRelationReplacement, TopicRelationComplementary:
		return true
	default:
		return false
	}
}

type TopicRelationResult struct {
	Relation       TopicRelation
	TargetMemoryID string
	Reason         string
}

// TopicRelationResolver judges one proposed topic note against the candidate
// notes of its final topic. The judgement runs once per explicit save on the
// edit path: a duplicate closes the draft without asking the learner to
// abandon anything, a replacement or complementary detail goes to the normal
// conflict decision. Batch 1 injects a deterministic resolver for acceptance;
// the production resolver arrives with the extraction batches.
type TopicRelationResolver interface {
	Classify(ctx context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (TopicRelationResult, error)
}

// NewDeterministicTopicRelationResolver builds a resolver for acceptance runs
// and local verification. Rules are matched in order against the proposed
// subject+content; the first matching rule decides, and a proposal that matches
// no rule is independent. This type is for tests and local harnesses only - it
// must never be wired into the production default service.
func NewDeterministicTopicRelationResolver(rules []DeterministicTopicRule) *DeterministicTopicRelationResolver {
	return &DeterministicTopicRelationResolver{rules: rules}
}

type DeterministicTopicRule struct {
	// Contains pins the rule to a case-insensitive keyword of the proposed
	// subject+content. Empty matches every proposal.
	Contains   string
	Relation   TopicRelation
	TargetHint string // substring of the target note's subject or content; empty picks the first candidate
}

type DeterministicTopicRelationResolver struct {
	rules []DeterministicTopicRule
}

func (r *DeterministicTopicRelationResolver) Classify(ctx context.Context, incoming ExplicitMemoryInput, existing []model.LongTermMemory) (TopicRelationResult, error) {
	if r == nil || len(existing) == 0 {
		return TopicRelationResult{Relation: TopicRelationIndependent, Reason: "no active notes in the topic"}, nil
	}
	proposed := strings.ToLower(incoming.Subject + "\n" + incoming.Content)
	for _, rule := range r.rules {
		if rule.Contains != "" && !strings.Contains(proposed, strings.ToLower(rule.Contains)) {
			continue
		}
		if !rule.Relation.Valid() {
			return TopicRelationResult{}, fmt.Errorf("invalid deterministic topic relation %q", rule.Relation)
		}
		result := TopicRelationResult{Relation: rule.Relation, Reason: "deterministic rule match"}
		if rule.Relation != TopicRelationIndependent {
			result.TargetMemoryID = pickDeterministicTarget(rule.TargetHint, existing)
			if result.TargetMemoryID == "" {
				return TopicRelationResult{}, fmt.Errorf("deterministic topic rule matched but no candidate target")
			}
		}
		return result, nil
	}
	return TopicRelationResult{Relation: TopicRelationIndependent, Reason: "no deterministic rule matched"}, nil
}

func pickDeterministicTarget(hint string, existing []model.LongTermMemory) string {
	hint = strings.ToLower(strings.TrimSpace(hint))
	for _, memory := range existing {
		if hint == "" || strings.Contains(strings.ToLower(memory.Subject+"\n"+memory.Content), hint) {
			return memory.ID
		}
	}
	return ""
}

// validateTopicRelationResult guards resolver output the same way the global
// classifier output is guarded: the target must be one of the supplied rows.
func validateTopicRelationResult(result TopicRelationResult, existing []model.LongTermMemory) error {
	if !result.Relation.Valid() || !validGlobalReason(result.Reason) || utf8.RuneCountInString(result.Reason) > 500 {
		return fmt.Errorf("invalid topic relation result")
	}
	if result.Relation != TopicRelationIndependent {
		for _, memory := range existing {
			if memory.ID == result.TargetMemoryID {
				return nil
			}
		}
		return fmt.Errorf("topic relation target is not one of the supplied rows")
	}
	return nil
}
