package rag

import (
	"errors"

	"GopherAI/config"
)

// OperatorMessage explains a rerank failure in one fixed sentence.
//
// The sentences name what is wrong and where to fix it, and never quote the
// endpoint, the token or any other configuration value. That is what lets the
// entry print them on a terminal: a startup error usually reaches a log or a
// console, and a configuration value in it is a leak.
//
// An error that is not about the reranker yields an empty string, which the
// caller reads as "say nothing specific here".
func OperatorMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, config.ErrRerankConfig):
		return "the rerank configuration is invalid: check ragModelConfig.rerankEnabled, rerankProvider, rerankEndpoint, rerankModel and rerankTimeoutMs, and set RAG_RERANK_AUTH_TOKEN in the environment (at least 32 bytes)"
	case errors.Is(err, ErrRerankRejected):
		return "the rerank service refused the credential: check that RAG_RERANK_AUTH_TOKEN holds the key this service expects"
	case errors.Is(err, ErrRerankRateLimited):
		return "the rerank service is rate limiting this deployment: retry later or raise the provider quota"
	case errors.Is(err, ErrRerankTimeout):
		return "the rerank service did not answer inside the configured timeout: raise ragModelConfig.rerankTimeoutMs or use a faster rerank service"
	case errors.Is(err, ErrRerankIdentity):
		return "the rerank service serves a different model than this deployment expects: start it with the expected model, or point rerankEndpoint at the service that serves it"
	case errors.Is(err, ErrRerankConfig):
		return "the rerank configuration is invalid: check ragModelConfig.rerankProvider, rerankEndpoint, rerankModel and rerankTimeoutMs"
	case errors.Is(err, ErrRerankUnavailable):
		return "the rerank service is unavailable: check that the endpoint is reachable and that the service is running"
	default:
		return ""
	}
}
