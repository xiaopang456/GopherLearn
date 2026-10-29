package domain

import (
	"errors"
	"strings"
)

// AnswerStrategy is the server-resolved execution strategy for one turn. It is
// derived, never supplied by a client: see ResolveAnswerStrategy.
type AnswerStrategy string

const (
	AnswerStrategyDirectChat  AnswerStrategy = "direct_chat"
	AnswerStrategyKnowledgeQA AnswerStrategy = "knowledge_qa"
	AnswerStrategyAgent       AnswerStrategy = "agent"
)

func (s AnswerStrategy) IsValid() bool {
	switch s {
	case AnswerStrategyDirectChat, AnswerStrategyKnowledgeQA, AnswerStrategyAgent:
		return true
	default:
		return false
	}
}

var (
	// ErrInvalidKnowledgeOptions reports a knowledge option shape that cannot
	// be honored, such as an empty collection ID or a toggle that conflicts
	// with the session mode.
	ErrInvalidKnowledgeOptions = errors.New("domain: invalid knowledge options")
	// ErrInvalidSessionMode reports a session mode outside the closed set.
	ErrInvalidSessionMode = errors.New("domain: invalid session mode")
)

// KnowledgeOptions is the per-turn knowledge grounding request of one Chat
// turn. The zero value means "grounding disabled", which is also the default
// when a client omits the option entirely.
type KnowledgeOptions struct {
	Enabled       bool
	CollectionIDs []string
}

// Normalize returns the canonical form of the options: every collection ID is
// trimmed, non-empty, and deduplicated while keeping first-occurrence order.
// The shape rules are mode independent; ResolveAnswerStrategy applies them so
// no caller can validate a turn partially.
func (o KnowledgeOptions) Normalize() (KnowledgeOptions, error) {
	if !o.Enabled {
		if len(o.CollectionIDs) > 0 {
			return KnowledgeOptions{}, ErrInvalidKnowledgeOptions
		}
		return KnowledgeOptions{}, nil
	}
	ids := make([]string, 0, len(o.CollectionIDs))
	seen := make(map[string]struct{}, len(o.CollectionIDs))
	for _, raw := range o.CollectionIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return KnowledgeOptions{}, ErrInvalidKnowledgeOptions
		}
		if _, repeated := seen[id]; repeated {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return KnowledgeOptions{}, ErrInvalidKnowledgeOptions
	}
	return KnowledgeOptions{Enabled: true, CollectionIDs: ids}, nil
}

// ResolveAnswerStrategy is the only place that maps a server-owned session mode
// plus one turn's knowledge options to an execution strategy. The returned
// options are the normalized values the strategy was resolved from, so callers
// persist and forward exactly what was validated.
func ResolveAnswerStrategy(mode SessionMode, knowledge KnowledgeOptions) (AnswerStrategy, KnowledgeOptions, error) {
	normalized, err := knowledge.Normalize()
	if err != nil {
		return "", KnowledgeOptions{}, err
	}
	switch mode {
	case SessionModeChat:
		if !normalized.Enabled {
			return AnswerStrategyDirectChat, normalized, nil
		}
		return AnswerStrategyKnowledgeQA, normalized, nil
	case SessionModeLearningAgent:
		// Whether an agent run retrieves knowledge is decided server-side by
		// tool capabilities, not by the per-turn Chat toggle.
		if normalized.Enabled {
			return "", KnowledgeOptions{}, ErrInvalidKnowledgeOptions
		}
		return AnswerStrategyAgent, normalized, nil
	default:
		return "", KnowledgeOptions{}, ErrInvalidSessionMode
	}
}
