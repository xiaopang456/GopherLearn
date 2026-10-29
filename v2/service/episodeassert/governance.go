// Package episodeassert governs the time relation between Episode assertions.
//
// It answers one question the rest of the system could not: given two things a
// learner said at different times, which one still stands. STABILIZE-057 showed
// the cost of not being able to answer it - a reversed decision and the decision
// that reversed it are, to every reader, the same kind of record.
//
// What this package does not do is decide the relation. It stores, validates
// and reads relations that someone else established. Inferring "this new
// decision replaces that old one" needs a model, it belongs on the write side,
// and until it has its own experiment it is only allowed to abstain.
package episodeassert

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"GopherAI/model"
)

// maxChainDepth bounds the walk that detects a cycle. A replacement chain
// longer than this is not a legitimate history; it is a loop or a mistake, and
// walking it forever would turn one bad write into an outage.
const maxChainDepth = 32

var (
	// ErrVersionConflict means someone else wrote first. The caller re-reads
	// rather than retrying blind: the state it was about to overwrite may now
	// be the one it wanted. It is the model's sentinel so that the DAO can
	// raise it without either package importing the other.
	ErrVersionConflict = model.ErrAssertionVersionConflict
	// ErrRelationRejected covers every integrity refusal. The message says
	// which rule failed; the sentinel lets a caller separate "your relation is
	// wrong" from "the store is down".
	ErrRelationRejected = errors.New("assertion relation rejected")
)

// Store is the persistence this service needs, and no more.
type Store interface {
	// LoadStates returns the governance rows for the given assertion ids.
	LoadStates(ctx context.Context, userID int64, assertionIDs []string) (map[string]model.EpisodeAssertionState, error)
	// LoadEpisode returns one Episode the caller is entitled to read, or an
	// error when it is missing, archived, or owned by someone else.
	LoadEpisode(ctx context.Context, userID int64, episodeID string) (model.EpisodeMemory, error)
	// SaveState writes a row, refusing when expectedVersion no longer matches.
	SaveState(ctx context.Context, state model.EpisodeAssertionState, expectedVersion uint64) error
	// MessageTimes dates the learner messages an assertion cites.
	//
	// It lives on the store because an assertion's time comes from its evidence
	// and the evidence is rows. A store that cannot date a message returns no
	// entry for it, and the assertion is then reported as time-unknown rather
	// than borrowing the card's timestamp.
	MessageTimes(ctx context.Context, userID int64, messageIDs []uint) (map[uint]time.Time, error)
}

// Governor applies and reads assertion governance.
type Governor struct {
	store Store
	now   func() time.Time
}

func NewGovernor(store Store) (*Governor, error) {
	if store == nil {
		return nil, fmt.Errorf("assertion governor needs a store")
	}
	return &Governor{store: store, now: time.Now}, nil
}

// UseClock makes the governor testable without making time a parameter of every
// call.
func (g *Governor) UseClock(clock func() time.Time) {
	if clock != nil {
		g.now = clock
	}
}

// ReplaceRequest records that one assertion took over from another.
type ReplaceRequest struct {
	UserID int64
	// SourceEpisodeID and TargetEpisodeID name the cards.
	//
	// They are explicit because a v1.1 identity is opaque: it deliberately
	// encodes no episode, so nothing can be inferred from it and nothing breaks
	// when a document is rearranged. The caller always knows which card it is
	// acting on.
	SourceEpisodeID   string
	SourceAssertionID string
	TargetEpisodeID   string
	TargetAssertionID string
	// ExpectedVersion is the version the caller read. Zero means "no row yet".
	ExpectedVersion uint64
}

// MarkReplaced establishes source -> target, refusing anything that would make
// the graph dishonest.
//
// Every check here corresponds to a way a relation can be wrong while looking
// plausible in a UI, which is why they are refusals rather than warnings.
func (g *Governor) MarkReplaced(ctx context.Context, request ReplaceRequest) error {
	source, target := strings.TrimSpace(request.SourceAssertionID), strings.TrimSpace(request.TargetAssertionID)
	if request.UserID <= 0 || source == "" || target == "" {
		return fmt.Errorf("%w: a replacement needs an owner, a source and a target", ErrRelationRejected)
	}
	if source == target {
		return fmt.Errorf("%w: an assertion cannot replace itself", ErrRelationRejected)
	}
	sourceEpisodeID, targetEpisodeID := strings.TrimSpace(request.SourceEpisodeID), strings.TrimSpace(request.TargetEpisodeID)
	if sourceEpisodeID == "" || targetEpisodeID == "" {
		return fmt.Errorf("%w: both cards must be named", ErrRelationRejected)
	}
	if !model.ValidAssertionID(source) || !model.ValidAssertionID(target) {
		return fmt.Errorf("%w: an assertion id must be one this server issued", ErrRelationRejected)
	}

	sourceAssertion, err := g.resolve(ctx, request.UserID, sourceEpisodeID, source)
	if err != nil {
		return err
	}
	targetAssertion, err := g.resolve(ctx, request.UserID, targetEpisodeID, target)
	if err != nil {
		return err
	}
	sourceKind := sourceAssertion.Kind
	if err := checkReplacementKinds(sourceAssertion.Kind, targetAssertion.Kind); err != nil {
		return err
	}
	// Ownership is checked on both sides even though the loads were scoped by
	// user: a scoped query that silently returns nothing and a scoped query
	// that returns someone else's row fail differently, and only one of them is
	// caught by the load.
	if sourceAssertion.UserID != request.UserID || targetAssertion.UserID != request.UserID {
		return fmt.Errorf("%w: a replacement must stay inside one learner's history", ErrRelationRejected)
	}
	// A replacement that happened earlier than the thing it replaces is not a
	// replacement; it is a mis-linked pair, and accepting it would let an older
	// record present itself as the newer one.
	//
	// The comparison is between the two assertions, not the two cards. A card's
	// timestamp is when its session ended, and two decisions taken inside one
	// session share it, so card-level ordering cannot separate them at all.
	if !sourceAssertion.TimeKnown || !targetAssertion.TimeKnown {
		return fmt.Errorf("%w: an assertion with no resolvable time cannot be ordered", ErrRelationRejected)
	}
	if targetAssertion.OccurredAt.Before(sourceAssertion.OccurredAt) {
		return fmt.Errorf("%w: the replacement is older than what it replaces", ErrRelationRejected)
	}
	if err := g.refuseCycle(ctx, request.UserID, source, target); err != nil {
		return err
	}

	state := model.EpisodeAssertionState{
		AssertionID: source, UserID: request.UserID, EpisodeID: sourceEpisodeID,
		Kind: sourceKind, State: model.AssertionReplaced,
		ReplacedByAssertionID: target, ReplacedByEpisodeID: targetEpisodeID,
		OccurredAt: sourceAssertion.OccurredAt, Version: request.ExpectedVersion + 1,
	}
	if err := state.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrRelationRejected, err)
	}
	return g.store.SaveState(ctx, state, request.ExpectedVersion)
}

// MarkCurrent records that an assertion represents the present.
//
// It is a deliberate, per-assertion act. There is no bulk equivalent, because a
// bulk "mark everything current" is exactly the declaration the unknown state
// exists to avoid.
func (g *Governor) MarkCurrent(ctx context.Context, userID int64, episodeID, assertionID string, expectedVersion uint64) error {
	assertionID, episodeID = strings.TrimSpace(assertionID), strings.TrimSpace(episodeID)
	if !model.ValidAssertionID(assertionID) || episodeID == "" {
		return fmt.Errorf("%w: a card and a server-issued assertion id are required", ErrRelationRejected)
	}
	assertion, err := g.resolve(ctx, userID, episodeID, assertionID)
	if err != nil {
		return err
	}
	state := model.EpisodeAssertionState{
		AssertionID: assertionID, UserID: userID, EpisodeID: episodeID,
		Kind: assertion.Kind, State: model.AssertionCurrent,
		OccurredAt: assertion.OccurredAt, Version: expectedVersion + 1,
	}
	if err := state.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrRelationRejected, err)
	}
	return g.store.SaveState(ctx, state, expectedVersion)
}

// Resolve reads one Episode's assertions with governance applied.
//
// An assertion with no row comes back unknown, which is what legacy data is.
func (g *Governor) Resolve(ctx context.Context, userID int64, episode model.EpisodeMemory) ([]model.Assertion, error) {
	items, err := g.ResolveWithTimes(ctx, userID, episode)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	states, err := g.store.LoadStates(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	return model.ApplyAssertionStates(items, states), nil
}

// ResolveWithTimes expands an Episode into assertions dated from their own
// evidence, with no governance applied.
func (g *Governor) ResolveWithTimes(ctx context.Context, userID int64,
	episode model.EpisodeMemory) ([]model.Assertion, error) {
	wanted := map[uint]struct{}{}
	for _, entries := range [][]model.EpisodeEvidence{
		episode.Document.Decisions, episode.Document.OpenLoops, episode.Document.UserCorrections} {
		for _, entry := range entries {
			for _, id := range entry.EvidenceMessageIDs {
				wanted[id] = struct{}{}
			}
		}
	}
	ids := make([]uint, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	times, err := g.store.MessageTimes(ctx, userID, ids)
	if err != nil {
		return nil, fmt.Errorf("date assertion evidence: %w", err)
	}
	clock := func(messageIDs []uint) (time.Time, bool) {
		// The assertion is dated by its latest piece of evidence: a decision is
		// made once every message that supports it has been said, not when the
		// first was.
		latest, known := time.Time{}, false
		for _, id := range messageIDs {
			at, ok := times[id]
			if !ok {
				continue
			}
			if !known || at.After(latest) {
				latest, known = at, true
			}
		}
		return latest, known
	}
	return model.AssertionsOfWithClock(episode, clock), nil
}

// resolve finds one assertion inside its Episode, verifying that the slot the
// id names actually exists. An id that parses but points at a slot the document
// does not have is a dangling relation, and it is refused at write time rather
// than discovered at read time.
func (g *Governor) resolve(ctx context.Context, userID int64, episodeID, assertionID string) (model.Assertion, error) {
	episode, err := g.store.LoadEpisode(ctx, userID, episodeID)
	if err != nil {
		return model.Assertion{}, fmt.Errorf("%w: %v", ErrRelationRejected, err)
	}
	items, err := g.ResolveWithTimes(ctx, userID, episode)
	if err != nil {
		return model.Assertion{}, err
	}
	for _, item := range items {
		if item.ID == assertionID {
			return item, nil
		}
	}
	return model.Assertion{}, fmt.Errorf("%w: assertion %q does not exist in its episode",
		ErrRelationRejected, assertionID)
}

// refuseCycle walks forward from the proposed target. If the walk reaches the
// source, the new edge would close a loop, and a loop means no assertion in it
// can be said to be the latest.
func (g *Governor) refuseCycle(ctx context.Context, userID int64, source, target string) error {
	visited := map[string]struct{}{source: {}}
	cursor := target
	for depth := 0; depth < maxChainDepth; depth++ {
		if _, seen := visited[cursor]; seen {
			return fmt.Errorf("%w: this replacement would close a cycle", ErrRelationRejected)
		}
		visited[cursor] = struct{}{}
		states, err := g.store.LoadStates(ctx, userID, []string{cursor})
		if err != nil {
			return err
		}
		state, ok := states[cursor]
		if !ok || state.State != model.AssertionReplaced ||
			strings.TrimSpace(state.ReplacedByAssertionID) == "" {
			return nil
		}
		cursor = state.ReplacedByAssertionID
	}
	return fmt.Errorf("%w: replacement chain is longer than %d", ErrRelationRejected, maxChainDepth)
}
