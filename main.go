package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/universonic/xiaoya-emby/engine"
)

var (
	cfg = new(engine.Config)
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cfg.Command().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
