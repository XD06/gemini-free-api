package server

import (
	_ "embed"

	"github.com/gofiber/fiber/v3"
)

//go:embed static/console.html
var consoleHTML []byte

// ConsoleUI serves the account management console at /console.
// The page is a single-file SPA that calls /admin/* APIs directly.
func ConsoleUI(c fiber.Ctx) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.Send(consoleHTML)
}
