package rag

import (
	"fmt"
	"strings"
)

type KnowledgePolicy string

const (
	KnowledgePreferred KnowledgePolicy = "knowledge_preferred"
	KnowledgeOnly      KnowledgePolicy = "knowledge_only"
	// KnowledgeDisabled is the per-turn answer of the "use my notes" switch, and
	// it is the only way to say "not this turn".
	//
	// It has to be a value rather than the absence of one. Two different things
	// collapse if it is not: the turn where the learner switched the library off,
	// and the turn where an older client simply did not say. An absent value is
	// already treated as KnowledgePreferred downstream, so an off turn would
	// reach the same run identity as a preferred turn - and the replay path would
	// hand back the first answer to a second, different question. The value is
	// also what the ledger records, so a turn can be audited for what it was
	// allowed to read.
	KnowledgeDisabled KnowledgePolicy = "knowledge_disabled"
)

func (p KnowledgePolicy) Valid() bool {
	return p == KnowledgePreferred || p == KnowledgeOnly || p == KnowledgeDisabled
}

// PermitsDocumentKnowledge reports whether a turn may reach the learner's own
// documents at all. The three decisions that must agree - whether the window
// reserves evidence tokens, whether the two evidence tools are registered, and
// whether the prompt names them - all read this, so a disabled turn cannot be
// half-enabled by one of them forgetting.
func (p KnowledgePolicy) PermitsDocumentKnowledge() bool {
	return p == KnowledgePreferred || p == KnowledgeOnly
}

// CarriesAnyCapability reports whether this policy describes a turn that may use
// a tool at all. An ordinary chat turn - the shape the switch selects when it is
// off - has none: one generation over the conversation, nothing else.
func (p KnowledgePolicy) CarriesAnyCapability() bool {
	return p != KnowledgeDisabled
}

func NormalizeKnowledgePolicy(value KnowledgePolicy, fallback KnowledgePolicy) KnowledgePolicy {
	value = KnowledgePolicy(strings.TrimSpace(string(value)))
	if value.Valid() {
		return value
	}
	return fallback
}

// ResolveKnowledgePolicy separates "not said" from "said something unrecognised".
//
// An empty value comes back empty so the caller can apply what the session
// actually permits - a legacy chat session has no library to turn on, an Agent
// session defaults to preferred. Anything else must be one of the known values
// or it is refused: silently folding a misspelt or stale value into a default
// once meant a turn that asked not to use the notes could still read them, and
// the direction of that mistake is the one that costs someone their privacy.
func ResolveKnowledgePolicy(value KnowledgePolicy) (KnowledgePolicy, error) {
	value = KnowledgePolicy(strings.TrimSpace(string(value)))
	if value == "" {
		return "", nil
	}
	if !value.Valid() {
		return "", fmt.Errorf("unknown knowledgePolicy %q", strings.TrimSpace(string(value)))
	}
	return value, nil
}

type AnswerBasis string

const (
	AnswerBasisConversation      AnswerBasis = "conversation"
	AnswerBasisPersonalKnowledge AnswerBasis = "personal_knowledge"
	AnswerBasisGeneralKnowledge  AnswerBasis = "general_knowledge"
	AnswerBasisRefused           AnswerBasis = "refused"
	AnswerBasisLegacyUnspecified AnswerBasis = "legacy_unspecified"
)

func (b AnswerBasis) Valid() bool {
	switch b {
	case AnswerBasisConversation, AnswerBasisPersonalKnowledge, AnswerBasisGeneralKnowledge,
		AnswerBasisRefused, AnswerBasisLegacyUnspecified:
		return true
	default:
		return false
	}
}

type KnowledgeStatus string

const (
	KnowledgeStatusNotRequired       KnowledgeStatus = "not_required"
	KnowledgeStatusGrounded          KnowledgeStatus = "grounded"
	KnowledgeStatusNoEvidence        KnowledgeStatus = "no_evidence"
	KnowledgeStatusUnavailable       KnowledgeStatus = "unavailable"
	KnowledgeStatusInvalidGrounding  KnowledgeStatus = "invalid_grounding"
	KnowledgeStatusLegacyUnspecified KnowledgeStatus = "legacy_unspecified"
)

func (s KnowledgeStatus) Valid() bool {
	switch s {
	case KnowledgeStatusNotRequired, KnowledgeStatusGrounded, KnowledgeStatusNoEvidence,
		KnowledgeStatusUnavailable, KnowledgeStatusInvalidGrounding, KnowledgeStatusLegacyUnspecified:
		return true
	default:
		return false
	}
}

type KnowledgeReason string

const (
	KnowledgeReasonNone             KnowledgeReason = "none"
	KnowledgeReasonSearchRequired   KnowledgeReason = "search_required"
	KnowledgeReasonNoActiveDocument KnowledgeReason = "no_active_document"
	KnowledgeReasonNoCandidate      KnowledgeReason = "no_candidate"
	KnowledgeReasonModelAbstained   KnowledgeReason = "model_abstained"
	KnowledgeReasonEmbedding        KnowledgeReason = "embedding_unavailable"
	KnowledgeReasonVector           KnowledgeReason = "vector_unavailable"
	KnowledgeReasonLexical          KnowledgeReason = "lexical_unavailable"
	KnowledgeReasonIndex            KnowledgeReason = "index_unavailable"
	KnowledgeReasonDocumentMetadata KnowledgeReason = "document_metadata_unavailable"
	KnowledgeReasonInvalidCitation  KnowledgeReason = "invalid_citation"
	KnowledgeReasonMissingCitation  KnowledgeReason = "missing_citation"
	KnowledgeReasonFallbackCitation KnowledgeReason = "fallback_citation"
	// KnowledgeReasonSearchNotPerformed says the turn answered without
	// retrieving from the learner's material at all.
	//
	// It is a distinct reason because "no evidence" and "no search" are
	// different facts, and a reader can act on them differently: one means the
	// material was searched and had nothing, the other means the material was
	// never consulted. The old vocabulary could only say the first, so a turn
	// that answered from general knowledge without searching was recorded as
	// though a search had found nothing - and the learner's own card said the
	// notes had nothing to say about it.
	KnowledgeReasonSearchNotPerformed KnowledgeReason = "search_not_performed"
)

func (r KnowledgeReason) Valid() bool {
	switch r {
	case KnowledgeReasonNone, KnowledgeReasonSearchRequired, KnowledgeReasonNoActiveDocument,
		KnowledgeReasonNoCandidate, KnowledgeReasonModelAbstained, KnowledgeReasonEmbedding,
		KnowledgeReasonVector, KnowledgeReasonLexical, KnowledgeReasonIndex,
		KnowledgeReasonDocumentMetadata, KnowledgeReasonInvalidCitation,
		KnowledgeReasonMissingCitation, KnowledgeReasonFallbackCitation,
		KnowledgeReasonSearchNotPerformed:
		return true
	default:
		return false
	}
}

func KnowledgeReasonFromUnavailable(reason RetrievalUnavailableReason) KnowledgeReason {
	switch reason {
	case RetrievalReasonEmbedding:
		return KnowledgeReasonEmbedding
	case RetrievalReasonVector:
		return KnowledgeReasonVector
	case RetrievalReasonLexical:
		return KnowledgeReasonLexical
	case RetrievalReasonDocumentMetadata:
		return KnowledgeReasonDocumentMetadata
	default:
		return KnowledgeReasonIndex
	}
}
