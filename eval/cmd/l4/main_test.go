package main

import (
	"io"
	"testing"
)

func TestRefusesImplicitOrRealEnvironment(t *testing.T) {
	for _, args := range [][]string{nil, {"-env", "real"}, {"-env", "fake", "unexpected"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
}
