package rag

import "fmt"

// RetrievalMode selects which candidate channels a retrieval uses.
//
// The zero value is the production behaviour: the vector channel and the
// keyword channel both run and are fused with reciprocal-rank fusion.
type RetrievalMode string

const (
	// RetrievalModeHybrid runs both channels and fuses them. It is the zero value.
	RetrievalModeHybrid RetrievalMode = ""
	// RetrievalModeVector runs only the vector channel. The keyword search does not run.
	RetrievalModeVector RetrievalMode = "vector"
	// RetrievalModeKeyword runs only the keyword channel. The query is not embedded
	// and the vector search does not run.
	RetrievalModeKeyword RetrievalMode = "keyword"
)

// RerankPolicy decides whether the attached reranker is applied.
//
// The zero value is the production rule.
type RerankPolicy int

const (
	// RerankDefault is the production rule: rerank when a reranker is attached and
	// TopK <= DefaultRerankMaxTopK. Larger requests are candidate-pool requests
	// (document-filtered searches fetch 100, learning evidence fetches 30) and do
	// not pay for a rerank.
	RerankDefault RerankPolicy = iota
	// RerankForceOn reranks whenever a reranker is attached, whatever TopK is.
	// It requires TopK <= RerankCandidateK, because the reranker only sees the
	// top RerankCandidateK fused candidates.
	RerankForceOn
	// RerankForceOff never reranks. An attached reranker is reported as bypassed,
	// exactly as it is for a request above DefaultRerankMaxTopK.
	RerankForceOff
)

// DefaultRerankMaxTopK is the largest TopK the production rule reranks. It is the
// value the literal 8 had inside Retrieve; naming it changes nothing.
const DefaultRerankMaxTopK = 8

// maxRetrievalTopK is the largest TopK and candidate depth a retrieval accepts.
const maxRetrievalTopK = 100

// RerankStatus says what happened to the reranker in one retrieval.
type RerankStatus string

const (
	// RerankNotAttached means the service has no reranker.
	RerankNotAttached RerankStatus = "not_attached"
	// RerankBypassed means a reranker is attached but this retrieval did not use it.
	RerankBypassed RerankStatus = "bypassed"
	// RerankApplied means the reranker ordered the candidates.
	RerankApplied RerankStatus = "applied"
	// RerankFallback means the reranker was unavailable and the fused order was kept.
	RerankFallback RerankStatus = "fallback"
)

// RetrieveOptions are the knobs of RetrieveWithOptions. Every zero value is the
// production behaviour, so a caller that sets only TopK gets exactly what
// Retrieve returns.
type RetrieveOptions struct {
	// TopK is the number of results to return, 1..100. Required.
	TopK int
	// Mode selects the candidate channels. Zero is hybrid.
	Mode RetrievalMode
	// Rerank decides whether the reranker runs. Zero is the production rule.
	Rerank RerankPolicy
	// CandidateDepth is how many candidates each channel fetches. Zero uses the
	// production formula: max(TopK*6, 30), or 48 when the reranker will run,
	// capped at 100. A positive value is used as given, 1..100.
	CandidateDepth int
	// WantCandidates also returns the fused candidate pool the reranker would
	// see (at most RerankCandidateK candidates, fused order, before any rerank),
	// whether or not the reranker runs.
	WantCandidates bool
}

// RetrieveOutcome is what RetrieveWithOptions returns.
type RetrieveOutcome struct {
	// Results is the final top-K, the same value Retrieve returns for the same
	// effective settings. It is nil when the retrieval failed.
	Results []RetrievalResult
	// Candidates is the fused pool before any rerank. Set only with WantCandidates.
	Candidates []RetrievalResult
	// RerankStatus says whether the reranker ran, was bypassed, or fell back.
	RerankStatus RerankStatus
	// EffectiveCandidateDepth is the depth each channel actually fetched. It is
	// zero when the retrieval ended before any search ran.
	EffectiveCandidateDepth int
}

// retrievalDiagnostics carries the extra outputs of retrieve. The production
// path passes nil and pays nothing for them.
type retrievalDiagnostics struct {
	candidates   []RetrievalResult
	rerankStatus RerankStatus
	depth        int
}

func (m RetrievalMode) valid() bool {
	return m == RetrievalModeHybrid || m == RetrievalModeVector || m == RetrievalModeKeyword
}

func (p RerankPolicy) valid() bool {
	return p == RerankDefault || p == RerankForceOn || p == RerankForceOff
}

// validate rejects option combinations that cannot be honoured. The zero value
// of RetrieveOptions (apart from TopK) always passes.
func (o RetrieveOptions) validate(rerankerAttached bool) error {
	switch {
	case !o.Mode.valid():
		return fmt.Errorf("%w: unknown retrieval mode %q", ErrInvalidRetrievalRequest, string(o.Mode))
	case !o.Rerank.valid():
		return fmt.Errorf("%w: unknown rerank policy %d", ErrInvalidRetrievalRequest, int(o.Rerank))
	case o.CandidateDepth < 0 || o.CandidateDepth > maxRetrievalTopK:
		return fmt.Errorf("%w: candidate depth %d is outside 0..%d", ErrInvalidRetrievalRequest, o.CandidateDepth, maxRetrievalTopK)
	case o.Rerank == RerankForceOn && !rerankerAttached:
		return fmt.Errorf("%w: rerank was forced on but no reranker is attached", ErrInvalidRetrievalRequest)
	case o.Rerank == RerankForceOn && o.TopK > RerankCandidateK:
		return fmt.Errorf("%w: a forced rerank needs TopK <= %d, got %d", ErrInvalidRetrievalRequest, RerankCandidateK, o.TopK)
	}
	return nil
}
