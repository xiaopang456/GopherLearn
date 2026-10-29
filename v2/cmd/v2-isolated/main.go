package main

import (
	"GopherAI/internal/isolated"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
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
