package main

import (
	"context"
	"fmt"
	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/modules"
	"gemini-free-api/internal/server"
	"gemini-free-api/pkg/logger"
	"os"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

func main() {
	app := fx.New(
		fx.Provide(
			configs.New,
			func(cfg *configs.Config) (*zap.Logger, error) {
				return logger.New(cfg.LogLevel)
			},
		),
		server.Module,
		modules.Module,
		fx.NopLogger,
	)

	startCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := app.Start(startCtx); err != nil {
		fmt.Fprintf(os.Stderr, "startup error: %v\n", err)
		os.Exit(1)
	}

	<-app.Done()

	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := app.Stop(stopCtx); err != nil {
		fmt.Fprintf(os.Stderr, "shutdown error: %v\n", err)
		os.Exit(1)
	}
}
