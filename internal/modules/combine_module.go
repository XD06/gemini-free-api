package modules

import (
	"gemini-free-api/internal/modules/admin"
	"gemini-free-api/internal/modules/claude"
	"gemini-free-api/internal/modules/gemini"
	"gemini-free-api/internal/modules/openai"
	"gemini-free-api/internal/modules/providers"
	"go.uber.org/fx"
)

var Module = fx.Options(
	admin.Module,
	gemini.Module,
	claude.Module,
	openai.Module,
	providers.Module,
)
