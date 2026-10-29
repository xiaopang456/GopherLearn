package episoderelation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"GopherAI/model"
)

// The classifier boundary.
//
// The model decides one thing: which of five words describes a pair. It does
// not write a state, does not name a row, does not choose whether anything is
// applied, and cannot reach the governor. Everything it returns is validated
// server-side against the pair the server itself assembled, so a classifier
// that answered about a different pair, invented an id, or returned a word
// outside the set is refused rather than interpreted.
//
// That separation is the point. STABILIZE-062 spent three versions learning
// that a component which reads free text must return a closed value, and that
// the closed value must be checked by something that did not take the model's
// word for what it was looking at.

// ClassifyRequest is exactly what a classifier may see about one pair.
//
// The fields are the minimum a relation decision needs: two bodies, two kinds,
// and which came first. There is no gold label, no expected relation, no
// subject id, no experiment tag and no session identity, because a request
// that could carry one eventually will.
type ClassifyRequest struct {
	// PairID is a per-request opaque handle. The classifier must echo it, so
	// an answer about another pair is detectable; it encodes nothing.
	PairID string `json:"pair_id"`
	// Earlier and Later are ordered by the server, never by the model.
	Earlier ClassifySide `json:"earlier"`
	Later   ClassifySide `json:"later"`
}

// ClassifySide is one end of the pair as the classifier sees it.
type ClassifySide struct {
	Kind model.AssertionKind `json:"kind"`
	Text string              `json:"text"`
}

// ClassifyResponse is the closed answer.
type ClassifyResponse struct {
	PairID string                      `json:"pair_id"`
	Kind   model.AssertionRelationKind `json:"relation"`
}

// Classifier decides one pair. Implementations may be a scripted local, or a
// metered provider client in a later stage; nothing above this interface
// knows which.
type Classifier interface {
	// Identity names the classifier for the audit row. It is a server-chosen
	// constant, never text the model produced.
	Identity() string
	Classify(ctx context.Context, request ClassifyRequest) (ClassifyResponse, error)
}

// ErrClassifierProtocol is the closed failure for an answer that cannot be
// used: a wrong pair, an unknown word, an empty reply.
//
// It is separate from a transport failure on purpose. A classifier that
// answered about the wrong pair and a classifier that could not be reached are
// different problems, and a round that merged them would report a protocol
// defect as an outage.
var ErrClassifierProtocol = fmt.Errorf("relation classifier protocol failure")

// ValidateResponse checks an answer against the request the server sent.
//
// It reads the model's output and returns either one closed word or a
// refusal; nothing downstream ever sees the raw response.
func ValidateResponse(request ClassifyRequest, response ClassifyResponse) (model.AssertionRelationKind, error) {
	if strings.TrimSpace(response.PairID) != strings.TrimSpace(request.PairID) {
		return "", fmt.Errorf("%w: the answer names pair %q, the request was %q",
			ErrClassifierProtocol, response.PairID, request.PairID)
	}
	if !model.ValidAssertionRelationKind(response.Kind) {
		return "", fmt.Errorf("%w: %q is not one of the five relations",
			ErrClassifierProtocol, response.Kind)
	}
	return response.Kind, nil
}

// AbstainOnProtocolFailure is what a refused answer becomes.
//
// Not a guess, and not an error that aborts the Session's projection. A pair
// the classifier could not answer about is a pair nothing is known about, and
// the honest record of that is uncertain - which changes no state and leaves
// the new assertion unknown.
const AbstainOnProtocolFailure = model.RelationUncertain

// OpaquePairHandle turns a server-side pair key into a handle a classifier may
// hold.
//
// The key names an owner and two assertion ids. A classifier that saw it would
// be seeing internal identity it has no use for - and identity that travels to
// a model is identity that ends up in a log. The digest keeps the one property
// the handle exists for: an answer about a different pair does not match.
func OpaquePairHandle(pairKey string) string {
	sum := sha256.Sum256([]byte(pairKey))
	return "pair_" + hex.EncodeToString(sum[:8])
}
