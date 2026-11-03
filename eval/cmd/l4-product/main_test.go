package main

import (
	"io"
	"testing"
)

func TestRefusesOnlineOrImplicitMode(t *testing.T) {
	for _, args := range [][]string{nil, {"-env", "real"}, {"-env", "fake", "-phase", "online"}} {
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
