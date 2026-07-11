package claude

import (
	"bufio"
	"context"
	"sync"
	"time"

	common "gemini-free-api/internal/commons/utils"
	"gemini-free-api/internal/modules/admin"
	"gemini-free-api/internal/modules/claude/dto"
	"gemini-free-api/internal/modules/providers"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type ClaudeController struct {
	service *ClaudeService
	log     *zap.Logger
	mu      sync.RWMutex
}

func NewClaudeController(service *ClaudeService) *ClaudeController {
	return &ClaudeController{
		service: service,
		log:     zap.NewNop(),
	}
}

// SetLogger sets the logger for this handler
func (h *ClaudeController) SetLogger(log *zap.Logger) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = log
}

// HandleModels returns a list of Claude models
// @Summary List Claude Models
// @Description Returns a list of available Claude models
// @Tags Claude
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /claude/v1/models [get]
func (h *ClaudeController) HandleModels(c fiber.Ctx) error {
	models := h.service.ListModels()
	data := []fiber.Map{}
	for _, m := range models {
		data = append(data, fiber.Map{
			"id":           m.ID,
			"type":         "model",
			"created_at":   m.Created,
			"display_name": m.ID,
		})
	}
	return c.JSON(fiber.Map{
		"data": data,
	})
}

// HandleModelByID returns a specific Claude model by ID
// @Summary Get Claude Model
// @Description Get details of a specific Claude model
// @Tags Claude
// @Accept json
// @Produce json
// @Param model_id path string true "Model ID"
// @Success 200 {object} map[string]interface{}
// @Router /claude/v1/models/{model_id} [get]
func (h *ClaudeController) HandleModelByID(c fiber.Ctx) error {
	modelID := c.Params("model_id")
	return c.JSON(fiber.Map{
		"id":           modelID,
		"type":         "model",
		"created_at":   time.Now().Unix(),
		"display_name": modelID,
	})
}

// HandleMessages handles the main chat endpoint
// @Summary Send Message (Claude)
// @Description Sends a message to the Claude model. Supports both standard JSON response and streaming (SSE) response.
// @Tags Claude
// @Accept json
// @Produce json
// @Produce text/event-stream
// @Param request body dto.MessageRequest true "Message Request"
// @Success 200 {object} dto.MessageResponse
// @Success 200 {string} string "SSE stream of dto.StreamEvent JSON objects"
// @Failure 400 {object} map[string]interface{}
// @Failure 500 {object} map[string]interface{}
// @Router /claude/v1/messages [post]
func (h *ClaudeController) HandleMessages(c fiber.Ctx) error {
	requestID := uuid.NewString()
	startTime := time.Now()
	userAgent := string(c.Request().Header.UserAgent())

	var req dto.MessageRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"type":  "error",
			"error": fiber.Map{"type": "invalid_request_error", "message": "Invalid JSON body"},
		})
	}

	if req.Stream {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		c.Set("X-Accel-Buffering", "no")

		c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			ctx = providers.ContextWithRetryBudget(ctx, 4)
			ctx, accountHolder := providers.ContextWithAccountID(ctx)

			var firstByteTime time.Time
			var firstByteRecorded bool

			err := h.service.GenerateMessageStream(ctx, req, func(ev dto.StreamEvent) bool {
				if !firstByteRecorded {
					firstByteTime = time.Now()
					firstByteRecorded = true
				}
				return common.SendSSEChunk(w, h.log, ev.Type, ev) == nil
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
				h.log.Error("GenerateMessageStream failed", zap.Error(err), zap.String("model", req.Model))
				errEv := dto.StreamEvent{
					Type: "error",
					Error: &dto.Error{
						Type:    "api_error",
						Message: err.Error(),
					},
				}
				_ = common.SendSSEChunk(w, h.log, "error", errEv)
			}

			admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
				ID:               requestID,
				Timestamp:        startTime,
				Model:            req.Model,
				Stream:           true,
				AccountID:        accountHolder.Get(),
				Status:           status,
				ErrorMessage:     errMsg,
				Duration:         duration,
				FirstByteLatency: firstByteLatency,
				UserAgent:        userAgent,
				RequestPath:      "/claude/v1/messages",
			})
		})

		return nil
	}

	// Non-streaming mode
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx = providers.ContextWithRetryBudget(ctx, 4)
	ctx, accountHolder := providers.ContextWithAccountID(ctx)

	response, err := h.service.GenerateMessage(ctx, req)
	duration := time.Since(startTime).Milliseconds()

	if err != nil {
		h.log.Error("GenerateMessage failed", zap.Error(err), zap.String("model", req.Model))
		admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
			ID:               requestID,
			Timestamp:        startTime,
			Model:            req.Model,
			Stream:           false,
			AccountID:        accountHolder.Get(),
			Status:           "error",
			ErrorMessage:     err.Error(),
			Duration:         duration,
			FirstByteLatency: duration,
			UserAgent:        userAgent,
			RequestPath:      "/claude/v1/messages",
		})
		return c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{
			"type":  "error",
			"error": fiber.Map{"type": "api_error", "message": err.Error()},
		})
	}

	admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
		ID:               requestID,
		Timestamp:        startTime,
		Model:            req.Model,
		Stream:           false,
		AccountID:        accountHolder.Get(),
		Status:           "success",
		Duration:         duration,
		FirstByteLatency: duration,
		UserAgent:        userAgent,
		RequestPath:      "/claude/v1/messages",
	})

	return c.JSON(response)
}

// HandleCountTokens handles token counting
// @Summary Count Tokens (Claude)
// @Description Estimates the number of tokens for a request
// @Tags Claude
// @Accept json
// @Produce json
// @Param request body dto.MessageRequest true "Message Request"
// @Success 200 {object} map[string]interface{}
// @Router /claude/v1/messages/count_tokens [post]
func (h *ClaudeController) HandleCountTokens(c fiber.Ctx) error {
	var req dto.MessageRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"type":  "error",
			"error": fiber.Map{"type": "invalid_request_error", "message": "Invalid JSON body"},
		})
	}

	// Simple estimation
	totalChars := len(req.System)
	for _, m := range req.Messages {
		totalChars += len(m.Content)
	}

	return c.JSON(fiber.Map{
		"input_tokens": totalChars / 4,
	})
}

// Register registers the Claude routes onto the provided group
func (c *ClaudeController) Register(group fiber.Router) {
	group.Get("/models", c.HandleModels)
	group.Get("/models/:model_id", c.HandleModelByID)
	group.Post("/messages", c.HandleMessages)
	group.Post("/messages/count_tokens", c.HandleCountTokens)
}
