package openai

import (
	"strings"
	"testing"
	"time"

	"gemini-free-api/internal/modules/openai/dto"
	"gemini-free-api/internal/modules/providers"
)

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
