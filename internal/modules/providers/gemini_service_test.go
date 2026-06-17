package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestParseResponseExtractsGeneratedImages(t *testing.T) {
	imageURL := "https://lh3.googleusercontent.com/generated-image=w1024-h1024"
	iconURL := "https://fonts.gstatic.com/s/i/short-term/release/googlesymbols/expand/default/24px.svg"
	payload := []interface{}{
		nil,
		"conversation-id",
		nil,
		nil,
		[]interface{}{
			[]interface{}{
				"response-id",
				[]interface{}{"done", []interface{}{iconURL, imageURL}},
			},
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	root := []interface{}{
		[]interface{}{nil, nil, string(payloadJSON)},
	}
	rootJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}

	client := &Client{log: zap.NewNop()}
	resp, err := client.parseResponse(string(rootJSON))
	if err != nil {
		t.Fatalf("parseResponse returned error: %v", err)
	}

	if resp.Text != "done" {
		t.Fatalf("expected text %q, got %q", "done", resp.Text)
	}
	if len(resp.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(resp.Images))
	}
	if resp.Images[0].URL != imageURL {
		t.Fatalf("expected image URL %q, got %q", imageURL, resp.Images[0].URL)
	}
	if resp.ConversationID != "conversation-id" {
		t.Fatalf("expected conversation id, got %q", resp.ConversationID)
	}
	if got := resp.Metadata["rcid"]; got != "response-id" {
		t.Fatalf("expected response choice id, got %q", got)
	}
}

func TestBuildGenerateInnerIncludesConversationMetadataAndContextToken(t *testing.T) {
	metadata := []interface{}{"c_1", "r_1", "rc_1", nil, nil, nil, nil, nil, nil, ""}
	inner := buildGenerateInner("hello", nil, "en", "request-id", metadata, "opaque-token")

	got, ok := inner[2].([]interface{})
	if !ok {
		t.Fatalf("expected conversation metadata array, got %#v", inner[2])
	}
	if len(got) != 10 || got[0] != "c_1" || got[1] != "r_1" || got[2] != "rc_1" {
		t.Fatalf("unexpected conversation metadata: %#v", got)
	}
	if inner[3] != "opaque-token" {
		t.Fatalf("unexpected conversation context token: %#v", inner[3])
	}
}

func TestExtractConversationMetadataFromStreamBuffer(t *testing.T) {
	first := buildStreamLine(t,
		[]interface{}{nil, []interface{}{nil, "r_1"}, map[string]interface{}{"18": "r_1", "21": []interface{}{"opaque-token"}, "44": true}},
	)
	second := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文"}, nil, nil, nil, nil, true},
		}},
	)

	metadata := extractConversationMetadataFromBuffer([]byte(")]}'\n\n" + first + "\n" + second + "\n"))
	if metadata["cid"] != "c_1" || metadata["rid"] != "r_1" || metadata["rcid"] != "rc_1" || metadata["context_token"] != "opaque-token" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func TestExtractConversationMetadataFromMultilineStreamDocument(t *testing.T) {
	first := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"18": "r_1", "21": []interface{}{"opaque-token"}, "44": true},
	})}
	second := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文"}, nil, nil, nil, nil, true},
		},
	})}
	doc := mustMarshalString(t, []interface{}{first, second})
	raw := []byte(")]}'\n\n" + strings.Replace(doc, "],[", "],\n[", 1))

	metadata := extractConversationMetadataFromBuffer(raw)
	if metadata["cid"] != "c_1" || metadata["rid"] != "r_1" || metadata["rcid"] != "rc_1" || metadata["context_token"] != "opaque-token" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func TestExtractConversationMetadataFromIncompleteOuterStreamDocument(t *testing.T) {
	first := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"18": "r_1", "21": []interface{}{"opaque-token"}, "44": true},
	})}
	second := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文"}, nil, nil, nil, nil, true},
		},
	})}
	doc := mustMarshalString(t, []interface{}{first, second})
	raw := []byte(")]}'\n\n" + strings.TrimSuffix(doc, "]"))

	metadata := extractConversationMetadataFromBuffer(raw)
	if metadata["cid"] != "c_1" || metadata["rid"] != "r_1" || metadata["rcid"] != "rc_1" || metadata["context_token"] != "opaque-token" {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
}

func TestConversationMetadataAndContextTokenUseStoredMetadata(t *testing.T) {
	client := &Client{}
	client.updateConversation("client-thread", map[string]any{
		"cid":           "c_1",
		"rid":           "r_1",
		"rcid":          "rc_1",
		"context_token": "opaque-token",
	})

	metadata := client.conversationMetadata("client-thread")
	if len(metadata) != 10 || metadata[0] != "c_1" || metadata[1] != "r_1" || metadata[2] != "rc_1" {
		t.Fatalf("unexpected conversation metadata: %#v", metadata)
	}
	if token := client.conversationContextToken("client-thread"); token != "opaque-token" {
		t.Fatalf("unexpected conversation context token: %#v", token)
	}
	if sourcePath := client.conversationSourcePath("client-thread"); sourcePath != "/app/1" {
		t.Fatalf("unexpected conversation source path: %#v", sourcePath)
	}
}

func TestPruneConversationsRemovesExpiredEntries(t *testing.T) {
	client := &Client{
		conversations: map[string]*SessionMetadata{
			"old":   {ConversationID: "c_old"},
			"fresh": {ConversationID: "c_fresh"},
		},
		conversationSeen: map[string]time.Time{
			"old":   time.Now().Add(-conversationCacheTTL - time.Minute),
			"fresh": time.Now(),
		},
	}

	client.conversationMu.Lock()
	client.pruneConversationsLocked(time.Now())
	client.conversationMu.Unlock()

	if _, ok := client.conversations["old"]; ok {
		t.Fatal("expected expired conversation to be pruned")
	}
	if client.conversations["fresh"].ConversationID != "c_fresh" {
		t.Fatal("expected fresh conversation to remain")
	}
}

func TestGenerateContentWithConversationIDSendsGeminiWebContinuationRequest(t *testing.T) {
	var captured *http.Request
	var capturedBody string
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			capturedBody = string(body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(buildGenerateResponse(t, "松月", "c_6a27075d298c2a1d"))),
				Request:    req,
			}, nil
		})},
		at:           "test-at",
		cookieHeader: "SID=test",
		buildLabel:   "test-build",
		sessionID:    "test-session",
		language:     "zh-CN",
		log:          zap.NewNop(),
		cachedModels: []ModelInfo{{ID: "fbb127bbb056c959"}},
		cachedAliases: map[string]string{
			"gemini-3.5-flash": "fbb127bbb056c959",
		},
		conversations: make(map[string]*SessionMetadata),
	}
	client.updateConversation("client-thread", map[string]any{
		"cid":           "c_6a27075d298c2a1d",
		"rid":           "r_61dc3d479730df7f",
		"rcid":          "rc_34f69c60774c9654",
		"context_token": "opaque-token",
	})

	resp, err := client.GenerateContent(context.Background(), "刚才让你记住的词是什么？只回复这个词", WithModel("gemini-3.5-flash"), WithConversationID("client-thread"))
	if err != nil {
		t.Fatalf("GenerateContent returned error: %v", err)
	}
	if resp.Text != "松月" {
		t.Fatalf("expected fake response text, got %q", resp.Text)
	}
	if captured == nil {
		t.Fatal("expected upstream request to be captured")
	}
	if got := captured.URL.Query().Get("source-path"); got != "" {
		t.Fatalf("source-path should be disabled by default, got %q in %s", got, captured.URL.String())
	}

	inner := decodeGenerateInnerFromForm(t, capturedBody)
	messageContent, ok := inner[0].([]interface{})
	if !ok || len(messageContent) == 0 || messageContent[0] != "刚才让你记住的词是什么？只回复这个词" {
		t.Fatalf("expected latest user prompt in inner[0], got %#v", inner[0])
	}
	metadata, ok := inner[2].([]interface{})
	if !ok || len(metadata) < 3 {
		t.Fatalf("expected conversation metadata in inner[2], got %#v", inner[2])
	}
	if metadata[0] != "c_6a27075d298c2a1d" || metadata[1] != "r_61dc3d479730df7f" || metadata[2] != "rc_34f69c60774c9654" {
		t.Fatalf("unexpected conversation metadata: %#v", metadata)
	}
	if inner[3] != "opaque-token" {
		t.Fatalf("expected context token in inner[3], got %#v", inner[3])
	}
}

func TestGenerateContentWithConversationIDCanSendSourcePathWhenEnabled(t *testing.T) {
	var captured *http.Request
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(buildGenerateResponse(t, "松月", "c_6a27075d298c2a1d"))),
				Request:    req,
			}, nil
		})},
		at:            "test-at",
		cookieHeader:  "SID=test",
		buildLabel:    "test-build",
		sessionID:     "test-session",
		language:      "zh-CN",
		log:           zap.NewNop(),
		cachedModels:  []ModelInfo{{ID: "fbb127bbb056c959"}},
		cachedAliases: map[string]string{"gemini-3.5-flash": "fbb127bbb056c959"},
		conversations: make(map[string]*SessionMetadata),
	}
	client.updateConversation("client-thread", map[string]any{
		"cid":  "c_6a27075d298c2a1d",
		"rid":  "r_61dc3d479730df7f",
		"rcid": "rc_34f69c60774c9654",
	})

	_, err := client.GenerateContent(context.Background(), "继续", WithModel("gemini-3.5-flash"), WithConversationID("client-thread"), WithSourcePath(true))
	if err != nil {
		t.Fatalf("GenerateContent returned error: %v", err)
	}
	if captured == nil {
		t.Fatal("expected upstream request to be captured")
	}
	if got := captured.URL.Query().Get("source-path"); got != "/app/6a27075d298c2a1d" {
		t.Fatalf("expected source-path continuation URL, got %q in %s", got, captured.URL.String())
	}
}

func TestClientNextRequestIDIsMonotonic(t *testing.T) {
	client := &Client{requestSeq: 100000}

	first := client.nextRequestID()
	second := client.nextRequestID()

	if first != "200000" || second != "300000" {
		t.Fatalf("expected monotonic request ids, got first=%q second=%q", first, second)
	}
}

func TestGenerateContentWithConversationIDRejectsCIDMismatch(t *testing.T) {
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(buildGenerateResponse(t, "松月", "c_new_topic"))),
				Request:    req,
			}, nil
		})},
		at:           "test-at",
		cookieHeader: "SID=test",
		buildLabel:   "test-build",
		sessionID:    "test-session",
		language:     "zh-CN",
		log:          zap.NewNop(),
		cachedModels: []ModelInfo{{ID: "fbb127bbb056c959"}},
		cachedAliases: map[string]string{
			"gemini-3.5-flash": "fbb127bbb056c959",
		},
		conversations: make(map[string]*SessionMetadata),
	}
	client.updateConversation("client-thread", map[string]any{
		"cid":           "c_original_topic",
		"rid":           "r_1",
		"rcid":          "rc_1",
		"context_token": "opaque-token",
	})

	_, err := client.GenerateContent(context.Background(), "继续", WithModel("gemini-3.5-flash"), WithConversationID("client-thread"))
	if err == nil || !strings.Contains(err.Error(), "conversation continuity mismatch") {
		t.Fatalf("expected conversation continuity mismatch error, got %v", err)
	}
}

func TestGenerateContentStreamForOpenAISendsWebStreamQueryForNewTopic(t *testing.T) {
	var captured *http.Request
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(buildStreamLine(t, []interface{}{
					nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"21": []interface{}{"token"}}, nil, []interface{}{
						[]interface{}{"rc_1", []interface{}{"你好"}, nil, nil, nil, nil, true},
					},
				}))),
				Request: req,
			}, nil
		})},
		at:           "test-at",
		cookieHeader: "SID=test",
		buildLabel:   "test-build",
		sessionID:    "test-session",
		language:     "zh-CN",
		log:          zap.NewNop(),
		cachedModels: []ModelInfo{{ID: "fbb127bbb056c959"}},
		cachedAliases: map[string]string{
			"gemini-3.5-flash": "fbb127bbb056c959",
		},
		conversations: make(map[string]*SessionMetadata),
	}

	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool { return true }, WithModel("gemini-3.5-flash"))
	if err != nil {
		t.Fatalf("GenerateContentStreamForOpenAI returned error: %v", err)
	}
	if captured == nil {
		t.Fatal("expected upstream request to be captured")
	}
	query := captured.URL.Query()
	if query.Get("rt") != "c" || query.Get("bl") != "test-build" || query.Get("f.sid") != "test-session" || query.Get("hl") != "zh-CN" || query.Get("_reqid") == "" {
		t.Fatalf("expected full web stream query params, got %s", captured.URL.RawQuery)
	}
	if query.Get("source-path") != "" {
		t.Fatalf("new topic should not include source-path, got %q", query.Get("source-path"))
	}
}

func TestGenerateContentStreamForOpenAIStoresConversationStateBeforeStreamError(t *testing.T) {
	streamChunk := buildStreamLine(t, []interface{}{
		nil, []interface{}{"c_stream", "r_stream"}, map[string]interface{}{"21": []interface{}{"token"}}, nil, []interface{}{
			[]interface{}{"rc_stream", []interface{}{"部分正文"}, nil, nil, nil, nil, true},
		},
	})
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(&errAfterReader{data: []byte(streamChunk), err: fmt.Errorf("upstream broke")}),
				Request:    req,
			}, nil
		})},
		at:            "test-at",
		cookieHeader:  "SID=test",
		buildLabel:    "test-build",
		sessionID:     "test-session",
		language:      "zh-CN",
		log:           zap.NewNop(),
		cachedModels:  []ModelInfo{{ID: "fbb127bbb056c959"}},
		cachedAliases: map[string]string{"gemini-3.5-flash": "fbb127bbb056c959"},
		conversations: make(map[string]*SessionMetadata),
	}

	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool { return true }, WithModel("gemini-3.5-flash"), WithConversationID("client-thread"))
	if err == nil || !strings.Contains(err.Error(), "stream read") {
		t.Fatalf("expected stream read error, got %v", err)
	}
	if !client.HasConversationState("client-thread") {
		t.Fatal("expected stream metadata to be stored before the read error")
	}
	if sourcePath := client.conversationSourcePath("client-thread"); sourcePath != "/app/stream" {
		t.Fatalf("expected cached source path, got %q", sourcePath)
	}
}

func TestResolveModelsExposesOnlyUIModelAliases(t *testing.T) {
	_, aliases := resolveModels([]ModelInfo{
		{ID: "cf41b0e0dd7d53e5", Created: time.Now().Unix(), OwnedBy: "google", Provider: "gemini"},
		{ID: "fbb127bbb056c959", Created: time.Now().Unix(), OwnedBy: "google", Provider: "gemini"},
		{ID: "9d8ca3786ebdfbea", Created: time.Now().Unix(), OwnedBy: "google", Provider: "gemini"},
	})

	want := map[string]string{
		"gemini-3.1-flash-lite": "cf41b0e0dd7d53e5",
		"gemini-3.5-flash":      "fbb127bbb056c959",
		"gemini-3.1-pro":        "9d8ca3786ebdfbea",
	}
	for alias, id := range want {
		if aliases[alias] != id {
			t.Fatalf("expected %q to resolve to %q, got %q", alias, id, aliases[alias])
		}
	}
	if len(aliases) != len(want) {
		t.Fatalf("expected only UI aliases, got %#v", aliases)
	}
}

func TestSetGenerationHeadersUsesModelSpecificMode(t *testing.T) {
	flashReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(flashReq, "fbb127bbb056c959", "flash-req", "")

	proReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(proReq, "9d8ca3786ebdfbea", "pro-req", "")

	var flashHeader []interface{}
	if err := json.Unmarshal([]byte(flashReq.Header.Get("x-goog-ext-525001261-jspb")), &flashHeader); err != nil {
		t.Fatalf("unmarshal flash header: %v", err)
	}
	var proHeader []interface{}
	if err := json.Unmarshal([]byte(proReq.Header.Get("x-goog-ext-525001261-jspb")), &proHeader); err != nil {
		t.Fatalf("unmarshal pro header: %v", err)
	}

	if got := int(flashHeader[14].(float64)); got != 1 {
		t.Fatalf("expected flash mode 1, got %d", got)
	}
	if got := int(proHeader[14].(float64)); got != 3 {
		t.Fatalf("expected pro mode 3, got %d", got)
	}
	if got := proHeader[4].(string); got != "9d8ca3786ebdfbea" {
		t.Fatalf("expected pro model id in header, got %q", got)
	}
}

func TestSetGenerationHeadersUsesThinkingLevel(t *testing.T) {
	standardReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(standardReq, "fbb127bbb056c959", "standard-req", "standard")

	extendedReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(extendedReq, "fbb127bbb056c959", "extended-req", "extended")

	var standardHeader []interface{}
	if err := json.Unmarshal([]byte(standardReq.Header.Get("x-goog-ext-525001261-jspb")), &standardHeader); err != nil {
		t.Fatalf("unmarshal standard header: %v", err)
	}
	var extendedHeader []interface{}
	if err := json.Unmarshal([]byte(extendedReq.Header.Get("x-goog-ext-525001261-jspb")), &extendedHeader); err != nil {
		t.Fatalf("unmarshal extended header: %v", err)
	}

	if got := int(standardHeader[15].(float64)); got != 1 {
		t.Fatalf("expected standard thinking mode 1, got %d", got)
	}
	if got := int(extendedHeader[15].(float64)); got != 2 {
		t.Fatalf("expected extended thinking mode 2, got %d", got)
	}
}

func TestExtractStreamTextPrefersCandidateResponseText(t *testing.T) {
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"11": []interface{}{"HTML title"}, "44": true}},
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文开始"}, nil, nil, nil, nil, true},
		}, []interface{}{"Tokyo, Japan"}, nil, nil, "JP", nil, nil, nil, nil, nil, true},
	)

	if got := extractStreamText(line); got != "正文开始" {
		t.Fatalf("expected candidate response text, got %q", got)
	}
}

func TestExtractStreamTextUsesLatestCumulativeCandidate(t *testing.T) {
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文第一段"}, nil, nil, nil, nil, true},
		}},
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文第一段，第二段"}, nil, nil, nil, nil, true},
		}},
	)

	if got := extractStreamText(line); got != "正文第一段，第二段" {
		t.Fatalf("expected latest cumulative candidate, got %q", got)
	}
}

func TestExtractStreamTextIgnoresStateOnlyLine(t *testing.T) {
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"11": []interface{}{"Answer now"}, "44": true}},
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{""}, nil, nil, nil, nil, true},
		}},
	)

	if got := extractStreamText(line); got != "" {
		t.Fatalf("expected empty content for state-only line, got %q", got)
	}
}

func TestExtractStreamTextIgnoresUIActionText(t *testing.T) {
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []interface{}{
			[]interface{}{[]interface{}{1001, "Try again"}},
		}},
	)

	if got := extractStreamText(line); got != "" {
		t.Fatalf("expected UI action text to be ignored, got %q", got)
	}
}

func TestGenerateContentStreamForOpenAIDoesNotBlockContentAfterEarlyState(t *testing.T) {
	emptyRCLine := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{""}, nil, nil, nil, nil, true},
		}, map[string]interface{}{"11": []interface{}{"Answer now"}, "44": true}},
	)
	contentLine := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文开始"}, nil, nil, nil, nil, true},
		}},
	)
	buffer := []byte(emptyRCLine + "\n" + contentLine)

	var events []StreamEvent
	state := &StreamState{}
	nextState := ExtractStreamState(buffer)
	deltaText := extractStreamTextFromBuffer(buffer)
	for _, text := range nextState.ThinkingTexts {
		if !containsString(state.ThinkingTexts, text) {
			state.ThinkingTexts = append(state.ThinkingTexts, text)
			events = append(events, StreamEvent{Kind: "thinking_text", Delta: text})
		}
	}
	if deltaText != "" {
		state.HasContent = true
		events = append(events, StreamEvent{Kind: "content_delta", Delta: deltaText})
	}

	if len(events) != 2 {
		t.Fatalf("expected thinking and content events, got %#v", events)
	}
	if events[0].Kind != "thinking_text" || events[0].Delta != "Answer now" {
		t.Fatalf("unexpected thinking event: %#v", events[0])
	}
	if events[1].Kind != "content_delta" || events[1].Delta != "正文开始" {
		t.Fatalf("unexpected content event: %#v", events[1])
	}
}

func TestExtractBardErrorCode(t *testing.T) {
	raw := `)]}'

122
[["wrb.fr",null,null,null,null,[13,null,[["type.googleapis.com/assistant.boq.bard.application.BardErrorInfo",[1097]]]]]]`

	if got := extractBardErrorCode([]byte(raw)); got != "1097" {
		t.Fatalf("expected bard error code 1097, got %q", got)
	}
}

func TestExtractStreamStateIncludesAllStructuredThinkingSignals(t *testing.T) {
	summary := "**Defining the Scope**\n\nI've clarified the project goal."
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{
			"7":  []interface{}{nil, nil, nil, nil, nil, []interface{}{"Defining the Scope"}, nil, nil, nil, nil, nil, []interface{}{}},
			"44": true,
		}},
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			buildCandidateWithThinking("rc_1", "正文开始", summary),
		}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, []interface{}{
			[]interface{}{[]interface{}{1001, "Try again"}},
			[]interface{}{"Answer now", nil, 2},
		}},
	)

	state := ExtractStreamState([]byte(line))
	for _, want := range []string{
		"Defining the Scope",
		summary,
		"Answer now",
	} {
		if !containsString(state.ThinkingTexts, want) {
			t.Fatalf("expected thinking text %q in %#v", want, state.ThinkingTexts)
		}
	}
}

func TestExtractStreamStateIgnoresTopicTitleField11(t *testing.T) {
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{
			"11": []interface{}{"旋转五边形与弹跳小球"},
			"44": true,
		}},
	)

	state := ExtractStreamState([]byte(line))
	if containsString(state.ThinkingTexts, "旋转五边形与弹跳小球") {
		t.Fatalf("topic title should not be emitted as thinking: %#v", state.ThinkingTexts)
	}
}

func TestExtractStreamStateReadsEscapedThinkingSummaryFromRawBuffer(t *testing.T) {
	raw := `[["wrb.fr",null,"[null,[\"c_1\",\"r_1\"],null,null,[[\"rc_1\",[\"正文\"],null,null,null,null,true,null,[1],\"zh\",null,null,null,null,null,true,null,null,null,null,null,[false],null,false,[],null,null,null,[],null,null,null,null,null,null,null,null,[[\"**Defining the Core Elements**\\n\\nI've defined the key elements: red ball, rotating pentagon, bouncing physics.\"]],null,null,null,null,null,null,null,null,null,false,true]]]"]]`

	state := ExtractStreamState([]byte(raw))
	want := "**Defining the Core Elements**\n\nI've defined the key elements: red ball, rotating pentagon, bouncing physics."
	if !containsString(state.ThinkingTexts, want) {
		t.Fatalf("expected raw thinking summary %q in %#v", want, state.ThinkingTexts)
	}
}

func TestStreamTextDeltaRequiresCumulativePrefix(t *testing.T) {
	if got := streamTextDelta("正文第一段", "正文第一段，第二段"); got != "，第二段" {
		t.Fatalf("unexpected cumulative delta %q", got)
	}
	if got := streamTextDelta("正文第一段，第二段", "正文第一段"); got != "" {
		t.Fatalf("expected shorter snapshot to be ignored, got %q", got)
	}
	if got := streamTextDelta("正文第一段，第二段", "另一段正文"); got != "" {
		t.Fatalf("expected non-prefix snapshot to be ignored, got %q", got)
	}
}

func TestNextThinkingDeltaStreamsGrowingSummaryIncrementally(t *testing.T) {
	state := &StreamState{}
	first := nextThinkingDelta(state, "A\n\nB")
	second := nextThinkingDelta(state, "A\n\nB\n\nC")
	duplicate := nextThinkingDelta(state, "A\n\nB\n\nC")

	if first != "A\n\nB" {
		t.Fatalf("unexpected first delta %q", first)
	}
	if second != "\n\nC" {
		t.Fatalf("unexpected second delta %q", second)
	}
	if duplicate != "" {
		t.Fatalf("expected duplicate to be ignored, got %q", duplicate)
	}
}

func TestNextThinkingDeltaStreamsIndependentStateOnce(t *testing.T) {
	state := &StreamState{}
	if got := nextThinkingDelta(state, "Defining the Scope"); got != "Defining the Scope" {
		t.Fatalf("unexpected state delta %q", got)
	}
	if got := nextThinkingDelta(state, "Defining the Scope"); got != "" {
		t.Fatalf("expected duplicate state to be ignored, got %q", got)
	}
	if got := nextThinkingDelta(state, "Answer now"); got != "Answer now" {
		t.Fatalf("unexpected independent state delta %q", got)
	}
}

func TestStreamFinishIdleTimeoutConfig(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "")
	if got := streamFinishIdleTimeout(); got != defaultStreamFinishIdleTimeout {
		t.Fatalf("expected default idle timeout %v, got %v", defaultStreamFinishIdleTimeout, got)
	}
	if defaultStreamFinishIdleTimeout != 1500*time.Millisecond {
		t.Fatalf("expected default idle timeout to be responsive, got %v", defaultStreamFinishIdleTimeout)
	}

	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "0")
	if got := streamFinishIdleTimeout(); got != 0 {
		t.Fatalf("expected disabled idle timeout, got %v", got)
	}

	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "2500")
	if got := streamFinishIdleTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("expected configured idle timeout, got %v", got)
	}
}

func TestStreamFinishIdleRemainingRequiresContent(t *testing.T) {
	if _, ok := streamFinishIdleRemaining("", time.Now(), time.Second); ok {
		t.Fatal("expected empty content to disable finish idle timeout")
	}
	if _, ok := streamFinishIdleRemaining("done", time.Time{}, time.Second); ok {
		t.Fatal("expected missing content timestamp to disable finish idle timeout")
	}
	if _, ok := streamFinishIdleRemaining("done", time.Now(), 0); ok {
		t.Fatal("expected zero timeout to disable finish idle timeout")
	}
	if remaining, ok := streamFinishIdleRemaining("done", time.Now(), time.Second); !ok || remaining <= 0 {
		t.Fatalf("expected positive remaining idle timeout, got remaining=%v ok=%v", remaining, ok)
	}
}

func TestReadStreamChunksCopiesDataAndEnds(t *testing.T) {
	out := make(chan streamReadResult, 2)
	readStreamChunks(context.Background(), strings.NewReader("hello"), out)

	first, ok := <-out
	if !ok {
		t.Fatal("expected first read result")
	}
	if string(first.data) != "hello" || first.err != nil {
		t.Fatalf("unexpected first read result: data=%q err=%v", string(first.data), first.err)
	}

	second, ok := <-out
	if !ok {
		t.Fatal("expected eof read result")
	}
	if len(second.data) != 0 || second.err != io.EOF {
		t.Fatalf("unexpected eof read result: data=%q err=%v", string(second.data), second.err)
	}

	if _, ok := <-out; ok {
		t.Fatal("expected read channel to close")
	}
}

func TestStreamEntryTraceCapturesChunksOnceInOrder(t *testing.T) {
	first := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"11": []interface{}{"Answer now"}, "44": true}},
	)
	second := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"正文开始"}, nil, nil, nil, nil, true},
		}},
	)

	trace := newStreamEntryTrace(20)
	trace.CaptureChunk([]byte(first[:12]), 10*time.Millisecond)
	trace.CaptureChunk([]byte(first[12:]+"\n"), 20*time.Millisecond)
	trace.CaptureChunk([]byte(first+"\n"), 30*time.Millisecond)
	trace.CaptureChunk([]byte(second+"\n"), 40*time.Millisecond)

	if len(trace.records) != 2 {
		t.Fatalf("expected 2 unique records in arrival order, got %#v", trace.records)
	}
	if trace.records[0].Index != 1 || !trace.records[0].Has11 {
		t.Fatalf("unexpected first trace record: %#v", trace.records[0])
	}
	if trace.records[1].Index != 2 || !trace.records[1].HasRC {
		t.Fatalf("unexpected second trace record: %#v", trace.records[1])
	}
}

func TestBuildOrderedEntryRecordsReportsCumulativeContentDeltas(t *testing.T) {
	first := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"你好"}, nil, nil, nil, nil, true},
		},
	})}
	second := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
			[]interface{}{"rc_1", []interface{}{"你好，世界"}, nil, nil, nil, nil, true},
		},
	})}
	topic := []interface{}{"wrb.fr", nil, mustMarshalString(t, []interface{}{
		nil, []interface{}{"c_1", "r_1"}, map[string]interface{}{"11": []interface{}{"话题标题"}, "44": true},
	})}
	raw := []byte(")]}'\n\n" + mustMarshalString(t, []interface{}{first, second, topic}))

	records, err := buildOrderedEntryRecords(raw)
	if err != nil {
		t.Fatalf("buildOrderedEntryRecords returned error: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %#v", records)
	}
	if records[0].ContentTextLen != len("你好") || records[0].ContentDeltaLen != len("你好") {
		t.Fatalf("unexpected first content metrics: %#v", records[0])
	}
	if records[1].ContentTextLen != len("你好，世界") || records[1].ContentDeltaLen != len("，世界") {
		t.Fatalf("unexpected cumulative delta metrics: %#v", records[1])
	}
	if !records[2].Has11 || len(records[2].ThinkingTexts) != 0 {
		t.Fatalf("unexpected topic record: %#v", records[2])
	}

	doc1 := mustMarshalString(t, []interface{}{first})
	doc2 := mustMarshalString(t, []interface{}{second, topic})
	lengthPrefixedRaw := []byte(fmt.Sprintf(")]}'\n\n%d\n%s\n%d\n%s\n", len(doc1), doc1, len(doc2), doc2))
	records, err = buildOrderedEntryRecords(lengthPrefixedRaw)
	if err != nil {
		t.Fatalf("buildOrderedEntryRecords returned error for length-prefixed stream: %v", err)
	}
	if len(records) != 3 || records[1].ContentDeltaLen != len("，世界") || !records[2].Has11 || len(records[2].ThinkingTexts) != 0 {
		t.Fatalf("unexpected length-prefixed records: %#v", records)
	}
}

func TestResolveAvailableModelAllowsSingleDatedAlias(t *testing.T) {
	model, ok := resolveAvailableModel("gemini-3-pro-image-preview", []ModelInfo{
		{ID: "gemini-3-pro-image-preview-11-2025"},
		{ID: "gemini-3.1-flash-image-preview"},
	})

	if !ok {
		t.Fatal("expected model alias to resolve")
	}
	if model != "gemini-3-pro-image-preview-11-2025" {
		t.Fatalf("expected dated model, got %q", model)
	}
}

func buildStreamLine(t *testing.T, payloads ...[]interface{}) string {
	t.Helper()

	entries := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		entryJSON, err := json.Marshal([]interface{}{"wrb.fr", nil, string(payloadJSON)})
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, string(entryJSON))
	}
	return fmt.Sprintf("[%s]", strings.Join(entries, ","))
}

func mustMarshalString(t *testing.T, value interface{}) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func buildCandidateWithThinking(id, content, thinking string) []interface{} {
	candidate := make([]interface{}, 49)
	candidate[0] = id
	candidate[1] = []interface{}{content}
	candidate[37] = []interface{}{[]interface{}{thinking}}
	return candidate
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errAfterReader struct {
	data []byte
	err  error
	done bool
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

func buildGenerateResponse(t *testing.T, text, cid string) string {
	t.Helper()
	if cid == "" {
		cid = "c_next"
	}
	payload := []interface{}{
		nil,
		[]interface{}{cid, "r_next"},
		map[string]interface{}{"21": []interface{}{"next-token"}},
		nil,
		[]interface{}{
			[]interface{}{"rc_next", []interface{}{text}, nil, nil, nil, nil, true},
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	root := []interface{}{
		[]interface{}{nil, nil, string(payloadJSON)},
	}
	rootJSON, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return string(rootJSON)
}

func decodeGenerateInnerFromForm(t *testing.T, formBody string) []interface{} {
	t.Helper()
	values, err := url.ParseQuery(formBody)
	if err != nil {
		t.Fatalf("parse form body: %v", err)
	}
	var outer []interface{}
	if err := json.Unmarshal([]byte(values.Get("f.req")), &outer); err != nil {
		t.Fatalf("decode f.req outer: %v", err)
	}
	if len(outer) < 2 {
		t.Fatalf("unexpected f.req outer: %#v", outer)
	}
	innerJSON, ok := outer[1].(string)
	if !ok {
		t.Fatalf("expected inner JSON string, got %#v", outer[1])
	}
	var inner []interface{}
	if err := json.Unmarshal([]byte(innerJSON), &inner); err != nil {
		t.Fatalf("decode inner: %v", err)
	}
	return inner
}

func TestNormalizeImageURLHandlesGoogleusercontentReferences(t *testing.T) {
	tests := map[string]string{
		"http://googleusercontent.com/image_generation_content/211": "",
		"googleusercontent.com/image_generation_content/211":        "",
		"//lh3.googleusercontent.com/generated-image=w1024-h1024":   "https://lh3.googleusercontent.com/generated-image=w1024-h1024",
	}

	for input, want := range tests {
		if got := normalizeImageURL(input); got != want {
			t.Fatalf("normalizeImageURL(%q): expected %q, got %q", input, want, got)
		}
	}
}
