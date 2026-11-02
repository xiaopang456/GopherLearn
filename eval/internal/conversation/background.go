package conversation

import (
	"fmt"
	"time"
)

// JobState is a point-in-time observation. Attempts includes failed executions.
type JobState struct {
	ID, Kind, Status string
	Attempts         int
}

// BackgroundCall records the start of a model request, not the time it completed.
type BackgroundCall struct {
	Role    string
	Started time.Time
}

// BackgroundAudit is gathered by a monitoring system which must provide complete
// observation, not only periodic samples, for the entire protected interval.
type BackgroundAudit struct {
	Started, Answered time.Time
	Before, After     []JobState
	Calls             []BackgroundCall
	// Complete is true only if the caller verified uninterrupted job and proxy coverage
	// with a common clock. Incomplete or unattributed telemetry must fail closed.
	Complete             bool
	ExecutedBeforeAnswer bool
}

// ValidateBackground refuses to certify a run when evidence is incomplete or a
// background task ran before the answer. After-answer calls remain reportable.
func (a BackgroundAudit) ValidateBackground() error {
	if !a.Complete || a.Started.IsZero() || a.Answered.IsZero() || a.Answered.Before(a.Started) {
		return fmt.Errorf("background observation incomplete")
	}
	if _, err := CountJobs(a.Before); err != nil {
		return fmt.Errorf("before jobs: %w", err)
	}
	if _, err := CountJobs(a.After); err != nil {
		return fmt.Errorf("after jobs: %w", err)
	}
	for _, j := range a.Before {
		if j.ID == "" || j.Kind == "" || j.Status != "pending" || j.Attempts != 0 {
			return fmt.Errorf("job already executed before question")
		}
	}
	if a.ExecutedBeforeAnswer {
		return fmt.Errorf("background job executed before answer")
	}
	for _, c := range a.Calls {
		if c.Started.IsZero() {
			return fmt.Errorf("unattributed model call")
		}
		switch c.Role {
		case "answer", "summary", "background", "embedding", "rerank":
		default:
			return fmt.Errorf("unattributed model call")
		}
		if c.Started.Before(a.Started) {
			return fmt.Errorf("call outside observed interval")
		}
		if c.Role == "background" && !c.Started.After(a.Answered) {
			return fmt.Errorf("background call before answer")
		}
	}
	return nil
}
