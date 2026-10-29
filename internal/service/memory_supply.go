package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/repository"
)

// The context-supply side of long-term memory. The source project did not make
// the model search for its profile facts: global memory was injected into the
// window on every turn, bounded by a token budget and gated by nothing but
// confirmation, and topic memory was injected for exactly the subjects an
// explicit authorization named. This file is that supply, rebuilt for this
// project's seam: a context builder calls it, gets the hits and their
// citations, and renders them into the window itself.
//
// It is deliberately query-free. A supply that took a query would re-import
// the lexical-intersection problem the recall path needed correcting for: a
// profile fact like "我住在成都" shares no word with "请用中文解释", and a
// supply that only returns what the question shares words with is a supply
// that never shows the profile. Nothing here scores a match; the caller's
// budget is the only selector.

// MemoryContextSupply assembles the window-injection supply. It shares the
// read-side repository and the same authorization seam the tool recall uses,
// so the two paths answer "what may this caller see" with one rule.
type MemoryContextSupply struct {
	repo       repository.Repository
	authorizer MemoryAuthorizer
	options    MemorySearchOptions
}

// NewMemoryContextSupply wires the supply. A missing store or authorizer is
// refused at construction, exactly like the tool-side recall: an absent
// authorizer must never read as "allow everything".
func NewMemoryContextSupply(repo repository.Repository, authorizer MemoryAuthorizer,
	options MemorySearchOptions,
) (*MemoryContextSupply, error) {
	if repo == nil {
		return nil, ErrNoMemoryStore
	}
	if authorizer == nil {
		return nil, ErrNoMemoryAuthorizer
	}
	return &MemoryContextSupply{repo: repo, authorizer: authorizer, options: options.Normalize()}, nil
}

// GlobalContext returns the user's confirmed global memories for window
// injection, newest first, trimmed to the caller's budget.
//
// There is no query and therefore no match gate: global memory is the profile
// the user confirmed for their whole account, and the source project carried it
// into every agent window without asking the question first. The confidence
// floor still applies - "confirmed" is what makes a fact injectable, and a
// low-confidence record stays out of the window exactly as it stays out of the
// tool. The scope note is static because the supply is static: it says what was
// read, not that it matched anything.
func (s *MemoryContextSupply) GlobalContext(ctx context.Context, userID string, maxItems, maxRunes int) ([]MemoryHit, string, error) {
	if ctx == nil {
		return nil, "", ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, "", ErrUnauthorized
	}
	grant, err := s.authorizer.AuthorizeMemoryScope(ctx, userID, "")
	if err != nil {
		return nil, "", mapRepositoryError(err)
	}
	if !grant.AllowGlobal {
		// The authorization seam decides, not the caller: a deployment that
		// has not granted global memory gets an honest empty supply.
		return []MemoryHit{}, "本轮没有全局记忆供给授权，因此没有读取全局记忆。", nil
	}
	items, err := s.repo.ListActiveMemories(ctx, userID, domain.MemoryScopeGlobal, "")
	if err != nil {
		return nil, "", mapRepositoryError(err)
	}
	return s.trim(sortGlobalMemories(items), maxItems, maxRunes),
		"本轮供给范围：当前用户的全局记忆（按确认时间由新到旧）。", nil
}

// RelevantTopicContext supplies legacy_v2 topic memories using server-owned
// session consent, strong topic keys, and the learning topic matcher.
func (s *MemoryContextSupply) RelevantTopicContext(ctx context.Context, userID, sessionID, question string, strongKeys []string, maxItems, maxRunes int) ([]MemoryHit, string, error) {
	if ctx == nil {
		return nil, "", ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(sessionID) == "" {
		return nil, "", ErrUnauthorized
	}
	if _, err := s.repo.GetSession(ctx, userID, sessionID); err != nil {
		return nil, "", mapRepositoryError(err)
	}
	preference, err := s.repo.GetTopicMemoryPreference(ctx, userID, sessionID)
	if err != nil && !errors.Is(err, repository.ErrNotFound) {
		return nil, "", mapRepositoryError(err)
	}
	if !preference.Enabled {
		return []MemoryHit{}, "本轮没有主题记忆供给授权。", nil
	}
	items, err := s.repo.ListMemoriesByStatus(ctx, userID, domain.MemoryStatusActive)
	if err != nil {
		return nil, "", mapRepositoryError(err)
	}
	matcher := newLearningTopicMatcher(question)
	strong := make(map[string]struct{}, len(strongKeys))
	for _, key := range normalizeKeys(strongKeys) {
		strong[key] = struct{}{}
	}
	type candidate struct {
		memory domain.Memory
		score  float64
		strong bool
	}
	candidates := make([]candidate, 0)
	for _, item := range items {
		if item.Scope != domain.MemoryScopeTopic {
			continue
		}
		_, isStrong := strong[item.TopicKey]
		score, relevant := matcher.match(item.TopicKey)
		if !isStrong && !relevant {
			continue
		}
		candidates = append(candidates, candidate{item, score, isStrong})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].strong != candidates[j].strong {
			return candidates[i].strong
		}
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		if !candidates[i].memory.CreatedAt.Equal(candidates[j].memory.CreatedAt) {
			return candidates[i].memory.CreatedAt.After(candidates[j].memory.CreatedAt)
		}
		return candidates[i].memory.MemoryID < candidates[j].memory.MemoryID
	})
	selected := make([]domain.Memory, 0, len(candidates))
	seen := make(map[string]struct{})
	for _, item := range candidates {
		if _, ok := seen[item.memory.TopicKey]; ok {
			continue
		}
		seen[item.memory.TopicKey] = struct{}{}
		selected = append(selected, item.memory)
	}
	return s.trim(selected, maxItems, maxRunes), "本轮供给范围：已授权且与当前问题相关的主题记忆。", nil
}

// only, newest first, trimmed to the caller's budget.
//
// The keys come from the caller; the authorization does not. A key the
// authorizer's grant does not name is dropped before any read, so a context
// builder cannot widen the supply by asking for more subjects - the same
// per-subject consent the tool path enforces, at the same seam.
func (s *MemoryContextSupply) TopicContext(ctx context.Context, userID, sessionID string, topicKeys []string, maxItems, maxRunes int) ([]MemoryHit, string, error) {
	if ctx == nil {
		return nil, "", ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(userID) == "" {
		return nil, "", ErrUnauthorized
	}
	grant, err := s.authorizer.AuthorizeMemoryScope(ctx, userID, sessionID)
	if err != nil {
		return nil, "", mapRepositoryError(err)
	}
	authorized := make(map[string]struct{}, len(grant.TopicKeys))
	for _, key := range normalizeKeys(grant.TopicKeys) {
		authorized[key] = struct{}{}
	}
	requested := normalizeKeys(topicKeys)
	collected := make([]domain.Memory, 0)
	used := 0
	for _, key := range requested {
		if _, ok := authorized[key]; !ok {
			continue
		}
		items, err := s.repo.ListActiveMemories(ctx, userID, domain.MemoryScopeTopic, key)
		if err != nil {
			return nil, "", mapRepositoryError(err)
		}
		collected = append(collected, items...)
		used++
	}
	note := "本轮没有已授权主题的记忆供给。"
	if used > 0 {
		note = "本轮供给范围：已授权主题（" + strings.Join(requested, "、") + "）的记忆。"
	}
	return s.trim(sortGlobalMemories(collected), maxItems, maxRunes), note, nil
}

// sortGlobalMemories orders a supply deterministically: newest confirmation
// first, then the store's own identity as the tie-break.
func sortGlobalMemories(items []domain.Memory) []domain.Memory {
	ordered := append([]domain.Memory(nil), items...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if !ordered[left].CreatedAt.Equal(ordered[right].CreatedAt) {
			return ordered[left].CreatedAt.After(ordered[right].CreatedAt)
		}
		return ordered[left].MemoryID < ordered[right].MemoryID
	})
	return ordered
}

// trim applies the confidence floor, the item cap and the rune budget. The
// floor is the same one the tool recall enforces: a supply is a prompt input,
// and a guess priced into a prompt becomes a fact the answer is built from.
func (s *MemoryContextSupply) trim(items []domain.Memory, maxItems, maxRunes int) []MemoryHit {
	if maxItems <= 0 || maxItems > s.options.MaxResults*4 {
		maxItems = s.options.MaxResults * 4
	}
	if maxRunes <= 0 || maxRunes > s.options.TotalRunes*4 {
		maxRunes = s.options.TotalRunes * 4
	}
	hits := make([]MemoryHit, 0, len(items))
	used := 0
	for _, memory := range items {
		if len(hits) >= maxItems {
			break
		}
		if memory.Confidence < s.options.MinConfidence {
			continue
		}
		content := clipRunes(memory.Content, s.options.ItemRunes)
		if remaining := maxRunes - used; len([]rune(content)) > remaining {
			if remaining < 2 {
				break
			}
			content = clipRunes(content, remaining-1)
		}
		used += len([]rune(content))
		hits = append(hits, MemoryHit{
			Memory:    memory,
			Reference: referenceFor(memory),
			Content:   content,
		})
	}
	return hits
}
