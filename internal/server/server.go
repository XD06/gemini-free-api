package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gemini-free-api/internal/commons/configs"
	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/limiter"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// New creates a new Fiber app instance
func NewGeminiFreeAPI(log *zap.Logger, cfg *configs.Config, gemini providers.GeminiClient) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      "Gemini Free API",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // streaming responses need no write deadline
		IdleTimeout:  30 * time.Second,
		BodyLimit:    32 * 1024 * 1024,
	})

	app.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization", "X-Requested-With", "x-api-key", "anthropic-version", "X-Cookie-Sync-Token"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowCredentials: false,
	}))

	app.Use(recover.New())

	if cfg.RateLimit.Enabled {
		app.Use(limiter.New(limiter.Config{
			Max:        cfg.RateLimit.MaxRequests,
			Expiration: time.Duration(cfg.RateLimit.WindowMs) * time.Millisecond,
			Next: func(c fiber.Ctx) bool {
				// Skip rate limiting for non-API routes: console UI, docs,
				// health check, and admin management endpoints.
				path := c.Path()
				if path == "/console" || path == "/docs" || path == "/openapi.json" || path == "/health" {
					return true
				}
				if strings.HasPrefix(path, "/admin/") {
					return true
				}
				return false
			},
		}))
	}

	app.Get("/docs", ScalarUI)
	app.Get("/openapi.json", OpenAPISpec)
	app.Get("/console", ConsoleUI)

	app.Get("/health", HealthCheck)
	app.Get("/ready", ReadinessCheck(gemini))

	return app
}

func HealthCheck(c fiber.Ctx) error {
	return c.Status(fiber.StatusOK).JSON(fiber.Map{
		"status":  "ok",
		"service": "gemini-free-api",
	})
}

func ReadinessCheck(gemini providers.GeminiClient) fiber.Handler {
	return func(c fiber.Ctx) error {
		total, healthy := 0, 0
		state := "unavailable"
		if manager, ok := gemini.(providers.AccountManager); ok {
			statuses := manager.ListAccountStatuses()
			total = len(statuses)
			for _, account := range statuses {
				if account.Healthy {
					healthy++
				}
				if account.State == providers.AccountStateRefreshing || account.State == providers.AccountStateUninitialized {
					state = "initializing"
				}
			}
		}
		ready := gemini != nil && gemini.IsHealthy()
		if ready {
			state = "ready"
		}
		status := fiber.StatusServiceUnavailable
		if ready {
			status = fiber.StatusOK
		}
		return c.Status(status).JSON(fiber.Map{"status": state, "accounts_total": total, "accounts_healthy": healthy})
	}
}

// Register404Handler registers the 404 handler for unmatched routes
// This must be called AFTER all other routes are registered
func Register404Handler(app *fiber.App) {
	app.All("*", func(c fiber.Ctx) error {
		method := c.Method()
		path := c.Path()
		return c.Status(fiber.StatusNotFound).JSON(fiber.Map{
			"status":  fiber.StatusNotFound,
			"error":   "Not Found",
			"message": fmt.Sprintf("Cannot %s %s", method, path),
		})
	})
}

// RegisterFiberLifecycle registers the Fiber app lifecycle hooks
func RegisterFiberLifecycle(lc fx.Lifecycle, app *fiber.App, cfg *configs.Config, log *zap.Logger) {
	port := cfg.Server.Port
	address := fmt.Sprintf(":%s", port)

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			Register404Handler(app)
			log.Info("Starting server", zap.String("address", address))
			// Start server in a goroutine to not block
			go func() {
				if err := app.Listen(address); err != nil {
					log.Error("Server error", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("Shutting down server")
			return app.ShutdownWithContext(ctx)
		},
	})
}
