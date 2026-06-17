package openai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gemini-free-api/internal/modules/openai/dto"
	"gemini-free-api/internal/modules/providers"
)

type fakeGeminiClient struct {
	streamEvents []providers.StreamEvent
	streamErrs   []error
	prompts      []string
	configs      []providers.GenerateConfig
}

func (f *fakeGeminiClient) Init(context.Context) error { return nil }

func (f *fakeGeminiClient) GenerateContent(context.Context, string, ...providers.GenerateOption) (*providers.Response, error) {
	return &providers.Response{}, nil
}

func (f *fakeGeminiClient) StartChat(...providers.ChatOption) providers.ChatSession { return nil }

func (f *fakeGeminiClient) Close() error { return nil }

func (f *fakeGeminiClient) GetName() string { return "fake" }

func (f *fakeGeminiClient) IsHealthy() bool { return true }

func (f *fakeGeminiClient) ListModels() []providers.ModelInfo { return nil }

func (f *fakeGeminiClient) GenerateContentStreamForOpenAI(ctx context.Context, prompt string, onEvent func(event providers.StreamEvent) bool, options ...providers.GenerateOption) error {
	f.prompts = append(f.prompts, prompt)
	var cfg providers.GenerateConfig
	for _, opt := range options {
		opt(&cfg)
	}
	f.configs = append(f.configs, cfg)
	for _, event := range f.streamEvents {
		if !onEvent(event) {
			return nil
		}
	}
	if len(f.streamErrs) > 0 {
		err := f.streamErrs[0]
		f.streamErrs = f.streamErrs[1:]
		return err
	}
	return nil
}

func (f *fakeGeminiClient) HasConversationState(string) bool { return true }

func TestModelAndThinkingLevelUsesExplicitLevel(t *testing.T) {
	model, level := modelAndThinkingLevel("gemini-3.5-flash:thinking=extended", "standard")

	if model != "gemini-3.5-flash" {
		t.Fatalf("expected clean model, got %q", model)
	}
	if level != "standard" {
		t.Fatalf("expected explicit thinking level, got %q", level)
	}
}

func TestModelAndThinkingLevelReadsModelSuffix(t *testing.T) {
	model, level := modelAndThinkingLevel("gemini-3.5-flash:thinking=extended", "")

	if model != "gemini-3.5-flash" {
		t.Fatalf("expected clean model, got %q", model)
	}
	if level != "extended" {
		t.Fatalf("expected extended thinking level, got %q", level)
	}
}

func TestModelAndThinkingLevelIgnoresUnknownSuffix(t *testing.T) {
	model, level := modelAndThinkingLevel("gemini-3.5-flash:foo=bar", "")

	if model != "gemini-3.5-flash:foo=bar" {
		t.Fatalf("expected original model, got %q", model)
	}
	if level != "" {
		t.Fatalf("expected empty thinking level, got %q", level)
	}
}

func TestRequestThinkingLevelReadsReasoningEffort(t *testing.T) {
	level := requestThinkingLevel(dto.ChatCompletionRequest{ReasoningEffort: "high"})

	if level != "extended" {
		t.Fatalf("expected high reasoning effort to map to extended, got %q", level)
	}
}

func TestRequestThinkingLevelReadsNestedReasoningEffort(t *testing.T) {
	level := requestThinkingLevel(dto.ChatCompletionRequest{Reasoning: &dto.ReasoningConfig{Effort: "low"}})

	if level != "standard" {
		t.Fatalf("expected low reasoning effort to map to standard, got %q", level)
	}
}

func TestChatCompletionMessageKeepsToolCallFields(t *testing.T) {
	var msg dto.ChatCompletionMessage
	err := json.Unmarshal([]byte(`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"query\":\"GitHub trending\"}"}}]}`), &msg)
	if err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "search" {
		t.Fatalf("expected tool call fields to be preserved, got %#v", msg.ToolCalls)
	}
	modelMsg := msg.ToModelMessage()
	if !strings.Contains(modelMsg.Content, "Assistant requested tool calls") || !strings.Contains(modelMsg.Content, "GitHub trending") {
		t.Fatalf("expected model message to describe tool call, got %q", modelMsg.Content)
	}

	var toolMsg dto.ChatCompletionMessage
	err = json.Unmarshal([]byte(`{"role":"tool","tool_call_id":"call_1","content":"search result text"}`), &toolMsg)
	if err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	modelToolMsg := toolMsg.ToModelMessage()
	if !strings.Contains(modelToolMsg.Content, "Tool result (call_1): search result text") {
		t.Fatalf("expected model message to describe tool result, got %q", modelToolMsg.Content)
	}
}

func TestPlanRequestContextInfersOpenAIClientHistory(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	firstReq := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：松月。只回复：已记住"},
		},
	}

	firstPlan := service.planRequestContext(firstReq)
	if firstPlan.ProviderConversationID == "" {
		t.Fatal("expected first request to allocate an internal provider conversation id")
	}
	if !strings.Contains(firstPlan.Prompt, "松月") {
		t.Fatalf("expected first prompt to include user message, got %q", firstPlan.Prompt)
	}

	firstPlan.ResponseText = "已记住"
	service.rememberRequestContext(firstPlan)

	secondReq := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：松月。只回复：已记住"},
			{Role: "assistant", Content: "已记住"},
			{Role: "user", Content: "刚才让你记住的词是什么？只回复这个词"},
		},
	}
	secondPlan := service.planRequestContext(secondReq)
	if secondPlan.ProviderConversationID != firstPlan.ProviderConversationID {
		t.Fatalf("expected second request to reuse provider conversation id %q, got %q", firstPlan.ProviderConversationID, secondPlan.ProviderConversationID)
	}
	if !secondPlan.AutoContext {
		t.Fatal("expected second request to use inferred server-side context")
	}
	if strings.Contains(secondPlan.Prompt, "已记住") || strings.Contains(secondPlan.Prompt, "记住一个词") {
		t.Fatalf("expected second prompt to contain only latest user turn, got %q", secondPlan.Prompt)
	}
	if strings.Contains(secondPlan.Prompt, "User:") {
		t.Fatalf("expected raw user prompt without role prefix, got %q", secondPlan.Prompt)
	}
	if !strings.Contains(secondPlan.Prompt, "刚才让你记住的词是什么") {
		t.Fatalf("expected second prompt to include latest user turn, got %q", secondPlan.Prompt)
	}
}

func TestPlanRequestContextFormatsFirstTurnSystemAsPersona(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	req := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "system", Content: "你是善于使用工具的ai助手"},
			{Role: "user", Content: "你好"},
		},
	}

	plan := service.planRequestContext(req)
	want := "**Persona**: `你是善于使用工具的ai助手`\n\n你好"
	if plan.Prompt != want {
		t.Fatalf("expected first-turn persona prompt %q, got %q", want, plan.Prompt)
	}
	if strings.Contains(plan.Prompt, "System:") || strings.Contains(plan.Prompt, "User:") {
		t.Fatalf("first-turn prompt should not contain role prefixes, got %q", plan.Prompt)
	}
}

func TestPlanRequestContextAllocatesFreshIDForRepeatedNewTopics(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	req := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你会使用mermaid语法画图表吗"},
		},
	}

	first := service.planRequestContext(req)
	second := service.planRequestContext(req)
	if first.ProviderConversationID == "" || second.ProviderConversationID == "" {
		t.Fatalf("expected provider conversation ids, got first=%q second=%q", first.ProviderConversationID, second.ProviderConversationID)
	}
	if first.ProviderConversationID == second.ProviderConversationID {
		t.Fatalf("new topics with identical first messages must not reuse provider conversation id %q", first.ProviderConversationID)
	}
}

func TestPlanRequestContextPrefersExplicitConversationID(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	req := dto.ChatCompletionRequest{
		ConversationID: "client-thread",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "刚才让你记住的词是什么？只回复这个词"},
		},
	}

	plan := service.planRequestContext(req)
	if plan.ProviderConversationID != "client-thread" {
		t.Fatalf("expected explicit conversation id, got %q", plan.ProviderConversationID)
	}
	if !strings.Contains(plan.Prompt, "刚才让你记住的词是什么") {
		t.Fatalf("expected prompt to contain current user turn, got %q", plan.Prompt)
	}
	if strings.Contains(plan.Prompt, "User:") {
		t.Fatalf("expected explicit conversation prompt without role prefix, got %q", plan.Prompt)
	}
}

func TestPlanRequestContextUsesExplicitConversationIDWhenHistoryMatches(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	first := openAIContextPlan{
		ProviderConversationID: "client-thread",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：松月"},
		},
		ResponseText: "已记住",
	}
	service.rememberRequestContext(first)

	req := dto.ChatCompletionRequest{
		ConversationID: "client-thread",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：松月"},
			{Role: "assistant", Content: "已记住"},
			{Role: "user", Content: "刚才的词是什么？"},
		},
	}
	plan := service.planRequestContext(req)
	if plan.ProviderConversationID != "client-thread" {
		t.Fatalf("expected matching explicit history to reuse client-thread, got %q", plan.ProviderConversationID)
	}
	if !plan.AutoContext {
		t.Fatal("expected matching explicit history to use server-side context")
	}
	if strings.Contains(plan.Prompt, "松月") || strings.Contains(plan.Prompt, "已记住") {
		t.Fatalf("expected only latest user turn, got %q", plan.Prompt)
	}
}

func TestPlanRequestContextRebuildsWhenExplicitHistoryWasEdited(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	first := openAIContextPlan{
		ProviderConversationID: "client-thread",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：松月"},
		},
		ResponseText: "已记住",
	}
	service.rememberRequestContext(first)

	req := dto.ChatCompletionRequest{
		ConversationID: "client-thread",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：海棠"},
			{Role: "assistant", Content: "已记住"},
			{Role: "user", Content: "刚才的词是什么？"},
		},
	}
	plan := service.planRequestContext(req)
	if plan.ProviderConversationID == "" || plan.ProviderConversationID == "client-thread" {
		t.Fatalf("expected edited explicit history to allocate repaired provider id, got %q", plan.ProviderConversationID)
	}
	if plan.AutoContext {
		t.Fatal("edited explicit history must not use stale server-side context")
	}
	if !strings.Contains(plan.Prompt, "海棠") || !strings.Contains(plan.Prompt, "刚才的词是什么") {
		t.Fatalf("expected repaired prompt to include full edited history, got %q", plan.Prompt)
	}
}

func TestPlanRequestContextRebuildsWhenClientRetriesFromEarlierPrefix(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	firstReq := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
	}
	firstPlan := service.planRequestContext(firstReq)
	firstPlan.ResponseText = "我很好，你好吗？"
	service.rememberRequestContext(firstPlan)

	secondReq := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
			{Role: "assistant", Content: "我很好，你好吗？"},
			{Role: "user", Content: "你能做什么？"},
		},
	}
	secondPlan := service.planRequestContext(secondReq)
	if secondPlan.ProviderConversationID != firstPlan.ProviderConversationID || !secondPlan.AutoContext {
		t.Fatalf("expected natural second turn to reuse provider context, got id=%q auto=%v", secondPlan.ProviderConversationID, secondPlan.AutoContext)
	}
	secondPlan.ResponseText = "做你想做的事"
	service.rememberRequestContext(secondPlan)

	retryPlan := service.planRequestContext(secondReq)
	if retryPlan.ProviderConversationID == "" || retryPlan.ProviderConversationID == firstPlan.ProviderConversationID {
		t.Fatalf("expected retry from earlier prefix to allocate a new provider context, got %q", retryPlan.ProviderConversationID)
	}
	if retryPlan.AutoContext {
		t.Fatal("retry from earlier prefix must not append to the already advanced Gemini Web topic")
	}
	if !strings.Contains(retryPlan.Prompt, "你好") || !strings.Contains(retryPlan.Prompt, "你能做什么") {
		t.Fatalf("expected retry prompt to include full visible history, got %q", retryPlan.Prompt)
	}
}

func TestPlanRequestContextReusesProviderWhenClientDropsOldestTurns(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	providerID := "provider-long-thread"
	transcript := []dto.ChatCompletionMessage{
		{Role: "user", Content: "第一轮：记住词语松月"},
		{Role: "assistant", Content: "已记住松月"},
		{Role: "user", Content: "第二轮：解释连接池"},
		{Role: "assistant", Content: "连接池用于复用连接"},
		{Role: "user", Content: "第三轮：解释 keep-alive"},
		{Role: "assistant", Content: "keep-alive 可以降低握手开销"},
		{Role: "user", Content: "第四轮：解释超时"},
		{Role: "assistant", Content: "超时会关闭闲置连接"},
	}
	service.rememberRequestContext(openAIContextPlan{
		ProviderConversationID: providerID,
		RequestMessages:        transcript[:len(transcript)-1],
		ResponseText:           transcript[len(transcript)-1].Content,
	})

	req := dto.ChatCompletionRequest{
		Messages: append(cloneChatMessages(transcript[2:]), dto.ChatCompletionMessage{
			Role:    "user",
			Content: "第五轮：刚才第一轮让你记住的词是什么？",
		}),
	}
	plan := service.planRequestContext(req)

	if plan.ProviderConversationID != providerID {
		t.Fatalf("expected truncated client history to reuse %q, got %q", providerID, plan.ProviderConversationID)
	}
	if !plan.AutoContext {
		t.Fatal("expected truncated client history to use server-side context")
	}
	if plan.Prompt != "第五轮：刚才第一轮让你记住的词是什么？" {
		t.Fatalf("expected only latest user prompt, got %q", plan.Prompt)
	}
}

func TestPlanRequestContextFallsBackToRootWhenAssistantTextDiffers(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	firstPlan := openAIContextPlan{
		ProviderConversationID: "provider-thread",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "介绍一下 Mermaid"},
		},
		ResponseText: "Mermaid 是一种用文本描述图表的语法。",
	}
	service.rememberRequestContext(firstPlan)

	req := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "system", Content: "你是助手"},
			{Role: "user", Content: "介绍一下 Mermaid"},
			{Role: "assistant", Content: "Mermaid是一种用文本描述图表的语法。"},
			{Role: "user", Content: "给我一个流程图例子"},
		},
	}
	plan := service.planRequestContext(req)
	if plan.ProviderConversationID != "provider-thread" || !plan.AutoContext {
		t.Fatalf("expected root fallback to reuse provider-thread, got id=%q auto=%v", plan.ProviderConversationID, plan.AutoContext)
	}
	if plan.Prompt != "给我一个流程图例子" {
		t.Fatalf("expected only latest user prompt, got %q", plan.Prompt)
	}
}

func TestPruneTranscriptContextsRemovesExpiredEntries(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	service.transcriptContexts["old"] = "provider-old"
	service.transcriptContextUpdated["old"] = time.Now().Add(-transcriptContextTTL - time.Minute)
	service.transcriptContexts["fresh"] = "provider-fresh"
	service.transcriptContextUpdated["fresh"] = time.Now()

	service.contextMu.Lock()
	service.pruneTranscriptContextsLocked(time.Now())
	service.contextMu.Unlock()

	if _, ok := service.transcriptContexts["old"]; ok {
		t.Fatal("expected expired transcript context to be pruned")
	}
	if service.transcriptContexts["fresh"] != "provider-fresh" {
		t.Fatal("expected fresh transcript context to remain")
	}
}

func TestFallbackOptionsRebasesProviderConversation(t *testing.T) {
	plan := openAIContextPlan{
		ProviderConversationID: "stale-provider",
		AutoContext:            true,
	}
	opts := fallbackOptionsWithFreshConversation(&plan, []providers.GenerateOption{providers.WithModel("gemini-3.5-flash")})

	if plan.ProviderConversationID == "" || plan.ProviderConversationID == "stale-provider" {
		t.Fatalf("expected fallback to allocate a fresh provider id, got %q", plan.ProviderConversationID)
	}
	if plan.AutoContext {
		t.Fatal("fallback rebuilt from full OpenAI history should no longer be marked as stale auto-context append")
	}

	var cfg providers.GenerateConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.Model != "gemini-3.5-flash" {
		t.Fatalf("expected fallback opts to preserve base model, got %q", cfg.Model)
	}
	if cfg.ConversationID != plan.ProviderConversationID {
		t.Fatalf("expected fallback opts to use fresh provider id %q, got %q", plan.ProviderConversationID, cfg.ConversationID)
	}
}

func TestRememberRequestContextSkipsMissingProviderConversationState(t *testing.T) {
	service := NewOpenAIService(&providers.Client{}, nil)
	plan := openAIContextPlan{
		ProviderConversationID: "provider-without-gemini-state",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
		ResponseText: "你好，有什么可以帮你？",
	}

	service.rememberRequestContext(plan)

	if len(service.transcriptContexts) != 0 {
		t.Fatalf("expected missing provider state not to be remembered, got %#v", service.transcriptContexts)
	}
}

func TestStreamSkipsSameContextRetryForGemini1097(t *testing.T) {
	client := &fakeGeminiClient{
		streamErrs: []error{errors.New("gemini bard error 1097")},
	}
	service := NewOpenAIService(client, nil)

	prefix := []dto.ChatCompletionMessage{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好，有什么可以帮你？"},
	}
	key := transcriptFingerprint(prefix)
	service.transcriptContexts[key] = "provider-1"
	service.transcriptContextUpdated[key] = time.Now()
	service.providerLatestTranscript["provider-1"] = key
	service.providerLatestLength["provider-1"] = len(prefix)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: append(cloneChatMessages(prefix), dto.ChatCompletionMessage{
			Role:    "user",
			Content: "继续",
		}),
		Stream: true,
	}

	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(client.configs) != 2 {
		t.Fatalf("expected context attempt plus stateless fallback only, got %d calls: %#v", len(client.configs), client.configs)
	}
	if client.configs[0].ConversationID != "provider-1" {
		t.Fatalf("expected first call to use provider-1, got %q", client.configs[0].ConversationID)
	}
	if client.configs[1].ConversationID == "" || client.configs[1].ConversationID == "provider-1" {
		t.Fatalf("expected fallback to use fresh provider conversation, got %q", client.configs[1].ConversationID)
	}
	if len(service.transcriptContexts) != 0 {
		t.Fatalf("expected stale provider context to be forgotten, got %#v", service.transcriptContexts)
	}
}

func TestCreateChatCompletionStreamEmitsToolCallsForToolBridgeJSON(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: `{"tool_calls":[{"name":"mcp__exa__web_search_exa","arguments":{"query":"trending repositories on GitHub today"}}]}`},
		},
	}, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "帮我搜索一下 GitHub 的热点"},
		},
		Tools: []dto.ToolDefinition{
			{
				Type: "function",
				Function: dto.ToolFunctionDefinition{
					Name:        "mcp__exa__web_search_exa",
					Description: "Search the web",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
				},
			},
		},
		Stream: true,
	}

	var chunks []dto.ChatCompletionChunk
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		chunks = append(chunks, chunk)
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}

	var gotToolCall *dto.ChatCompletionChunkDeltaToolCall
	for _, chunk := range chunks {
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if strings.Contains(choice.Delta.Content, `"tool_calls"`) {
			t.Fatalf("tool bridge JSON leaked as content: %q", choice.Delta.Content)
		}
		if len(choice.Delta.ToolCalls) > 0 {
			gotToolCall = &choice.Delta.ToolCalls[0]
		}
	}
	if gotToolCall == nil {
		t.Fatalf("expected streamed tool call chunk, got %#v", chunks)
	}
	if gotToolCall.Type != "function" || gotToolCall.Function.Name != "mcp__exa__web_search_exa" {
		t.Fatalf("unexpected tool call: %#v", gotToolCall)
	}
	if gotToolCall.Function.Arguments != `{"query":"trending repositories on GitHub today"}` {
		t.Fatalf("unexpected arguments: %q", gotToolCall.Function.Arguments)
	}

	lastChoice := chunks[len(chunks)-1].Choices[0]
	if lastChoice.FinishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", lastChoice.FinishReason)
	}
}

func TestCreateChatCompletionStreamTreatsToolChoiceNoneAsNormalStreaming(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: "普通"},
			{Kind: "content_delta", Delta: "回答"},
		},
	}, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
		Tools:         []dto.ToolDefinition{{Type: "function", Function: dto.ToolFunctionDefinition{Name: "noop"}}},
		ToolChoiceRaw: json.RawMessage(`"none"`),
		Stream:        true,
	}

	var content strings.Builder
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if content.String() != "普通回答" {
		t.Fatalf("expected normal streaming content, got %q", content.String())
	}
}

func TestCreateChatCompletionStreamWithoutToolsBypassesToolBridge(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: "真"},
			{Kind: "content_delta", Delta: "流式"},
		},
	}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
		Stream: true,
	}

	var deltas []string
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			deltas = append(deltas, chunk.Choices[0].Delta.Content)
		}
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if strings.Join(deltas, "") != "真流式" || len(deltas) != 2 {
		t.Fatalf("expected direct streaming without buffering, got %#v", deltas)
	}
	if len(client.prompts) != 1 || strings.Contains(client.prompts[0], "OpenAI-compatible assistant running behind a bridge") {
		t.Fatalf("request without tools must bypass tool bridge, got prompts %#v", client.prompts)
	}
}

func TestCreateChatCompletionStreamStreamsNormalAnswerWhenToolsAreAuto(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: "这是"},
			{Kind: "content_delta", Delta: "普通回答"},
		},
	}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "解释一下递归是什么"},
		},
		Tools:  []dto.ToolDefinition{{Type: "function", Function: dto.ToolFunctionDefinition{Name: "mcp__exa__web_search_exa"}}},
		Stream: true,
	}

	var deltas []string
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			deltas = append(deltas, chunk.Choices[0].Delta.Content)
		}
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(deltas) != 2 {
		t.Fatalf("expected content to stream in two chunks, got %#v", deltas)
	}
	if strings.Join(deltas, "") != "这是普通回答" {
		t.Fatalf("unexpected streamed content: %#v", deltas)
	}
	if len(client.prompts) != 1 || !strings.Contains(client.prompts[0], "OpenAI-compatible assistant running behind a bridge") {
		t.Fatalf("expected auto tools request to use temporary tool planner, got prompts %#v", client.prompts)
	}
}

func TestCreateChatCompletionStreamDoesNotUseToolBridgeAfterToolResult(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: "根据结果，"},
			{Kind: "content_delta", Delta: "热门项目包括 Webwright。"},
		},
	}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "帮我搜索一下今天GitHub的热门项目。"},
			{Role: "assistant", ToolCalls: []dto.ChatCompletionToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: dto.ChatCompletionToolCallFunction{
					Name:      "mcp__exa__web_search_exa",
					Arguments: `{"query":"GitHub trending today"}`,
				},
			}}},
			{Role: "tool", ToolCallID: "call_1", Name: "mcp__exa__web_search_exa", Content: "Title: Microsoft/Webwright"},
		},
		Tools:  []dto.ToolDefinition{{Type: "function", Function: dto.ToolFunctionDefinition{Name: "mcp__exa__web_search_exa"}}},
		Stream: true,
	}

	var deltas []string
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			deltas = append(deltas, chunk.Choices[0].Delta.Content)
		}
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(deltas) != 2 {
		t.Fatalf("expected tool-result answer to stream in two chunks, got %#v", deltas)
	}
	if strings.Join(deltas, "") != "根据结果，热门项目包括 Webwright。" {
		t.Fatalf("unexpected streamed content: %#v", deltas)
	}
	if len(client.prompts) != 1 || strings.Contains(client.prompts[0], "OpenAI-compatible assistant running behind a bridge") {
		t.Fatalf("expected tool-result answer to skip tool bridge, got prompts %#v", client.prompts)
	}
	if len(client.configs) != 1 || client.configs[0].ConversationID == "" {
		t.Fatalf("expected tool-result answer to use a provider conversation, got configs %#v", client.configs)
	}
}

func TestCreateChatCompletionStreamUsesTemporaryToolBridgeForGreetingWithTools(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: "你好"},
		},
	}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
		Tools:  []dto.ToolDefinition{{Type: "function", Function: dto.ToolFunctionDefinition{Name: "mcp__exa__web_search_exa"}}},
		Stream: true,
	}

	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool { return true })
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(client.prompts) != 1 || !strings.Contains(client.prompts[0], "OpenAI-compatible assistant running behind a bridge") {
		t.Fatalf("expected greeting to use tool bridge planner, got prompts %#v", client.prompts)
	}
	if len(client.configs) != 1 || client.configs[0].ConversationID != "" {
		t.Fatalf("tool bridge planner must be temporary, got configs %#v", client.configs)
	}
}

func TestCreateChatCompletionStreamDoesNotAppendToolBridgeToExistingConversation(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: `{"tool_calls":[{"name":"mcp__exa__web_search_exa","arguments":{"query":"GitHub trending today"}}]}`},
		},
	}
	service := NewOpenAIService(client, nil)
	firstPlan := openAIContextPlan{
		ProviderConversationID: "provider-thread",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
		ResponseText: "你好！",
	}
	service.rememberRequestContext(firstPlan)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
			{Role: "assistant", Content: "你好！"},
			{Role: "user", Content: "今天 GitHub 的热点是什么？"},
		},
		Tools:  []dto.ToolDefinition{{Type: "function", Function: dto.ToolFunctionDefinition{Name: "mcp__exa__web_search_exa"}}},
		Stream: true,
	}

	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool { return true })
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(client.configs) != 1 {
		t.Fatalf("expected one provider call, got %#v", client.configs)
	}
	if client.configs[0].ConversationID != "" {
		t.Fatalf("tool bridge planning must not append to existing conversation, got %q", client.configs[0].ConversationID)
	}
}
