package claude

import (
	"github.com/gofiber/fiber/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

var Module = fx.Options(
	fx.Provide(NewClaudeService),
	fx.Provide(NewClaudeController),
	fx.Invoke(RegisterRoutes),
)

func RegisterRoutes(app *fiber.App, c *ClaudeController, log *zap.Logger) {
	c.SetLogger(log)
	// Claude routes (prefixed with /claude)
	claudeGroup := app.Group("/claude")
	claudeV1 := claudeGroup.Group("/v1")
	c.Register(claudeV1)
}
