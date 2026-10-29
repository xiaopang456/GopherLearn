package episode

import (
	"context"
	"sync/atomic"
)

// allowAllVerifier is a test double for the tests that are about something
// other than semantic support - abstention, slots, identity, watermarks.
//
// It exists only here, in a _test file. There is deliberately no permissive
// verifier in production code: one would let a deployment look verified while
// checking nothing, which is the exact condition C1.1 removed.
type allowAllVerifier struct{ calls int64 }

func (v *allowAllVerifier) Verify(context.Context, SupportRequest) (SupportVerdict, error) {
	atomic.AddInt64(&v.calls, 1)
	return SupportEntailed, nil
}

func (v *allowAllVerifier) Calls() int { return int(atomic.LoadInt64(&v.calls)) }

// allowAll is the convenience constructor for call sites that never inspect it.
func allowAll() *allowAllVerifier { return &allowAllVerifier{} }
