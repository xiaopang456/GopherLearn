package session

import "time"

// The database stores these timestamps with millisecond precision, and MySQL
// rounds a value it is given rather than truncating it. A timestamp written
// as 12:00:00.6005 comes back as 12:00:00.601.
//
// That difference is enough to make a comparison against the original
// nanosecond value come out the wrong way: a job that was written as due now
// reads back as due in half a millisecond, and a worker that claims it right
// away is told the job is not due yet. The failure is intermittent, which
// makes it look like a race rather than a precision problem.
//
// Every timestamp that goes into or comes out of a comparison is therefore
// quantized to the precision the column actually holds, on both sides of the
// comparison. Two values that were meant to be equal compare equal.
const summarySegmentTimePrecision = time.Millisecond

// quantizeSummarySegmentTime truncates a timestamp to the precision the
// database column holds.
//
// Truncation rather than rounding is deliberate: it can only move a value
// earlier, never later, so a timestamp that was due stays due.
func quantizeSummarySegmentTime(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.Truncate(summarySegmentTimePrecision)
}
