package providers

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Options(
	fx.Provide(NewProviderManager),
	fx.Provide(NewGeminiClient),
	fx.Invoke(RegisterProvider),
)

func RegisterProvider(lc fx.Lifecycle, pm *ProviderManager, c GeminiClient, log *zap.Logger) {
	pm.Register("gemini", c)

	// Select Gemini as the active provider immediately.
	// This is pure in-memory (no I/O), so it's safe to call before Init.
	if err := pm.SelectProvider("gemini"); err != nil {
		log.Error("Failed to select Gemini provider", zap.Error(err))
	}

	// Initialize the provider in a background goroutine so that the HTTP
	// server starts listening right away. Init() makes HTTP requests to
	// Gemini (through proxy) to fetch session tokens; for multi-account
	// pools it iterates sequentially, which can take tens of seconds.
	//
	// Until Init completes, API requests will fail with auth errors and
	// the background refresh mechanism (refreshInvalidClientsAsync /
	// refreshClientAsync) will kick in to recover automatically.
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				log.Info("Gemini provider initialization started (background)")
				if err := c.Init(context.Background()); err != nil {
					log.Error("Gemini provider initialization failed (will retry in background)", zap.Error(err))
				} else {
					log.Info("Gemini provider initialization completed")
				}
			}()
			return nil
		},
	})
}
