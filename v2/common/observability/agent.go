package observability

import (
	"log"
	"sync"
	"time"
)

// AgentRecorder keeps the counters of the agent's own model traffic.
//
// It has the same shape as MemoryRecorder: low-cardinality counters and a
// structured log line, so an ordinary Docker deployment can read them without
// a metrics backend. The two recorders stay apart because they answer
// different questions - one watches the memory pipeline, the other watches
// the calls the agent makes to the model provider.
type AgentRecorder struct {
	mu       sync.RWMutex
	counters map[string]uint64
	totals   map[string]int64
	logf     func(string, ...any)
}

func NewAgentRecorder(logf func(string, ...any)) *AgentRecorder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &AgentRecorder{counters: map[string]uint64{}, totals: map[string]int64{}, logf: logf}
}

// Agent is the process-wide recorder of the agent's model calls.
var Agent = NewAgentRecorder(log.Printf)

// ModelRetry records that a generation the provider did not process is about
// to be sent again.
//
// The status is the reason the provider gave, so a reader can tell a gateway
// that was unavailable from a connection that was refused without reading the
// provider's own text.
func (r *AgentRecorder) ModelRetry(reason string, pause time.Duration) {
	reason = boundedLabel(reason, "status_502", "status_503", "status_504", "transport", "other")
	r.add("agent_model_call_total{status=retried}", 1)
	r.add("agent_model_retry_total{reason="+reason+"}", 1)
	r.addTotal("agent_model_retry_backoff_ms", pause.Milliseconds())
	r.logf("agent_metric name=model_retry status=retried reason=%s backoff_ms=%d", reason, pause.Milliseconds())
}

// AgentSnapshot is an in-process, metadata-only view of the agent's counters.
type AgentSnapshot struct {
	Counters map[string]uint64
	Totals   map[string]int64
}

func (r *AgentRecorder) Snapshot() AgentSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := AgentSnapshot{Counters: make(map[string]uint64, len(r.counters)), Totals: make(map[string]int64, len(r.totals))}
	for key, value := range r.counters {
		result.Counters[key] = value
	}
	for key, value := range r.totals {
		result.Totals[key] = value
	}
	return result
}

func (r *AgentRecorder) add(key string, value uint64) {
	r.mu.Lock()
	r.counters[key] += value
	r.mu.Unlock()
}

func (r *AgentRecorder) addTotal(key string, value int64) {
	r.mu.Lock()
	r.totals[key] += value
	r.mu.Unlock()
}
