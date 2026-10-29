package memoryplan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"GopherAI/model"
)

// verifierCandidate is the payload shape one candidate takes in the Verifier's
// prompt.
//
// It carries text and nothing else. No message id, no session id, no user id, no
// version and no source handle appears here, which is what makes "never write a
// message id" a rule the model *cannot* satisfy by copying rather than a rule it
// is merely asked to follow.
type verifierCandidate struct {
	CandidateID string   `json:"candidate_id"`
	Kind        string   `json:"kind"`
	Subject     string   `json:"subject,omitempty"`
	Content     string   `json:"content,omitempty"`
	Title       string   `json:"title,omitempty"`
	Synopsis    string   `json:"synopsis,omitempty"`
	Decisions   []string `json:"decisions,omitempty"`
	OpenLoops   []string `json:"open_loops,omitempty"`
	Corrections []string `json:"user_corrections,omitempty"`
}

// RenderCandidates is the exact payload the Verifier is given.
func RenderCandidates(candidates []Candidate) (string, error) {
	payload := make([]verifierCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		switch candidate.Kind {
		case CandidateSemantic:
			payload = append(payload, verifierCandidate{
				CandidateID: candidate.ID, Kind: string(CandidateSemantic),
				Subject: strings.TrimSpace(candidate.Memory.Subject),
				Content: strings.TrimSpace(candidate.Memory.Content),
			})
		case CandidateEpisode:
			entry := verifierCandidate{
				CandidateID: candidate.ID, Kind: string(CandidateEpisode),
				Title:    strings.TrimSpace(candidate.Episode.Title),
				Synopsis: strings.TrimSpace(candidate.Episode.Document.Synopsis),
			}
			entry.Decisions = evidenceTexts(candidate.Episode.Document.Decisions)
			entry.OpenLoops = evidenceTexts(candidate.Episode.Document.OpenLoops)
			entry.Corrections = evidenceTexts(candidate.Episode.Document.UserCorrections)
			payload = append(payload, entry)
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("render verifier candidates: %w", err)
	}
	return string(data), nil
}

func evidenceTexts(entries []model.EpisodeEvidence) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, strings.TrimSpace(entry.Text))
	}
	return out
}

// ValidateVerdicts checks the Verifier's answer against the candidates it was
// given, and refuses everything the design says to refuse.
//
// The set must match exactly. A verdict for a candidate that was not sent means
// the model produced an id; a missing verdict means a candidate would be
// silently dropped or silently kept depending on which default someone picked,
// and neither default is a measurement.
func ValidateVerdicts(set VerdictSet, candidates []Candidate) ([]CandidateVerdict, error) {
	byID := make(map[string]Candidate, len(candidates))
	order := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
		order = append(order, candidate.ID)
	}
	if len(set.Verdicts) != len(candidates) {
		return nil, fmt.Errorf("%w: %d verdicts for %d candidates",
			ErrProtocolFailure, len(set.Verdicts), len(candidates))
	}
	seen := make(map[string]struct{}, len(set.Verdicts))
	for _, verdict := range set.Verdicts {
		id := strings.TrimSpace(verdict.CandidateID)
		candidate, known := byID[id]
		if !known {
			return nil, fmt.Errorf("%w: verdict names candidate %q, which was not sent", ErrProtocolFailure, id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: candidate %q is judged twice", ErrProtocolFailure, id)
		}
		seen[id] = struct{}{}
		if !verdict.Relation.Valid() {
			return nil, fmt.Errorf("%w: relation %q is unknown", ErrProtocolFailure, verdict.Relation)
		}
		if err := validateFieldRefs(candidate, verdict); err != nil {
			return nil, err
		}
	}
	return SortVerdicts(set.Verdicts, order), nil
}

func validateFieldRefs(candidate Candidate, verdict CandidateVerdict) error {
	if !verdict.Relation.Injectable() {
		// A related_only or insufficient verdict may name the field it looked
		// at. The reference is inert - nothing about this candidate reaches the
		// context - so it is checked for being well formed and in range and
		// then ignored.
		//
		// An earlier version refused the verdict set outright when one of these
		// carried a reference. The provider pre-check is what caught it: the
		// model routinely says "I read decisions[0]; it does not answer this",
		// which is a correct judgement, and refusing it turned a good answer
		// into a protocol failure, a retry, and then a whole failed stage. The
		// design lists unknown relations, out-of-range indices, extra schema
		// fields and missing verdicts as grounds for refusal; this was a rule
		// this code invented, and it converted the common case into the worst
		// outcome.
		return checkRefsResolve(candidate, verdict, false)
	}
	if len(verdict.FieldRefs) == 0 {
		return fmt.Errorf("%w: candidate %q is %s and must cite the field it rests on",
			ErrProtocolFailure, candidate.ID, verdict.Relation)
	}
	return checkRefsResolve(candidate, verdict, true)
}

// checkRefsResolve checks that every reference addresses a field the candidate
// actually has. requireProvenance is true only for the relations whose evidence
// the learner will be told they produced.
func checkRefsResolve(candidate Candidate, verdict CandidateVerdict, requireProvenance bool) error {
	seen := make(map[string]struct{}, len(verdict.FieldRefs))
	for _, ref := range verdict.FieldRefs {
		if _, duplicate := seen[ref]; duplicate {
			return fmt.Errorf("%w: candidate %q repeats field ref %q", ErrProtocolFailure, candidate.ID, ref)
		}
		seen[ref] = struct{}{}
		target, err := ParseFieldRef(ref)
		if err != nil {
			return err
		}
		if candidate.Kind == CandidateSemantic {
			// A semantic memory has exactly one addressable field. Allowing an
			// episode-shaped reference here would let a verdict claim structure
			// the record does not have.
			if target.List != "content" {
				return fmt.Errorf("%w: candidate %q is a memory and may only cite content, not %q",
					ErrProtocolFailure, candidate.ID, ref)
			}
			continue
		}
		var entries []model.EpisodeEvidence
		switch target.List {
		case "synopsis":
			if strings.TrimSpace(candidate.Episode.Document.Synopsis) == "" {
				return fmt.Errorf("%w: candidate %q cites a synopsis it does not have", ErrProtocolFailure, candidate.ID)
			}
			continue
		case "content":
			return fmt.Errorf("%w: candidate %q is an episode and may not cite content", ErrProtocolFailure, candidate.ID)
		case "decisions":
			entries = candidate.Episode.Document.Decisions
		case "open_loops":
			entries = candidate.Episode.Document.OpenLoops
		case "user_corrections":
			entries = candidate.Episode.Document.UserCorrections
		}
		if target.Index >= len(entries) {
			return fmt.Errorf("%w: candidate %q cites %s but has %d of them",
				ErrProtocolFailure, candidate.ID, ref, len(entries))
		}
		// A structured entry the answer will rest on has to still be traceable
		// to real messages. An entry whose provenance is gone is a compressed
		// claim with nothing behind it, and the design refuses to let the two
		// strongest relations rest on one.
		if requireProvenance && (verdict.Relation == RelationAnswer || verdict.Relation == RelationContradiction) {
			if len(entries[target.Index].EvidenceMessageIDs) == 0 {
				return fmt.Errorf("%w: candidate %q rests %s on %s, which cites no message",
					ErrProtocolFailure, candidate.ID, verdict.Relation, ref)
			}
		}
	}
	return nil
}

// SelectInjectable keeps only what may reach the context, in the order the
// design fixes: contradictions, then answers, then personalisation, and inside
// each relation the candidate pool's own order.
//
// Sorting by relation and then by pool position rather than by any score is what
// makes the result reproducible from the plan alone. A model-supplied ranking
// would make two runs of one plan differ, which is exactly what the experiment
// needs to be able to rule out.
func SelectInjectable(verdicts []CandidateVerdict, candidates []Candidate) []Candidate {
	position := make(map[string]int, len(candidates))
	byID := make(map[string]Candidate, len(candidates))
	for index, candidate := range candidates {
		position[candidate.ID] = index
		byID[candidate.ID] = candidate
	}
	type ranked struct {
		candidate Candidate
		relation  Relation
	}
	kept := make([]ranked, 0, len(verdicts))
	for _, verdict := range verdicts {
		if !verdict.Relation.Injectable() {
			continue
		}
		candidate, known := byID[verdict.CandidateID]
		if !known {
			continue
		}
		kept = append(kept, ranked{candidate: candidate, relation: verdict.Relation})
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].relation.InjectionRank() != kept[j].relation.InjectionRank() {
			return kept[i].relation.InjectionRank() < kept[j].relation.InjectionRank()
		}
		return position[kept[i].candidate.ID] < position[kept[j].candidate.ID]
	})
	out := make([]Candidate, 0, len(kept))
	for _, item := range kept {
		out = append(out, item.candidate)
	}
	return out
}

// RelationLabel is the internal tag a rendered memory carries into the context.
//
// The three relations read differently and must not be presented as one thing:
// personalisation is not a technical fact, and a contradiction is not a
// suggestion. The label is what stops the assembled block from flattening them.
func RelationLabel(relation Relation) string {
	switch relation {
	case RelationContradiction:
		return "contradicts a premise in the question"
	case RelationAnswer:
		return "direct personal evidence"
	case RelationPersonalization:
		return "personalisation only, not a technical fact"
	}
	return ""
}
