package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	callledger "github.com/xiaopang456/GopherLearn/eval/internal/calls"
)

// RoleUsage sums only explicitly reported fields. Unknown quantities are
// counted as unmetered calls; a zero reported sum is never a zero total claim.
type RoleUsage struct {
	Calls               int   `json:"calls"`
	ReportedInput       int64 `json:"reported_input_tokens"`
	ReportedOutput      int64 `json:"reported_output_tokens"`
	ReportedCachedInput int64 `json:"reported_cached_input_tokens"`
	UnmeteredInput      int   `json:"unmetered_input_calls"`
	UnmeteredOutput     int   `json:"unmetered_output_calls"`
	UnmeteredCache      int   `json:"unmetered_cache_calls"`
	ReportedReasoning   int64 `json:"reported_reasoning_tokens"`
	UnmeteredReasoning  int   `json:"unmetered_reasoning_calls"`
}

// AttributeCalls keeps the operation's purpose while attributing every call
// after final-answer collection to background cost. No foreground work remains
// scheduled after that boundary in this one-question runner.
//
// The phase is derived from the answer boundary, not from the role the prompt
// suggested: a call the product sent after the answer came back is background
// work whatever its prompt looked like.
func AttributeCalls(calls []ModelCall, answered time.Time) []ModelCall {
	result := append([]ModelCall(nil), calls...)
	for i := range result {
		result[i].Purpose = result[i].Role
		if result[i].Started.After(answered) {
			result[i].Role = "background"
			result[i].Phase = callledger.PhaseAfterAnswer
		} else {
			result[i].Phase = callledger.PhaseAnswer
		}
	}
	return result
}

// BindRun stamps the identity of the run the calls belong to. A ledger that
// does not say which run a call was made for cannot be accounted for by run.
func BindRun(calls []ModelCall, sessionID, runID string) []ModelCall {
	result := append([]ModelCall(nil), calls...)
	for i := range result {
		result[i].SessionID = sessionID
		result[i].RunID = runID
	}
	return result
}

// BindRuns stamps the identity of the run every call belongs to.
//
// An attempt that asks one question has one run, and every call belongs to
// it. An attempt that asks several questions has one run per question, and a
// call belongs to the question whose window it was sent in. A call outside
// every window - the background work that starts after the last answer came
// back - is attributed to the last run, which is the only one it can belong
// to.
func BindRuns(calls []ModelCall, sessionID string, questions []QuestionResult) []ModelCall {
	result := append([]ModelCall(nil), calls...)
	for i := range result {
		result[i].SessionID = sessionID
		runID := ""
		for _, question := range questions {
			if question.RunID == "" || question.StartedAt.IsZero() {
				continue
			}
			if result[i].Started.Before(question.StartedAt) {
				continue
			}
			if !question.FinishedAt.IsZero() && result[i].Started.After(question.FinishedAt) {
				continue
			}
			runID = question.RunID
		}
		if runID == "" && len(questions) > 0 {
			runID = questions[len(questions)-1].RunID
		}
		result[i].RunID = runID
	}
	return result
}

// CallLedger turns the model ledger into the call ledger the attribution
// reads: one entry per call, carrying the identity of the run it belongs to
// and the phase it was sent in.
func CallLedger(calls []ModelCall) []callledger.Call {
	result := make([]callledger.Call, 0, len(calls))
	for _, call := range calls {
		entry := callledger.Call{
			At:        call.Started,
			Lane:      callLane(call.Method),
			Model:     call.Model,
			RunID:     call.RunID,
			SessionID: call.SessionID,
			Phase:     call.Phase,
		}
		if call.Usage != nil {
			prompt := int64(call.Usage.PromptTokens)
			completion := int64(call.Usage.CompletionTokens)
			total := int64(call.Usage.TotalTokens)
			entry.Usage = &callledger.Usage{Prompt: &prompt, Completion: &completion, Total: &total}
			if cached := call.Usage.PromptTokenDetails.CachedTokens; cached > 0 {
				value := int64(cached)
				entry.Usage.Cached = &value
			}
			if reasoning := call.Usage.CompletionTokensDetails.ReasoningTokens; reasoning > 0 {
				value := int64(reasoning)
				entry.Usage.Reasoning = &value
			}
		}
		result = append(result, entry)
	}
	return result
}

// callLane names the lane a call went through, so the attribution can tell a
// chat call from an embedding or a rerank call.
func callLane(method string) string {
	switch {
	case strings.Contains(method, "embeddings"):
		return "embed"
	case strings.Contains(method, "rerank"):
		return "rerank"
	case method == "generate" || method == "stream" || strings.Contains(method, "chat/completions"):
		return "chat"
	default:
		return "model"
	}
}

func SummarizeCalls(calls []ModelCall) (map[string]RoleUsage, error) {
	result := map[string]RoleUsage{}
	for _, call := range calls {
		switch call.Role {
		case "answer", "summary", "embedding", "rerank", "background":
		default:
			return nil, fmt.Errorf("unknown ledger role")
		}
		sum := result[call.Role]
		sum.Calls++
		var in, out, cache, reasoning *int64
		if len(call.RawUsageSamples) > 0 {
			var usage struct {
				Input   *int64 `json:"prompt_tokens"`
				Output  *int64 `json:"completion_tokens"`
				Details struct {
					Cached *int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CacheHit          *int64 `json:"prompt_cache_hit_tokens"`
				CompletionDetails struct {
					Reasoning *int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			}
			if err := json.Unmarshal(call.RawUsageSamples[len(call.RawUsageSamples)-1], &usage); err != nil {
				return nil, fmt.Errorf("invalid ledger usage")
			}
			in, out, cache = usage.Input, usage.Output, usage.Details.Cached
			if cache == nil {
				cache = usage.CacheHit
			}
			reasoning = usage.CompletionDetails.Reasoning
		} else if call.Usage != nil {
			// SDK-only warmup adapters expose total input/output usage, but cannot
			// distinguish absent cache details from a reported zero. Keep cache unknown.
			a, b := int64(call.Usage.PromptTokens), int64(call.Usage.CompletionTokens)
			in, out = &a, &b
		}
		for _, value := range []*int64{in, out, cache, reasoning} {
			if value != nil && *value < 0 {
				return nil, fmt.Errorf("negative ledger usage")
			}
		}
		if in == nil {
			sum.UnmeteredInput++
		} else {
			sum.ReportedInput += *in
		}
		if out == nil {
			sum.UnmeteredOutput++
		} else {
			sum.ReportedOutput += *out
		}
		if cache == nil {
			sum.UnmeteredCache++
		} else {
			sum.ReportedCachedInput += *cache
		}
		if reasoning == nil {
			sum.UnmeteredReasoning++
		} else {
			sum.ReportedReasoning += *reasoning
		}
		result[call.Role] = sum
	}
	return result, nil
}
