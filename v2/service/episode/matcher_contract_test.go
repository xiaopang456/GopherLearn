package episode

// Candidate edge generator contract.
//
// This is NOT a matcher. It generates candidate edges between Gold units and
// generated assertions; it does not decide semantic equivalence. Until the
// semantic adjudication stage runs, durable recall, per-kind recall and slot
// accuracy are null (terminal=awaiting_semantic_match_adjudication).
//
// Contract:
//   1. edges are generated only within the same session;
//   2. only user citations generate edges; assistant citations are refused;
//   3. evidence overlap must be non-empty;
//   4. kind match marks slot_consistent, mismatch marks slot_dispute
//      (slot_dispute edges can never count as covered);
//   5. many-to-many edges are allowed — they are candidates, not decisions;
//   6. output is ordered by (UnitID, AssertionID) regardless of input order;
//   7. no semantic equivalence is decided at this stage.

import (
	"sort"
	"testing"
)

type UnitSpec struct {
	UnitID          string
	Session         string
	Kind            string
	EvidenceDigests []string
}

type Citation struct {
	Role          string
	MessageDigest string
}

type AssertionSpec struct {
	AssertionID string
	Session     string
	Kind        string
	Citations   []Citation
}

type CandidateEdge struct {
	UnitID      string
	AssertionID string
	Session     string
	SlotLabel   string // slot_consistent | slot_dispute
	Overlap     int
}

// GenerateCandidateEdges is the reference implementation the production
// candidate-edge generator must agree with.
func GenerateCandidateEdges(units []UnitSpec, assertions []AssertionSpec) []CandidateEdge {
	var edges []CandidateEdge
	for _, u := range units {
		for _, a := range assertions {
			if u.Session != a.Session {
				continue // rule 1
			}
			overlap := 0
			for _, c := range a.Citations {
				if c.Role != "user" {
					continue // rule 2: only user citations
				}
				for _, e := range u.EvidenceDigests {
					if c.MessageDigest == e {
						overlap++
					}
				}
			}
			if overlap == 0 {
				continue // rule 3
			}
			label := "slot_dispute"
			if u.Kind == a.Kind {
				label = "slot_consistent"
			}
			edges = append(edges, CandidateEdge{
				UnitID: u.UnitID, AssertionID: a.AssertionID,
				Session: u.Session, SlotLabel: label, Overlap: overlap,
			})
		}
	}
	// rule 6: deterministic output order
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].UnitID != edges[j].UnitID {
			return edges[i].UnitID < edges[j].UnitID
		}
		return edges[i].AssertionID < edges[j].AssertionID
	})
	return edges
}

func TestEdgesWithinSessionOnly(t *testing.T) {
	units := []UnitSpec{{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a"}}}
	assertions := []AssertionSpec{{AssertionID: "A1", Session: "S2", Kind: "decision",
		Citations: []Citation{{Role: "user", MessageDigest: "a"}}}}
	edges := GenerateCandidateEdges(units, assertions)
	if len(edges) != 0 {
		t.Fatalf("cross-session edge must not exist, got %+v", edges)
	}
}

func TestEdgesAssistantCitationRefused(t *testing.T) {
	units := []UnitSpec{{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a"}}}
	assertions := []AssertionSpec{{AssertionID: "A1", Session: "S1", Kind: "decision",
		Citations: []Citation{{Role: "assistant", MessageDigest: "a"}}}}
	edges := GenerateCandidateEdges(units, assertions)
	if len(edges) != 0 {
		t.Fatalf("assistant citation must be refused, got %+v", edges)
	}
}

func TestEdgesZeroOverlapNoEdge(t *testing.T) {
	units := []UnitSpec{{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a"}}}
	assertions := []AssertionSpec{{AssertionID: "A1", Session: "S1", Kind: "decision",
		Citations: []Citation{{Role: "user", MessageDigest: "zzz"}}}}
	if edges := GenerateCandidateEdges(units, assertions); len(edges) != 0 {
		t.Fatalf("zero overlap must produce no edge, got %+v", edges)
	}
}

func TestEdgesSlotConsistentAndDispute(t *testing.T) {
	units := []UnitSpec{
		{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a"}},
		{UnitID: "U2", Session: "S1", Kind: "preference", EvidenceDigests: []string{"a"}},
	}
	assertions := []AssertionSpec{{AssertionID: "A1", Session: "S1", Kind: "decision",
		Citations: []Citation{{Role: "user", MessageDigest: "a"}}}}
	edges := GenerateCandidateEdges(units, assertions)
	if len(edges) != 2 {
		t.Fatalf("expected 2 candidate edges, got %d", len(edges))
	}
	byUnit := map[string]CandidateEdge{}
	for _, e := range edges {
		byUnit[e.UnitID] = e
	}
	if byUnit["U1"].SlotLabel != "slot_consistent" {
		t.Fatalf("U1 kind=decision should be slot_consistent")
	}
	if byUnit["U2"].SlotLabel != "slot_dispute" {
		t.Fatalf("U2 kind=preference should be slot_dispute")
	}
}

func TestEdgesManyToManyAllowed(t *testing.T) {
	units := []UnitSpec{
		{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a", "b"}},
		{UnitID: "U2", Session: "S1", Kind: "decision", EvidenceDigests: []string{"b"}},
	}
	assertions := []AssertionSpec{
		{AssertionID: "A1", Session: "S1", Kind: "decision", Citations: []Citation{{Role: "user", MessageDigest: "a"}}},
		{AssertionID: "A2", Session: "S1", Kind: "decision", Citations: []Citation{{Role: "user", MessageDigest: "b"}}},
	}
	edges := GenerateCandidateEdges(units, assertions)
	// U1-A1 (a), U1-A2 (b), U2-A2 (b) = 3 edges; U2-A1 has no overlap
	if len(edges) != 3 {
		t.Fatalf("expected 3 many-to-many candidate edges, got %d: %+v", len(edges), edges)
	}
}

func TestEdgesInputOrderIndependent(t *testing.T) {
	units := []UnitSpec{
		{UnitID: "U2", Session: "S1", Kind: "decision", EvidenceDigests: []string{"b"}},
		{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a"}},
	}
	assertions := []AssertionSpec{
		{AssertionID: "A2", Session: "S1", Kind: "decision", Citations: []Citation{{Role: "user", MessageDigest: "b"}}},
		{AssertionID: "A1", Session: "S1", Kind: "decision", Citations: []Citation{{Role: "user", MessageDigest: "a"}}},
	}
	edges := GenerateCandidateEdges(units, assertions)
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(edges))
	}
	if edges[0].UnitID != "U1" || edges[0].AssertionID != "A1" {
		t.Fatalf("output must be sorted by (UnitID, AssertionID), first edge = %+v", edges[0])
	}
	if edges[1].UnitID != "U2" || edges[1].AssertionID != "A2" {
		t.Fatalf("second edge should be U2-A2, got %+v", edges[1])
	}
}

func TestEdgesNoSemanticDecision(t *testing.T) {
	// The edge generator must not produce any "covered", "equivalent" or
	// recall-like output. Its only outputs are candidate edges with labels.
	units := []UnitSpec{{UnitID: "U1", Session: "S1", Kind: "decision", EvidenceDigests: []string{"a"}}}
	assertions := []AssertionSpec{{AssertionID: "A1", Session: "S1", Kind: "decision",
		Citations: []Citation{{Role: "user", MessageDigest: "a"}}}}
	edges := GenerateCandidateEdges(units, assertions)
	for _, e := range edges {
		if e.SlotLabel != "slot_consistent" && e.SlotLabel != "slot_dispute" {
			t.Fatalf("edge has unexpected label %q", e.SlotLabel)
		}
	}
}
