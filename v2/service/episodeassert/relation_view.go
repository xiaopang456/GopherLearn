package episodeassert

import (
	"context"
	"sort"
	"strings"

	"GopherAI/model"
)

// The relation-aware read.
//
// Governor.Resolve answers what an assertion *is* - current, replaced,
// unknown - because that is what EpisodeAssertionState holds. It cannot answer
// what a pair *is*, and after Phase 1.1 the store holds those too: supplements
// and equivalences live in their own table and nothing on the read side looked
// at it.
//
// A relation nobody can read is a dead column. Worse, it is a dead column that
// looks like a feature: "the system records that these two decisions
// complement each other" is true of the database and false of anything the
// learner will ever see.
//
// This is the smallest thing that fixes that. It does not change what Resolve
// returns, does not change legacy_v2, and does not render anything itself - it
// annotates assertions with the edges they participate in, and the governed
// rendering decides what to show.

// RelationReader is the read side of the relation store. It is an interface
// here so this package does not depend on the relation DAO, exactly as Store
// keeps it independent of the state DAO.
//
// The method is batched over one window's assertions rather than "give me
// everything this learner has". The first shape of this interface was the
// latter, and it would have read every edge a learner ever accumulated on every
// governed search in order to annotate a handful of slots - a cost that grows
// with the learner's history while the thing being annotated does not.
type RelationReader interface {
	RelationsForAssertions(ctx context.Context, userID int64,
		assertionIDs []string) ([]model.EpisodeAssertionRelation, error)
}

// AssertionRelationView is one edge an assertion participates in, as the read
// side sees it.
type AssertionRelationView struct {
	Kind model.AssertionRelationKind `json:"relation_kind"`
	// RelatedAssertionID and RelatedEpisodeID name the other end.
	RelatedAssertionID string `json:"related_assertion_id"`
	RelatedEpisodeID   string `json:"related_episode_id"`
	// Direction says which end this assertion is: "from" when it is the
	// earlier one, "to" when it is the later one. Without it "supplements"
	// is ambiguous in a way that matters - "this adds to that" and "that adds
	// to this" are different sentences.
	Direction string `json:"direction"`
	// Citable says whether the other end is inside the evidence the reader
	// was given. When it is false the relation may be stated but the target
	// may not be cited: a citation to a slot outside the window would be a
	// reference the reader cannot check, which is the thing slot citations
	// exist to prevent.
	Citable bool `json:"citable"`
}

const (
	// DirectionFrom means this assertion is the earlier end.
	DirectionFrom = "from"
	// DirectionTo means this assertion is the later end.
	DirectionTo = "to"
)

// RelationView is the annotated result of a governed read.
type RelationView struct {
	Assertion model.Assertion
	Relations []AssertionRelationView
}

// ResolveWithRelations resolves an Episode's assertions and annotates each
// with the edges it participates in.
//
// citableIDs is the set of assertion ids the caller is showing this turn.
// Anything outside it is reported as a relation whose target exists and cannot
// be cited, rather than as a citable reference the reader could not follow.
func ResolveWithRelations(ctx context.Context, governor *Governor,
	reader RelationReader, userID int64, episode model.EpisodeMemory,
	citableIDs map[string]struct{}) ([]RelationView, error) {
	assertions, err := governor.Resolve(ctx, userID, episode)
	if err != nil {
		return nil, err
	}
	views := make([]RelationView, 0, len(assertions))
	if reader == nil {
		for _, assertion := range assertions {
			views = append(views, RelationView{Assertion: assertion,
				Relations: []AssertionRelationView{}})
		}
		return views, nil
	}
	// One read for the whole card: the reader is asked once, for exactly the
	// assertions being annotated, not once per slot and not for the learner's
	// whole history.
	ids := make([]string, 0, len(assertions))
	for _, assertion := range assertions {
		ids = append(ids, assertion.ID)
	}
	edges, err := reader.RelationsForAssertions(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	byAssertion := IndexRelations(edges, userID, citableIDs)
	for _, assertion := range assertions {
		related := byAssertion[assertion.ID]
		if related == nil {
			related = []AssertionRelationView{}
		}
		views = append(views, RelationView{Assertion: assertion, Relations: related})
	}
	return views, nil
}

// IndexRelations groups edges by the assertion each one touches.
//
// Every edge appears twice, once from each end, because an assertion is equally
// a supplement's source and its target and the two are different statements.
// Rows belonging to another owner are dropped here rather than trusted: the
// reader was asked for one learner, and a store that ever answered with someone
// else's row would otherwise reach a rendering path that has no owner to check
// against.
//
// citableIDs is the set of assertion ids the caller is showing this turn.
// Anything outside it is marked not citable rather than dropped - a relation
// whose target is outside the window still exists, and saying so without a
// reference is more honest than saying nothing.
func IndexRelations(edges []model.EpisodeAssertionRelation, userID int64,
	citableIDs map[string]struct{}) map[string][]AssertionRelationView {
	out := map[string][]AssertionRelationView{}
	for _, edge := range edges {
		if edge.UserID != userID {
			continue
		}
		out[edge.SourceAssertionID] = append(out[edge.SourceAssertionID],
			AssertionRelationView{Kind: edge.Kind,
				RelatedAssertionID: edge.TargetAssertionID,
				RelatedEpisodeID:   edge.TargetEpisodeID, Direction: DirectionFrom,
				Citable: citable(citableIDs, edge.TargetAssertionID)})
		out[edge.TargetAssertionID] = append(out[edge.TargetAssertionID],
			AssertionRelationView{Kind: edge.Kind,
				RelatedAssertionID: edge.SourceAssertionID,
				RelatedEpisodeID:   edge.SourceEpisodeID, Direction: DirectionTo,
				Citable: citable(citableIDs, edge.SourceAssertionID)})
	}
	// A stable order, so two reads of one card annotate it identically.
	for id, related := range out {
		sort.SliceStable(related, func(i, j int) bool {
			if related[i].Kind != related[j].Kind {
				return related[i].Kind < related[j].Kind
			}
			if related[i].Direction != related[j].Direction {
				return related[i].Direction < related[j].Direction
			}
			return related[i].RelatedAssertionID < related[j].RelatedAssertionID
		})
		out[id] = related
	}
	return out
}

// citable reports whether the other end is inside this turn's evidence.
func citable(ids map[string]struct{}, assertionID string) bool {
	if len(ids) == 0 {
		return false
	}
	_, ok := ids[strings.TrimSpace(assertionID)]
	return ok
}

// RelationNote is one model-visible statement about one edge.
//
// It carries a kind, a closed-vocabulary sentence, and - only when the other
// end is inside this turn's evidence - the label under which that end was
// issued. It carries no assertion id, no Episode id and no database id: those
// stay server-side for the same reason a slot's identity does.
type RelationNote struct {
	Kind model.AssertionRelationKind
	// Ref is the other end's turn label, empty when it is outside the window.
	Ref string
	// Text is the sentence, drawn from the closed template set below.
	Text string
}

// RenderRelationNotes turns one assertion's edges into the notes a governed
// evidence card may carry.
//
// labelOf maps an assertion id to the label this turn issued for it, and
// returns empty for anything the turn did not issue. That is the only way a
// citable reference is produced here: a note may say a relation exists without
// naming a target, but it may never name a target the turn never showed.
//
// skip lets the caller drop a relation that the card already states another
// way. The replaced end of a replacement already carries replaced_by, and
// rendering the same fact twice would present one relation as two.
func RenderRelationNotes(views []AssertionRelationView,
	labelOf func(assertionID string) string,
	skip func(AssertionRelationView) bool) []RelationNote {
	notes := make([]RelationNote, 0, len(views))
	for _, view := range views {
		if skip != nil && skip(view) {
			continue
		}
		phrase := relationPhrase(view)
		if phrase == "" {
			// A kind with no template is not rendered at all, rather than
			// rendered as its own enum value. "uncertain" is not a sentence.
			continue
		}
		note := RelationNote{Kind: view.Kind, Text: phrase}
		if labelOf != nil {
			if label := strings.TrimSpace(labelOf(view.RelatedAssertionID)); label != "" {
				note.Ref, note.Text = label, phrase+label
			}
		}
		if note.Ref == "" {
			note.Text = phrase + "（未在本次证据中）"
		}
		notes = append(notes, note)
	}
	return notes
}

// RenderRelationNote turns one assertion's edges into the sentence a governed
// evidence package may show.
//
// It names the relation and, only when the other end is citable, its slot. A
// relation to something outside the window is stated as existing - "this was
// added to by a later decision" - without a reference the reader cannot
// follow, because inventing a citable target is worse than saying less.
func RenderRelationNote(view RelationView, slotOf func(string) string) string {
	if len(view.Relations) == 0 {
		return ""
	}
	parts := make([]string, 0, len(view.Relations))
	for _, relation := range view.Relations {
		phrase := relationPhrase(relation)
		if phrase == "" {
			continue
		}
		if relation.Citable && slotOf != nil {
			if slot := strings.TrimSpace(slotOf(relation.RelatedAssertionID)); slot != "" {
				parts = append(parts, phrase+" "+slot)
				continue
			}
		}
		parts = append(parts, phrase+"（未在本次证据中）")
	}
	return strings.Join(parts, "；")
}

// relationPhrase is the closed vocabulary the read side may say. A relation
// kind with no phrase is not rendered at all rather than rendered as its own
// enum value.
func relationPhrase(relation AssertionRelationView) string {
	switch relation.Kind {
	case model.RelationReplaces:
		if relation.Direction == DirectionFrom {
			return "已被后续决定取代："
		}
		return "取代了更早的决定："
	case model.RelationSupplements:
		if relation.Direction == DirectionFrom {
			return "被后续决定补充："
		}
		return "补充了更早的决定："
	case model.RelationEquivalent:
		return "与另一条记录表述相同："
	}
	return ""
}
