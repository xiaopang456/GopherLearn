//go:build v2_entry_reference

// The reference entry of this module.
//
// It used to carry its own wiring: the ordinary AI helper factory, the
// ordinary routes and the ordinary chat endpoints, all of which were removed
// when the ordinary conversation path was retired. What is left of it is the
// same runtime the shipped isolated entry runs, so this file is the thin
// wrapper around that runtime rather than a second copy of the wiring.
//
// The build tag is what keeps this out of an ordinary build. The CI job for
// this module builds the tag as well, so this file cannot rot unnoticed.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"GopherAI/internal/isolated"
)

func main() {
	path := flag.String("config", "", "required independent V2 configuration file")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := isolated.Run(ctx, *path); err != nil {
		log.Printf("isolated V2 refused/stopped: %v", err)
	}
}
