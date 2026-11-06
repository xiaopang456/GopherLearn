package config

import (
	"errors"
	"fmt"
)

// Refusal is a configuration refusal whose sentence is safe to put in front of
// an operator: it names sections, keys and the change to make, and quotes no
// value from the file or the environment.
//
// The startup entry prints a single generic line for every failure, because a
// failure coming out of a dependency can carry a configuration value in its
// message - a data source name, an address with credentials - and an operator's
// terminal is not where that belongs. That is correct for dependencies and wrong
// for configuration refusals: a deployment whose file still holds a retired key
// gets one sentence with nothing in it to act on, and the reason it will not
// start is precisely the thing it has to change. So the refusals that name a key
// instead of a value are marked with this type, and only those are printed.
type Refusal struct {
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// Refusef builds a Refusal. Callers must pass a sentence that contains key
// paths, mode names and numbers only - never a value read from the file or the
// environment.
func Refusef(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// SafeRefusalMessage returns the operator-facing sentence when err is, or wraps,
// a Refusal - and an empty string for anything else, which the caller must then
// report with its generic line.
func SafeRefusalMessage(err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) && refusal != nil {
		return refusal.Message
	}
	return ""
}
