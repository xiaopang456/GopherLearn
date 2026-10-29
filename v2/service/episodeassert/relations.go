package episodeassert

import (
	"fmt"

	"GopherAI/model"
)

// Which replacements are meaningful.
//
// v1 parsed the target's kind and then ignored it, so "this open loop replaces
// that synopsis" was accepted. That is not a relation anyone can act on, and an
// unusable relation in the table is worse than none: it renders, it looks
// authoritative, and nothing downstream can tell it apart from a real one.

// replacementMatrix is the allowed source -> target kinds.
//
//	decision   -> decision    a later decision overrides an earlier one
//	decision   -> correction  a correction overrides the decision it corrects
//	correction -> correction  a later correction refines an earlier one
//
// Everything else is refused, and two refusals are worth stating explicitly:
//
//   - A synopsis can neither replace nor be replaced. It paraphrases a session
//     rather than asserting anything, so "this decision replaces that summary"
//     names no fact that stopped being true.
//   - An open loop is not closed by replacement. "Still outstanding" becoming
//     "done" is a different relation with a different meaning, and expressing
//     it as `replaced` would make "what did I abandon" return finished work.
//     If open loops need a resolved state, it gets its own design rather than
//     smuggling itself in through this one.
var replacementMatrix = map[model.AssertionKind]map[model.AssertionKind]bool{
	model.AssertionDecision: {
		model.AssertionDecision:   true,
		model.AssertionCorrection: true,
	},
	model.AssertionCorrection: {
		model.AssertionCorrection: true,
	},
}

// ReplacementAllowed reports whether source may be replaced by target.
func ReplacementAllowed(source, target model.AssertionKind) bool {
	return replacementMatrix[source][target]
}

// checkReplacementKinds turns the matrix into a refusal with a reason the
// caller can show.
func checkReplacementKinds(source, target model.AssertionKind) error {
	if source == model.AssertionSynopsis || target == model.AssertionSynopsis {
		return fmt.Errorf("%w: a synopsis asserts nothing, so it cannot replace or be replaced", ErrRelationRejected)
	}
	if source == model.AssertionOpenLoop || target == model.AssertionOpenLoop {
		return fmt.Errorf("%w: an open loop is not closed by replacement; that needs its own state", ErrRelationRejected)
	}
	if !ReplacementAllowed(source, target) {
		return fmt.Errorf("%w: a %s may not be replaced by a %s", ErrRelationRejected, source, target)
	}
	return nil
}
