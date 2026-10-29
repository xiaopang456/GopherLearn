package episode

import (
	"context"

	"GopherAI/model"
)

// The typed projection outcome, and the support verifier seam.
//
// STABILIZE-065 Phase 2-C1. Both exist because of the same defect: the chain
// had exactly one way to finish - produce a document - so "this session holds
// nothing worth remembering" could not be said, and "this sentence is not
// something the learner said" could not be checked.

// OutcomeKind is what a projection concluded. It is closed.
type OutcomeKind string

const (
	// OutcomeDocument means the session produced memory.
	OutcomeDocument OutcomeKind = "document"
	// OutcomeAbstained means the session was read and produced none.
	OutcomeAbstained OutcomeKind = "abstained"
)

// ProjectionOutcome is what a Generator returns.
//
// It is a typed result rather than a flag on the document on purpose. A bool
// would mean every caller has to remember to check it, and forgetting does not
// fail - it walks on with an empty document and writes it. An empty document
// would be worse still: EpisodeDocument.Validate already refuses an empty
// synopsis, so expressing abstention that way would mean weakening a check in
// order to add a meaning.
//
// Exactly one of the two payloads is meaningful, and which one is not something
// the caller has to infer.
type ProjectionOutcome struct {
	Kind OutcomeKind
	// Document is meaningful only when Kind is OutcomeDocument.
	Document model.EpisodeDocument
	// Abstain is meaningful only when Kind is OutcomeAbstained.
	Abstain AbstainRecord
}

// AbstainRecord is why a projection produced no memory.
type AbstainRecord struct {
	// Reason is a closed vocabulary value, never model free text.
	Reason model.AbstainReason
	// ConsideredUserMessages is how much of the learner's own input was read
	// before concluding there was nothing. Without it, an abstention from a
	// one-line session and an abstention from a forty-turn session look alike.
	ConsideredUserMessages int
}

// Generated wraps a document as a successful projection.
func Generated(document model.EpisodeDocument) ProjectionOutcome {
	return ProjectionOutcome{Kind: OutcomeDocument, Document: document}
}

// Abstained wraps a refusal to remember.
func Abstained(reason model.AbstainReason, consideredUserMessages int) ProjectionOutcome {
	return ProjectionOutcome{Kind: OutcomeAbstained,
		Abstain: AbstainRecord{Reason: reason, ConsideredUserMessages: consideredUserMessages}}
}

// SupportVerdict is the closed answer to "is this assertion entailed by the
// learner's own messages".
type SupportVerdict string

const (
	// SupportEntailed means the cited user messages say this.
	SupportEntailed SupportVerdict = "entailed"
	// SupportNotEntailed means they do not.
	SupportNotEntailed SupportVerdict = "not_entailed"
	// SupportUncertain means they cannot settle it. It is treated exactly like
	// not_entailed: an assertion nobody can confirm is not evidence about a
	// learner, and keeping it because the check was inconclusive would make the
	// check decorative.
	SupportUncertain SupportVerdict = "uncertain"
)

func (v SupportVerdict) Valid() bool {
	switch v {
	case SupportEntailed, SupportNotEntailed, SupportUncertain:
		return true
	}
	return false
}

// SupportRequest is everything a verifier is allowed to see.
//
// The narrowness is the design. The generator already saw the assistant turns,
// the retrieved knowledge, the previous summary and every other assertion, and
// it is the thing whose output is being checked. A verifier given the same
// context would be able to reproduce the same mistake for the same reasons; one
// that can only see the sentence and the messages it claims to rest on cannot.
type SupportRequest struct {
	// AssertionKind is the slot the entry is claimed to belong to.
	AssertionKind model.AssertionKind
	// AssertionText is the sentence under test.
	AssertionText string
	// UserMessages are the bodies of the learner messages this assertion cites,
	// and nothing else. No assistant text, no synopsis, no sibling assertion.
	UserMessages []string
}

// SupportVerifier is the second layer of the two-layer semantic boundary.
//
// The first layer is deterministic and already existed in part: the cited id
// must exist, must belong to this session and this owner, and must be a user
// turn. It answers "who was cited". It cannot answer "is this what they said",
// and the pass-1 audit found assertions that passed it while describing what
// the assistant had suggested - the pointer was a real learner question and the
// body was about something else entirely.
//
// It is an interface so this package does not depend on any particular
// verifier, and so a deployment can run without one. A nil verifier means the
// second layer is not configured, which is a deployment decision that has to be
// made deliberately - it is not the same as a verifier that failed, and a
// verifier that fails trims.
type SupportVerifier interface {
	Verify(ctx context.Context, request SupportRequest) (SupportVerdict, error)
}
