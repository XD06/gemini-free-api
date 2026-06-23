package openai

import (
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Options(
	fx.Provide(NewOpenAIService),
	fx.Provide(NewOpenAIController),
	fx.Invoke(RegisterRoutes),
)

func RegisterRoutes(app *fiber.App, c *OpenAIController, log *zap.Logger) {
	c.SetLogger(log)
	// OpenAI routes (prefixed with /openai)
	openaiGroup := app.Group("/openai")
	openaiV1 := openaiGroup.Group("/v1")
	c.Register(openaiV1)
}
