package openai

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	models "gemini-free-api/internal/commons/models"
	utils "gemini-free-api/internal/commons/utils"
	"gemini-free-api/internal/modules/admin"
	"gemini-free-api/internal/modules/openai/dto"

	"github.com/gofiber/fiber/v3"
	"go.uber.org/zap"
)

type OpenAIController struct {
	service *OpenAIService
	log     *zap.Logger
}

func NewOpenAIController(service *OpenAIService) *OpenAIController {
	return &OpenAIController{
		service: service,
		log:     zap.NewNop(),
	}
}

// SetLogger sets the logger for this handler
func (h *OpenAIController) SetLogger(log *zap.Logger) {
	h.log = log
}

// GetModelData returns raw model data for internal use (e.g. unified list)
func (h *OpenAIController) GetModelData() []models.ModelData {
	availableModels := h.service.ListModels()

	var data []models.ModelData
	for _, m := range availableModels {
		data = append(data, models.ModelData{
			ID:      m.ID,
			Object:  "model",
			Created: m.Created,
			OwnedBy: m.OwnedBy,
		})
	}
	return data
}

// HandleModels returns the list of supported models
// @Summary List OpenAI Models
// @Description Returns a list of models supported by the OpenAI-compatible API
// @Tags OpenAI
// @Accept json
// @Produce json
// @Success 200 {object} models.ModelListResponse
// @Router /openai/v1/models [get]
func (h *OpenAIController) HandleModels(c fiber.Ctx) error {
	data := h.GetModelData()

	return c.JSON(models.ModelListResponse{
		Object: "list",
		Data:   data,
	})
}

// HandleChatCompletions accepts requests in OpenAI format
// @Summary Chat Completions (OpenAI)
// @Description Generates a completion for the chat message. Supports both standard JSON and streaming (SSE) response.
// @Tags OpenAI
// @Accept json
// @Produce json
// @Produce text/event-stream
// @Param request body dto.ChatCompletionRequest true "Chat Completion Request"
// @Success 200 {object} dto.ChatCompletionResponse
// @Success 200 {string} string "SSE stream of dto.ChatCompletionChunk JSON objects"
// @Failure 400 {object} map[string]interface{}
// @Failure 500 {object} map[string]interface{}
// @Router /openai/v1/chat/completions [post]
func (h *OpenAIController) HandleChatCompletions(c fiber.Ctx) error {
	requestID := generateChatID()
	startTime := time.Now()
	rawBody := append([]byte(nil), c.Body()...)
	bindBody := trimJSONBOM(rawBody)
	if len(bindBody) != len(rawBody) {
		c.Request().SetBody(bindBody)
	}
	var req dto.ChatCompletionRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}
	dumpOpenAIRawRequest(requestID, bindBody, h.log)
	if openAIRequestDebugEnabled() {
		h.log.Info("OpenAI chat request received",
			zap.String("request_id", requestID),
			zap.String("model", req.Model),
			zap.Bool("stream", req.Stream),
			zap.Bool("tools_enabled", req.HasToolsEnabled()),
			zap.Int("tool_count", len(req.Tools)),
			zap.String("tool_choice_mode", req.ToolChoiceMode()),
			zap.String("forced_tool_name", req.ForcedToolName()),
			zap.Bool("has_conversation_id", strings.TrimSpace(req.ConversationID) != ""),
			zap.String("conversation_id", strings.TrimSpace(req.ConversationID)),
			zap.Int("message_count", len(req.Messages)),
			zap.Any("messages", summarizeOpenAIMessages(req.Messages)),
		)
	}

	if req.Stream {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")
		c.Set("X-Accel-Buffering", "no")

		// Capture client info before starting stream
	userAgent := string(c.Request().Header.UserAgent())

	c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			ctx = context.WithValue(ctx, openAIRequestIDContextKey{}, requestID)

			var firstByteTime time.Time
			var firstByteRecorded bool

			traceForward := openAIStreamForwardTraceEnabled()
			streamErr := h.service.CreateChatCompletionStream(ctx, req, func(chunk dto.ChatCompletionChunk) bool {
				// Record first byte latency on first content chunk
				if !firstByteRecorded {
					firstByteTime = time.Now()
					firstByteRecorded = true
				}

				flushStart := time.Now()
				ok := utils.SendSSEEvent(w, h.log, chunk)
				if traceForward {
					contentLen, reasoningLen, toolCallCount, finishReason := summarizeStreamChunkForTrace(chunk)
					h.log.Info("OpenAI SSE flush trace",
						zap.String("request_id", requestID),
						zap.Bool("ok", ok),
						zap.Duration("flush_elapsed", time.Since(flushStart)),
						zap.Int("content_len", contentLen),
						zap.Int("reasoning_len", reasoningLen),
						zap.Int("tool_call_count", toolCallCount),
						zap.String("finish_reason", finishReason),
					)
				}
				return ok
			})

			duration := time.Since(startTime).Milliseconds()
			firstByteLatency := int64(0)
			if firstByteRecorded {
				firstByteLatency = firstByteTime.Sub(startTime).Milliseconds()
			}

			status := "success"
			errMsg := ""
			if streamErr != nil {
				status = "error"
				errMsg = streamErr.Error()
				h.log.Error("CreateChatCompletionStream failed", zap.Error(streamErr), zap.String("model", req.Model))
				errResponse := utils.ErrorToResponse(streamErr, "api_error")
				utils.SendSSEEvent(w, h.log, errResponse)
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				_ = w.Flush()
			} else {
				// Success - send DONE
				_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
				_ = w.Flush()
			}

			// Update request record (use captured values, not c.*)
			admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
				ID:               requestID,
				Timestamp:        startTime,
				Model:            req.Model,
				Stream:           req.Stream,
				Status:           status,
				ErrorMessage:     errMsg,
				Duration:         duration,
				FirstByteLatency: firstByteLatency,
				UserAgent:        userAgent,
				RequestPath:      "/v1/chat/completions",
			})
		})

		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	ctx = context.WithValue(ctx, openAIRequestIDContextKey{}, requestID)

	response, err := h.service.CreateChatCompletion(ctx, req)
	duration := time.Since(startTime).Milliseconds()

	status := "success"
	errMsg := ""
	tokensOutput := 0
	if err != nil {
		status = "error"
		errMsg = err.Error()
		h.log.Error("CreateChatCompletion failed", zap.Error(err), zap.String("model", req.Model))
		// Update request record with error
		admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
			ID:               requestID,
			Timestamp:        startTime,
			Model:            req.Model,
			Stream:           req.Stream,
			Status:           status,
			ErrorMessage:     errMsg,
			Duration:         duration,
			FirstByteLatency: duration, // For non-streaming, first byte = total duration
			UserAgent:        string(c.Request().Header.UserAgent()),
			RequestPath:      "/v1/chat/completions",
		})
		return c.Status(fiber.StatusInternalServerError).JSON(utils.ErrorToResponse(err, "api_error"))
	}

	tokensOutput = response.Usage.CompletionTokens

	// Update request record with success
	admin.GetGlobalLogger().LogRequest(admin.RequestRecord{
		ID:               requestID,
		Timestamp:        startTime,
		Model:            req.Model,
		Stream:           req.Stream,
		Status:           status,
		Duration:         duration,
		FirstByteLatency: duration, // For non-streaming, first byte = total duration
		TokensOutput:     tokensOutput,
		UserAgent:        string(c.Request().Header.UserAgent()),
		RequestPath:      "/v1/chat/completions",
	})

	return c.JSON(response)
}

// HandleImageGenerations accepts image generation requests in OpenAI format
// @Summary Image Generations (OpenAI)
// @Description Generates images from a text prompt.
// @Tags OpenAI
// @Accept json
// @Produce json
// @Param request body dto.ImageGenerationRequest true "Image Generation Request"
// @Success 200 {object} dto.ImageGenerationResponse
// @Failure 400 {object} map[string]interface{}
// @Failure 500 {object} map[string]interface{}
// @Router /openai/v1/images/generations [post]
func (h *OpenAIController) HandleImageGenerations(c fiber.Ctx) error {
	var req dto.ImageGenerationRequest
	if err := c.Bind().Body(&req); err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("invalid request body: %w", err), "invalid_request_error"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	response, err := h.service.CreateImageGeneration(ctx, req)
	if err != nil {
		if err.Error() == "prompt is required" || err.Error() == "n must be between 1 and 10" {
			return c.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(err, "invalid_request_error"))
		}
		h.log.Error("CreateImageGeneration failed", zap.Error(err), zap.String("model", req.Model))
		return c.Status(fiber.StatusInternalServerError).JSON(utils.ErrorToResponse(err, "api_error"))
	}

	return c.JSON(response)
}

func (h *OpenAIController) HandleUploadFile(c fiber.Ctx) error {
	file, err := c.FormFile("file")
	if err != nil {
		return c.Status(fiber.StatusBadRequest).JSON(utils.ErrorToResponse(fmt.Errorf("file is required"), "invalid_request_error"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	obj, err := h.service.fileStore.saveUploadedFile(ctx, file, c.FormValue("purpose"))
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(utils.ErrorToResponse(err, "api_error"))
	}
	return c.JSON(obj)
}

func (h *OpenAIController) HandleListFiles(c fiber.Ctx) error {
	files, err := h.service.fileStore.listFiles()
	if err != nil {
		return c.Status(fiber.StatusInternalServerError).JSON(utils.ErrorToResponse(err, "api_error"))
	}
	return c.JSON(fiber.Map{
		"object": "list",
		"data":   files,
	})
}

func (h *OpenAIController) HandleGetFile(c fiber.Ctx) error {
	obj, err := h.service.fileStore.getFile(c.Params("file_id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(utils.ErrorToResponse(err, "not_found_error"))
	}
	return c.JSON(obj)
}

func (h *OpenAIController) HandleDeleteFile(c fiber.Ctx) error {
	result, err := h.service.fileStore.deleteFile(c.Params("file_id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(utils.ErrorToResponse(err, "not_found_error"))
	}
	return c.JSON(result)
}

func (h *OpenAIController) HandleFileContent(c fiber.Ctx) error {
	data, filename, mimeType, err := h.service.fileStore.readFileContent(c.Params("file_id"))
	if err != nil {
		return c.Status(fiber.StatusNotFound).JSON(utils.ErrorToResponse(err, "not_found_error"))
	}
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "application/octet-stream"
	}
	c.Set("Content-Type", mimeType)
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, sanitizeStoredFilename(filename)))
	return c.Send(data)
}

// Register registers the OpenAI routes onto the provided group
func (c *OpenAIController) Register(group fiber.Router) {
	group.Get("/models", c.HandleModels)
	group.Post("/chat/completions", c.HandleChatCompletions)
	group.Post("/images/generations", c.HandleImageGenerations)
	group.Post("/files", c.HandleUploadFile)
	group.Get("/files", c.HandleListFiles)
	group.Get("/files/:file_id", c.HandleGetFile)
	group.Delete("/files/:file_id", c.HandleDeleteFile)
	group.Get("/files/:file_id/content", c.HandleFileContent)
}

func trimJSONBOM(body []byte) []byte {
	return bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
}

type openAIMessageLogSummary struct {
	Index           int    `json:"index"`
	Role            string `json:"role"`
	ContentLength   int    `json:"content_length"`
	ContentPreview  string `json:"content_preview,omitempty"`
	ToolCallCount   int    `json:"tool_call_count,omitempty"`
	ToolCallID      string `json:"tool_call_id,omitempty"`
	Name            string `json:"name,omitempty"`
	AttachmentCount int    `json:"attachment_count,omitempty"`
}

func summarizeOpenAIMessages(messages []dto.ChatCompletionMessage) []openAIMessageLogSummary {
	summaries := make([]openAIMessageLogSummary, 0, len(messages))
	for i, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		summaries = append(summaries, openAIMessageLogSummary{
			Index:           i,
			Role:            msg.Role,
			ContentLength:   len(content),
			ContentPreview:  previewForLog(content, 160),
			ToolCallCount:   len(msg.ToolCalls),
			ToolCallID:      strings.TrimSpace(msg.ToolCallID),
			Name:            strings.TrimSpace(msg.Name),
			AttachmentCount: len(msg.Attachments),
		})
	}
	return summaries
}

func previewForLog(content string, limit int) string {
	content = strings.ReplaceAll(content, "\r", "\\r")
	content = strings.ReplaceAll(content, "\n", "\\n")
	if limit <= 0 || len(content) <= limit {
		return content
	}
	return content[:limit] + "..."
}

func dumpOpenAIRawRequest(requestID string, rawBody []byte, log *zap.Logger) {
	dir := strings.TrimSpace(os.Getenv("GEMINI_DEBUG_STREAM_DIR"))
	if dir == "" || len(rawBody) == 0 {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Warn("failed to create OpenAI request debug directory", zap.String("dir", dir), zap.Error(err))
		return
	}
	shortID := requestID
	if len(shortID) > 16 {
		shortID = shortID[:16]
	}
	name := time.Now().Format("20060102_150405.000") + "_" + sanitizeOpenAIDebugFilename(shortID) + "_openai_chat_request.json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, rawBody, 0600); err != nil {
		log.Warn("failed to write OpenAI request debug capture", zap.String("path", path), zap.Error(err))
		return
	}
	log.Info("OpenAI raw request debug capture written", zap.String("path", path))
}

func sanitizeOpenAIDebugFilename(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	if b.Len() == 0 {
		return "request"
	}
	return b.String()
}

func openAIRequestDebugEnabled() bool {
	if strings.TrimSpace(os.Getenv("GEMINI_DEBUG_STREAM_DIR")) != "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENAI_DEBUG_REQUEST_LOG"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func summarizeStreamChunkForTrace(chunk dto.ChatCompletionChunk) (contentLen, reasoningLen, toolCallCount int, finishReason string) {
	for _, choice := range chunk.Choices {
		contentLen += len(choice.Delta.Content)
		reasoningLen += len(choice.Delta.ReasoningContent)
		toolCallCount += len(choice.Delta.ToolCalls)
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
	}
	return contentLen, reasoningLen, toolCallCount, finishReason
}
