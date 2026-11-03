package conversation

import (
	"encoding/json"
	"fmt"
	"time"
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
}

// AttributeCalls keeps the operation's purpose while attributing every call
// after final-answer collection to background cost. No foreground work remains
// scheduled after that boundary in this one-question runner.
func AttributeCalls(calls []ModelCall, answered time.Time) []ModelCall {
	result := append([]ModelCall(nil), calls...)
	for i := range result {
		result[i].Purpose = result[i].Role
		if result[i].Started.After(answered) {
			result[i].Role = "background"
		}
	}
	return result
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
		var in, out, cache *int64
		if len(call.RawUsageSamples) > 0 {
			var usage struct {
				Input   *int64 `json:"prompt_tokens"`
				Output  *int64 `json:"completion_tokens"`
				Details struct {
					Cached *int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			}
			if err := json.Unmarshal(call.RawUsageSamples[len(call.RawUsageSamples)-1], &usage); err != nil {
				return nil, fmt.Errorf("invalid ledger usage")
			}
			in, out, cache = usage.Input, usage.Output, usage.Details.Cached
		} else if call.Usage != nil {
			// SDK-only warmup adapters expose total input/output usage, but cannot
			// distinguish absent cache details from a reported zero. Keep cache unknown.
			a, b := int64(call.Usage.PromptTokens), int64(call.Usage.CompletionTokens)
			in, out = &a, &b
		}
		for _, value := range []*int64{in, out, cache} {
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
		result[call.Role] = sum
	}
	return result, nil
}
