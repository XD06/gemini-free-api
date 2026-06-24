package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gemini-free-api/internal/commons/models"
	"gemini-free-api/internal/modules/openai/dto"
	"gemini-free-api/internal/modules/providers"
)

type fakeGeminiClient struct {
	streamEvents       []providers.StreamEvent
	streamEventsByCall [][]providers.StreamEvent
	streamErrs         []error
	responses          []string
	generateErrs       []error
	generatePrompts    []string
	prompts            []string
	configs            []providers.GenerateConfig
	untrusted          bool
}

func (f *fakeGeminiClient) Init(context.Context) error { return nil }

func (f *fakeGeminiClient) GenerateContent(ctx context.Context, prompt string, options ...providers.GenerateOption) (*providers.Response, error) {
	_ = ctx
	f.generatePrompts = append(f.generatePrompts, prompt)
	var cfg providers.GenerateConfig
	for _, opt := range options {
		opt(&cfg)
	}
	f.configs = append(f.configs, cfg)
	if len(f.generateErrs) > 0 {
		err := f.generateErrs[0]
		f.generateErrs = f.generateErrs[1:]
		return nil, err
	}
	text := ""
	if len(f.responses) > 0 {
		text = f.responses[0]
		f.responses = f.responses[1:]
	}
	return &providers.Response{Text: text}, nil
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
	events := f.streamEvents
	if len(f.streamEventsByCall) > 0 {
		events = f.streamEventsByCall[0]
		f.streamEventsByCall = f.streamEventsByCall[1:]
	}
	for _, event := range events {
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

func (f *fakeGeminiClient) HasConversationState(string) bool    { return true }
func (f *fakeGeminiClient) IsConversationUntrusted(string) bool { return f.untrusted }

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

func TestNormalizeArgumentsUnwrapsMarkdownURLValues(t *testing.T) {
	raw := json.RawMessage(`{"urls":["[[https://opencode.ai/docs/zh-cn/go](https://opencode.ai/docs/zh-cn/go)](https://opencode.ai/docs/zh-cn/go)"],"nested":{"url":"[OpenCode](https://opencode.ai/docs/zh-cn/go)"}}`)

	got := normalizeArguments(raw)

	expected := `{"nested":{"url":"https://opencode.ai/docs/zh-cn/go"},"urls":["https://opencode.ai/docs/zh-cn/go"]}`
	if got != expected {
		t.Fatalf("unexpected normalized arguments:\n got: %s\nwant: %s", got, expected)
	}
}

func TestNormalizeArgumentsKeepsNonURLMarkdownText(t *testing.T) {
	raw := json.RawMessage(`{"query":"read [OpenCode Go docs](https://opencode.ai/docs/zh-cn/go) and summarize","numResults":3}`)

	got := normalizeArguments(raw)

	expected := `{"numResults":3,"query":"read [OpenCode Go docs](https://opencode.ai/docs/zh-cn/go) and summarize"}`
	if got != expected {
		t.Fatalf("unexpected normalized arguments:\n got: %s\nwant: %s", got, expected)
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

func TestPlanRequestContextReusesProviderWhenAssistantWindowDiffers(t *testing.T) {
	service := NewOpenAIService(nil, nil)
	providerID := "provider-window"
	transcript := []dto.ChatCompletionMessage{
		{Role: "user", Content: "第一轮：记住词语松月"},
		{Role: "assistant", Content: "已记住松月"},
		{Role: "user", Content: "第二轮：解释连接池"},
		{Role: "assistant", Content: "连接池用于复用连接。"},
		{Role: "user", Content: "第三轮：解释 keep-alive"},
		{Role: "assistant", Content: "keep-alive 可以降低握手开销。"},
	}
	service.rememberRequestContext(openAIContextPlan{
		ProviderConversationID: providerID,
		RequestMessages:        transcript[:len(transcript)-1],
		ResponseText:           transcript[len(transcript)-1].Content,
	})

	req := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "第一轮：记住词语松月"},
			{Role: "assistant", Content: "好的，已经记住。"},
			{Role: "user", Content: "第二轮：解释连接池"},
			{Role: "assistant", Content: "连接池会复用连接"},
			{Role: "user", Content: "第三轮：解释 keep-alive"},
			{Role: "assistant", Content: "keep-alive 能减少握手"},
			{Role: "user", Content: "继续说说这些概念的关系"},
		},
	}
	plan := service.planRequestContext(req)
	if plan.ProviderConversationID != providerID || !plan.AutoContext {
		t.Fatalf("expected relaxed user-window match to reuse %q, got id=%q auto=%v", providerID, plan.ProviderConversationID, plan.AutoContext)
	}
	if plan.Prompt != "继续说说这些概念的关系" {
		t.Fatalf("expected only latest user prompt, got %q", plan.Prompt)
	}
}

func TestUserWindowFingerprintRequiresAtLeastTwoUserMessages(t *testing.T) {
	key := userWindowFingerprint([]dto.ChatCompletionMessage{
		{Role: "user", Content: "介绍一下 Mermaid"},
		{Role: "assistant", Content: "Mermaid 是图表语法。"},
	}, 2, 4)
	if key != "" {
		t.Fatalf("single user message must not produce relaxed user-window key, got %q", key)
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
		streamEventsByCall: [][]providers.StreamEvent{
			nil,
			{{Kind: "content_delta", Delta: "fallback answer"}},
		},
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
	for _, providerID := range service.transcriptContexts {
		if providerID == "provider-1" {
			t.Fatalf("expected stale provider context to be forgotten, got %#v", service.transcriptContexts)
		}
	}
}

func TestStreamRebindsExplicitConversationIDAfterFallback(t *testing.T) {
	client := &fakeGeminiClient{
		streamErrs: []error{nil, errors.New("gemini bard error 1097"), nil, nil},
		streamEventsByCall: [][]providers.StreamEvent{
			{{Kind: "content_delta", Delta: "已记住松月"}},
			nil,
			{{Kind: "content_delta", Delta: "松月"}},
			{{Kind: "content_delta", Delta: "还是松月"}},
		},
	}
	service := NewOpenAIService(client, nil)

	firstReq := dto.ChatCompletionRequest{
		Model:          "gemini-3.5-flash",
		ConversationID: "cherry-thread",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住一个词：松月"},
		},
		Stream: true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), firstReq, func(chunk dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("first stream returned error: %v", err)
	}

	secondReq := dto.ChatCompletionRequest{
		Model:          "gemini-3.5-flash",
		ConversationID: "cherry-thread",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "刚才让你记住的词是什么？"},
		},
		Stream: true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), secondReq, func(chunk dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("second stream returned error: %v", err)
	}

	if len(client.configs) != 3 {
		t.Fatalf("expected first call, failed continuation, and fallback; got %#v", client.configs)
	}
	if client.configs[1].ConversationID != "cherry-thread" {
		t.Fatalf("expected failed second call to use original explicit id, got %q", client.configs[1].ConversationID)
	}
	fallbackID := client.configs[2].ConversationID
	if fallbackID == "" || fallbackID == "cherry-thread" {
		t.Fatalf("expected fallback to allocate repaired provider id, got %q", fallbackID)
	}

	thirdReq := dto.ChatCompletionRequest{
		Model:          "gemini-3.5-flash",
		ConversationID: "cherry-thread",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "再说一遍刚才的词"},
		},
		Stream: true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), thirdReq, func(chunk dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("third stream returned error: %v", err)
	}
	if len(client.configs) != 4 {
		t.Fatalf("expected third request to make one provider call, got %#v", client.configs)
	}
	if client.configs[3].ConversationID != fallbackID {
		t.Fatalf("expected third request to reuse repaired provider id %q, got %q", fallbackID, client.configs[3].ConversationID)
	}
}

func TestChatCompletionMessageParsesOpenAIFileContentParts(t *testing.T) {
	body := `{
		"role":"user",
		"content":[
			{"type":"input_text","text":"请总结附件"},
			{"type":"input_file","filename":"note.txt","file_data":"data:text/plain;base64,SGVsbG8="},
			{"type":"file","file":{"file_id":"file_abc","filename":"stored.pdf","mime_type":"application/pdf"}},
			{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}
		]
	}`
	var msg dto.ChatCompletionMessage
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("Unmarshal returned error: %v", err)
	}
	if msg.Content != "请总结附件" {
		t.Fatalf("expected text content, got %q", msg.Content)
	}
	if len(msg.Attachments) != 3 {
		t.Fatalf("expected 3 attachments, got %#v", msg.Attachments)
	}
	if msg.Attachments[0].Name != "note.txt" || msg.Attachments[0].MimeType != "text/plain" || msg.Attachments[0].Data != "SGVsbG8=" {
		t.Fatalf("unexpected inline file attachment: %#v", msg.Attachments[0])
	}
	if msg.Attachments[1].FileID != "file_abc" || msg.Attachments[1].Name != "stored.pdf" || msg.Attachments[1].MimeType != "application/pdf" {
		t.Fatalf("unexpected file_id attachment: %#v", msg.Attachments[1])
	}
	if msg.Attachments[2].URL != "https://example.com/image.png" {
		t.Fatalf("unexpected remote image attachment: %#v", msg.Attachments[2])
	}
}

func TestOpenAIServiceInputFilesResolveStoredFileID(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{}, nil)
	service.fileStore = newOpenAIFileStore(t.TempDir())
	fileID := "file-test"
	path := filepath.Join(service.fileStore.dir, fileID+"_note.txt")
	if err := os.WriteFile(path, []byte("stored content"), 0600); err != nil {
		t.Fatal(err)
	}
	meta := openAIFileMetadata{
		openAIFileObject: openAIFileObject{
			ID:        fileID,
			Object:    "file",
			Bytes:     int64(len("stored content")),
			CreatedAt: time.Now().Unix(),
			Filename:  "note.txt",
			Purpose:   "assistants",
		},
		Path:     path,
		MimeType: "text/plain",
	}
	if err := service.fileStore.writeMetadata(meta); err != nil {
		t.Fatal(err)
	}

	files, err := service.inputFilesFromModelMessages(context.Background(), []models.Message{{
		Role: "user",
		Attachments: []models.Attachment{{
			FileID: fileID,
		}},
	}})
	if err != nil {
		t.Fatalf("inputFilesFromModelMessages returned error: %v", err)
	}
	if len(files) != 1 || files[0].Name != "note.txt" || files[0].MimeType != "text/plain" || string(files[0].Data) != "stored content" {
		t.Fatalf("unexpected resolved file: %#v", files)
	}
}

func TestOpenAIServiceInputFilesResolveInlineData(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{}, nil)
	files, err := service.inputFilesFromModelMessages(context.Background(), []models.Message{{
		Role: "user",
		Attachments: []models.Attachment{{
			Name:     "note.txt",
			MimeType: "text/plain",
			Data:     base64.StdEncoding.EncodeToString([]byte("inline content")),
		}},
	}})
	if err != nil {
		t.Fatalf("inputFilesFromModelMessages returned error: %v", err)
	}
	if len(files) != 1 || string(files[0].Data) != "inline content" {
		t.Fatalf("unexpected inline file: %#v", files)
	}
}

func TestCreateChatCompletionStreamDoesNotSilentlyStopWhenProviderEmitsNoContent(t *testing.T) {
	client := &fakeGeminiClient{}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
		},
		Stream: true,
	}

	var content strings.Builder
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
		return true
	})

	if err == nil {
		t.Fatalf("expected empty provider stream to return an error instead of silent stop; content=%q", content.String())
	}
}

func TestCreateChatCompletionStreamWithToolsDoesNotSilentlyStopWhenProviderEmitsNoContent(t *testing.T) {
	client := &fakeGeminiClient{}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "帮我搜索一下"},
		},
		Tools: []dto.ToolDefinition{{
			Type: "function",
			Function: dto.ToolFunctionDefinition{
				Name:       "search",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
		Stream: true,
	}

	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		return true
	})

	if err == nil {
		t.Fatal("expected empty tool-bridge provider stream to return an error instead of silent stop")
	}
}

func TestCreateChatCompletionStreamEmitsToolCallsForToolBridgeJSON(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: `{"status":"tool_calls","tool_calls":[{"name":"mcp__exa__web_search_exa","arguments":{"query":"trending repositories on GitHub today"}}]}`},
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

func TestCreateChatCompletionStreamEmitsReasoningBeforeToolBridgeJSON(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "thinking_text", Delta: "需要先查询外部信息。"},
			{Kind: "content_delta", Delta: `{"status":"tool_calls","tool_calls":[{"name":"mcp__exa__web_search_exa","arguments":{"query":"Gemini tool bridge"}}]}`},
		},
	}, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash:thinking=extended",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "帮我搜索 Gemini tool bridge"},
		},
		Tools: []dto.ToolDefinition{{
			Type: "function",
			Function: dto.ToolFunctionDefinition{
				Name:       "mcp__exa__web_search_exa",
				Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
			},
		}},
		Stream: true,
	}

	var reasoning []string
	var content []string
	var toolCalls []dto.ChatCompletionChunkDeltaToolCall
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) == 0 {
			return true
		}
		delta := chunk.Choices[0].Delta
		if delta.ReasoningContent != "" {
			reasoning = append(reasoning, delta.ReasoningContent)
		}
		if delta.Content != "" {
			content = append(content, delta.Content)
		}
		toolCalls = append(toolCalls, delta.ToolCalls...)
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if strings.Join(reasoning, "") != "需要先查询外部信息。" {
		t.Fatalf("expected reasoning_content before tool call, got %#v", reasoning)
	}
	if strings.Contains(strings.Join(content, ""), `"tool_calls"`) {
		t.Fatalf("tool bridge JSON leaked as content: %#v", content)
	}
	if len(toolCalls) != 1 || toolCalls[0].Function.Name != "mcp__exa__web_search_exa" {
		t.Fatalf("expected parsed tool call after reasoning, got %#v", toolCalls)
	}
}

func TestParseToolBridgePlanAcceptsFencedAndWrappedJSON(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{}, nil)
	req := dto.ChatCompletionRequest{
		Tools: []dto.ToolDefinition{{
			Type: "function",
			Function: dto.ToolFunctionDefinition{
				Name:       "search",
				Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
			},
		}},
	}

	for _, text := range []string{
		"```json\n{\"status\":\"tool_calls\",\"tool_calls\":[{\"name\":\"search\",\"arguments\":{\"query\":\"news\"}}]}\n```",
		"Here is the plan: {\"status\":\"tool_calls\",\"tool_calls\":[{\"name\":\"search\",\"arguments\":{\"query\":\"news\"}}]}",
	} {
		plan := service.parseToolBridgePlan(req, text)
		if plan.Err != nil {
			t.Fatalf("expected valid plan for %q, got %v", text, plan.Err)
		}
		if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Function.Name != "search" {
			t.Fatalf("unexpected tool calls: %#v", plan.ToolCalls)
		}
	}
}

func TestParseToolBridgePlanValidatesDynamicArguments(t *testing.T) {
	service := NewOpenAIService(&fakeGeminiClient{}, nil)
	req := dto.ChatCompletionRequest{
		Tools: []dto.ToolDefinition{{
			Type: "function",
			Function: dto.ToolFunctionDefinition{
				Name:       "search",
				Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"],"additionalProperties":false}`),
			},
		}},
	}

	cases := []string{
		`{"status":"tool_calls","tool_calls":[{"name":"search","arguments":{"limit":3}}]}`,
		`{"status":"tool_calls","tool_calls":[{"name":"search","arguments":{"query":7}}]}`,
		`{"status":"tool_calls","tool_calls":[{"name":"search","arguments":{"query":"news","extra":true}}]}`,
		`{"status":"tool_calls","tool_calls":[{"name":"search","arguments":[]}]}`,
	}
	for _, text := range cases {
		if plan := service.parseToolBridgePlan(req, text); plan.Err == nil {
			t.Fatalf("expected invalid plan for %s, got %#v", text, plan)
		}
	}
}

func TestCreateChatCompletionRepairsMalformedToolPlannerOutput(t *testing.T) {
	client := &fakeGeminiClient{
		responses: []string{
			`{"tool_calls":[{"name":"search","arguments":{"query":"weather"}}]}`,
			`{"status":"tool_calls","tool_calls":[{"name":"search","arguments":{"query":"weather"}}]}`,
		},
	}
	service := NewOpenAIService(client, nil)
	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "search weather"},
		},
		Tools: []dto.ToolDefinition{{
			Type: "function",
			Function: dto.ToolFunctionDefinition{
				Name:       "search",
				Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
			},
		}},
	}

	resp, err := service.CreateChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls response, got %#v", resp.Choices)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one repaired tool call, got %#v", resp.Choices[0].Message.ToolCalls)
	}
	if len(client.generatePrompts) != 2 || !strings.Contains(client.generatePrompts[1], "Repair the previous tool-planning output") {
		t.Fatalf("expected one repair request, got prompts %#v", client.generatePrompts)
	}
}

func TestCreateChatCompletionRequiredToolReturnsErrorWhenRepairFails(t *testing.T) {
	client := &fakeGeminiClient{
		responses: []string{
			`not json`,
			`{"status":"message","content":"I cannot call a tool"}`,
		},
	}
	service := NewOpenAIService(client, nil)
	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "search weather"},
		},
		Tools: []dto.ToolDefinition{{
			Type:     "function",
			Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		ToolChoiceRaw: json.RawMessage(`"required"`),
	}

	if _, err := service.CreateChatCompletion(context.Background(), req); err == nil {
		t.Fatal("expected required tool failure to return an error")
	}
}

func TestCreateChatCompletionAutoToolsNormalMessageDoesNotRepair(t *testing.T) {
	client := &fakeGeminiClient{
		responses: []string{"这是普通回答，不需要工具。"},
	}
	service := NewOpenAIService(client, nil)
	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "解释递归"},
		},
		Tools: []dto.ToolDefinition{{
			Type:     "function",
			Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	}

	resp, err := service.CreateChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "这是普通回答，不需要工具。" {
		t.Fatalf("unexpected content: %q", got)
	}
	if len(client.generatePrompts) != 1 {
		t.Fatalf("auto normal message should complete in one main-topic request, got %d prompts", len(client.generatePrompts))
	}
	if !strings.Contains(client.generatePrompts[0], "If a tool is needed") {
		t.Fatalf("expected tools request to include tool-call protocol, got %q", client.generatePrompts[0])
	}
}

func TestNewAutoProviderConversationIDUsesGeminiWebLikePathID(t *testing.T) {
	id := newAutoProviderConversationID()
	if len(id) != 16 {
		t.Fatalf("expected 16 hex characters, got %q", id)
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("expected lowercase hex id, got %q", id)
		}
	}
	if strings.Contains(id, "openai-auto") {
		t.Fatalf("auto provider id still contains old marker: %q", id)
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
		streamEventsByCall: [][]providers.StreamEvent{
			{
				{Kind: "content_delta", Delta: "这是"},
				{Kind: "content_delta", Delta: "普通回答"},
			},
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
	if len(client.prompts) != 1 {
		t.Fatalf("expected one main-topic stream prompt, got %#v", client.prompts)
	}
	if !strings.Contains(client.prompts[0], "If a tool is needed") {
		t.Fatalf("expected stream prompt to include tool-call protocol, got %#v", client.prompts)
	}
}

func TestCreateChatCompletionStreamRetriesInitialToolBridgeEmptyStreamWithFreshConversation(t *testing.T) {
	client := &fakeGeminiClient{
		streamEventsByCall: [][]providers.StreamEvent{
			nil,
			{{Kind: "content_delta", Delta: `{"status":"tool_calls","tool_calls":[{"name":"search","arguments":{"query":"today news"}}]}`}},
		},
	}
	service := NewOpenAIService(client, nil)
	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "查一下今天新闻"},
		},
		Tools: []dto.ToolDefinition{{
			Type:     "function",
			Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		Stream: true,
	}

	var toolCalls []dto.ChatCompletionChunkDeltaToolCall
	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		if len(chunk.Choices) > 0 {
			toolCalls = append(toolCalls, chunk.Choices[0].Delta.ToolCalls...)
		}
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].Function.Name != "search" {
		t.Fatalf("expected retried tool call, got %#v", toolCalls)
	}
	if len(client.configs) != 2 {
		t.Fatalf("expected initial failed stream and one fresh retry, got %#v", client.configs)
	}
	if client.configs[0].ConversationID == "" || client.configs[1].ConversationID == "" || client.configs[0].ConversationID == client.configs[1].ConversationID {
		t.Fatalf("expected retry to use fresh conversation, got %#v", client.configs)
	}
}

func TestCreateChatCompletionStreamDoesNotRetryInitialToolBridgeAuthError(t *testing.T) {
	client := &fakeGeminiClient{
		streamErrs: []error{errors.New("authentication failed: cookies invalid")},
	}
	service := NewOpenAIService(client, nil)
	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "查一下今天新闻"},
		},
		Tools: []dto.ToolDefinition{{
			Type:     "function",
			Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		Stream: true,
	}

	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		return true
	})
	if err == nil {
		t.Fatal("expected auth error")
	}
	if len(client.configs) != 1 {
		t.Fatalf("auth errors should not be retried by tool bridge fallback, got %#v", client.configs)
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

func TestCreateChatCompletionToolResultUsesMainConversationAndAggregatesTools(t *testing.T) {
	client := &fakeGeminiClient{
		responses: []string{"根据工具结果，A 和 B 都符合条件。"},
	}
	service := NewOpenAIService(client, nil)

	req := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "对比 A 和 B 的最新信息。"},
			{Role: "assistant", ToolCalls: []dto.ChatCompletionToolCall{
				{
					ID:   "call_a",
					Type: "function",
					Function: dto.ChatCompletionToolCallFunction{
						Name:      "search",
						Arguments: `{"query":"A latest"}`,
					},
				},
				{
					ID:   "call_b",
					Type: "function",
					Function: dto.ChatCompletionToolCallFunction{
						Name:      "search",
						Arguments: `{"query":"B latest"}`,
					},
				},
			}},
			{Role: "tool", ToolCallID: "call_a", Name: "search", Content: "A result"},
			{Role: "tool", ToolCallID: "call_b", Name: "search", Content: "B result"},
		},
		Tools: []dto.ToolDefinition{{
			Type:     "function",
			Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	}

	resp, err := service.CreateChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateChatCompletion returned error: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "根据工具结果，A 和 B 都符合条件。" {
		t.Fatalf("unexpected content: %q", got)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("expected final answer, got choice %#v", resp.Choices[0])
	}
	if len(client.generatePrompts) != 1 || strings.Contains(client.generatePrompts[0], "OpenAI-compatible assistant running behind a bridge") {
		t.Fatalf("expected tool-result answer to skip tool bridge, got prompts %#v", client.generatePrompts)
	}
	if !strings.Contains(client.generatePrompts[0], "A result") || !strings.Contains(client.generatePrompts[0], "B result") {
		t.Fatalf("expected both tool results in prompt, got %q", client.generatePrompts[0])
	}
	if len(client.configs) != 1 || client.configs[0].ConversationID == "" {
		t.Fatalf("expected tool-result answer to use a provider conversation, got configs %#v", client.configs)
	}

	followup := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "对比 A 和 B 的最新信息。"},
			{Role: "assistant", Content: "根据工具结果，A 和 B 都符合条件。"},
			{Role: "user", Content: "继续说。"},
		},
	}
	followupPlan := service.planRequestContext(followup)
	if followupPlan.ProviderConversationID != client.configs[0].ConversationID || !followupPlan.AutoContext {
		t.Fatalf("expected follow-up to reuse main provider conversation %q, got id=%q auto=%v", client.configs[0].ConversationID, followupPlan.ProviderConversationID, followupPlan.AutoContext)
	}
}

func TestToolCallThenToolResultStaysOnSameMainConversation(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{{
			Kind:  "content_delta",
			Delta: `{"status":"tool_calls","tool_calls":[{"name":"search","arguments":{"query":"today news"}}]}`,
		}},
		responses: []string{"这是基于工具结果的回答。"},
	}
	service := NewOpenAIService(client, nil)
	tools := []dto.ToolDefinition{{
		Type:     "function",
		Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}

	firstReq := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "查一下今天新闻"},
		},
		Tools:  tools,
		Stream: true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), firstReq, func(chunk dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("first stream returned error: %v", err)
	}
	if len(client.configs) != 1 || client.configs[0].ConversationID == "" {
		t.Fatalf("expected first tool decision to use main conversation, got %#v", client.configs)
	}
	mainConversationID := client.configs[0].ConversationID

	secondReq := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "查一下今天新闻"},
			{Role: "assistant", ToolCalls: []dto.ChatCompletionToolCall{{
				ID:   "call_search",
				Type: "function",
				Function: dto.ChatCompletionToolCallFunction{
					Name:      "search",
					Arguments: `{"query":"today news"}`,
				},
			}}},
			{Role: "tool", ToolCallID: "call_search", Name: "search", Content: "新闻结果"},
		},
		Tools: tools,
	}
	resp, err := service.CreateChatCompletion(context.Background(), secondReq)
	if err != nil {
		t.Fatalf("tool result completion returned error: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "这是基于工具结果的回答。" {
		t.Fatalf("unexpected tool result answer: %q", got)
	}
	if len(client.configs) != 2 {
		t.Fatalf("expected second provider call, got %#v", client.configs)
	}
	if client.configs[1].ConversationID != mainConversationID {
		t.Fatalf("tool result must reuse main conversation %q, got %q", mainConversationID, client.configs[1].ConversationID)
	}
}

func TestCreateChatCompletionStreamUsesMainTopicForGreetingWithTools(t *testing.T) {
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
	if len(client.prompts) != 1 || !strings.Contains(client.prompts[0], "If a tool is needed") {
		t.Fatalf("expected greeting to use one main-topic tool prompt, got prompts %#v", client.prompts)
	}
	if len(client.configs) != 1 || client.configs[0].ConversationID == "" {
		t.Fatalf("tool bridge must use a main conversation id, got configs %#v", client.configs)
	}
}

func TestCreateChatCompletionStreamAppendsToolBridgeToExistingMainConversation(t *testing.T) {
	client := &fakeGeminiClient{
		streamEvents: []providers.StreamEvent{
			{Kind: "content_delta", Delta: `{"status":"tool_calls","tool_calls":[{"name":"mcp__exa__web_search_exa","arguments":{"query":"GitHub trending today"}}]}`},
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
	if client.configs[0].ConversationID != "provider-thread" {
		t.Fatalf("tool bridge must append to main conversation, got %q", client.configs[0].ConversationID)
	}
	if client.prompts[0] != "今天 GitHub 的热点是什么？" {
		t.Fatalf("same-topic tool bridge should send only latest user request, got %q", client.prompts[0])
	}
}

func TestCreateChatCompletionStreamReusesToolBridgeInstructionsInSameConversation(t *testing.T) {
	tools := []dto.ToolDefinition{{
		Type: "function",
		Function: dto.ToolFunctionDefinition{
			Name:        "lookup_weather",
			Description: "Get weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		},
	}}
	client := &fakeGeminiClient{
		streamEventsByCall: [][]providers.StreamEvent{
			{{Kind: "content_delta", Delta: "你好"}},
			{{Kind: "content_delta", Delta: "SpaceX 是一家航天公司。"}},
		},
	}
	service := NewOpenAIService(client, nil)

	firstReq := dto.ChatCompletionRequest{
		Model:    "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{{Role: "user", Content: "你好"}},
		Tools:    tools,
		Stream:   true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), firstReq, func(dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("first stream returned error: %v", err)
	}
	if len(client.prompts) != 1 || !strings.Contains(client.prompts[0], "Available tools:") {
		t.Fatalf("first tool bridge prompt should include full tools, got %#v", client.prompts)
	}

	secondReq := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
			{Role: "assistant", Content: "你好"},
			{Role: "user", Content: "你知道 SpaceX 吗？"},
		},
		Tools:  tools,
		Stream: true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), secondReq, func(dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("second stream returned error: %v", err)
	}
	if len(client.prompts) != 2 {
		t.Fatalf("expected two prompts, got %#v", client.prompts)
	}
	if strings.Contains(client.prompts[1], "Available tools:") || strings.Contains(client.prompts[1], "parameters:") {
		t.Fatalf("second prompt should reuse existing tool instructions, got %q", client.prompts[1])
	}
	if strings.Contains(client.prompts[1], "already defined in this Gemini conversation") {
		t.Fatalf("auto follow-up should not repeat tool reminders, got %q", client.prompts[1])
	}
	if client.prompts[1] != "你知道 SpaceX 吗？" {
		t.Fatalf("second prompt should be only the current user request, got %q", client.prompts[1])
	}
	if len(client.configs) < 2 || client.configs[0].ConversationID == "" || client.configs[1].ConversationID != client.configs[0].ConversationID {
		t.Fatalf("expected second request to reuse provider conversation, configs=%#v", client.configs)
	}
}

func TestCreateChatCompletionStreamDoesNotResendToolBridgeInstructionsInSameConversationWhenToolsChange(t *testing.T) {
	firstTools := []dto.ToolDefinition{{
		Type:     "function",
		Function: dto.ToolFunctionDefinition{Name: "lookup_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	secondTools := []dto.ToolDefinition{{
		Type:     "function",
		Function: dto.ToolFunctionDefinition{Name: "lookup_time", Parameters: json.RawMessage(`{"type":"object"}`)},
	}}
	client := &fakeGeminiClient{
		streamEventsByCall: [][]providers.StreamEvent{
			{{Kind: "content_delta", Delta: "你好"}},
			{{Kind: "content_delta", Delta: "现在继续。"}},
		},
	}
	service := NewOpenAIService(client, nil)

	firstReq := dto.ChatCompletionRequest{
		Model:    "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{{Role: "user", Content: "你好"}},
		Tools:    firstTools,
		Stream:   true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), firstReq, func(dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("first stream returned error: %v", err)
	}

	secondReq := dto.ChatCompletionRequest{
		Model: "gemini-3.5-flash",
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "你好"},
			{Role: "assistant", Content: "你好"},
			{Role: "user", Content: "继续"},
		},
		Tools:  secondTools,
		Stream: true,
	}
	if err := service.CreateChatCompletionStream(context.Background(), secondReq, func(dto.ChatCompletionChunk) bool { return true }); err != nil {
		t.Fatalf("second stream returned error: %v", err)
	}
	if len(client.prompts) != 2 {
		t.Fatalf("expected two prompts, got %#v", client.prompts)
	}
	if strings.Contains(client.prompts[1], "Available tools:") || strings.Contains(client.prompts[1], "lookup_time") {
		t.Fatalf("same-topic follow-up should not resend tool instructions, got %q", client.prompts[1])
	}
	if client.prompts[1] != "继续" {
		t.Fatalf("same-topic follow-up should send only latest user request, got %q", client.prompts[1])
	}
}

func TestCreateChatCompletionStreamFallbackFreshConversationRestoresFullToolPrompt(t *testing.T) {
	client := &fakeGeminiClient{
		streamErrs: []error{errors.New("gemini bard error 1097")},
		streamEventsByCall: [][]providers.StreamEvent{
			nil,
			{{Kind: "content_delta", Delta: "fallback answer"}},
		},
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
			Content: "查一下今天新闻",
		}),
		Tools: []dto.ToolDefinition{{
			Type:     "function",
			Function: dto.ToolFunctionDefinition{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
		Stream: true,
	}

	err := service.CreateChatCompletionStream(context.Background(), req, func(chunk dto.ChatCompletionChunk) bool {
		return true
	})
	if err != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", err)
	}
	if len(client.prompts) != 2 {
		t.Fatalf("expected failed same-topic call and fresh fallback, got prompts %#v", client.prompts)
	}
	if client.prompts[0] != "查一下今天新闻" {
		t.Fatalf("same-topic attempt should send only latest user request, got %q", client.prompts[0])
	}
	if !strings.Contains(client.prompts[1], "Available tools:") || !strings.Contains(client.prompts[1], "search") {
		t.Fatalf("fresh fallback must restore full tool prompt, got %q", client.prompts[1])
	}
	if !strings.Contains(client.prompts[1], "你好") || !strings.Contains(client.prompts[1], "查一下今天新闻") {
		t.Fatalf("fresh fallback must include full OpenAI context, got %q", client.prompts[1])
	}
	if len(client.configs) != 2 || client.configs[1].ConversationID == "" || client.configs[1].ConversationID == "provider-1" {
		t.Fatalf("fresh fallback should use a new provider conversation, got %#v", client.configs)
	}
}

// TestPlanRequestContextSkipsUntrustedProviderConversation reproduces the
// "lost early turns" root cause: a provider conversation flagged untrusted
// must not be reused for server-side context, forcing the next turn to
// rebuild from the full OpenAI history with a fresh provider id.
func TestPlanRequestContextSkipsUntrustedProviderConversation(t *testing.T) {
	client := &fakeGeminiClient{untrusted: true}
	service := NewOpenAIService(client, nil)

	first := openAIContextPlan{
		ProviderConversationID: "provider-thread",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住第一个词：海棠"},
		},
		ResponseText: "已记住海棠",
	}
	service.rememberRequestContext(first)

	// Second turn whose prefix matches the remembered transcript. With
	// OPENAI_CONTEXT_LOCAL_FALLBACK on (default), the untrusted provider
	// conversation must NOT be reused.
	t.Setenv("OPENAI_CONTEXT_LOCAL_FALLBACK", "true")
	req := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住第一个词：海棠"},
			{Role: "assistant", Content: "已记住海棠"},
			{Role: "user", Content: "第一个词是什么？"},
		},
	}
	plan := service.planRequestContext(req)
	if plan.AutoContext {
		t.Fatalf("expected untrusted provider to NOT be reused for auto context, got provider_id=%q", plan.ProviderConversationID)
	}
	if plan.ProviderConversationID == "provider-thread" {
		t.Fatal("expected a fresh provider conversation id, not the untrusted one")
	}
	// Rebuilt prompt must carry the full history, not just the latest user turn.
	if !strings.Contains(plan.Prompt, "海棠") {
		t.Fatalf("expected rebuilt prompt to include full history, got %q", plan.Prompt)
	}
}

// TestPlanRequestContextReusesUntrustedWhenDisabled verifies the env switch
// restores legacy behavior (untrusted conversations are still reused) so the
// fix can be A/B tested or rolled back without a redeploy.
func TestPlanRequestContextReusesUntrustedWhenDisabled(t *testing.T) {
	client := &fakeGeminiClient{untrusted: true}
	service := NewOpenAIService(client, nil)

	first := openAIContextPlan{
		ProviderConversationID: "provider-thread-2",
		RequestMessages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住第一个词：芭蕉"},
		},
		ResponseText: "已记住芭蕉",
	}
	service.rememberRequestContext(first)

	t.Setenv("OPENAI_CONTEXT_LOCAL_FALLBACK", "false")
	req := dto.ChatCompletionRequest{
		Messages: []dto.ChatCompletionMessage{
			{Role: "user", Content: "记住第一个词：芭蕉"},
			{Role: "assistant", Content: "已记住芭蕉"},
			{Role: "user", Content: "第一个词是什么？"},
		},
	}
	plan := service.planRequestContext(req)
	if !plan.AutoContext || plan.ProviderConversationID != "provider-thread-2" {
		t.Fatalf("expected legacy behavior to reuse untrusted provider thread-2, got id=%q auto=%v", plan.ProviderConversationID, plan.AutoContext)
	}
	if plan.Prompt != "第一个词是什么？" {
		t.Fatalf("expected only latest user prompt under legacy mode, got %q", plan.Prompt)
	}
}
