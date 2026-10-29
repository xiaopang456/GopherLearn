package rag

import "strings"

type KnowledgePolicy string

const (
	KnowledgePreferred KnowledgePolicy = "knowledge_preferred"
	KnowledgeOnly      KnowledgePolicy = "knowledge_only"
)

func (p KnowledgePolicy) Valid() bool {
	return p == KnowledgePreferred || p == KnowledgeOnly
}

func NormalizeKnowledgePolicy(value KnowledgePolicy, fallback KnowledgePolicy) KnowledgePolicy {
	value = KnowledgePolicy(strings.TrimSpace(string(value)))
	if value.Valid() {
		return value
	}
	return fallback
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
)

func (r KnowledgeReason) Valid() bool {
	switch r {
	case KnowledgeReasonNone, KnowledgeReasonSearchRequired, KnowledgeReasonNoActiveDocument,
		KnowledgeReasonNoCandidate, KnowledgeReasonModelAbstained, KnowledgeReasonEmbedding,
		KnowledgeReasonVector, KnowledgeReasonLexical, KnowledgeReasonIndex,
		KnowledgeReasonDocumentMetadata, KnowledgeReasonInvalidCitation,
		KnowledgeReasonMissingCitation, KnowledgeReasonFallbackCitation:
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
