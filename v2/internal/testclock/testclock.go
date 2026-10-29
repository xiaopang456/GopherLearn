// Package testclock pins the machine-local timezone for the duration of one test.
//
// The propose_memory tool description renders the server's local clock
// (`now.In(time.Local)`), so a recorded contract fixture carries the +08:00 rendering it was
// captured with. A test that compares against such a fixture must pin the zone it asserts
// against: on a runner whose local zone is UTC the same instant renders as `...Z`, the
// comparison fails, and the failure says nothing about the contract it was meant to check.
package testclock

import (
	"testing"
	"time"
)

// PinLocalCST sets time.Local to the fixed +08:00 zone the recorded fixtures were captured in
// and restores the previous value when the test ends.
func PinLocalCST(t testing.TB) {
	t.Helper()
	previous := time.Local
	time.Local = time.FixedZone("CST", 8*60*60)
	t.Cleanup(func() { time.Local = previous })
}
