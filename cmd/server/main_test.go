package main

import (
	"testing"

	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/modules"
	"gemini-free-api/internal/server"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

func TestFxGraphValidates(t *testing.T) {
	err := fx.ValidateApp(
		fx.Provide(
			func() *configs.Config {
				return &configs.Config{
					Server: configs.ServerConfig{Port: "8787"},
					Gemini: configs.GeminiConfig{
						Accounts: []configs.GeminiAccountConfig{
							{ID: "default", Secure1PSID: "test", StayMinutes: 180, RefreshInterval: 2, MaxRetries: 1},
						},
					},
					RateLimit: configs.RateLimitConfig{Enabled: false},
				}
			},
			func() *zap.Logger { return zap.NewNop() },
		),
		server.Module,
		modules.Module,
		fx.NopLogger,
	)
	if err != nil {
		t.Fatal(err)
	}
}
