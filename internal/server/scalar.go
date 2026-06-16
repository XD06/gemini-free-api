package server

import (
	_ "embed"

	"github.com/gofiber/fiber/v3"
)

//go:embed static/openapi.json
var openapiSpec []byte

const scalarHTML = `<!doctype html>
<html>
  <head>
    <title>Gemini Web To API</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/openapi.json"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>`

func ScalarUI(c fiber.Ctx) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(scalarHTML)
}

func OpenAPISpec(c fiber.Ctx) error {
	c.Set("Content-Type", "application/json; charset=utf-8")
	return c.Send(openapiSpec)
}
