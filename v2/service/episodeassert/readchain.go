package episodeassert

import (
	"context"
	"fmt"
	"sort"
	"time"

	"GopherAI/model"
)

// The v1.1 read chain.
//
// memory_search returns Episodes; this turns each of them into the slots a
// governed answer can actually cite:
//
//	memory_search(kinds=episodic)
//	  -> production Searcher returns Episodes
//	  -> assertion-state repository
//	  -> expand each Episode into its slots
//	  -> merge current / replaced / unknown
//	  -> issue [E#.d#] / [E#.o#] / [E#.c#] for this turn
//	  -> the tool result shows kind, state, time and replacement target
//	  -> the Registry keeps label -> stable identity
//
// Two properties are deliberate. Replaced assertions are neither filtered nor
// down-ranked, because "which approach did I abandon" needs them. And a failure
// of the state store is a failure of the call: returning ungoverned cards would
// present every replaced decision as though nothing were known against it,
// which is precisely the state this feature exists to distinguish.

// GovernedSlot is one assertion as the tool renders it.
//
// It carries no stable identity: the model sees a per-turn label and the text,
// and the server keeps the mapping. An internal id in front of the model would
// be a handle it could echo into places it was never issued for.
type GovernedSlot struct {
	Label string               `json:"ref"`
	Kind  model.AssertionKind  `json:"kind"`
	State model.AssertionState `json:"state"`
	Text  string               `json:"text"`
	// OccurredAt is the assertion's own time, absent when it cannot be resolved.
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	// ReplacedByLabel is the per-turn label of the assertion that took over,
	// present only when that assertion is also in this turn's results. A
	// replacement outside the result set is reported as a bare flag, because
	// naming a label the turn did not issue would be an invalid reference.
	ReplacedByLabel string `json:"replaced_by,omitempty"`
	HasReplacement  bool   `json:"has_replacement,omitempty"`
}

// GovernedCard is one Episode expanded into slots.
type GovernedCard struct {
	Card  string         `json:"card"`
	Title string         `json:"title"`
	Slots []GovernedSlot `json:"slots"`
	// EpisodeID is server-side only and never rendered to the model. It is
	// here because a slot's identity cannot always be traced back to its card:
	// a synopsis has no id of its own in the document, so it is assigned a
	// derived one, and anything reconstructing the card from the identity
	// would lose exactly those slots.
	EpisodeID string `json:"-"`
}

// ReadChainResult is what the tool renders and what the validator checks
// against.
type ReadChainResult struct {
	Cards    []GovernedCard `json:"cards"`
	Registry *Registry      `json:"-"`
	// Versions pins each card's version at issue time so a citation can be
	// refused when the card has moved on.
	Versions CurrentEpisodeVersions `json:"-"`
}

// ExpandEpisodes builds the governed view of the Episodes a search returned.
//
// Card numbers follow the order the search produced, because that order is the
// retrieval's answer and re-sorting here would quietly re-rank results.
func (g *Governor) ExpandEpisodes(ctx context.Context, userID int64,
	episodes []model.EpisodeMemory) (ReadChainResult, error) {
	result := ReadChainResult{Cards: []GovernedCard{}, Registry: NewRegistry(),
		Versions: CurrentEpisodeVersions{}}
	if userID <= 0 {
		return result, fmt.Errorf("governed expansion needs a trusted owner")
	}

	resolved := make([][]model.Assertion, 0, len(episodes))
	allIDs := []string{}
	for _, episode := range episodes {
		items, err := g.ResolveWithTimes(ctx, userID, episode)
		if err != nil {
			return ReadChainResult{}, err
		}
		resolved = append(resolved, items)
		for _, item := range items {
			allIDs = append(allIDs, item.ID)
		}
	}
	// One state read for the whole result set. A per-card read would be N
	// queries for a list the caller already has in hand.
	states, err := g.store.LoadStates(ctx, userID, allIDs)
	if err != nil {
		return ReadChainResult{}, fmt.Errorf("load assertion states: %w", err)
	}

	// The label of every assertion in this result set, so a replacement inside
	// the set can be named rather than merely flagged.
	labelByAssertion := map[string]string{}
	issued := make([][]SlotRef, len(episodes))
	for index, episode := range episodes {
		card := fmt.Sprintf("E%d", index+1)
		governed := model.ApplyAssertionStates(resolved[index], states)
		refs, err := result.Registry.Issue(card, governed)
		if err != nil {
			return ReadChainResult{}, err
		}
		issued[index] = refs
		for _, ref := range refs {
			labelByAssertion[ref.AssertionID] = ref.Label
		}
		result.Versions[episode.ID] = episode.Version
	}

	for index, episode := range episodes {
		card := GovernedCard{Card: fmt.Sprintf("E%d", index+1), Title: episode.Title,
			EpisodeID: episode.ID, Slots: make([]GovernedSlot, 0, len(issued[index]))}
		for _, ref := range issued[index] {
			slot := GovernedSlot{Label: ref.Label, Kind: ref.Kind, State: ref.State, Text: ref.Text}
			if state, ok := states[ref.AssertionID]; ok && state.ReplacedByAssertionID != "" {
				slot.HasReplacement = true
				slot.ReplacedByLabel = labelByAssertion[state.ReplacedByAssertionID]
			}
			for _, item := range resolved[index] {
				if item.ID == ref.AssertionID && item.TimeKnown {
					at := item.OccurredAt
					slot.OccurredAt = &at
				}
			}
			card.Slots = append(card.Slots, slot)
		}
		result.Cards = append(result.Cards, card)
	}
	return result, nil
}

// ExpandCandidates is ExpandEpisodes for a search result, which hands back ids
// rather than documents.
//
// Each Episode is re-loaded through the store, which applies the same owner,
// status, index and live-session conditions the retrieval layer does. That is a
// second read of rows the searcher already saw, and it is deliberate: this is
// the door through which slot text reaches the model, and a card that
// memory_search would refuse must not become readable because it arrived by a
// different path. An id whose card is no longer readable is skipped rather than
// failing the whole call, because one archived card should not blank an answer
// the rest of the results could support.
func (g *Governor) ExpandCandidates(ctx context.Context, userID int64,
	episodeIDs []string) (ReadChainResult, error) {
	episodes := make([]model.EpisodeMemory, 0, len(episodeIDs))
	for _, id := range episodeIDs {
		episode, err := g.store.LoadEpisode(ctx, userID, id)
		if err != nil {
			continue
		}
		episodes = append(episodes, episode)
	}
	return g.ExpandEpisodes(ctx, userID, episodes)
}

// StateCounts summarises a governed result for diagnostics. It counts states,
// never text.
func (r ReadChainResult) StateCounts() map[model.AssertionState]int {
	counts := map[model.AssertionState]int{}
	for _, card := range r.Cards {
		for _, slot := range card.Slots {
			counts[slot.State]++
		}
	}
	return counts
}

// Labels lists every issued label in card order, for artefacts that need a
// stable rendering.
func (r ReadChainResult) Labels() []string {
	out := []string{}
	for _, card := range r.Cards {
		for _, slot := range card.Slots {
			out = append(out, slot.Label)
		}
	}
	sort.Strings(out)
	return out
}
