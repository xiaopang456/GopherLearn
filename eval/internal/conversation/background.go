package conversation

import (
	"fmt"
	"time"
)

// JobState is a point-in-time observation. Attempts includes failed executions.
type JobState struct {
	ID, Kind, Status                                            string
	Attempts                                                    int
	AvailableAt, CreatedAt, UpdatedAt, LeaseEndsAt, CompletedAt *time.Time
}

// BackgroundCall records the start of a model request, not the time it completed.
type BackgroundCall struct {
	Role    string
	Started time.Time
}

// BackgroundAudit is gathered by a monitoring system which must provide complete
// observation, not only periodic samples, for the entire protected interval.
//
// The audit has two phases. The preparation phase runs from Started to
// Prepared: the registered summary, projection and index tasks are allowed to
// run there, because the card arm writes its card on purpose before the
// question is asked. The query phase runs from Prepared to Answered: nothing
// may be generated there, because the source data and the memory version are
// fixed and a task that ran in between would have changed the answer's own
// ground.
type BackgroundAudit struct {
	Started, Prepared, Answered time.Time
	Before, After               []JobState
	Calls                       []BackgroundCall
	// Complete is true only if the caller verified uninterrupted job and proxy coverage
	// with a common clock. Incomplete or unattributed telemetry must fail closed.
	Complete bool
	// ExecutedInQueryPhase says that a task ran between the prepare barrier
	// and the answer barrier. A run that reports it is not decidable: the
	// answer was not asked against a fixed memory.
	ExecutedInQueryPhase bool
}

// ValidateBackground refuses to certify a run when evidence is incomplete, when
// a background task ran in the query phase, or when a model call was made
// between the two barriers. After-answer calls remain reportable.
func (a BackgroundAudit) ValidateBackground() error {
	if !a.Complete || a.Started.IsZero() || a.Prepared.IsZero() || a.Answered.IsZero() {
		return fmt.Errorf("background observation incomplete")
	}
	if a.Answered.Before(a.Started) || a.Prepared.Before(a.Started) || a.Answered.Before(a.Prepared) {
		return fmt.Errorf("background observation is out of order")
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
	if a.ExecutedInQueryPhase {
		return fmt.Errorf("background job executed in the query phase")
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
		// A background call may run in the preparation phase - that is where
		// the card is written - and after the answer, where it is reportable.
		// It may not run in the query phase: the memory the answer is asked
		// against has to be the memory that was fixed before the question.
		if c.Role == "background" && c.Started.After(a.Prepared) && !c.Started.After(a.Answered) {
			return fmt.Errorf("background call in the query phase")
		}
	}
	return nil
}
