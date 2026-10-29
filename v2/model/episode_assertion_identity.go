package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Stable assertion identity.
//
// v1 numbered assertions by position and called it stable. It is not: inserting
// a decision at the front, reordering two, or deleting one all renumber the
// survivors, and a governance relation recorded against :d1 then silently
// describes a different fact. Losing a relation would be bad; re-pointing one
// at the wrong sentence and continuing to display it is worse.
//
// v1.1 gives every assertion a server-generated identity that is written into
// the document and survives re-projection. The identity is opaque on purpose:
// it encodes no position, no kind and no episode, so nothing can be inferred
// from it and nothing breaks when the document is rearranged.

const (
	// assertionIDPrefix marks a server-issued identity.
	assertionIDPrefix = "ea_"
	// legacyAssertionIDPrefix marks an identity derived from a legacy document
	// that has none stored. It is deterministic, so reading the same old row
	// twice yields the same id, and it is distinguishable so that "this record
	// predates stable identity" stays visible.
	legacyAssertionIDPrefix = "lg_"
)

// NewAssertionID issues a fresh identity.
//
// It is generated on the server. The projector model proposes text and may echo
// back an identity it was given, but it can never mint one: an id the model
// invented would be an unowned handle into another learner's governance table.
func NewAssertionID() (string, error) {
	buffer := make([]byte, 12)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("issue assertion id: %w", err)
	}
	return assertionIDPrefix + hex.EncodeToString(buffer), nil
}

// DeterministicAssertionID mints a server-shaped identity from a seed.
//
// It exists for frozen fixture corpora, which must produce the same identities
// on every seeding or their digests and their recorded relations would drift.
// Production issues random identities; nothing here weakens that, because a
// seed is not something a model can supply.
func DeterministicAssertionID(seed string) string {
	sum := sha256.Sum256([]byte("stabilize058-fixture|" + seed))
	return assertionIDPrefix + hex.EncodeToString(sum[:])[:24]
}

// ValidAssertionID reports whether a string is an identity this server issued
// or derived. It is the check applied to anything a model echoes back.
func ValidAssertionID(value string) bool {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, assertionIDPrefix):
		_, err := hex.DecodeString(strings.TrimPrefix(value, assertionIDPrefix))
		return err == nil && len(value) == len(assertionIDPrefix)+24
	case strings.HasPrefix(value, legacyAssertionIDPrefix):
		_, err := hex.DecodeString(strings.TrimPrefix(value, legacyAssertionIDPrefix))
		return err == nil && len(value) == len(legacyAssertionIDPrefix)+32
	}
	return false
}

// IsLegacyAssertionID reports whether an identity was derived rather than
// issued. A legacy identity is usable, but it tells a reader that this record
// existed before identities were stored.
func IsLegacyAssertionID(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), legacyAssertionIDPrefix)
}

// LegacyAssertionID derives a one-time identity for a document that has none.
//
// The fingerprint is kind + sorted evidence message ids + normalised text,
// which is exactly the information that identifies the fact rather than its
// position. It is deterministic so two reads agree, and it is stable under
// insertion, deletion and reordering.
func LegacyAssertionID(episodeID string, kind AssertionKind, messageIDs []uint, text string) string {
	sum := sha256.Sum256([]byte(assertionFingerprint(episodeID, kind, messageIDs, text)))
	return legacyAssertionIDPrefix + hex.EncodeToString(sum[:])[:32]
}

func assertionFingerprint(episodeID string, kind AssertionKind, messageIDs []uint, text string) string {
	sorted := append([]uint(nil), messageIDs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return strings.Join([]string{episodeID, string(kind), strings.Join(parts, ","),
		NormalizeAssertionText(text)}, "|")
}

// NormalizeAssertionText collapses whitespace and case so that a cosmetic edit
// does not read as a different fact.
func NormalizeAssertionText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), "")
}

// evidenceKey is kind plus the sorted message ids: the handle on the underlying
// fact, independent of wording and position.
func evidenceKey(kind AssertionKind, messageIDs []uint) string {
	sorted := append([]uint(nil), messageIDs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	parts := make([]string, 0, len(sorted))
	for _, id := range sorted {
		parts = append(parts, fmt.Sprintf("%d", id))
	}
	return string(kind) + "|" + strings.Join(parts, ",")
}

// ErrAmbiguousReconciliation is returned when a re-projection cannot be matched
// to prior assertions without guessing.
//
// It is an error rather than a best effort because the alternative is exactly
// the v1 defect: silently attaching a recorded relation to whichever sentence
// happens to occupy the old position.
type ErrAmbiguousReconciliation struct {
	EpisodeID string
	Kind      AssertionKind
	Detail    string
}

func (e *ErrAmbiguousReconciliation) Error() string {
	return fmt.Sprintf("assertion reconciliation is ambiguous for %s %s: %s", e.EpisodeID, e.Kind, e.Detail)
}

// ReconcileAssertionIdentities assigns identities to a re-projected Episode,
// inheriting from the assertions that were there before.
//
// The order of preference is the order of how much the match is trusted:
//
//  1. the projector echoed back an identity it was given - the strongest
//     signal, and the only one that survives both a rewording and a move;
//  2. the same kind and the same evidence messages, uniquely - the fact is the
//     same even though the wording changed;
//  3. the same kind and the same normalised text, uniquely - the wording is the
//     same even though the cited messages moved;
//  4. nothing matched, so this is a new fact and gets a new identity.
//
// When step 2 or 3 finds more than one candidate it does not fall through to
// position. It fails.
func ReconcileAssertionIdentities(prior []Assertion, episode EpisodeMemory) ([]Assertion, error) {
	document, err := ReconcileDocumentIdentities(prior, episode)
	if err != nil {
		return nil, err
	}
	episode.Document = document
	return AssertionsOf(episode), nil
}

// ReconcileDocumentIdentities is the same reconciliation, returning the document
// so a caller can persist the assigned identities.
//
// Matching is order-independent by construction. A first-come pass would let
// whichever entry the projector happened to emit first claim a shared
// candidate, which is position-based matching wearing a different hat - the
// exact defect this file exists to remove.
func ReconcileDocumentIdentities(prior []Assertion, episode EpisodeMemory) (EpisodeDocument, error) {
	priorByKind := map[AssertionKind][]Assertion{}
	knownIDs := map[string]Assertion{}
	for _, item := range prior {
		if item.Kind == AssertionSynopsis {
			continue
		}
		knownIDs[item.ID] = item
		priorByKind[item.Kind] = append(priorByKind[item.Kind], item)
	}

	document := episode.Document
	claimed := map[string]struct{}{}
	// Every assertion slot, including the two added in Phase 2-C1. Listing them
	// by hand here is how a new kind silently loses its identities.
	for _, group := range document.AssertionGroups() {
		reconciled, err := reconcileSlot(episode.ID, group.Kind, *group.Entries,
			priorByKind[group.Kind], knownIDs, claimed)
		if err != nil {
			return EpisodeDocument{}, err
		}
		*group.Entries = reconciled
	}
	return document, nil
}

// reconcileSlot matches one kind's entries against that kind's prior
// assertions, in three passes of decreasing confidence. A pass only assigns a
// pair when it is unique in both directions: one entry and one candidate.
func reconcileSlot(episodeID string, kind AssertionKind, entries []EpisodeEvidence,
	priors []Assertion, knownIDs map[string]Assertion, claimed map[string]struct{}) ([]EpisodeEvidence, error) {
	assigned := make([]string, len(entries))

	// Pass 0: an identity the projector echoed back. Strongest signal, and the
	// only one that survives both a rewording and a move.
	for index, entry := range entries {
		echoed := strings.TrimSpace(entry.ID)
		if echoed == "" {
			continue
		}
		prior, known := knownIDs[echoed]
		if !known {
			return nil, fmt.Errorf("assertion id %q was never issued for episode %s", echoed, episodeID)
		}
		if prior.Kind != kind {
			return nil, fmt.Errorf("assertion id %q is a %s and was returned as a %s", echoed, prior.Kind, kind)
		}
		if _, taken := claimed[echoed]; taken {
			return nil, fmt.Errorf("assertion id %q was returned twice", echoed)
		}
		claimed[echoed] = struct{}{}
		assigned[index] = echoed
	}

	// Passes 1-3, each keyed differently, each requiring a one-to-one match.
	for _, keyOf := range []struct {
		entry func(EpisodeEvidence) string
		prior func(Assertion) string
	}{
		{ // same evidence and same wording: the fact is unchanged
			entry: func(e EpisodeEvidence) string {
				return evidenceKey(kind, e.EvidenceMessageIDs) + "|" + NormalizeAssertionText(e.Text)
			},
			prior: func(a Assertion) string {
				return evidenceKey(kind, a.EvidenceMessageIDs) + "|" + NormalizeAssertionText(a.Text)
			},
		},
		{ // same evidence, reworded
			entry: func(e EpisodeEvidence) string { return evidenceKey(kind, e.EvidenceMessageIDs) },
			prior: func(a Assertion) string { return evidenceKey(kind, a.EvidenceMessageIDs) },
		},
		{ // same wording, evidence moved
			entry: func(e EpisodeEvidence) string { return NormalizeAssertionText(e.Text) },
			prior: func(a Assertion) string { return NormalizeAssertionText(a.Text) },
		},
	} {
		entryBuckets := map[string][]int{}
		for index, entry := range entries {
			if assigned[index] != "" {
				continue
			}
			key := keyOf.entry(entry)
			entryBuckets[key] = append(entryBuckets[key], index)
		}
		priorBuckets := map[string][]Assertion{}
		for _, candidate := range priors {
			if _, taken := claimed[candidate.ID]; taken {
				continue
			}
			priorBuckets[keyOf.prior(candidate)] = append(priorBuckets[keyOf.prior(candidate)], candidate)
		}
		for key, indexes := range entryBuckets {
			candidates := priorBuckets[key]
			if len(candidates) == 0 {
				continue
			}
			// One entry and one candidate is the only unambiguous shape. Any
			// other count means something has to be guessed, and guessing is
			// how a governance relation silently changes meaning.
			if len(indexes) != 1 || len(candidates) != 1 {
				return nil, &ErrAmbiguousReconciliation{EpisodeID: episodeID, Kind: kind,
					Detail: fmt.Sprintf("%d re-projected assertions and %d prior assertions share one handle",
						len(indexes), len(candidates))}
			}
			assigned[indexes[0]] = candidates[0].ID
			claimed[candidates[0].ID] = struct{}{}
		}
	}

	// Whatever is left is a new fact.
	out := make([]EpisodeEvidence, 0, len(entries))
	for index, entry := range entries {
		if assigned[index] == "" {
			fresh, err := NewAssertionID()
			if err != nil {
				return nil, err
			}
			assigned[index] = fresh
		}
		entry.ID = assigned[index]
		out = append(out, entry)
	}
	return out, nil
}

// OrphanedGovernance lists governed assertion ids that a re-projection would
// leave without a target.
//
// The caller is expected to refuse the re-projection rather than write it: a
// dangling relation is invisible, and an invisible dangling relation is how a
// replaced decision quietly becomes current again.
func OrphanedGovernance(governed []string, reprojected []Assertion) []string {
	present := make(map[string]struct{}, len(reprojected))
	for _, item := range reprojected {
		present[item.ID] = struct{}{}
	}
	orphans := []string{}
	for _, id := range governed {
		if _, ok := present[id]; !ok {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)
	return orphans
}
