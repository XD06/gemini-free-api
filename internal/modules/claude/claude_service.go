package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gemini-free-api/internal/commons/models"
	common "gemini-free-api/internal/commons/utils"
	"gemini-free-api/internal/modules/claude/dto"
	"gemini-free-api/internal/modules/providers"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

type ClaudeService struct {
	client providers.GeminiClient
	log    *zap.Logger
}

func NewClaudeService(client providers.GeminiClient, log *zap.Logger) *ClaudeService {
	return &ClaudeService{
		client: client,
		log:    log,
	}
}

// intPtr returns a pointer to the given int value. Used for *int fields in StreamEvent
// so that index=0 is correctly serialized (not omitted by omitempty).
func intPtr(v int) *int { return &v }

func (s *ClaudeService) ListModels() []providers.ModelInfo {
	return s.client.ListModels()
}

func (s *ClaudeService) GenerateMessage(ctx context.Context, req dto.MessageRequest) (*dto.MessageResponse, error) {
	// Logic: Validate
	if err := common.ValidateMessages(req.Messages); err != nil {
		return nil, err
	}

	// Logic: Build Prompt
	prompt := common.BuildPromptFromMessages(req.Messages, req.System)
	if prompt == "" {
		return nil, fmt.Errorf("no valid content in messages")
	}

	hasTools := len(req.Tools) > 0
	if hasTools {
		prompt = s.buildToolBridgePrompt(req, prompt)
	}

	opts := []providers.GenerateOption{}
	if req.Model != "" {
		opts = append(opts, providers.WithModel(req.Model))
	}
	inputFiles, err := providers.InputFilesFromAttachments(req.Messages)
	if err != nil {
		return nil, err
	}
	if len(inputFiles) > 0 {
		opts = append(opts, providers.WithInputFiles(inputFiles))
	}

	// Logic: Call Provider
	response, err := s.client.GenerateContent(ctx, prompt, opts...)
	if err != nil {
		return nil, err
	}

	// Logic: Construct Response
	msgID := fmt.Sprintf("msg_%s", uuid.New().String())
	resContent := []dto.ConfigContent{}
	stopReason := "end_turn"

	if hasTools {
		toolUses, text := s.parseToolBridgeOutput(req, response.Text)
		if len(toolUses) > 0 {
			for _, tu := range toolUses {
				resContent = append(resContent, tu)
			}
			stopReason = "tool_use"
		} else {
			resContent = append(resContent, dto.ConfigContent{Type: "text", Text: text})
		}
	} else {
		resContent = append(resContent, dto.ConfigContent{Type: "text", Text: response.Text})
	}

	return &dto.MessageResponse{
		ID:         msgID,
		Type:       "message",
		Role:       "assistant",
		Model:      req.Model,
		Content:    resContent,
		StopReason: stopReason,
		Usage: models.Usage{
			InputTokens:  len(prompt) / 4,
			OutputTokens: len(response.Text) / 4,
		},
	}, nil
}

// GenerateMessageStream handles real streaming of Claude events using the provider's
// native stream capability. When tools are present, the response is buffered so the
// tool-bridge JSON can be parsed; otherwise deltas are forwarded immediately.
func (s *ClaudeService) GenerateMessageStream(ctx context.Context, req dto.MessageRequest, onEvent func(dto.StreamEvent) bool) error {
	// Validate
	if err := common.ValidateMessages(req.Messages); err != nil {
		return err
	}

	// Build prompt
	prompt := common.BuildPromptFromMessages(req.Messages, req.System)
	if prompt == "" {
		return fmt.Errorf("no valid content in messages")
	}

	hasTools := len(req.Tools) > 0
	if hasTools {
		prompt = s.buildToolBridgePrompt(req, prompt)
	}

	opts := []providers.GenerateOption{}
	if req.Model != "" {
		opts = append(opts, providers.WithModel(req.Model))
	}
	inputFiles, err := providers.InputFilesFromAttachments(req.Messages)
	if err != nil {
		return err
	}
	if len(inputFiles) > 0 {
		opts = append(opts, providers.WithInputFiles(inputFiles))
	}

	msgID := fmt.Sprintf("msg_%s", uuid.New().String())
	inputTokens := len(prompt) / 4

	if hasTools {
		return s.streamWithToolBridge(ctx, req, prompt, opts, msgID, inputTokens, onEvent)
	}
	return s.streamDirect(ctx, req, prompt, opts, msgID, inputTokens, onEvent)
}

// streamDirect forwards provider stream deltas to the client in real time,
// supporting both thinking and text content blocks.
func (s *ClaudeService) streamDirect(
	ctx context.Context,
	req dto.MessageRequest,
	prompt string,
	opts []providers.GenerateOption,
	msgID string,
	inputTokens int,
	onEvent func(dto.StreamEvent) bool,
) error {
	// message_start
	if !onEvent(dto.StreamEvent{
		Type: "message_start",
		Message: &dto.MessageResponse{
			ID:    msgID,
			Type:  "message",
			Role:  "assistant",
			Model: req.Model,
			Usage: models.Usage{InputTokens: inputTokens},
		},
	}) {
		return nil
	}

	var contentIndex int
	var blockOpen bool
	var inThinking bool
	var outputLen int

	closeBlock := func() bool {
		if blockOpen {
			blockOpen = false
			if !onEvent(dto.StreamEvent{Type: "content_block_stop", Index: intPtr(contentIndex)}) {
				return false
			}
			contentIndex++
		}
		return true
	}

	handleStreamEvent := func(event providers.StreamEvent) bool {
		switch event.Kind {
		case "thinking_text":
			if !inThinking {
				if !closeBlock() {
					return false
				}
				if !onEvent(dto.StreamEvent{
					Type:         "content_block_start",
					Index:        intPtr(contentIndex),
					ContentBlock: &dto.ConfigContent{Type: "thinking"},
				}) {
					return false
				}
				blockOpen = true
				inThinking = true
			}
			if event.Delta != "" {
				return onEvent(dto.StreamEvent{
					Type:  "content_block_delta",
					Index: intPtr(contentIndex),
					DeltaField: &models.Delta{
						Type:     "thinking_delta",
						Thinking: event.Delta,
					},
				})
			}
			return true

		case "content_delta":
			if inThinking {
				if !closeBlock() {
					return false
				}
				inThinking = false
			}
			if !blockOpen {
				if !onEvent(dto.StreamEvent{
					Type:         "content_block_start",
					Index:        intPtr(contentIndex),
					ContentBlock: &dto.ConfigContent{Type: "text"},
				}) {
					return false
				}
				blockOpen = true
			}
			outputLen += len(event.Delta)
			return onEvent(dto.StreamEvent{
				Type:  "content_block_delta",
				Index: intPtr(contentIndex),
				DeltaField: &models.Delta{
					Type: "text_delta",
					Text: event.Delta,
				},
			})
		}
		return true
	}

	streamErr := s.client.GenerateContentStreamForOpenAI(ctx, prompt, handleStreamEvent, opts...)

	// Close any open block
	if !closeBlock() {
		return nil
	}

	// If stream errored, return immediately without sending empty content or closing events
	if streamErr != nil {
		return streamErr
	}

	// If nothing was received, send an empty text block
	if contentIndex == 0 && !blockOpen {
		if !onEvent(dto.StreamEvent{
			Type:         "content_block_start",
			Index:        intPtr(0),
			ContentBlock: &dto.ConfigContent{Type: "text"},
		}) {
			return nil
		}
		if !onEvent(dto.StreamEvent{Type: "content_block_stop", Index: intPtr(0)}) {
			return nil
		}
	}

	// message_delta
	if !onEvent(dto.StreamEvent{
		Type: "message_delta",
		DeltaField: &models.Delta{
			StopReason: "end_turn",
		},
		UsageField: &models.Usage{
			OutputTokens: outputLen / 4,
		},
	}) {
		return nil
	}

	// message_stop
	_ = onEvent(dto.StreamEvent{Type: "message_stop"})
	return nil
}

// streamWithToolBridge buffers the provider stream, then parses the JSON output for
// tool calls. If tool calls are found, they are emitted as tool_use content blocks;
// otherwise the text is streamed out in chunks.
func (s *ClaudeService) streamWithToolBridge(
	ctx context.Context,
	req dto.MessageRequest,
	prompt string,
	opts []providers.GenerateOption,
	msgID string,
	inputTokens int,
	onEvent func(dto.StreamEvent) bool,
) error {
	var fullText strings.Builder

	handleStreamEvent := func(event providers.StreamEvent) bool {
		if event.Kind == "content_delta" {
			fullText.WriteString(event.Delta)
		}
		return true
	}

	streamErr := s.client.GenerateContentStreamForOpenAI(ctx, prompt, handleStreamEvent, opts...)
	if streamErr != nil {
		return streamErr
	}

	// Parse buffered output for tool calls
	toolUses, text := s.parseToolBridgeOutput(req, fullText.String())
	outputTokens := fullText.Len() / 4

	// message_start
	if !onEvent(dto.StreamEvent{
		Type: "message_start",
		Message: &dto.MessageResponse{
			ID:    msgID,
			Type:  "message",
			Role:  "assistant",
			Model: req.Model,
			Usage: models.Usage{InputTokens: inputTokens},
		},
	}) {
		return nil
	}

	contentIndex := 0
	stopReason := "end_turn"

	if len(toolUses) > 0 {
		stopReason = "tool_use"
		for _, tu := range toolUses {
			// content_block_start
			if !onEvent(dto.StreamEvent{
				Type:         "content_block_start",
				Index:        intPtr(contentIndex),
				ContentBlock: &tu,
			}) {
				return nil
			}

			// content_block_delta (input_json_delta)
			inputJSON, err := json.Marshal(tu.Input)
			if err != nil {
				s.log.Error("Failed to marshal tool input", zap.Error(err))
				return fmt.Errorf("failed to marshal tool input: %w", err)
			}
			if !onEvent(dto.StreamEvent{
				Type:  "content_block_delta",
				Index: intPtr(contentIndex),
				DeltaField: &models.Delta{
					Type:        "input_json_delta",
					PartialJSON: string(inputJSON),
				},
			}) {
				return nil
			}

			// content_block_stop
			if !onEvent(dto.StreamEvent{Type: "content_block_stop", Index: intPtr(contentIndex)}) {
				return nil
			}
			contentIndex++
		}
	} else {
		// Text content – stream in chunks
		if !onEvent(dto.StreamEvent{
			Type:         "content_block_start",
			Index:        intPtr(contentIndex),
			ContentBlock: &dto.ConfigContent{Type: "text"},
		}) {
			return nil
		}

		chunks := common.SplitResponseIntoChunks(text, 30)
		for _, chunk := range chunks {
			if !onEvent(dto.StreamEvent{
				Type:  "content_block_delta",
				Index: intPtr(contentIndex),
				DeltaField: &models.Delta{
					Type: "text_delta",
					Text: chunk,
				},
			}) {
				return nil
			}
		}

		if !onEvent(dto.StreamEvent{Type: "content_block_stop", Index: intPtr(contentIndex)}) {
			return nil
		}
		contentIndex++
	}

	// message_delta
	if !onEvent(dto.StreamEvent{
		Type: "message_delta",
		DeltaField: &models.Delta{
			StopReason: stopReason,
		},
		UsageField: &models.Usage{
			OutputTokens: outputTokens,
		},
	}) {
		return nil
	}

	// message_stop
	_ = onEvent(dto.StreamEvent{Type: "message_stop"})
	return nil
}

func (s *ClaudeService) buildToolBridgePrompt(req dto.MessageRequest, basePrompt string) string {
	var b strings.Builder
	b.WriteString("You are a Claude-compatible assistant running behind a bridge that supports tool use.\n")
	b.WriteString("You MUST respond with JSON only. Do not output markdown code fences.\n")
	b.WriteString("Output schema:\n")
	b.WriteString("{\"status\":\"tool_use\",\"tool_calls\":[{\"id\":\"<unique_id>\",\"name\":\"<tool_name>\",\"input\":{}}]} OR {\"status\":\"text\",\"content\":\"<assistant_text>\"}\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Use only tool names listed below.\n")
	b.WriteString("- input must be valid JSON object.\n")

	b.WriteString("Available tools:\n")
	for _, t := range req.Tools {
		b.WriteString("- name: ")
		b.WriteString(t.Name)
		if t.Description != "" {
			b.WriteString(" | description: ")
			b.WriteString(t.Description)
		}
		if len(t.InputSchema) > 0 {
			b.WriteString(" | input_schema: ")
			b.Write(t.InputSchema)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nConversation:\n")
	b.WriteString(basePrompt)
	return b.String()
}

func (s *ClaudeService) parseToolBridgeOutput(req dto.MessageRequest, text string) ([]dto.ConfigContent, string) {
	cleaned := common.StripCodeFence(text)
	if cleaned == "" {
		return nil, ""
	}

	var payload struct {
		Status    string `json:"status"`
		ToolCalls []struct {
			ID    string                 `json:"id"`
			Name  string                 `json:"name"`
			Input map[string]interface{} `json:"input"`
		} `json:"tool_calls"`
		Content string `json:"content"`
	}

	if err := json.Unmarshal([]byte(cleaned), &payload); err != nil {
		return nil, text
	}

	if payload.Status == "tool_use" && len(payload.ToolCalls) > 0 {
		uses := make([]dto.ConfigContent, 0, len(payload.ToolCalls))
		for _, tc := range payload.ToolCalls {
			id := tc.ID
			if id == "" {
				id = fmt.Sprintf("toolu_%s", uuid.New().String())
			}
			uses = append(uses, dto.ConfigContent{
				Type:  "tool_use",
				ID:    id,
				Name:  tc.Name,
				Input: tc.Input,
			})
		}
		return uses, ""
	}

	return nil, payload.Content
}
