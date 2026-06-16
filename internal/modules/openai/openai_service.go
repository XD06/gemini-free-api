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
	client                   *providers.Client
	log                      *zap.Logger
	contextMu                sync.Mutex
	transcriptContexts       map[string]string
	transcriptContextUpdated map[string]time.Time
	providerLatestTranscript map[string]string
	providerLatestLength     map[string]int
	rootContexts             map[string]string
	rootContextUpdated       map[string]time.Time
}

func NewOpenAIService(client *providers.Client, log *zap.Logger) *OpenAIService {
	return &OpenAIService{
		client:                   client,
		log:                      log,
		transcriptContexts:       make(map[string]string),
		transcriptContextUpdated: make(map[string]time.Time),
		providerLatestTranscript: make(map[string]string),
		providerLatestLength:     make(map[string]int),
		rootContexts:             make(map[string]string),
		rootContextUpdated:       make(map[string]time.Time),
	}
}

const (
	maxTranscriptContextEntries = 1000
	transcriptContextTTL        = 12 * time.Hour
)

func generateChatID() string {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	return fmt.Sprintf("chatcmpl-%d%06d", time.Now().Unix(), r.Intn(1000000))
}

func (s *OpenAIService) ListModels() []providers.ModelInfo {
	return s.client.ListModels()
}

func (s *OpenAIService) CreateChatCompletion(ctx context.Context, req dto.ChatCompletionRequest) (*dto.ChatCompletionResponse, error) {
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

	if req.HasToolsEnabled() {
		prompt = s.buildToolBridgePrompt(req, prompt)
	}

	providerModel, thinkingLevel := modelAndThinkingLevel(req.Model, requestThinkingLevel(req))
	baseOpts := []providers.GenerateOption{}
	if providerModel != "" {
		baseOpts = append(baseOpts, providers.WithModel(providerModel))
	}
	if thinkingLevel != "" {
		baseOpts = append(baseOpts, providers.WithThinkingLevel(thinkingLevel))
	}
	opts := append([]providers.GenerateOption{}, baseOpts...)
	if contextPlan.ProviderConversationID != "" {
		opts = append(opts, providers.WithConversationID(contextPlan.ProviderConversationID))
	}
	inputFiles, err := providers.InputFilesFromAttachments(modelMessages)
	if err != nil {
		return nil, err
	}
	if len(inputFiles) > 0 {
		baseOpts = append(baseOpts, providers.WithInputFiles(inputFiles))
		opts = append(opts, providers.WithInputFiles(inputFiles))
	}

	// Logic: Call Provider
	response, err := s.client.GenerateContent(ctx, prompt, opts...)
	if err != nil && contextPlan.AutoContext {
		if s.log != nil {
			s.log.Warn("server-side context request failed, retrying stateless OpenAI prompt", zap.Error(err))
		}
		fallbackPrompt := buildGeminiWebPromptFromOpenAIMessages(req.Messages)
		if req.HasToolsEnabled() {
			fallbackPrompt = s.buildToolBridgePrompt(req, fallbackPrompt)
		}
		if fallbackPrompt != "" {
			fallbackOpts := fallbackOptionsWithFreshConversation(&contextPlan, baseOpts)
			response, err = s.client.GenerateContent(ctx, fallbackPrompt, fallbackOpts...)
			prompt = fallbackPrompt
		}
	}
	if err != nil {
		return nil, err
	}
	contextPlan.ResponseText = response.Text
	s.rememberRequestContext(contextPlan)

	message := dto.ChatCompletionResponseMessage{Role: "assistant"}
	finishReason := "stop"

	if req.HasToolsEnabled() {
		toolCalls, content := s.parseToolBridgeOutput(req, response.Text)
		if len(toolCalls) == 0 {
			fallback := s.buildFallbackToolCalls(req)
			if len(fallback) > 0 && (req.ToolChoiceMode() == "required" || req.ToolChoiceMode() == "function") {
				toolCalls = fallback
			}
		}

		if len(toolCalls) > 0 {
			message.ToolCalls = toolCalls
			finishReason = "tool_calls"
		} else {
			message.Content = content
		}
	} else {
		message.Content = response.Text
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
	if req.HasToolsEnabled() {
		prompt = s.buildToolBridgePrompt(req, prompt)
	}

	providerModel, thinkingLevel := modelAndThinkingLevel(req.Model, requestThinkingLevel(req))
	baseOpts := []providers.GenerateOption{}
	if providerModel != "" {
		baseOpts = append(baseOpts, providers.WithModel(providerModel))
	}
	if thinkingLevel != "" {
		baseOpts = append(baseOpts, providers.WithThinkingLevel(thinkingLevel))
	}
	opts := append([]providers.GenerateOption{}, baseOpts...)
	if contextPlan.ProviderConversationID != "" {
		opts = append(opts, providers.WithConversationID(contextPlan.ProviderConversationID))
	}
	inputFiles, err := providers.InputFilesFromAttachments(modelMessages)
	if err != nil {
		return err
	}
	if len(inputFiles) > 0 {
		baseOpts = append(baseOpts, providers.WithInputFiles(inputFiles))
		opts = append(opts, providers.WithInputFiles(inputFiles))
	}

	onEvent(dto.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []dto.ChunkChoice{{Index: 0, Delta: dto.ChatCompletionChunkDelta{Role: "assistant"}}},
	})

	var completionLen int
	var completionText strings.Builder
	handleStreamEvent := func(event providers.StreamEvent) bool {
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
	err = s.client.GenerateContentStreamForOpenAI(ctx, prompt, handleStreamEvent, opts...)
	if err != nil && contextPlan.AutoContext && completionLen == 0 {
		if s.log != nil {
			s.log.Warn("server-side context stream failed before content, retrying same Gemini context once", zap.Error(err))
		}
		err = s.client.GenerateContentStreamForOpenAI(ctx, prompt, handleStreamEvent, opts...)
	}
	if err != nil && contextPlan.AutoContext && completionLen == 0 {
		if s.log != nil {
			s.log.Warn("server-side context stream failed before content, retrying stateless OpenAI prompt", zap.Error(err))
		}
		fallbackPrompt := buildGeminiWebPromptFromOpenAIMessages(req.Messages)
		if req.HasToolsEnabled() {
			fallbackPrompt = s.buildToolBridgePrompt(req, fallbackPrompt)
		}
		if fallbackPrompt != "" {
			prompt = fallbackPrompt
			fallbackOpts := fallbackOptionsWithFreshConversation(&contextPlan, baseOpts)
			err = s.client.GenerateContentStreamForOpenAI(ctx, fallbackPrompt, handleStreamEvent, fallbackOpts...)
		}
	}
	if err != nil {
		return err
	}
	contextPlan.ResponseText = completionText.String()
	s.rememberRequestContext(contextPlan)

	onEvent(dto.ChatCompletionChunk{
		ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []dto.ChunkChoice{{Index: 0, FinishReason: "stop"}},
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
	Prompt                 string
	ProviderConversationID string
	AutoContext            bool
	RequestMessages        []dto.ChatCompletionMessage
	ResponseText           string
}

func (s *OpenAIService) planRequestContext(req dto.ChatCompletionRequest) openAIContextPlan {
	plan := openAIContextPlan{
		Prompt:          buildGeminiWebPromptFromOpenAIMessages(req.Messages),
		RequestMessages: cloneChatMessages(req.Messages),
	}

	if explicitID := strings.TrimSpace(req.ConversationID); explicitID != "" {
		plan.ProviderConversationID = explicitID
		if prefix, latest, ok := splitOpenAIHistoryPrefix(req.Messages); ok {
			if providerID := s.providerConversationForTranscript(prefix); providerID != "" {
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
	if providerID := s.providerConversationForRoot(prefix); providerID != "" {
		plan.ProviderConversationID = providerID
		plan.Prompt = rawUserPrompt(latest)
		plan.AutoContext = true
		return plan
	}

	plan.ProviderConversationID = newAutoProviderConversationID()
	return plan
}

func (s *OpenAIService) rememberRequestContext(plan openAIContextPlan) {
	if plan.ProviderConversationID == "" || strings.TrimSpace(plan.ResponseText) == "" {
		return
	}
	if !s.providerConversationReady(plan.ProviderConversationID) {
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
	now := time.Now()
	s.pruneTranscriptContextsLocked(time.Now())
	s.transcriptContexts[key] = plan.ProviderConversationID
	s.transcriptContextUpdated[key] = now
	s.providerLatestTranscript[plan.ProviderConversationID] = key
	s.providerLatestLength[plan.ProviderConversationID] = len(next)
	if rootKey := conversationRootFingerprint(next); rootKey != "" {
		s.rootContexts[rootKey] = plan.ProviderConversationID
		s.rootContextUpdated[rootKey] = now
	}
}

func (s *OpenAIService) providerConversationReady(id string) bool {
	if s.client == nil {
		return true
	}
	return s.client.HasConversationState(id)
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
	if providerID != "" {
		if s.transcriptContextUpdated == nil {
			s.transcriptContextUpdated = make(map[string]time.Time)
		}
		s.transcriptContextUpdated[key] = time.Now()
	}
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
	return fmt.Sprintf("openai-auto-%d-%06d", time.Now().UnixNano(), rand.Intn(1000000))
}

func fallbackOptionsWithFreshConversation(plan *openAIContextPlan, baseOpts []providers.GenerateOption) []providers.GenerateOption {
	fallbackID := newAutoProviderConversationID()
	plan.ProviderConversationID = fallbackID
	plan.AutoContext = false

	opts := append([]providers.GenerateOption{}, baseOpts...)
	return append(opts, providers.WithConversationID(fallbackID))
}

func transcriptFingerprint(messages []dto.ChatCompletionMessage) string {
	type normalizedMessage struct {
		Role        string              `json:"role"`
		Content     string              `json:"content"`
		Attachments []models.Attachment `json:"attachments,omitempty"`
	}
	normalized := make([]normalizedMessage, 0, len(messages))
	for _, msg := range messages {
		normalized = append(normalized, normalizedMessage{
			Role:        strings.ToLower(strings.TrimSpace(msg.Role)),
			Content:     strings.TrimSpace(msg.Content),
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

func cloneChatMessages(messages []dto.ChatCompletionMessage) []dto.ChatCompletionMessage {
	if len(messages) == 0 {
		return nil
	}
	out := make([]dto.ChatCompletionMessage, len(messages))
	copy(out, messages)
	return out
}

type toolBridgePayload struct {
	ToolCalls []toolBridgeCall `json:"tool_calls"`
	Content   string           `json:"content"`
}

type toolBridgeCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *OpenAIService) buildToolBridgePrompt(req dto.ChatCompletionRequest, basePrompt string) string {
	var b strings.Builder
	b.WriteString("You are an OpenAI-compatible assistant running behind a bridge to Gemini web.\n")
	b.WriteString("You MUST respond with JSON only. Do not output markdown code fences.\n")
	b.WriteString("Output schema:\n")
	b.WriteString("{\"tool_calls\":[{\"name\":\"<tool_name>\",\"arguments\":{}}]} OR {\"content\":\"<assistant_text>\"}\n")
	b.WriteString("Rules:\n")
	b.WriteString("- Use only tool names listed below.\n")
	b.WriteString("- arguments must be valid JSON object.\n")

	toolChoiceMode := req.ToolChoiceMode()
	if toolChoiceMode == "required" {
		b.WriteString("- You must return at least one tool call.\n")
	}
	if toolChoiceMode == "function" {
		forced := req.ForcedToolName()
		if forced != "" {
			b.WriteString("- You must return exactly one tool call with name: ")
			b.WriteString(forced)
			b.WriteString("\n")
		}
	}
	if toolChoiceMode == "none" {
		b.WriteString("- Tool calling disabled. Return only {\"content\":\"...\"}.\n")
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

func (s *OpenAIService) parseToolBridgeOutput(req dto.ChatCompletionRequest, text string) ([]dto.ChatCompletionToolCall, string) {
	cleaned := utils.StripCodeFence(text)
	if cleaned == "" {
		return nil, ""
	}

	payload, ok := decodeToolBridgePayload(cleaned)
	if !ok {
		return nil, strings.TrimSpace(text)
	}

	allowed := make(map[string]struct{}, len(req.Tools))
	for _, t := range req.Tools {
		if strings.EqualFold(t.Type, "function") {
			name := strings.TrimSpace(t.Function.Name)
			if name != "" {
				allowed[name] = struct{}{}
			}
		}
	}

	forcedName := req.ForcedToolName()
	calls := make([]dto.ChatCompletionToolCall, 0, len(payload.ToolCalls))
	for i, tc := range payload.ToolCalls {
		name := strings.TrimSpace(tc.Name)
		if name == "" {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[name]; !ok {
				continue
			}
		}
		if forcedName != "" && name != forcedName {
			continue
		}

		calls = append(calls, dto.ChatCompletionToolCall{
			ID:   fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i),
			Type: "function",
			Function: dto.ChatCompletionToolCallFunction{
				Name:      name,
				Arguments: normalizeArguments(tc.Arguments),
			},
		})
	}

	content := strings.TrimSpace(payload.Content)
	if content == "" && len(calls) == 0 {
		content = strings.TrimSpace(text)
	}
	return calls, content
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

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(trimmed)); err != nil {
		return "{}"
	}
	return compact.String()
}
