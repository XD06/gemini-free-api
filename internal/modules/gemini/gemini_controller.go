package gemini

import (
	"bufio"
	"context"
	"fmt"
	"sync"
	"time"

	common "gemini-free-api/internal/commons/utils"
	"gemini-free-api/internal/modules/admin"
	"gemini-free-api/internal/modules/gemini/dto"
	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type GeminiController struct {
	service *GeminiService
	log     *zap.Logger
	mu      sync.RWMutex
}

func NewGeminiController(service *GeminiService) *GeminiController {
	return &GeminiController{
		service: service,
		log:     zap.NewNop(),
	}
}

// SetLogger sets the logger for this handler
func (h *GeminiController) SetLogger(log *zap.Logger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = log
}

// IsHealthy returns the health status of the underlying Gemini service
func (h *GeminiController) IsHealthy() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.service == nil {
		return false
	}
	return h.service.IsHealthy()
}

// HandleV1BetaModels returns the list of models in Gemini format
// @Summary List Gemini Models
// @Description Returns a list of models supported by the Gemini API
// @Tags Gemini
// @Accept json
// @Produce json
// @Success 200 {object} dto.GeminiModelsResponse
// @Router /gemini/v1beta/models [get]
func (h *GeminiController) HandleV1BetaModels(c fiber.Ctx) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	availableModels := h.service.ListModels()
	var geminiModels []dto.GeminiModel
	for _, m := range availableModels {
		geminiModels = append(geminiModels, dto.GeminiModel{
			Name:                       "models/" + m.ID,
			DisplayName:                m.ID,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
		})
	}
	return c.JSON(dto.GeminiModelsResponse{Models: geminiModels})
}

// HandleV1BetaGenerateContent handles the official Gemini generateContent endpoint
// @Summary Generate Content (Gemini)
// @Description Generates content using the Gemini model
// @Tags Gemini
// @Accept json
// @Produce json
// @Param model path string true "Model ID"
// @Param request body dto.GeminiGenerateRequest true "Generate Request"
// @Success 200 {object} dto.GeminiGenerateResponse
// @Router /gemini/v1beta/models/{model}:generateContent [post]
func (h *GeminiController) HandleV1BetaGenerateContent(c fiber.Ctx) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	requestID := uuid.NewString()
	startTime := time.Now()
	userAgent := string(c.Request().Header.UserAgent())

	model := c.Params("model")
	var req dto.GeminiGenerateRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(common.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx, accountHolder := providers.ContextWithAccountID(ctx)

	response, err := h.service.GenerateContent(ctx, model, req)
	duration := time.Since(startTime).Milliseconds()

	if err != nil {
		if err.Error() == "empty content" {
			return c.Status(fiber.StatusBadRequest).JSON(common.ErrorToResponse(err, "invalid_request_error"))
		}
		h.log.Error("GenerateContent failed", zap.Error(err), zap.String("model", model))
		admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
			ID:               requestID,
			Timestamp:        startTime,
			Model:            model,
			Stream:           false,
			AccountID:        accountHolder.Get(),
			Status:           "error",
			ErrorMessage:     err.Error(),
			Duration:         duration,
			FirstByteLatency: duration,
			UserAgent:        userAgent,
			RequestPath:      "/gemini/v1beta/models:generateContent",
		})
		return c.Status(fiber.StatusInternalServerError).JSON(common.ErrorToResponse(err, "api_error"))
	}

	admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
		ID:               requestID,
		Timestamp:        startTime,
		Model:            model,
		Stream:           false,
		AccountID:        accountHolder.Get(),
		Status:           "success",
		Duration:         duration,
		FirstByteLatency: duration,
		UserAgent:        userAgent,
		RequestPath:      "/gemini/v1beta/models:generateContent",
	})

	return c.JSON(response)
}

// HandleV1BetaStreamGenerateContent handles the official Gemini streaming endpoint
// @Summary Stream Generate Content (Gemini)
// @Description Streams generated content using the Gemini model
// @Tags Gemini
// @Accept json
// @Produce json
// @Param model path string true "Model ID"
// @Param request body dto.GeminiGenerateRequest true "Generate Request"
// @Success 200 {string} string "Chunked JSON response"
// @Router /gemini/v1beta/models/{model}:streamGenerateContent [post]
func (h *GeminiController) HandleV1BetaStreamGenerateContent(c fiber.Ctx) error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	requestID := uuid.NewString()
	startTime := time.Now()
	userAgent := string(c.Request().Header.UserAgent())

	model := c.Params("model")
	var req dto.GeminiGenerateRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(common.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	c.Set("Content-Type", "application/json")
	c.Set("Transfer-Encoding", "chunked")

	c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ctx, accountHolder := providers.ContextWithAccountID(ctx)

		var firstByteTime time.Time
		var firstByteRecorded bool

		err := h.service.GenerateContentStream(ctx, model, req, func(resp dto.GeminiGenerateResponse) bool {
			if !firstByteRecorded {
				firstByteTime = time.Now()
				firstByteRecorded = true
			}
			return common.SendStreamChunk(w, h.log, resp) == nil
		})

		duration := time.Since(startTime).Milliseconds()
		firstByteLatency := int64(0)
		if firstByteRecorded {
			firstByteLatency = firstByteTime.Sub(startTime).Milliseconds()
		}

		status := "success"
		errMsg := ""
		if err != nil {
			status = "error"
			errMsg = err.Error()
			h.log.Error("GenerateContentStream failed", zap.Error(err), zap.String("model", model))
			_ = common.SendStreamChunk(w, h.log, common.ErrorToResponse(err, "api_error"))
		}

		admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
			ID:               requestID,
			Timestamp:        startTime,
			Model:            model,
			Stream:           true,
			AccountID:        accountHolder.Get(),
			Status:           status,
			ErrorMessage:     errMsg,
			Duration:         duration,
			FirstByteLatency: firstByteLatency,
			UserAgent:        userAgent,
			RequestPath:      "/gemini/v1beta/models:streamGenerateContent",
		})
	})

	return nil
}

// Register registers the Gemini routes on the provided router
func (g *GeminiController) Register(group fiber.Router) {
	group.Get("/models", g.HandleV1BetaModels)
	group.Post("/models/:model\\:generateContent", g.HandleV1BetaGenerateContent)
	group.Post("/models/:model\\:streamGenerateContent", g.HandleV1BetaStreamGenerateContent)
}
