package service

import (
	"sort"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/retrieval"
)

// Evidence is what a question is allowed to be about, so choosing it is a policy
// decision rather than a retrieval detail.
//
// The original workflow generated over a fixed Top-K of whatever the retriever
// returned. Against one topic that is stable to the point of being a recording: the
// same query text produces the same ranking, so a second quiz on the same subject
// reuses the same chunks and the generator writes the same question. The three
// defaults below are what make a second quiz different on purpose - a wider candidate
// pool to choose from, the learner's own gaps pulling priority items forward, and a
// cap on how much of one document or one section may back a single quiz.
//
// None of this changes retrieval. The candidate pool arrives already admitted and
// ranked by the pipeline's own rules; the selection below only reorders within that
// set and never adds a chunk the retriever did not consider relevant.

const (
	// DefaultLearningCandidateLimit is how many relevant chunks the selector may
	// choose from. It is wider than an answer's Top-K because its job is coverage,
	// not grounding: three questions need material that does not overlap.
	DefaultLearningCandidateLimit = 30
	// DefaultLearningEvidenceItems caps how many chunks back one quiz.
	DefaultLearningEvidenceItems = 8
	// DefaultLearningEvidenceItemRunes bounds one snapshot. It sits just under the
	// chunker's own hard cap so a whole chunk is normally kept and only an oversized
	// one is cut.
	DefaultLearningEvidenceItemRunes = 900
	// DefaultLearningEvidenceTotalRunes bounds every snapshot in one quiz together.
	// Widening the candidate pool must not widen the prompt, which is the whole
	// reason a cap on items alone is not a budget.
	DefaultLearningEvidenceTotalRunes = 4800
	// DefaultLearningEvidenceMinItemRunes drops a chunk that cannot survive the
	// budget. A two-character fragment is not evidence for anything, and keeping it
	// would let a quiz look covered while resting on noise.
	DefaultLearningEvidenceMinItemRunes = 128
	// DefaultLearningHistoryLimit is how many past questions on one topic are read
	// for exposure counting and novelty. A question that was generated counts as
	// seen whether or not it was answered, because the learner may have read it.
	DefaultLearningHistoryLimit = 50
	// DefaultLearningPromptHistory is how many of those are shown to the generator.
	// The rest are used server-side: a model told "do not repeat these 50" spends
	// attention on the list instead of on the notes.
	DefaultLearningPromptHistory = 20
	// DefaultLearningWeakPointReserve is how strongly a gap must appear in a chunk
	// before that chunk is allowed to bypass the diversity caps. Below it, a weak
	// point is a preference; at or above it, the learner's own failure outranks
	// even distribution.
	DefaultLearningWeakPointReserve = 0.50
	// DefaultLearningMaxItemsPerDocument and DefaultLearningMaxItemsPerSection are
	// the concentration limits. One long document otherwise satisfies "eight chunks"
	// by itself, and a quiz built that way measures how well one page was memorized.
	DefaultLearningMaxItemsPerDocument = 3
	DefaultLearningMaxItemsPerSection  = 2
)

// LearningBudget configures evidence selection and question generation. The zero
// value means "use the defaults"; every value may only be narrowed, never widened
// past what the prompt can carry.
type LearningBudget struct {
	CandidateLimit   int
	MaxEvidenceItems int
	MaxItemRunes     int
	TotalRunes       int
	MinItemRunes     int
	HistoryLimit     int
	PromptHistory    int
}

// Normalize replaces unusable limits with the defaults. Zero means "not configured",
// never "generate no evidence".
func (b LearningBudget) Normalize() LearningBudget {
	optional := []struct {
		have  *int
		want  int
		floor int
	}{
		{&b.CandidateLimit, DefaultLearningCandidateLimit, DefaultLearningEvidenceItems},
		{&b.MaxEvidenceItems, DefaultLearningEvidenceItems, 1},
		{&b.MaxItemRunes, DefaultLearningEvidenceItemRunes, 1},
		{&b.TotalRunes, DefaultLearningEvidenceTotalRunes, DefaultLearningEvidenceItemRunes},
		{&b.MinItemRunes, DefaultLearningEvidenceMinItemRunes, 1},
		{&b.HistoryLimit, DefaultLearningHistoryLimit, 1},
		{&b.PromptHistory, DefaultLearningPromptHistory, 1},
	}
	for _, field := range optional {
		if *field.have <= 0 {
			*field.have = field.want
		}
		if *field.have < field.floor {
			*field.have = field.floor
		}
	}
	// The candidate pool must be at least as wide as the set it is asked to fill, or
	// "prefer unseen evidence" has nothing to prefer among.
	if b.CandidateLimit < b.MaxEvidenceItems {
		b.CandidateLimit = b.MaxEvidenceItems
	}
	if b.PromptHistory > b.HistoryLimit {
		b.PromptHistory = b.HistoryLimit
	}
	return b
}

// LearningEvidenceRequest is one selection run.
type LearningEvidenceRequest struct {
	// Candidates are the pipeline's admitted, ranked results, widest first.
	Candidates []retrieval.Result
	// Titles maps document id to its display title, for the snapshot a learner may
	// later see beside a citation.
	Titles map[string]string
	// History is this topic's previously generated questions, newest first.
	History []domain.LearningQuestion
	// WeakPoints are only the reliable ones: a gap from a single unanswered attempt is
	// not a fact about a learner, and building a quiz on it would teach a mistake.
	WeakPoints []string
}

// SelectLearningEvidence chooses the chunks one quiz is written from.
//
// The order is a set of passes rather than one scored ranking, because the goals
// conflict in a way a single number hides: freshness, the learner's gaps, and not
// stacking eight chunks on one document. Each pass relaxes exactly one of those, in
// the order a reader would want them relaxed - never use old evidence if new will do,
// and only spread out within one document if the whole quiz would otherwise be one
// section. A pass that comes back short is filled by the next, so a learner with few
// notes is not refused a quiz for want of variety.
func SelectLearningEvidence(request LearningEvidenceRequest, budget LearningBudget) []domain.LearningEvidence {
	budget = budget.Normalize()
	candidates := request.Candidates
	if len(candidates) > budget.CandidateLimit {
		candidates = candidates[:budget.CandidateLimit]
	}
	if len(candidates) == 0 || budget.MaxEvidenceItems == 0 {
		return nil
	}

	// How often each chunk already backed a question on this topic. The identity is
	// the versioned chunk, so re-indexing a document legitimately resets the count:
	// new text is not the old text a learner has seen.
	used := make(map[string]int, len(candidates))
	for _, question := range request.History {
		for _, evidence := range question.Evidence {
			used[evidence.Identity()]++
		}
	}

	type prospect struct {
		rank       int
		evidence   domain.LearningEvidence
		identity   string
		documentID string
		section    string
		order      int
		weakScore  float64
		unseen     bool
		hits       int
	}
	prospects := make([]prospect, 0, len(candidates))
	for index, result := range candidates {
		chunk := result.Chunk
		title := request.Titles[chunk.DocumentID]
		evidence := domain.LearningEvidenceFromChunk(chunk, title, 1)
		evidence.Snapshot = clipLearningRunes(evidence.Snapshot, budget.MaxItemRunes)
		// A chunk too short to say anything after the cut is not evidence; dropping it
		// here keeps a later pass from spending a slot on it.
		if runeLen(evidence.Snapshot) < budget.MinItemRunes && runeLen(chunk.Text) < budget.MinItemRunes {
			continue
		}
		identity := evidence.Identity()
		prospects = append(prospects, prospect{
			rank: index,
			// The snapshot keeps the chunk's own order so a reader can see where in
			// the document the question came from.
			evidence:   evidence,
			identity:   identity,
			documentID: chunk.DocumentID,
			section:    chunk.DocumentID + "|" + strings.ToLower(chunk.Section),
			order:      chunk.Order,
			weakScore:  learningMaxCoverage(chunk.Text, request.WeakPoints),
			unseen:     used[identity] == 0,
			hits:       used[identity],
		})
	}

	// Untried evidence first, then whatever speaks to the learner's gaps, then the
	// least-reused, and finally the retriever's own order - which is the tie-break
	// that keeps a repeated call on the same corpus producing the same quiz.
	sorted := append([]prospect(nil), prospects...)
	sort.SliceStable(sorted, func(left, right int) bool {
		a, b := sorted[left], sorted[right]
		if a.unseen != b.unseen {
			return a.unseen
		}
		if a.weakScore != b.weakScore {
			return a.weakScore > b.weakScore
		}
		if a.hits != b.hits {
			return a.hits < b.hits
		}
		return a.rank < b.rank
	})

	var (
		chosen   []prospect
		perDoc   = map[string]int{}
		perSec   = map[string]int{}
		total    int
		weakSeen = len(request.WeakPoints) > 0
	)
	added := make(map[string]struct{}, budget.MaxEvidenceItems)
	add := func(item prospect, enforceDiversity bool) bool {
		if len(chosen) >= budget.MaxEvidenceItems {
			return false
		}
		// One chunk is chosen at most once, whatever pass reaches it. Without this guard
		// the reserve pass and the four ordering passes can each claim the same chunk,
		// and a quiz whose two citations point at identical text measures one passage
		// twice while looking like it covers two.
		if _, taken := added[item.identity]; taken {
			return false
		}
		cost := runeLen(item.evidence.Snapshot)
		if total+cost > budget.TotalRunes {
			return false
		}
		if enforceDiversity {
			if perDoc[item.documentID] >= DefaultLearningMaxItemsPerDocument ||
				perSec[item.section] >= DefaultLearningMaxItemsPerSection {
				return false
			}
			for _, taken := range chosen {
				// Adjacent chunks of one document are usually one continuous
				// argument; two of them are one passage wearing two citations.
				if taken.documentID == item.documentID && absInt(taken.order-item.order) == 1 {
					return false
				}
			}
		}
		chosen = append(chosen, item)
		added[item.identity] = struct{}{}
		perDoc[item.documentID]++
		perSec[item.section]++
		total += cost
		return true
	}

	// A chunk that clearly speaks to a weak point is claimed before the caps are
	// consulted. A learner who failed the same thing twice does not need a quiz that
	// spread that failure politely across three documents.
	if weakSeen {
		for _, item := range sorted {
			if item.weakScore >= DefaultLearningWeakPointReserve {
				add(item, false)
			}
		}
	}
	// Fresh evidence under the caps, then fresh without them, then recycled - in that
	// order, so variety is given up before reuse and reuse is given up last.
	for _, pass := range []struct {
		unseenOnly  bool
		diversifies bool
	}{
		{true, true}, {true, false}, {false, true}, {false, false},
	} {
		for _, item := range sorted {
			if len(chosen) >= budget.MaxEvidenceItems {
				break
			}
			if pass.unseenOnly && !item.unseen {
				continue
			}
			add(item, pass.diversifies)
		}
	}

	// The chosen set is returned in the retriever's own order, so the evidence a
	// question cites is numbered by relevance and not by the order the passes
	// happened to fill slots. A learner reading "[2]" should see the second most
	// relevant chunk, not the second pass's leftover.
	sort.SliceStable(chosen, func(left, right int) bool { return chosen[left].rank < chosen[right].rank })
	evidence := make([]domain.LearningEvidence, 0, len(chosen))
	for index, item := range chosen {
		kept := item.evidence
		kept.Ordinal = index + 1
		evidence = append(evidence, kept)
	}
	return evidence
}

func runeLen(value string) int {
	return len([]rune(value))
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
