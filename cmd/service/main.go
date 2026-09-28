// Command service runs the Opora backend gateway.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/100bench/OPORA/internal/app"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	runtime, err := app.NewRuntime(os.Getenv, logger)
	if err != nil {
		logger.Error("service configuration failed")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runtime.Run(ctx); err != nil {
		logger.Error("service stopped", "error", "runtime failure")
		os.Exit(1)
	}
}
