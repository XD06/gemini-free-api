package gemini

import (
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
)

var Module = fx.Options(
	fx.Provide(NewGeminiService),
	fx.Provide(NewGeminiController),
	fx.Invoke(RegisterRoutes),
)

func RegisterRoutes(app *fiber.App, c *GeminiController) {
	// Gemini routes (prefixed with /gemini)
	geminiGroup := app.Group("/gemini")
	geminiV1 := geminiGroup.Group("/v1beta")
	c.Register(geminiV1)
}
