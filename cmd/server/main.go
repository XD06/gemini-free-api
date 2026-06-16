package main

import (
	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/modules"
	"gemini-free-api/internal/server"
	"gemini-free-api/pkg/logger"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

func main() {
	fx.New(
		fx.Provide(
			configs.New,
			func(cfg *configs.Config) (*zap.Logger, error) {
				return logger.New(cfg.LogLevel)
			},
		),
		server.Module,
		modules.Module,
		fx.NopLogger,
	).Run()
}
