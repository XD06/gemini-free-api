package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gemini-free-api/internal/commons/models"
	"gemini-free-api/internal/commons/utils"
	"gemini-free-api/internal/modules/openai/dto"
	"gemini-free-api/internal/modules/providers"

	"go.uber.org/zap"
)

type OpenAIService struct {
	client                   providers.GeminiClient
	log                      *zap.Logger
	fileStore                *openAIFileStore
	contextMu                sync.Mutex
	transcriptContexts       map[string]string
	transcriptContextUpdated map[string]time.Time
	transcriptSuffixContexts map[string]string
	transcriptSuffixUpdated  map[string]time.Time
	userWindowContexts       map[string]string
	userWindowUpdated        map[string]time.Time
	providerLatestTranscript map[string]string
	providerLatestLength     map[string]int
	rootContexts             map[string]string
	rootContextUpdated       map[string]time.Time
	explicitProviderContexts map[string]string
	explicitProviderUpdated  map[string]time.Time
	toolPlannerContexts      map[string]string
	toolPlannerUpdated       map[string]time.Time
}

type openAIRequestIDContextKey struct{}

func openAIRequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(openAIRequestIDContextKey{}).(string)
	return strings.TrimSpace(id)
}

func geminiSourcePathEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GEMINI_USE_SOURCE_PATH"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// openAILocalFallbackEnabled controls whether a provider conversation flagged
// untrusted is treated as not-reusable, forcing the next turn to rebuild the
// prompt from the locally retained full history. Defaults to enabled so the
// "lost early turns" symptom is fixed out of the box; set
// OPENAI_CONTEXT_LOCAL_FALLBACK=false to restore the legacy behavior for A/B.
func openAILocalFallbackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENAI_CONTEXT_LOCAL_FALLBACK"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func NewOpenAIService(client providers.GeminiClient, log *zap.Logger) *OpenAIService {
	return &OpenAIService{
		client:                   client,
		log:                      log,
		fileStore:                newOpenAIFileStore(""),
		transcriptContexts:       make(map[string]string),
		transcriptContextUpdated: make(map[string]time.Time),
		transcriptSuffixContexts: make(map[string]string),
		transcriptSuffixUpdated:  make(map[string]time.Time),
		userWindowContexts:       make(map[string]string),
		userWindowUpdated:        make(map[string]time.Time),
		providerLatestTranscript: make(map[string]string),
		providerLatestLength:     make(map[string]int),
		rootContexts:             make(map[string]string),
		rootContextUpdated:       make(map[string]time.Time),
		explicitProviderContexts: make(map[string]string),
		explicitProviderUpdated:  make(map[string]time.Time),
		toolPlannerContexts:      make(map[string]string),
		toolPlannerUpdated:       make(map[string]time.Time),
	}
}

const (
	maxTranscriptContextEntries = 1000
	transcriptContextTTL        = 12 * time.Hour
)

var errEmptyProviderStream = fmt.Errorf("provider stream completed without content")

func generateChatID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return fmt.Sprintf("chatcmpl-%d%06d", time.Now().Unix(), r.Intn(1000000))
}

func (s *OpenAIService) ListModels() []providers.ModelInfo {
	return s.client.ListModels()
}

func (s *OpenAIService) CreateChatCompletion(ctx context.Context, req dto.ChatCompletionRequest) (*dto.ChatCompletionResponse, error) {
	requestID := openAIRequestIDFromContext(ctx)
	modelMessages := req.ToModelMessages()

	// Logic: Validate messages
	if err := utils.ValidateMessages(modelMessages); err != nil {
		return nil, err
	}

	// Logic: Validate generation parameters
	if err := utils.ValidateGenerationRequest(req.Model, req.MaxTokens, req.Temperature); err != nil {
		return nil, err
	}

	contextPlan := s.planRequestContext(req)

	// Logic: Build Prompt
	prompt := contextPlan.Prompt
	if prompt == "" {
		return nil, fmt.Errorf("no valid content in messages")
	}

	useToolBridge := shouldUseToolBridge(req)
	if useToolBridge {
		plannerContext := s.toolPlannerConversationPlan(contextPlan, req)
		if plannerContext.Reused {
			prompt = s.buildToolBridgeContinuationPrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		} else {
			prompt = s.buildToolBridgePrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		}
		contextPlan.ToolPlannerConversationID = plannerContext.ConversationID
	}

	providerModel, thinkingLevel := modelAndThinkingLevel(req.Model, requestThinkingLevel(req))
	baseOpts := []providers.GenerateOption{}
	plannerBaseOpts := []providers.GenerateOption{}
	if providerModel != "" {
		modelOpt := providers.WithModel(providerModel)
		baseOpts = append(baseOpts, modelOpt)
		plannerBaseOpts = append(plannerBaseOpts, modelOpt)
	}
	if thinkingLevel != "" {
		baseOpts = append(baseOpts, providers.WithThinkingLevel(thinkingLevel))
	}
	if geminiSourcePathEnabled() {
		baseOpts = append(baseOpts, providers.WithSourcePath(true))
	}
	optsBase := baseOpts
	if useToolBridge {
		optsBase = plannerBaseOpts
	}
	opts := append([]providers.GenerateOption{}, optsBase...)
	if useToolBridge {
		opts = append(opts, providers.WithConversationID(contextPlan.ToolPlannerConversationID))
	} else if contextPlan.ProviderConversationID != "" {
		opts = append(opts, providers.WithConversationID(contextPlan.ProviderConversationID))
	}
	inputFiles, err := s.inputFilesFromModelMessages(ctx, modelMessages)
	if err != nil {
		return nil, err
	}
	if len(inputFiles) > 0 {
		baseOpts = append(baseOpts, providers.WithInputFiles(inputFiles))
		if !useToolBridge {
			opts = append(opts, providers.WithInputFiles(inputFiles))
		}
	}
	s.dumpOpenAITrace(ctx, req, contextPlan, "non_stream_initial", prompt, opts, inputFiles, useToolBridge, nil)

	// Logic: Call Provider
	response, err := s.client.GenerateContent(ctx, prompt, opts...)
	if err != nil && useToolBridge {
		s.forgetToolPlannerConversation(contextPlan.ToolPlannerConversationID)
		if s.log != nil {
			s.log.Warn("tool planner request failed, retrying with fresh Gemini conversation",
				zap.String("request_id", requestID),
				zap.String("stale_tool_planner_conversation_id", strings.TrimSpace(contextPlan.ToolPlannerConversationID)),
				zap.Error(err),
			)
		}
		plannerContext := s.toolPlannerConversationPlan(contextPlan, req)
		contextPlan.ToolPlannerConversationID = plannerContext.ConversationID
		fallbackPrompt := s.buildToolBridgePrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		fallbackOpts := append([]providers.GenerateOption{}, plannerBaseOpts...)
		fallbackOpts = append(fallbackOpts, providers.WithConversationID(contextPlan.ToolPlannerConversationID))
		s.dumpOpenAITrace(ctx, req, contextPlan, "non_stream_tool_planner_fallback", fallbackPrompt, fallbackOpts, inputFiles, true, err)
		response, err = s.client.GenerateContent(ctx, fallbackPrompt, fallbackOpts...)
		prompt = fallbackPrompt
		opts = fallbackOpts
	}
	if err != nil && contextPlan.AutoContext && !useToolBridge {
		if s.log != nil {
			s.log.Warn("server-side context request failed, retrying stateless OpenAI prompt",
				zap.String("request_id", requestID),
				zap.String("stale_provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
				zap.Error(err),
			)
		}
		fallbackPrompt := buildGeminiWebPromptFromOpenAIMessages(req.Messages)
		if useToolBridge {
			fallbackPrompt = s.buildToolBridgePrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		}
		if fallbackPrompt != "" {
			s.forgetProviderConversation(contextPlan.ProviderConversationID)
			fallbackOpts := fallbackOptionsWithFreshConversation(&contextPlan, baseOpts)
			s.dumpOpenAITrace(ctx, req, contextPlan, "non_stream_fallback", fallbackPrompt, fallbackOpts, inputFiles, useToolBridge, err)
			response, err = s.client.GenerateContent(ctx, fallbackPrompt, fallbackOpts...)
			prompt = fallbackPrompt
		}
	}
	if err != nil {
		return nil, err
	}
	contextPlan.ResponseText = response.Text

	message := dto.ChatCompletionResponseMessage{Role: "assistant"}
	finishReason := "stop"

	if useToolBridge {
		plan := s.resolveToolBridgeOutput(ctx, req, response.Text, opts, inputFiles, requestID)
		if plan.Err != nil {
			return nil, plan.Err
		}

		if len(plan.ToolCalls) > 0 {
			message.ToolCalls = plan.ToolCalls
			finishReason = "tool_calls"
		} else if plan.NoTool {
			mainOpts := append([]providers.GenerateOption{}, baseOpts...)
			if contextPlan.ProviderConversationID != "" {
				mainOpts = append(mainOpts, providers.WithConversationID(contextPlan.ProviderConversationID))
			}
			s.dumpOpenAITrace(ctx, req, contextPlan, "non_stream_main_after_no_tool", contextPlan.Prompt, mainOpts, inputFiles, false, nil)
			mainResp, err := s.client.GenerateContent(ctx, contextPlan.Prompt, mainOpts...)
			if err != nil && strings.TrimSpace(contextPlan.ProviderConversationID) != "" {
				s.forgetProviderConversation(contextPlan.ProviderConversationID)
				if s.log != nil {
					s.log.Warn("tool no-op main request failed, retrying with fresh Gemini conversation",
						zap.String("request_id", requestID),
						zap.String("stale_provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
						zap.Error(err),
					)
				}
				fallbackPrompt := buildGeminiWebPromptFromOpenAIMessages(req.Messages)
				if strings.TrimSpace(fallbackPrompt) == "" {
					fallbackPrompt = contextPlan.Prompt
				}
				fallbackOpts := fallbackOptionsWithFreshConversation(&contextPlan, baseOpts)
				s.dumpOpenAITrace(ctx, req, contextPlan, "non_stream_main_after_no_tool_fallback", fallbackPrompt, fallbackOpts, inputFiles, false, err)
				mainResp, err = s.client.GenerateContent(ctx, fallbackPrompt, fallbackOpts...)
				prompt = fallbackPrompt
			}
			if err != nil {
				return nil, err
			}
			contextPlan.ResponseText = mainResp.Text
			message.Content = mainResp.Text
			response = mainResp
			prompt = contextPlan.Prompt
		} else {
			message.Content = plan.Content
		}
	} else {
		message.Content = response.Text
	}
	if !useToolBridge || finishReason != "tool_calls" {
		s.rememberRequestContext(contextPlan)
	}

	promptTokens := len(prompt) / 4
	completionTokens := len(response.Text) / 4
	totalTokens := promptTokens + completionTokens

	// Logic: Construct Response
	return &dto.ChatCompletionResponse{
		ID:      generateChatID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []dto.Choice{
			{
				Index:        0,
				Message:      message,
				FinishReason: finishReason,
			},
		},
		Usage: models.Usage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      totalTokens,
		},
	}, nil
}

func (s *OpenAIService) CreateImageGeneration(ctx context.Context, req dto.ImageGenerationRequest) (*dto.ImageGenerationResponse, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	n := req.N
	if n <= 0 {
		n = 1
	}
	if n > 10 {
		return nil, fmt.Errorf("n must be between 1 and 10")
	}

	imagePrompt := buildImageGenerationPrompt(prompt, req.Size)
	opts := []providers.GenerateOption{}
	if req.Model != "" {
		opts = append(opts, providers.WithModel(req.Model))
	}

	data := make([]dto.ImageGenerationData, 0, n)
	for len(data) < n {
		response, err := s.client.GenerateContent(ctx, imagePrompt, opts...)
		if err != nil {
			return nil, err
		}
		if len(response.Images) == 0 {
			return nil, fmt.Errorf("provider returned no generated images")
		}

		for _, image := range response.Images {
			if len(data) >= n {
				break
			}
			item := dto.ImageGenerationData{RevisedPrompt: prompt}
			if strings.EqualFold(req.ResponseFormat, "b64_json") {
				b64, err := fetchImageAsBase64(ctx, image.URL)
				if err != nil {
					return nil, err
				}
				item.B64JSON = b64
			} else {
				item.URL = image.URL
			}
			data = append(data, item)
		}
	}

	return &dto.ImageGenerationResponse{
		Created: time.Now().Unix(),
		Data:    data,
	}, nil
}

func buildImageGenerationPrompt(prompt, size string) string {
	var b strings.Builder
	b.WriteString("Generate an image from this prompt. Return the generated image, not only a text description.\n\nPrompt: ")
	b.WriteString(prompt)
	if strings.TrimSpace(size) != "" {
		b.WriteString("\nRequested size/aspect: ")
		b.WriteString(strings.TrimSpace(size))
	}
	return b.String()
}

func requestThinkingLevel(req dto.ChatCompletionRequest) string {
	if level := normalizeThinkingLevel(req.ThinkingLevel); level != "" {
		return level
	}
	if level := normalizeThinkingLevel(req.ReasoningEffort); level != "" {
		return level
	}
	if req.Reasoning != nil {
		return normalizeThinkingLevel(req.Reasoning.Effort)
	}
	return ""
}

func modelAndThinkingLevel(model, explicitLevel string) (string, string) {
	cleanModel, suffixLevel := splitThinkingLevelFromModel(model)
	level := normalizeThinkingLevel(explicitLevel)
	if level == "" {
		level = suffixLevel
	}
	return cleanModel, level
}

func splitThinkingLevelFromModel(model string) (string, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", ""
	}
	idx := strings.LastIndex(model, ":")
	if idx < 0 {
		return model, ""
	}

	base := strings.TrimSpace(model[:idx])
	suffix := strings.TrimSpace(model[idx+1:])
	if base == "" {
		return model, ""
	}
	level := ""
	for _, prefix := range []string{"thinking=", "thinking_level=", "thinking-level="} {
		if strings.HasPrefix(strings.ToLower(suffix), prefix) {
			level = strings.TrimSpace(suffix[len(prefix):])
			break
		}
	}
	if level == "" {
		level = suffix
	}
	level = normalizeThinkingLevel(level)
	if level == "" {
		return model, ""
	}
	return base, level
}

func normalizeThinkingLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "standard", "normal", "default", "minimal", "low", "medium", "none":
		return "standard"
	case "extended", "high", "deep", "xhigh":
		return "extended"
	default:
		return ""
	}
}

func fetchImageAsBase64(ctx context.Context, imageURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return "", fmt.Errorf("build image fetch request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch generated image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch generated image failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	if err != nil {
		return "", fmt.Errorf("read generated image: %w", err)
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// CreateChatCompletionStream handles OpenAI streaming logic within the service layer.
func (s *OpenAIService) CreateChatCompletionStream(ctx context.Context, req dto.ChatCompletionRequest, onEvent func(dto.ChatCompletionChunk) bool) error {
	requestID := openAIRequestIDFromContext(ctx)
	chunkID := generateChatID()
	created := time.Now().Unix()

	modelMessages := req.ToModelMessages()
	if err := utils.ValidateMessages(modelMessages); err != nil {
		return err
	}
	contextPlan := s.planRequestContext(req)
	prompt := contextPlan.Prompt
	if prompt == "" {
		return fmt.Errorf("no valid content in messages")
	}
	useToolBridge := shouldUseToolBridge(req)
	if useToolBridge {
		plannerContext := s.toolPlannerConversationPlan(contextPlan, req)
		if plannerContext.Reused {
			prompt = s.buildToolBridgeContinuationPrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		} else {
			prompt = s.buildToolBridgePrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		}
		contextPlan.ToolPlannerConversationID = plannerContext.ConversationID
	}
	if openAIRequestDebugEnabled() && s.log != nil {
		s.log.Info("OpenAI stream execution plan",
			zap.String("request_id", requestID),
			zap.String("model", req.Model),
			zap.Bool("tools_enabled", req.HasToolsEnabled()),
			zap.Bool("tool_bridge_enabled", useToolBridge),
			zap.Bool("tool_bridge_buffering", useToolBridge && mustBufferToolBridge(req)),
			zap.String("tool_choice_mode", req.ToolChoiceMode()),
			zap.Int("tool_count", len(req.Tools)),
			zap.Bool("auto_context", contextPlan.AutoContext),
			zap.Bool("has_provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID) != ""),
			zap.String("provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
			zap.Int("prompt_length", len(prompt)),
		)
	}

	providerModel, thinkingLevel := modelAndThinkingLevel(req.Model, requestThinkingLevel(req))
	baseOpts := []providers.GenerateOption{}
	plannerBaseOpts := []providers.GenerateOption{}
	if providerModel != "" {
		modelOpt := providers.WithModel(providerModel)
		baseOpts = append(baseOpts, modelOpt)
		plannerBaseOpts = append(plannerBaseOpts, modelOpt)
	}
	if thinkingLevel != "" {
		baseOpts = append(baseOpts, providers.WithThinkingLevel(thinkingLevel))
	}
	if geminiSourcePathEnabled() {
		baseOpts = append(baseOpts, providers.WithSourcePath(true))
	}
	optsBase := baseOpts
	if useToolBridge {
		optsBase = plannerBaseOpts
	}
	opts := append([]providers.GenerateOption{}, optsBase...)
	if useToolBridge {
		opts = append(opts, providers.WithConversationID(contextPlan.ToolPlannerConversationID))
	} else if contextPlan.ProviderConversationID != "" {
		opts = append(opts, providers.WithConversationID(contextPlan.ProviderConversationID))
	}
	inputFiles, err := s.inputFilesFromModelMessages(ctx, modelMessages)
	if err != nil {
		return err
	}
	if len(inputFiles) > 0 {
		baseOpts = append(baseOpts, providers.WithInputFiles(inputFiles))
		if !useToolBridge {
			opts = append(opts, providers.WithInputFiles(inputFiles))
		}
	}
	s.dumpOpenAITrace(ctx, req, contextPlan, "stream_initial", prompt, opts, inputFiles, useToolBridge, nil)

	onEvent(dto.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{Role: "assistant"}}},
	})

	var completionLen int
	var completionText strings.Builder
	toolBridgeUndecided := false
	toolBridgeStreamingContent := false
	handleStreamEvent := func(event providers.StreamEvent) bool {
		switch event.Kind {
		case "thinking_text":
			if useToolBridge {
				return true
			}
			return onEvent(dto.ChatCompletionChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{ReasoningContent: event.Delta}}},
			})
		case "content_delta":
			completionLen += len(event.Delta)
			completionText.WriteString(event.Delta)
			if useToolBridge {
				return true
			}
			if useToolBridge && mustBufferToolBridge(req) {
				return true
			}
			if toolBridgeUndecided {
				switch classifyToolBridgeStreamPrefix(completionText.String()) {
				case "unknown":
					return true
				case "tool_json":
					return true
				case "content":
					toolBridgeUndecided = false
					toolBridgeStreamingContent = true
					return onEvent(dto.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{Content: completionText.String()}}},
					})
				}
			}
			if useToolBridge && !toolBridgeStreamingContent {
				return true
			}
			return onEvent(dto.ChatCompletionChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{Content: event.Delta}}},
			})
		default:
			return true
		}
	}
	err = s.client.GenerateContentStreamForOpenAI(ctx, prompt, handleStreamEvent, opts...)
	if err == nil && completionLen == 0 {
		err = errEmptyProviderStream
	}
	if err != nil && useToolBridge && completionLen == 0 {
		s.forgetToolPlannerConversation(contextPlan.ToolPlannerConversationID)
		if s.log != nil {
			s.log.Warn("tool planner stream failed before content, retrying with fresh Gemini conversation",
				zap.String("request_id", requestID),
				zap.String("stale_tool_planner_conversation_id", strings.TrimSpace(contextPlan.ToolPlannerConversationID)),
				zap.Error(err),
			)
		}
		plannerContext := s.toolPlannerConversationPlan(contextPlan, req)
		contextPlan.ToolPlannerConversationID = plannerContext.ConversationID
		fallbackPrompt := s.buildToolBridgePrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		fallbackOpts := append([]providers.GenerateOption{}, plannerBaseOpts...)
		fallbackOpts = append(fallbackOpts, providers.WithConversationID(contextPlan.ToolPlannerConversationID))
		s.dumpOpenAITrace(ctx, req, contextPlan, "stream_tool_planner_fallback", fallbackPrompt, fallbackOpts, inputFiles, true, err)
		completionText.Reset()
		completionLen = 0
		prompt = fallbackPrompt
		opts = fallbackOpts
		err = s.client.GenerateContentStreamForOpenAI(ctx, fallbackPrompt, handleStreamEvent, fallbackOpts...)
		if err == nil && completionLen == 0 {
			err = errEmptyProviderStream
		}
	}
	if err != nil && contextPlan.AutoContext && !useToolBridge && completionLen == 0 && shouldRetrySameProviderContext(err) {
		if s.log != nil {
			s.log.Warn("server-side context stream failed before content, retrying same Gemini context once",
				zap.String("request_id", requestID),
				zap.String("provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
				zap.Error(err),
			)
		}
		err = s.client.GenerateContentStreamForOpenAI(ctx, prompt, handleStreamEvent, opts...)
		if err == nil && completionLen == 0 {
			err = errEmptyProviderStream
		}
	}
	if err != nil && contextPlan.AutoContext && !useToolBridge && completionLen == 0 {
		s.forgetProviderConversation(contextPlan.ProviderConversationID)
		if s.log != nil {
			s.log.Warn("server-side context stream failed before content, retrying stateless OpenAI prompt",
				zap.String("request_id", requestID),
				zap.String("stale_provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
				zap.Error(err),
			)
		}
		fallbackPrompt := buildGeminiWebPromptFromOpenAIMessages(req.Messages)
		if useToolBridge {
			fallbackPrompt = s.buildToolBridgePrompt(req, buildToolPlanningPrompt(req), toolBridgeRequiresToolCall(req))
		}
		if fallbackPrompt != "" {
			prompt = fallbackPrompt
			fallbackOpts := fallbackOptionsWithFreshConversation(&contextPlan, baseOpts)
			s.dumpOpenAITrace(ctx, req, contextPlan, "stream_fallback", fallbackPrompt, fallbackOpts, inputFiles, useToolBridge, err)
			if openAIRequestDebugEnabled() && s.log != nil {
				s.log.Info("OpenAI stream fallback plan",
					zap.String("request_id", requestID),
					zap.String("provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
					zap.Int("prompt_length", len(fallbackPrompt)),
					zap.Bool("tools_enabled", req.HasToolsEnabled()),
					zap.Bool("tool_bridge_enabled", useToolBridge),
				)
			}
			err = s.client.GenerateContentStreamForOpenAI(ctx, fallbackPrompt, handleStreamEvent, fallbackOpts...)
			if err == nil && completionLen == 0 {
				err = errEmptyProviderStream
			}
		}
	}
	if err != nil {
		return err
	}
	contextPlan.ResponseText = completionText.String()

	finishReason := "stop"
	emittedToolCalls := false
	if useToolBridge && !toolBridgeStreamingContent {
		plan := s.resolveToolBridgeOutput(ctx, req, completionText.String(), opts, inputFiles, requestID)
		if plan.Err != nil {
			return plan.Err
		}
		if openAIRequestDebugEnabled() && s.log != nil {
			s.log.Info("OpenAI tool bridge parsed",
				zap.String("request_id", requestID),
				zap.Int("tool_call_count", len(plan.ToolCalls)),
				zap.Int("buffered_output_length", completionText.Len()),
				zap.Int("content_length", len(strings.TrimSpace(plan.Content))),
			)
		}

		if len(plan.ToolCalls) > 0 {
			finishReason = "tool_calls"
			emittedToolCalls = true
			onEvent(dto.ChatCompletionChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []dto.ChunkChoice{{
					Index: 0,
					Delta: dto.ChatCompletionChunkDelta{ToolCalls: chunkToolCalls(plan.ToolCalls)},
				}},
			})
		} else if plan.NoTool {
			mainOpts := append([]providers.GenerateOption{}, baseOpts...)
			if contextPlan.ProviderConversationID != "" {
				mainOpts = append(mainOpts, providers.WithConversationID(contextPlan.ProviderConversationID))
			}
			s.dumpOpenAITrace(ctx, req, contextPlan, "stream_main_after_no_tool", contextPlan.Prompt, mainOpts, inputFiles, false, nil)
			completionText.Reset()
			completionLen = 0
			handleMainStreamEvent := func(event providers.StreamEvent) bool {
				switch event.Kind {
				case "thinking_text":
					return onEvent(dto.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{ReasoningContent: event.Delta}}},
					})
				case "content_delta":
					completionLen += len(event.Delta)
					completionText.WriteString(event.Delta)
					return onEvent(dto.ChatCompletionChunk{
						ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
						Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{Content: event.Delta}}},
					})
				default:
					return true
				}
			}
			mainPrompt := contextPlan.Prompt
			mainErr := s.client.GenerateContentStreamForOpenAI(ctx, mainPrompt, handleMainStreamEvent, mainOpts...)
			if mainErr == nil && completionLen == 0 {
				mainErr = errEmptyProviderStream
			}
			if mainErr != nil && strings.TrimSpace(contextPlan.ProviderConversationID) != "" {
				s.forgetProviderConversation(contextPlan.ProviderConversationID)
				if s.log != nil {
					s.log.Warn("tool no-op main stream failed, retrying with fresh Gemini conversation",
						zap.String("request_id", requestID),
						zap.String("stale_provider_conversation_id", strings.TrimSpace(contextPlan.ProviderConversationID)),
						zap.Error(mainErr),
					)
				}
				fallbackPrompt := buildGeminiWebPromptFromOpenAIMessages(req.Messages)
				if strings.TrimSpace(fallbackPrompt) == "" {
					fallbackPrompt = contextPlan.Prompt
				}
				fallbackOpts := fallbackOptionsWithFreshConversation(&contextPlan, baseOpts)
				s.dumpOpenAITrace(ctx, req, contextPlan, "stream_main_after_no_tool_fallback", fallbackPrompt, fallbackOpts, inputFiles, false, mainErr)
				completionText.Reset()
				completionLen = 0
				mainPrompt = fallbackPrompt
				mainErr = s.client.GenerateContentStreamForOpenAI(ctx, fallbackPrompt, handleMainStreamEvent, fallbackOpts...)
				if mainErr == nil && completionLen == 0 {
					mainErr = errEmptyProviderStream
				}
			}
			if mainErr != nil {
				return mainErr
			}
			contextPlan.ResponseText = completionText.String()
		}
	}
	if !useToolBridge || !emittedToolCalls {
		s.rememberRequestContext(contextPlan)
	}

	onEvent(dto.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []dto.ChunkChoice{{Index: 0, FinishReason: finishReason}},
	})

	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		promptTokens := len(prompt) / 4
		completionTokens := completionLen / 4
		totalTokens := promptTokens + completionTokens
		onEvent(dto.ChatCompletionChunk{
			ID:      chunkID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   req.Model,
			Choices: []dto.ChunkChoice{},
			Usage: &models.Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      totalTokens,
			},
		})
	}
	return nil
}

type openAIContextPlan struct {
	Prompt                    string
	ProviderConversationID    string
	ClientConversationID      string
	ToolPlannerConversationID string
	Phase                     string
	AutoContext               bool
	RequestMessages           []dto.ChatCompletionMessage
	ResponseText              string
}

func (s *OpenAIService) planRequestContext(req dto.ChatCompletionRequest) openAIContextPlan {
	plan := openAIContextPlan{
		Prompt:          buildGeminiWebPromptFromOpenAIMessages(req.Messages),
		Phase:           "main",
		RequestMessages: cloneChatMessages(req.Messages),
	}

	if toolPlan, ok := s.planToolResultContext(req); ok {
		return toolPlan
	}

	if explicitID := strings.TrimSpace(req.ConversationID); explicitID != "" {
		plan.ClientConversationID = explicitID
		plan.ProviderConversationID = explicitID
		if prefix, latest, ok := splitOpenAIHistoryPrefix(req.Messages); ok {
			if providerID := s.providerConversationForTranscript(prefix); providerID != "" {
				plan.ProviderConversationID = providerID
				plan.Prompt = rawUserPrompt(latest)
				plan.AutoContext = true
				return plan
			}
			if providerID := s.providerConversationForTranscriptSuffix(prefix); providerID != "" {
				plan.ProviderConversationID = providerID
				plan.Prompt = rawUserPrompt(latest)
				plan.AutoContext = true
				return plan
			}
			if providerID := s.providerConversationForUserWindow(prefix); providerID != "" {
				plan.ProviderConversationID = providerID
				plan.Prompt = rawUserPrompt(latest)
				plan.AutoContext = true
				return plan
			}
			if providerID := s.providerConversationForRoot(prefix); providerID != "" {
				plan.ProviderConversationID = providerID
				plan.Prompt = rawUserPrompt(latest)
				plan.AutoContext = true
				return plan
			}
			plan.ProviderConversationID = newAutoProviderConversationID()
			return plan
		}
		if latest, ok := latestUserMessage(req.Messages); ok {
			if providerID := s.providerConversationForExplicitID(explicitID); providerID != "" {
				plan.ProviderConversationID = providerID
			} else if !s.providerConversationReady(explicitID) {
				plan.ProviderConversationID = newAutoProviderConversationID()
			}
			plan.Prompt = rawUserPrompt(latest)
			plan.AutoContext = true
		}
		return plan
	}

	prefix, latest, ok := splitOpenAIHistoryPrefix(req.Messages)
	if !ok {
		plan.ProviderConversationID = newAutoProviderConversationID()
		return plan
	}

	if providerID := s.providerConversationForTranscript(prefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.Prompt = rawUserPrompt(latest)
		plan.AutoContext = true
		return plan
	}
	if providerID := s.providerConversationForTranscriptSuffix(prefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.Prompt = rawUserPrompt(latest)
		plan.AutoContext = true
		return plan
	}
	if providerID := s.providerConversationForUserWindow(prefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.Prompt = rawUserPrompt(latest)
		plan.AutoContext = true
		return plan
	}
	if providerID := s.providerConversationForRoot(prefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.Prompt = rawUserPrompt(latest)
		plan.AutoContext = true
		return plan
	}

	plan.ProviderConversationID = newAutoProviderConversationID()
	return plan
}

func (s *OpenAIService) planToolResultContext(req dto.ChatCompletionRequest) (openAIContextPlan, bool) {
	if len(req.Messages) < 3 {
		return openAIContextPlan{}, false
	}
	last := req.Messages[len(req.Messages)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "tool") {
		return openAIContextPlan{}, false
	}

	toolCallIndex := -1
	for i := len(req.Messages) - 2; i >= 0; i-- {
		if len(req.Messages[i].ToolCalls) > 0 {
			toolCallIndex = i
			break
		}
	}
	if toolCallIndex <= 0 {
		return openAIContextPlan{}, false
	}

	userIndex := toolCallIndex - 1
	if !strings.EqualFold(strings.TrimSpace(req.Messages[userIndex].Role), "user") {
		return openAIContextPlan{}, false
	}
	basePrefix := cloneChatMessages(req.Messages[:userIndex])
	mainMessages := append(cloneChatMessages(basePrefix), req.Messages[userIndex])
	toolMessages := collectToolResultMessages(req.Messages[toolCallIndex+1:])
	plan := openAIContextPlan{
		Prompt:                 buildToolResultAnswerPrompt(req.Messages[userIndex], req.Messages[toolCallIndex], toolMessages),
		ProviderConversationID: newAutoProviderConversationID(),
		Phase:                  "tool_result_answer",
		RequestMessages:        mainMessages,
	}

	if providerID := s.providerConversationForTranscript(basePrefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.AutoContext = true
		return plan, true
	}
	if providerID := s.providerConversationForTranscriptSuffix(basePrefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.AutoContext = true
		return plan, true
	}
	if providerID := s.providerConversationForUserWindow(basePrefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.AutoContext = true
		return plan, true
	}
	if providerID := s.providerConversationForRoot(basePrefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.AutoContext = true
		return plan, true
	}
	return plan, true
}

func collectToolResultMessages(messages []dto.ChatCompletionMessage) []dto.ChatCompletionMessage {
	out := make([]dto.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "tool") {
			out = append(out, msg)
		}
	}
	return out
}

func buildToolResultAnswerPrompt(userMsg, assistantMsg dto.ChatCompletionMessage, toolMsgs []dto.ChatCompletionMessage) string {
	var b strings.Builder
	b.WriteString("Use these tool results to answer the user's request in natural Markdown. Do not mention internal tool protocol unless needed.\n\n")
	b.WriteString("User request:\n")
	b.WriteString(compactText(userMsg.Content, 3000))
	if len(assistantMsg.ToolCalls) > 0 {
		b.WriteString("\n\nRequested tools:\n")
		b.WriteString(compactText(toolCallsForPrompt(assistantMsg.ToolCalls), 2000))
	}
	if len(toolMsgs) == 0 {
		b.WriteString("\n\nTool results:\nNo tool result content was provided.")
		return b.String()
	}
	b.WriteString("\n\nTool results:\n")
	for i, toolMsg := range toolMsgs {
		b.WriteString("\n[")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString("]")
		if strings.TrimSpace(toolMsg.Name) != "" {
			b.WriteString(" ")
			b.WriteString(strings.TrimSpace(toolMsg.Name))
		}
		if strings.TrimSpace(toolMsg.ToolCallID) != "" {
			b.WriteString(" (")
			b.WriteString(strings.TrimSpace(toolMsg.ToolCallID))
			b.WriteString(")")
		}
		b.WriteString(":\n")
		b.WriteString(compactText(toolMsg.Content, 8000))
		b.WriteString("\n")
	}
	return b.String()
}

func (s *OpenAIService) rememberRequestContext(plan openAIContextPlan) {
	if plan.ProviderConversationID == "" || strings.TrimSpace(plan.ResponseText) == "" {
		return
	}
	// remember only checks that the provider holds state for this id. It must
	// NOT be gated by the untrusted flag: even if a conversation was flagged
	// untrusted mid-stream, we still want to record its transcript so a later
	// rebuilt turn can reuse the locally retained full history.
	if s.client != nil && !s.client.HasConversationState(plan.ProviderConversationID) {
		if s.log != nil {
			s.log.Warn("skip OpenAI transcript context because provider conversation state is missing",
				zap.String("provider_conversation_id", plan.ProviderConversationID),
			)
		}
		return
	}

	next := append(cloneChatMessages(plan.RequestMessages), dto.ChatCompletionMessage{
		Role:    "assistant",
		Content: plan.ResponseText,
	})
	key := transcriptFingerprint(next)
	if key == "" {
		return
	}

	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.transcriptContexts == nil {
		s.transcriptContexts = make(map[string]string)
	}
	if s.transcriptContextUpdated == nil {
		s.transcriptContextUpdated = make(map[string]time.Time)
	}
	if s.transcriptSuffixContexts == nil {
		s.transcriptSuffixContexts = make(map[string]string)
	}
	if s.transcriptSuffixUpdated == nil {
		s.transcriptSuffixUpdated = make(map[string]time.Time)
	}
	if s.userWindowContexts == nil {
		s.userWindowContexts = make(map[string]string)
	}
	if s.userWindowUpdated == nil {
		s.userWindowUpdated = make(map[string]time.Time)
	}
	if s.providerLatestTranscript == nil {
		s.providerLatestTranscript = make(map[string]string)
	}
	if s.providerLatestLength == nil {
		s.providerLatestLength = make(map[string]int)
	}
	if s.rootContexts == nil {
		s.rootContexts = make(map[string]string)
	}
	if s.rootContextUpdated == nil {
		s.rootContextUpdated = make(map[string]time.Time)
	}
	if s.explicitProviderContexts == nil {
		s.explicitProviderContexts = make(map[string]string)
	}
	if s.explicitProviderUpdated == nil {
		s.explicitProviderUpdated = make(map[string]time.Time)
	}
	now := time.Now()
	s.pruneTranscriptContextsLocked(time.Now())
	if plan.ClientConversationID != "" {
		s.explicitProviderContexts[plan.ClientConversationID] = plan.ProviderConversationID
		s.explicitProviderUpdated[plan.ClientConversationID] = now
	}
	s.transcriptContexts[key] = plan.ProviderConversationID
	s.transcriptContextUpdated[key] = now
	s.rememberTranscriptSuffixesLocked(next, plan.ProviderConversationID, now)
	s.rememberUserWindowsLocked(next, plan.ProviderConversationID, now)
	s.providerLatestTranscript[plan.ProviderConversationID] = key
	s.providerLatestLength[plan.ProviderConversationID] = len(next)
	if rootKey := conversationRootFingerprint(next); rootKey != "" {
		s.rootContexts[rootKey] = plan.ProviderConversationID
		s.rootContextUpdated[rootKey] = now
		s.promoteToolPlannerContextLocked("root:"+rootKey, "provider:"+plan.ProviderConversationID, now)
	}
	if plan.ClientConversationID != "" {
		s.promoteToolPlannerContextLocked("client:"+plan.ClientConversationID, "provider:"+plan.ProviderConversationID, now)
	}
}

func (s *OpenAIService) promoteToolPlannerContextLocked(fromKey, toKey string, now time.Time) {
	fromKey = strings.TrimSpace(fromKey)
	toKey = strings.TrimSpace(toKey)
	if fromKey == "" || toKey == "" || fromKey == toKey {
		return
	}
	if s.toolPlannerContexts == nil {
		return
	}
	plannerID := strings.TrimSpace(s.toolPlannerContexts[fromKey])
	if plannerID == "" {
		return
	}
	if s.toolPlannerUpdated == nil {
		s.toolPlannerUpdated = make(map[string]time.Time)
	}
	if strings.TrimSpace(s.toolPlannerContexts[toKey]) == "" {
		s.toolPlannerContexts[toKey] = plannerID
	}
	s.toolPlannerUpdated[toKey] = now
}

func (s *OpenAIService) forgetToolPlannerConversation(plannerID string) {
	plannerID = strings.TrimSpace(plannerID)
	if plannerID == "" {
		return
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	for key, id := range s.toolPlannerContexts {
		if id == plannerID {
			delete(s.toolPlannerContexts, key)
			delete(s.toolPlannerUpdated, key)
		}
	}
}

func (s *OpenAIService) providerConversationReady(id string) bool {
	if s.client == nil {
		return true
	}
	if !s.client.HasConversationState(id) {
		return false
	}
	// A provider conversation flagged untrusted (continuity mismatch or bard
	// error) must not be reused for server-side context. This forces the next
	// turn to rebuild from the locally retained full history instead of
	// trusting a broken Gemini-side record. Guarded by an env switch so the
	// behavior can be disabled for diagnosis.
	if openAILocalFallbackEnabled() && s.client.IsConversationUntrusted(id) {
		return false
	}
	return true
}

func (s *OpenAIService) toolPlannerConversationID(plan openAIContextPlan, req dto.ChatCompletionRequest) string {
	return s.toolPlannerConversationPlan(plan, req).ConversationID
}

func (s *OpenAIService) toolPlannerConversationPlan(plan openAIContextPlan, req dto.ChatCompletionRequest) toolPlannerContextPlan {
	key := toolPlannerContextKey(plan, req)
	if key == "" {
		return toolPlannerContextPlan{ConversationID: newAutoProviderConversationID()}
	}

	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.toolPlannerContexts == nil {
		s.toolPlannerContexts = make(map[string]string)
	}
	if s.toolPlannerUpdated == nil {
		s.toolPlannerUpdated = make(map[string]time.Time)
	}
	now := time.Now()
	for k, updatedAt := range s.toolPlannerUpdated {
		if now.Sub(updatedAt) > transcriptContextTTL {
			delete(s.toolPlannerContexts, k)
			delete(s.toolPlannerUpdated, k)
		}
	}
	if id := strings.TrimSpace(s.toolPlannerContexts[key]); id != "" {
		if s.client == nil || s.client.HasConversationState(id) {
			s.toolPlannerUpdated[key] = now
			return toolPlannerContextPlan{ConversationID: id, Reused: true}
		}
		delete(s.toolPlannerContexts, key)
		delete(s.toolPlannerUpdated, key)
	}
	id := newAutoProviderConversationID()
	s.toolPlannerContexts[key] = id
	s.toolPlannerUpdated[key] = now
	return toolPlannerContextPlan{ConversationID: id}
}

func toolPlannerContextKey(plan openAIContextPlan, req dto.ChatCompletionRequest) string {
	if id := strings.TrimSpace(plan.ProviderConversationID); id != "" && plan.AutoContext {
		return "provider:" + id
	}
	if id := strings.TrimSpace(req.ConversationID); id != "" {
		return "client:" + id
	}
	if root := conversationRootFingerprint(req.Messages); root != "" {
		return "root:" + root
	}
	if latest, ok := latestUserMessage(req.Messages); ok {
		return "latest:" + sha256Hex(rawUserPrompt(latest))
	}
	return ""
}

func shouldRetrySameProviderContext(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, token := range []string{
		"gemini bard error 1097",
		"conversation continuity mismatch",
		"authentication failed",
		"cookies invalid",
		"status 401",
		"status 403",
	} {
		if strings.Contains(msg, token) {
			return false
		}
	}
	return true
}

func (s *OpenAIService) forgetProviderConversation(providerID string) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	for key, id := range s.transcriptContexts {
		if id == providerID {
			delete(s.transcriptContexts, key)
			delete(s.transcriptContextUpdated, key)
		}
	}
	for key, id := range s.transcriptSuffixContexts {
		if id == providerID {
			delete(s.transcriptSuffixContexts, key)
			delete(s.transcriptSuffixUpdated, key)
		}
	}
	for key, id := range s.userWindowContexts {
		if id == providerID {
			delete(s.userWindowContexts, key)
			delete(s.userWindowUpdated, key)
		}
	}
	for key, id := range s.rootContexts {
		if id == providerID {
			delete(s.rootContexts, key)
			delete(s.rootContextUpdated, key)
		}
	}
	for key, id := range s.explicitProviderContexts {
		if id == providerID {
			delete(s.explicitProviderContexts, key)
			delete(s.explicitProviderUpdated, key)
		}
	}
	delete(s.providerLatestTranscript, providerID)
	delete(s.providerLatestLength, providerID)
}

func (s *OpenAIService) providerConversationForExplicitID(clientID string) string {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return ""
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.explicitProviderContexts == nil {
		return ""
	}
	updatedAt, hasUpdatedAt := s.explicitProviderUpdated[clientID]
	if hasUpdatedAt && time.Since(updatedAt) > transcriptContextTTL {
		delete(s.explicitProviderContexts, clientID)
		delete(s.explicitProviderUpdated, clientID)
		return ""
	}
	providerID := s.explicitProviderContexts[clientID]
	if providerID == "" || !s.providerConversationReady(providerID) {
		return ""
	}
	if s.explicitProviderUpdated == nil {
		s.explicitProviderUpdated = make(map[string]time.Time)
	}
	s.explicitProviderUpdated[clientID] = time.Now()
	return providerID
}

func (s *OpenAIService) providerConversationForTranscript(messages []dto.ChatCompletionMessage) string {
	key := transcriptFingerprint(messages)
	if key == "" {
		return ""
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.transcriptContexts == nil {
		return ""
	}
	updatedAt, hasUpdatedAt := s.transcriptContextUpdated[key]
	if hasUpdatedAt && time.Since(updatedAt) > transcriptContextTTL {
		delete(s.transcriptContexts, key)
		delete(s.transcriptContextUpdated, key)
		return ""
	}
	providerID := s.transcriptContexts[key]
	if providerID != "" && s.providerLatestTranscript != nil && s.providerLatestTranscript[providerID] != "" && s.providerLatestTranscript[providerID] != key {
		return ""
	}
	if providerID != "" && !s.providerConversationReady(providerID) {
		return ""
	}
	if providerID != "" {
		if s.transcriptContextUpdated == nil {
			s.transcriptContextUpdated = make(map[string]time.Time)
		}
		s.transcriptContextUpdated[key] = time.Now()
	}
	return providerID
}

func (s *OpenAIService) providerConversationForTranscriptSuffix(messages []dto.ChatCompletionMessage) string {
	if len(messages) < 2 {
		return ""
	}
	key := transcriptFingerprint(messages)
	if key == "" {
		return ""
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.transcriptSuffixContexts == nil {
		return ""
	}
	updatedAt, hasUpdatedAt := s.transcriptSuffixUpdated[key]
	if hasUpdatedAt && time.Since(updatedAt) > transcriptContextTTL {
		delete(s.transcriptSuffixContexts, key)
		delete(s.transcriptSuffixUpdated, key)
		return ""
	}
	providerID := s.transcriptSuffixContexts[key]
	if providerID == "" || !s.providerConversationReady(providerID) {
		return ""
	}
	latestKey := ""
	if s.providerLatestTranscript != nil {
		latestKey = s.providerLatestTranscript[providerID]
	}
	if latestKey == "" || latestKey == key {
		return ""
	}
	if s.transcriptSuffixUpdated == nil {
		s.transcriptSuffixUpdated = make(map[string]time.Time)
	}
	s.transcriptSuffixUpdated[key] = time.Now()
	return providerID
}

func (s *OpenAIService) providerConversationForUserWindow(messages []dto.ChatCompletionMessage) string {
	key := userWindowFingerprint(messages, 2, 4)
	if key == "" {
		return ""
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.userWindowContexts == nil {
		return ""
	}
	updatedAt, hasUpdatedAt := s.userWindowUpdated[key]
	if hasUpdatedAt && time.Since(updatedAt) > transcriptContextTTL {
		delete(s.userWindowContexts, key)
		delete(s.userWindowUpdated, key)
		return ""
	}
	providerID := s.userWindowContexts[key]
	if providerID == "" || !s.providerConversationReady(providerID) {
		return ""
	}
	if s.userWindowUpdated == nil {
		s.userWindowUpdated = make(map[string]time.Time)
	}
	s.userWindowUpdated[key] = time.Now()
	return providerID
}

func (s *OpenAIService) providerConversationForRoot(messages []dto.ChatCompletionMessage) string {
	key := conversationRootFingerprint(messages)
	if key == "" {
		return ""
	}
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	if s.rootContexts == nil {
		return ""
	}
	updatedAt, hasUpdatedAt := s.rootContextUpdated[key]
	if hasUpdatedAt && time.Since(updatedAt) > transcriptContextTTL {
		delete(s.rootContexts, key)
		delete(s.rootContextUpdated, key)
		return ""
	}
	providerID := s.rootContexts[key]
	if providerID == "" || !s.providerConversationReady(providerID) {
		return ""
	}
	if latestLen := s.providerLatestLength[providerID]; latestLen > 0 && len(messages) < latestLen {
		return ""
	}
	if s.rootContextUpdated == nil {
		s.rootContextUpdated = make(map[string]time.Time)
	}
	s.rootContextUpdated[key] = time.Now()
	return providerID
}

func (s *OpenAIService) rememberTranscriptSuffixesLocked(messages []dto.ChatCompletionMessage, providerID string, now time.Time) {
	if len(messages) < 4 || providerID == "" {
		return
	}
	for start := 1; start < len(messages)-1; start++ {
		suffix := messages[start:]
		if len(suffix) < 2 {
			continue
		}
		key := transcriptFingerprint(suffix)
		if key == "" {
			continue
		}
		s.transcriptSuffixContexts[key] = providerID
		s.transcriptSuffixUpdated[key] = now
	}
}

func (s *OpenAIService) rememberUserWindowsLocked(messages []dto.ChatCompletionMessage, providerID string, now time.Time) {
	if providerID == "" {
		return
	}
	for size := 2; size <= 4; size++ {
		key := userWindowFingerprint(messages, size, size)
		if key == "" {
			continue
		}
		s.userWindowContexts[key] = providerID
		s.userWindowUpdated[key] = now
	}
}

func (s *OpenAIService) pruneTranscriptContextsLocked(now time.Time) {
	for key, updatedAt := range s.transcriptContextUpdated {
		if now.Sub(updatedAt) > transcriptContextTTL {
			providerID := s.transcriptContexts[key]
			delete(s.transcriptContexts, key)
			delete(s.transcriptContextUpdated, key)
			if s.providerLatestTranscript[providerID] == key {
				delete(s.providerLatestTranscript, providerID)
				delete(s.providerLatestLength, providerID)
			}
		}
	}
	for key, updatedAt := range s.rootContextUpdated {
		if now.Sub(updatedAt) > transcriptContextTTL {
			delete(s.rootContexts, key)
			delete(s.rootContextUpdated, key)
		}
	}
	for key, updatedAt := range s.transcriptSuffixUpdated {
		if now.Sub(updatedAt) > transcriptContextTTL {
			delete(s.transcriptSuffixContexts, key)
			delete(s.transcriptSuffixUpdated, key)
		}
	}
	for key, updatedAt := range s.userWindowUpdated {
		if now.Sub(updatedAt) > transcriptContextTTL {
			delete(s.userWindowContexts, key)
			delete(s.userWindowUpdated, key)
		}
	}
	for len(s.transcriptContexts) > maxTranscriptContextEntries {
		var oldestKey string
		var oldestTime time.Time
		for key := range s.transcriptContexts {
			updatedAt := s.transcriptContextUpdated[key]
			if oldestKey == "" || updatedAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = updatedAt
			}
		}
		if oldestKey == "" {
			return
		}
		providerID := s.transcriptContexts[oldestKey]
		delete(s.transcriptContexts, oldestKey)
		delete(s.transcriptContextUpdated, oldestKey)
		if s.providerLatestTranscript[providerID] == oldestKey {
			delete(s.providerLatestTranscript, providerID)
			delete(s.providerLatestLength, providerID)
		}
	}
	for len(s.rootContexts) > maxTranscriptContextEntries {
		var oldestKey string
		var oldestTime time.Time
		for key := range s.rootContexts {
			updatedAt := s.rootContextUpdated[key]
			if oldestKey == "" || updatedAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = updatedAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.rootContexts, oldestKey)
		delete(s.rootContextUpdated, oldestKey)
	}
	for len(s.transcriptSuffixContexts) > maxTranscriptContextEntries {
		var oldestKey string
		var oldestTime time.Time
		for key := range s.transcriptSuffixContexts {
			updatedAt := s.transcriptSuffixUpdated[key]
			if oldestKey == "" || updatedAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = updatedAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.transcriptSuffixContexts, oldestKey)
		delete(s.transcriptSuffixUpdated, oldestKey)
	}
	for len(s.userWindowContexts) > maxTranscriptContextEntries {
		var oldestKey string
		var oldestTime time.Time
		for key := range s.userWindowContexts {
			updatedAt := s.userWindowUpdated[key]
			if oldestKey == "" || updatedAt.Before(oldestTime) {
				oldestKey = key
				oldestTime = updatedAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(s.userWindowContexts, oldestKey)
		delete(s.userWindowUpdated, oldestKey)
	}
}

func splitOpenAIHistoryPrefix(messages []dto.ChatCompletionMessage) ([]dto.ChatCompletionMessage, dto.ChatCompletionMessage, bool) {
	if len(messages) < 2 {
		return nil, dto.ChatCompletionMessage{}, false
	}
	last, ok := latestUserMessage(messages)
	if !ok {
		return nil, dto.ChatCompletionMessage{}, false
	}
	prefix := cloneChatMessages(messages[:len(messages)-1])
	if len(prefix) == 0 {
		return nil, dto.ChatCompletionMessage{}, false
	}
	return prefix, last, true
}

func latestUserMessage(messages []dto.ChatCompletionMessage) (dto.ChatCompletionMessage, bool) {
	if len(messages) == 0 {
		return dto.ChatCompletionMessage{}, false
	}
	last := messages[len(messages)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "user") {
		return dto.ChatCompletionMessage{}, false
	}
	if strings.TrimSpace(last.Content) == "" && len(last.Attachments) == 0 {
		return dto.ChatCompletionMessage{}, false
	}
	return last, true
}

func rawUserPrompt(msg dto.ChatCompletionMessage) string {
	content := strings.TrimSpace(msg.Content)
	if content != "" {
		return content
	}
	if len(msg.Attachments) > 0 {
		return fmt.Sprintf("[%d file(s) attached]", len(msg.Attachments))
	}
	return ""
}

func buildGeminiWebPromptFromOpenAIMessages(messages []dto.ChatCompletionMessage) string {
	if prompt := buildFirstTurnPrompt(messages); prompt != "" {
		return prompt
	}
	modelMessages := make([]models.Message, 0, len(messages))
	for _, msg := range messages {
		modelMessages = append(modelMessages, msg.ToModelMessage())
	}
	return utils.BuildPromptFromMessages(modelMessages, "")
}

func buildFirstTurnPrompt(messages []dto.ChatCompletionMessage) string {
	if len(messages) == 0 {
		return ""
	}

	systemParts := make([]string, 0, len(messages))
	userMessages := make([]dto.ChatCompletionMessage, 0, 1)
	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system":
			if content := strings.TrimSpace(msg.Content); content != "" {
				systemParts = append(systemParts, content)
			}
		case "user":
			userMessages = append(userMessages, msg)
		default:
			return ""
		}
	}
	if len(userMessages) != 1 {
		return ""
	}

	userPrompt := rawUserPrompt(userMessages[0])
	if userPrompt == "" {
		return ""
	}
	if len(systemParts) == 0 {
		return userPrompt
	}

	return "**Persona**: " + strings.Join(inlineCodeParts(systemParts), "\n\n") + "\n\n" + userPrompt
}

func inlineCodeParts(parts []string) []string {
	wrapped := make([]string, 0, len(parts))
	for _, part := range parts {
		wrapped = append(wrapped, "`"+strings.ReplaceAll(part, "`", "'")+"`")
	}
	return wrapped
}

func newAutoProviderConversationID() string {
	return fmt.Sprintf("%016x", uint64(time.Now().UnixNano())^uint64(rand.Int63()))[:16]
}

func fallbackOptionsWithFreshConversation(plan *openAIContextPlan, baseOpts []providers.GenerateOption) []providers.GenerateOption {
	fallbackID := newAutoProviderConversationID()
	plan.ProviderConversationID = fallbackID
	plan.AutoContext = false

	opts := append([]providers.GenerateOption{}, baseOpts...)
	return append(opts, providers.WithConversationID(fallbackID))
}

type openAIUpstreamTrace struct {
	RequestID                 string                    `json:"request_id"`
	Stage                     string                    `json:"stage"`
	Phase                     string                    `json:"phase"`
	CreatedAt                 string                    `json:"created_at"`
	ClientConversationID      string                    `json:"client_conversation_id,omitempty"`
	ProviderConversationID    string                    `json:"provider_conversation_id,omitempty"`
	ToolPlannerConversationID string                    `json:"tool_planner_conversation_id,omitempty"`
	AutoContext               bool                      `json:"auto_context"`
	UseToolBridge             bool                      `json:"use_tool_bridge"`
	Model                     string                    `json:"model,omitempty"`
	Stream                    bool                      `json:"stream"`
	MessageCount              int                       `json:"message_count"`
	Messages                  []openAIMessageLogSummary `json:"messages"`
	PromptLength              int                       `json:"prompt_length"`
	PromptSHA256              string                    `json:"prompt_sha256,omitempty"`
	PromptPreview             string                    `json:"prompt_preview,omitempty"`
	ProviderConfig            openAIProviderTraceConfig `json:"provider_config"`
	InputFiles                []openAITraceInputFile    `json:"input_files,omitempty"`
	PreviousError             string                    `json:"previous_error,omitempty"`
}

type openAIProviderTraceConfig struct {
	Model          string `json:"model,omitempty"`
	ThinkingLevel  string `json:"thinking_level,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	SourcePath     bool   `json:"source_path,omitempty"`
	InputFileCount int    `json:"input_file_count,omitempty"`
	PathFileCount  int    `json:"path_file_count,omitempty"`
}

type openAITraceInputFile struct {
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Bytes    int    `json:"bytes"`
	SHA256   string `json:"sha256,omitempty"`
}

func (s *OpenAIService) dumpOpenAITrace(ctx context.Context, req dto.ChatCompletionRequest, plan openAIContextPlan, stage, prompt string, opts []providers.GenerateOption, inputFiles []providers.InputFile, useToolBridge bool, previousErr error) {
	dir := strings.TrimSpace(os.Getenv("GEMINI_DEBUG_STREAM_DIR"))
	if dir == "" || !openAIRequestDebugEnabled() {
		return
	}
	requestID := openAIRequestIDFromContext(ctx)
	if requestID == "" {
		requestID = generateChatID()
	}
	var cfg providers.GenerateConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	trace := openAIUpstreamTrace{
		RequestID:                 requestID,
		Stage:                     stage,
		Phase:                     openAITracePhase(stage, plan, useToolBridge),
		CreatedAt:                 time.Now().Format(time.RFC3339Nano),
		ClientConversationID:      strings.TrimSpace(req.ConversationID),
		ProviderConversationID:    strings.TrimSpace(plan.ProviderConversationID),
		ToolPlannerConversationID: strings.TrimSpace(plan.ToolPlannerConversationID),
		AutoContext:               plan.AutoContext,
		UseToolBridge:             useToolBridge,
		Model:                     req.Model,
		Stream:                    req.Stream,
		MessageCount:              len(req.Messages),
		Messages:                  summarizeOpenAIMessages(req.Messages),
		PromptLength:              len(prompt),
		PromptSHA256:              sha256Hex(prompt),
		PromptPreview:             previewForLog(prompt, 4000),
		ProviderConfig: openAIProviderTraceConfig{
			Model:          cfg.Model,
			ThinkingLevel:  cfg.ThinkingLevel,
			ConversationID: strings.TrimSpace(cfg.ConversationID),
			SourcePath:     cfg.SourcePath,
			InputFileCount: len(cfg.InputFiles),
			PathFileCount:  len(cfg.Files),
		},
		InputFiles: summarizeTraceInputFiles(inputFiles),
	}
	if previousErr != nil {
		trace.PreviousError = previousErr.Error()
	}
	body, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		if s.log != nil {
			s.log.Warn("failed to create OpenAI upstream trace directory", zap.String("dir", dir), zap.Error(err))
		}
		return
	}
	shortID := requestID
	if len(shortID) > 16 {
		shortID = shortID[:16]
	}
	name := fmt.Sprintf("%s_%s_%s_openai_upstream_trace.json", time.Now().Format("20060102_150405.000"), sanitizeOpenAIDebugFilename(shortID), sanitizeOpenAIDebugFilename(stage))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0600); err != nil {
		if s.log != nil {
			s.log.Warn("failed to write OpenAI upstream trace", zap.String("path", path), zap.Error(err))
		}
		return
	}
	if s.log != nil {
		s.log.Info("OpenAI upstream trace written", zap.String("path", path), zap.String("stage", stage), zap.String("request_id", requestID))
	}
}

func openAITracePhase(stage string, plan openAIContextPlan, useToolBridge bool) string {
	if strings.Contains(stage, "tool_repair") {
		return "tool_planner_repair"
	}
	if useToolBridge {
		return "tool_planner"
	}
	if strings.TrimSpace(plan.Phase) != "" {
		return strings.TrimSpace(plan.Phase)
	}
	return "main"
}

func summarizeTraceInputFiles(files []providers.InputFile) []openAITraceInputFile {
	out := make([]openAITraceInputFile, 0, len(files))
	for _, file := range files {
		out = append(out, openAITraceInputFile{
			Name:     strings.TrimSpace(file.Name),
			MimeType: strings.TrimSpace(file.MimeType),
			Bytes:    len(file.Data),
			SHA256:   sha256BytesHex(file.Data),
		})
	}
	return out
}

func sha256Hex(value string) string {
	if value == "" {
		return ""
	}
	return sha256BytesHex([]byte(value))
}

func sha256BytesHex(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func transcriptFingerprint(messages []dto.ChatCompletionMessage) string {
	type normalizedMessage struct {
		Role        string                       `json:"role"`
		Content     string                       `json:"content"`
		ToolCalls   []dto.ChatCompletionToolCall `json:"tool_calls,omitempty"`
		ToolCallID  string                       `json:"tool_call_id,omitempty"`
		Name        string                       `json:"name,omitempty"`
		Attachments []models.Attachment          `json:"attachments,omitempty"`
	}
	normalized := make([]normalizedMessage, 0, len(messages))
	for _, msg := range messages {
		normalized = append(normalized, normalizedMessage{
			Role:        strings.ToLower(strings.TrimSpace(msg.Role)),
			Content:     strings.TrimSpace(msg.Content),
			ToolCalls:   msg.ToolCalls,
			ToolCallID:  strings.TrimSpace(msg.ToolCallID),
			Name:        strings.TrimSpace(msg.Name),
			Attachments: msg.Attachments,
		})
	}
	if len(normalized) == 0 {
		return ""
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}

func conversationRootFingerprint(messages []dto.ChatCompletionMessage) string {
	if len(messages) == 0 {
		return ""
	}
	root := make([]dto.ChatCompletionMessage, 0, 2)
	for _, msg := range messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "system":
			root = append(root, msg)
		case "user":
			root = append(root, msg)
			return transcriptFingerprint(root)
		default:
			return ""
		}
	}
	return ""
}

func userWindowFingerprint(messages []dto.ChatCompletionMessage, minUsers, maxUsers int) string {
	if minUsers <= 0 || maxUsers < minUsers {
		return ""
	}
	type normalizedUserMessage struct {
		Content     string              `json:"content"`
		Attachments []models.Attachment `json:"attachments,omitempty"`
	}
	users := make([]normalizedUserMessage, 0, maxUsers)
	for i := len(messages) - 1; i >= 0 && len(users) < maxUsers; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" && len(msg.Attachments) == 0 {
			continue
		}
		users = append(users, normalizedUserMessage{
			Content:     content,
			Attachments: msg.Attachments,
		})
	}
	if len(users) < minUsers {
		return ""
	}
	for i, j := 0, len(users)-1; i < j; i, j = i+1, j-1 {
		users[i], users[j] = users[j], users[i]
	}
	body, err := json.Marshal(users)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])[:32]
}

func cloneChatMessages(messages []dto.ChatCompletionMessage) []dto.ChatCompletionMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]dto.ChatCompletionMessage, len(messages))
	copy(out, messages)
	return out
}

type toolBridgePayload struct {
	Status    string           `json:"status"`
	ToolCalls []toolBridgeCall `json:"tool_calls"`
	Content   string           `json:"content"`
}

type toolBridgeCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolBridgePlan struct {
	ToolCalls []dto.ChatCompletionToolCall
	Content   string
	NoTool    bool
	Err       error
}

type toolPlannerContextPlan struct {
	ConversationID string
	Reused         bool
}

func shouldUseToolBridge(req dto.ChatCompletionRequest) bool {
	if !req.HasToolsEnabled() {
		return false
	}
	if isToolResultRequest(req) {
		return false
	}
	_, ok := latestUserMessage(req.Messages)
	if !ok {
		return false
	}
	switch req.ToolChoiceMode() {
	case "required", "function":
		return true
	case "auto":
		return true
	default:
		return false
	}
}

func isToolResultRequest(req dto.ChatCompletionRequest) bool {
	if len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	return strings.EqualFold(strings.TrimSpace(last.Role), "tool")
}

func mustBufferToolBridge(req dto.ChatCompletionRequest) bool {
	switch req.ToolChoiceMode() {
	case "required", "function":
		return true
	default:
		return false
	}
}

func toolBridgeRequiresToolCall(req dto.ChatCompletionRequest) bool {
	switch req.ToolChoiceMode() {
	case "required", "function":
		return true
	}
	return false
}

func classifyToolBridgeStreamPrefix(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "unknown"
	}
	if strings.HasPrefix(trimmed, "```") {
		return "unknown"
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return "tool_json"
	}
	return "content"
}

func buildToolPlanningPrompt(req dto.ChatCompletionRequest) string {
	var b strings.Builder
	b.WriteString("Conversation context for deciding whether a tool is needed:\n")
	for _, msg := range compactToolPlanningMessages(req.Messages, 8) {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "assistant":
			if len(msg.ToolCalls) > 0 {
				b.WriteString("Assistant tool call: ")
				b.WriteString(compactText(toolCallsForPrompt(msg.ToolCalls), 800))
				b.WriteString("\n")
				continue
			}
			b.WriteString("Assistant: ")
			b.WriteString(compactText(msg.Content, 800))
			b.WriteString("\n")
		case "tool":
			b.WriteString("Tool result")
			if strings.TrimSpace(msg.Name) != "" {
				b.WriteString(" from ")
				b.WriteString(strings.TrimSpace(msg.Name))
			}
			if strings.TrimSpace(msg.ToolCallID) != "" {
				b.WriteString(" (")
				b.WriteString(strings.TrimSpace(msg.ToolCallID))
				b.WriteString(")")
			}
			b.WriteString(": ")
			b.WriteString(compactText(msg.Content, 1800))
			b.WriteString("\n")
		case "system":
			b.WriteString("System: ")
			b.WriteString(compactText(msg.Content, 500))
			b.WriteString("\n")
		default:
			b.WriteString("User: ")
			b.WriteString(compactText(msg.Content, 1000))
			b.WriteString("\n")
		}
	}
	if latest, ok := latestUserMessage(req.Messages); ok {
		b.WriteString("\nCurrent user request:\n")
		b.WriteString(compactText(latest.Content, 1500))
	}
	return strings.TrimSpace(b.String())
}

func compactToolPlanningMessages(messages []dto.ChatCompletionMessage, maxMessages int) []dto.ChatCompletionMessage {
	if maxMessages <= 0 || len(messages) <= maxMessages {
		return cloneChatMessages(messages)
	}
	return cloneChatMessages(messages[len(messages)-maxMessages:])
}

func toolCallsForPrompt(calls []dto.ChatCompletionToolCall) string {
	body, err := json.Marshal(calls)
	if err != nil {
		return ""
	}
	return string(body)
}

func compactText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit] + "\n[truncated]"
}

func (s *OpenAIService) buildToolBridgePrompt(req dto.ChatCompletionRequest, basePrompt string, requireToolCall bool) string {
	var b strings.Builder
	b.WriteString("You are a tool-planning router for an OpenAI-compatible bridge to Gemini web.\n")
	b.WriteString("You are NOT the final assistant. Decide whether the current request needs a tool.\n")
	b.WriteString("Return exactly one JSON object and no surrounding text.\n")
	b.WriteString("Output schema:\n")
	b.WriteString("{\"status\":\"tool_calls\",\"tool_calls\":[{\"name\":\"<tool_name>\",\"arguments\":{}}]} OR {\"status\":\"no_tool\"}\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Use only tool names listed below.\n")
	b.WriteString("- arguments must be a valid JSON object matching the tool's parameters schema.\n")
	b.WriteString("- Do not put JSON in markdown code fences.\n")
	b.WriteString("- If no tool is needed and tool_choice is auto, return {\"status\":\"no_tool\"}. Do not answer the user.\n")
	if requireToolCall {
		b.WriteString("- The current user request requires external/web/tool data. You must return status=tool_calls with at least one valid tool call. Do not answer from memory.\n")
	}

	toolChoiceMode := req.ToolChoiceMode()
	if toolChoiceMode == "required" {
		b.WriteString("- tool_choice is required: return status=tool_calls with at least one tool call.\n")
	}
	if toolChoiceMode == "function" {
		forced := req.ForcedToolName()
		if forced != "" {
			b.WriteString("- tool_choice selects one function. Return exactly one tool call with name: ")
			b.WriteString(forced)
			b.WriteString("\n")
		}
	}
	if toolChoiceMode == "none" {
		b.WriteString("- Tool calling disabled. Answer normally.\n")
	}

	b.WriteString("Available tools:\n")
	for _, t := range req.Tools {
		if !strings.EqualFold(t.Type, "function") || strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		b.WriteString("- name: ")
		b.WriteString(strings.TrimSpace(t.Function.Name))
		if strings.TrimSpace(t.Function.Description) != "" {
			b.WriteString(" | description: ")
			b.WriteString(strings.TrimSpace(t.Function.Description))
		}
		if len(t.Function.Parameters) > 0 {
			b.WriteString(" | parameters: ")
			b.Write(t.Function.Parameters)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nConversation:\n")
	b.WriteString(basePrompt)
	return b.String()
}

func (s *OpenAIService) buildToolBridgeContinuationPrompt(req dto.ChatCompletionRequest, basePrompt string, requireToolCall bool) string {
	var b strings.Builder
	b.WriteString("Continue as the same tool-planning router. Use the JSON protocol and available tools already defined in this Gemini conversation.\n")
	b.WriteString("Return exactly one JSON object: {\"status\":\"tool_calls\",\"tool_calls\":[...]} or {\"status\":\"no_tool\"}. Do not answer the user directly.\n")
	if requireToolCall {
		b.WriteString("This request requires a tool call; return status=tool_calls.\n")
	}
	toolChoiceMode := req.ToolChoiceMode()
	if toolChoiceMode == "required" {
		b.WriteString("tool_choice is required; return at least one valid tool call.\n")
	}
	if toolChoiceMode == "function" {
		if forced := req.ForcedToolName(); forced != "" {
			b.WriteString("tool_choice selects function: ")
			b.WriteString(forced)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nCurrent planning context:\n")
	b.WriteString(basePrompt)
	return b.String()
}

func (s *OpenAIService) parseToolBridgeOutput(req dto.ChatCompletionRequest, text string) ([]dto.ChatCompletionToolCall, string) {
	plan := s.parseToolBridgePlan(req, text)
	return plan.ToolCalls, plan.Content
}

func (s *OpenAIService) resolveToolBridgeOutput(ctx context.Context, req dto.ChatCompletionRequest, text string, opts []providers.GenerateOption, inputFiles []providers.InputFile, requestID string) toolBridgePlan {
	plan := s.parseToolBridgePlan(req, text)
	if plan.Err == nil {
		return plan
	}
	if !toolBridgeRequiresToolCall(req) && !looksLikeToolPlannerOutput(text) {
		return toolBridgePlan{Content: strings.TrimSpace(text), NoTool: true}
	}

	if s.log != nil {
		s.log.Warn("OpenAI tool bridge parse failed, attempting repair",
			zap.String("request_id", requestID),
			zap.String("tool_choice_mode", req.ToolChoiceMode()),
			zap.Error(plan.Err),
		)
	}

	repairPrompt := s.buildToolBridgeRepairPrompt(req, text, plan.Err)
	repairOpts := append([]providers.GenerateOption{}, opts...)
	s.dumpOpenAITrace(ctx, req, openAIContextPlan{}, "tool_repair", repairPrompt, repairOpts, inputFiles, true, plan.Err)
	repairResp, err := s.client.GenerateContent(ctx, repairPrompt, repairOpts...)
	if err == nil && repairResp != nil {
		repaired := s.parseToolBridgePlan(req, repairResp.Text)
		if repaired.Err == nil {
			return repaired
		}
		plan.Err = fmt.Errorf("%w; repair failed: %v", plan.Err, repaired.Err)
	} else if err != nil {
		plan.Err = fmt.Errorf("%w; repair request failed: %v", plan.Err, err)
	}

	if toolBridgeRequiresToolCall(req) {
		return toolBridgePlan{Err: plan.Err, Content: ""}
	}
	if strings.TrimSpace(plan.Content) != "" {
		return toolBridgePlan{Content: plan.Content, NoTool: true}
	}
	return toolBridgePlan{NoTool: true}
}

func looksLikeToolPlannerOutput(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "```") {
		return true
	}
	return strings.Contains(trimmed, `"tool_calls"`) || strings.Contains(trimmed, `"status"`)
}

func (s *OpenAIService) buildToolBridgeRepairPrompt(req dto.ChatCompletionRequest, invalidOutput string, parseErr error) string {
	var b strings.Builder
	b.WriteString("Repair the previous tool-planning output. Return exactly one JSON object and no surrounding text.\n")
	b.WriteString("Allowed output schemas:\n")
	b.WriteString("{\"status\":\"tool_calls\",\"tool_calls\":[{\"name\":\"<tool_name>\",\"arguments\":{}}]}\n")
	b.WriteString("{\"status\":\"message\",\"content\":\"<assistant_text>\"}\n")
	b.WriteString("Validation error:\n")
	b.WriteString(parseErr.Error())
	b.WriteString("\nAvailable tools:\n")
	for _, t := range req.Tools {
		if !strings.EqualFold(t.Type, "function") || strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		b.WriteString("- name: ")
		b.WriteString(strings.TrimSpace(t.Function.Name))
		if len(t.Function.Parameters) > 0 {
			b.WriteString(" | parameters: ")
			b.Write(t.Function.Parameters)
		}
		b.WriteString("\n")
	}
	b.WriteString("Invalid output:\n")
	b.WriteString(compactText(invalidOutput, 4000))
	return b.String()
}

func (s *OpenAIService) parseToolBridgePlan(req dto.ChatCompletionRequest, text string) toolBridgePlan {
	cleaned := utils.StripCodeFence(text)
	if cleaned == "" {
		return toolBridgePlan{Err: fmt.Errorf("empty tool planner output")}
	}

	payload, ok := decodeToolBridgePayload(cleaned)
	if !ok {
		return toolBridgePlan{Content: strings.TrimSpace(text), Err: fmt.Errorf("tool planner output is not valid JSON")}
	}
	return validateToolBridgePayload(req, payload, text)
}

func validateToolBridgePayload(req dto.ChatCompletionRequest, payload toolBridgePayload, originalText string) toolBridgePlan {
	status := strings.ToLower(strings.TrimSpace(payload.Status))
	if status == "" {
		return toolBridgePlan{Content: strings.TrimSpace(originalText), Err: fmt.Errorf("tool planner output missing status")}
	}
	if status != "tool_calls" && status != "message" && status != "no_tool" {
		return toolBridgePlan{Content: strings.TrimSpace(originalText), Err: fmt.Errorf("unsupported tool planner status %q", payload.Status)}
	}
	if status == "no_tool" {
		if toolBridgeRequiresToolCall(req) {
			return toolBridgePlan{Err: fmt.Errorf("tool_choice requires a tool call")}
		}
		return toolBridgePlan{NoTool: true}
	}
	if status == "message" {
		content := strings.TrimSpace(payload.Content)
		if toolBridgeRequiresToolCall(req) {
			return toolBridgePlan{Content: content, Err: fmt.Errorf("tool_choice requires a tool call")}
		}
		return toolBridgePlan{Content: content, NoTool: true}
	}

	if len(payload.ToolCalls) == 0 {
		return toolBridgePlan{Err: fmt.Errorf("status tool_calls requires at least one tool call")}
	}
	allowed := make(map[string]struct{}, len(req.Tools))
	schemas := make(map[string]json.RawMessage, len(req.Tools))
	for _, t := range req.Tools {
		if strings.EqualFold(t.Type, "function") {
			name := strings.TrimSpace(t.Function.Name)
			if name != "" {
				allowed[name] = struct{}{}
				schemas[name] = t.Function.Parameters
			}
		}
	}

	forcedName := req.ForcedToolName()
	calls := make([]dto.ChatCompletionToolCall, 0, len(payload.ToolCalls))
	for i, tc := range payload.ToolCalls {
		name := strings.TrimSpace(tc.Name)
		if name == "" {
			return toolBridgePlan{Err: fmt.Errorf("tool call %d missing name", i)}
		}
		if len(allowed) > 0 {
			if _, ok := allowed[name]; !ok {
				return toolBridgePlan{Err: fmt.Errorf("tool call %d uses unknown tool %q", i, name)}
			}
		}
		if forcedName != "" && name != forcedName {
			return toolBridgePlan{Err: fmt.Errorf("tool_choice requires %q, got %q", forcedName, name)}
		}
		normalizedArgs, err := validateAndNormalizeToolArguments(tc.Arguments, schemas[name])
		if err != nil {
			return toolBridgePlan{Err: fmt.Errorf("tool call %d arguments invalid: %w", i, err)}
		}

		calls = append(calls, dto.ChatCompletionToolCall{
			ID:   fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i),
			Type: "function",
			Function: dto.ChatCompletionToolCallFunction{
				Name:      name,
				Arguments: normalizedArgs,
			},
		})
	}

	return toolBridgePlan{ToolCalls: calls}
}

func (s *OpenAIService) buildFallbackToolCalls(req dto.ChatCompletionRequest) []dto.ChatCompletionToolCall {
	forced := req.ForcedToolName()
	if forced != "" {
		return []dto.ChatCompletionToolCall{
			{
				ID:   fmt.Sprintf("call_%d_0", time.Now().UnixNano()),
				Type: "function",
				Function: dto.ChatCompletionToolCallFunction{
					Name:      forced,
					Arguments: "{}",
				},
			},
		}
	}

	if req.ToolChoiceMode() == "required" {
		for _, t := range req.Tools {
			if strings.EqualFold(t.Type, "function") && strings.TrimSpace(t.Function.Name) != "" {
				return []dto.ChatCompletionToolCall{
					{
						ID:   fmt.Sprintf("call_%d_0", time.Now().UnixNano()),
						Type: "function",
						Function: dto.ChatCompletionToolCallFunction{
							Name:      strings.TrimSpace(t.Function.Name),
							Arguments: "{}",
						},
					},
				}
			}
		}
	}

	return nil
}

func chunkToolCalls(calls []dto.ChatCompletionToolCall) []dto.ChatCompletionChunkDeltaToolCall {
	out := make([]dto.ChatCompletionChunkDeltaToolCall, 0, len(calls))
	for i, call := range calls {
		out = append(out, dto.ChatCompletionChunkDeltaToolCall{
			Index: i,
			ID:    call.ID,
			Type:  call.Type,
			Function: dto.ChatCompletionChunkDeltaToolFunction{
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			},
		})
	}
	return out
}

func decodeToolBridgePayload(text string) (toolBridgePayload, bool) {
	var payload toolBridgePayload
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload, true
	}

	obj := extractFirstJSONObject(text)
	if obj == "" {
		return toolBridgePayload{}, false
	}
	if err := json.Unmarshal([]byte(obj), &payload); err != nil {
		return toolBridgePayload{}, false
	}
	return payload, true
}

func extractFirstJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return strings.TrimSpace(text[start : i+1])
			}
		}
	}
	return ""
}

func validateAndNormalizeToolArguments(raw json.RawMessage, schemaRaw json.RawMessage) (string, error) {
	normalized := normalizeArguments(raw)
	var args interface{}
	if err := json.Unmarshal([]byte(normalized), &args); err != nil {
		return "", fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	obj, ok := args.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("arguments must be a JSON object")
	}
	if len(schemaRaw) > 0 {
		var schema map[string]interface{}
		if err := json.Unmarshal(schemaRaw, &schema); err == nil && len(schema) > 0 {
			if err := validateJSONSchemaValue(obj, schema, "arguments", 0); err != nil {
				return "", err
			}
		}
	}
	return normalized, nil
}

func validateJSONSchemaValue(value interface{}, schema map[string]interface{}, path string, depth int) error {
	if depth > 4 || len(schema) == 0 {
		return nil
	}
	if err := validateJSONSchemaType(value, schema, path); err != nil {
		return err
	}

	obj, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}

	required := schemaStringSlice(schema["required"])
	for _, key := range required {
		if _, ok := obj[key]; !ok {
			return fmt.Errorf("%s.%s is required", path, key)
		}
	}

	properties, _ := schema["properties"].(map[string]interface{})
	propertySchemas := make(map[string]map[string]interface{}, len(properties))
	for key, rawProp := range properties {
		if propSchema, ok := rawProp.(map[string]interface{}); ok {
			propertySchemas[key] = propSchema
		}
	}

	if additional, ok := schema["additionalProperties"].(bool); ok && !additional && len(propertySchemas) > 0 {
		for key := range obj {
			if _, ok := propertySchemas[key]; !ok {
				return fmt.Errorf("%s.%s is not allowed by schema", path, key)
			}
		}
	}

	for key, propSchema := range propertySchemas {
		propValue, ok := obj[key]
		if !ok {
			continue
		}
		if err := validateJSONSchemaValue(propValue, propSchema, path+"."+key, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONSchemaType(value interface{}, schema map[string]interface{}, path string) error {
	types := schemaTypes(schema["type"])
	if len(types) == 0 {
		return nil
	}
	for _, typ := range types {
		switch typ {
		case "object":
			if _, ok := value.(map[string]interface{}); ok {
				return nil
			}
		case "array":
			if _, ok := value.([]interface{}); ok {
				return nil
			}
		case "string":
			if _, ok := value.(string); ok {
				return nil
			}
		case "number":
			if _, ok := value.(float64); ok {
				return nil
			}
		case "integer":
			if n, ok := value.(float64); ok && n == float64(int64(n)) {
				return nil
			}
		case "boolean":
			if _, ok := value.(bool); ok {
				return nil
			}
		case "null":
			if value == nil {
				return nil
			}
		default:
			return nil
		}
	}
	return fmt.Errorf("%s has wrong type, expected %s", path, strings.Join(types, " or "))
}

func schemaTypes(raw interface{}) []string {
	switch v := raw.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		return []string{v}
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

func schemaStringSlice(raw interface{}) []string {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

func normalizeArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}

	if strings.HasPrefix(trimmed, "\"") {
		var asString string
		if err := json.Unmarshal(raw, &asString); err == nil {
			trimmed = strings.TrimSpace(asString)
			if trimmed == "" {
				return "{}"
			}
		}
	}

	var parsed interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return "{}"
	}
	parsed = sanitizeToolArgumentValue(parsed)

	sanitized, err := json.Marshal(parsed)
	if err != nil {
		return "{}"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, sanitized); err != nil {
		return "{}"
	}
	return compact.String()
}

func sanitizeToolArgumentValue(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, item := range v {
			v[key] = sanitizeToolArgumentValue(item)
		}
		return v
	case []interface{}:
		for i, item := range v {
			v[i] = sanitizeToolArgumentValue(item)
		}
		return v
	case string:
		return sanitizeToolArgumentString(v)
	default:
		return value
	}
}

func sanitizeToolArgumentString(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value
	}
	if isHTTPURL(trimmed) {
		return trimmed
	}

	for i := 0; i < 4; i++ {
		extracted, ok := extractSingleMarkdownLinkURL(trimmed)
		if !ok || extracted == trimmed {
			break
		}
		trimmed = strings.TrimSpace(extracted)
		if isHTTPURL(trimmed) {
			return trimmed
		}
	}
	return value
}

func extractSingleMarkdownLinkURL(value string) (string, bool) {
	if !strings.HasPrefix(value, "[") {
		return "", false
	}
	textEnd := matchingBracketIndex(value, 0, '[', ']')
	if textEnd < 0 || textEnd+1 >= len(value) || value[textEnd+1] != '(' {
		return "", false
	}
	urlEnd := matchingBracketIndex(value, textEnd+1, '(', ')')
	if urlEnd < 0 || strings.TrimSpace(value[urlEnd+1:]) != "" {
		return "", false
	}
	return value[textEnd+2 : urlEnd], true
}

func matchingBracketIndex(value string, start int, open, close byte) int {
	if start < 0 || start >= len(value) || value[start] != open {
		return -1
	}
	depth := 0
	for i := start; i < len(value); i++ {
		switch value[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed == nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}
