package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/imroc/req/v3"
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

func TestRedactDebugHeadersRemovesSensitiveValues(t *testing.T) {
	headers := http.Header{}
	headers.Set("Cookie", "SID=secret")
	headers.Set("Authorization", "Bearer secret")
	headers.Set("X-Sync-Token", "secret-token")
	headers.Set("User-Agent", "test-agent")

	redacted := redactDebugHeaders(headers)

	if got := redacted.Get("Cookie"); got != "[REDACTED]" {
		t.Fatalf("expected cookie to be redacted, got %q", got)
	}
	if got := redacted.Get("Authorization"); got != "[REDACTED]" {
		t.Fatalf("expected authorization to be redacted, got %q", got)
	}
	if got := redacted.Get("X-Sync-Token"); got != "[REDACTED]" {
		t.Fatalf("expected token header to be redacted, got %q", got)
	}
	if got := redacted.Get("User-Agent"); got != "test-agent" {
		t.Fatalf("expected user agent to remain, got %q", got)
	}
	if got := headers.Get("Cookie"); got != "SID=secret" {
		t.Fatalf("expected original headers to remain untouched, got %q", got)
	}
}

func TestSelectStartupPSIDTSPreservesExplicitEnvValue(t *testing.T) {
	selected, source, clearCache := selectStartupPSIDTS("old-config-ts", "env", "fresh-cached-ts", nil)

	if selected != "old-config-ts" {
		t.Fatalf("expected explicit env PSIDTS to win, got %q", selected)
	}
	if source != "env" {
		t.Fatalf("expected env source, got %q", source)
	}
	if clearCache {
		t.Fatal("explicit config should not clear the runtime cache")
	}
}

func TestSelectStartupPSIDTSPreservesAccountCacheOverLegacyCache(t *testing.T) {
	selected, source, clearCache := selectStartupPSIDTS("account-cache-ts", "cache", "legacy-cache-ts", nil)

	if selected != "account-cache-ts" {
		t.Fatalf("expected account cache PSIDTS to win, got %q", selected)
	}
	if source != "cache" {
		t.Fatalf("expected account cache source, got %q", source)
	}
	if clearCache {
		t.Fatal("account cache should not clear legacy cache")
	}
}

func TestBuildGenerateInnerIncludesConversationMetadataAndContextToken(t *testing.T) {
		metadata := []interface{}{"c_1", "r_1", "rc_1", nil, nil, nil, nil, nil, nil, ""}
		inner := buildGenerateInner("hello", nil, "en", "request-id", modelIDFlash, metadata, "opaque-token")

		if len(inner) != 92 {
			t.Fatalf("expected live 92-element inner, got %d", len(inner))
		}
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

	func TestBuildGenerateInnerMatchesLiveWebShape(t *testing.T) {
		flash := buildGenerateInner("hi", nil, "en", "req-flash", modelIDFlash, nil, nil)
		lite := buildGenerateInner("hi", nil, "en", "req-lite", modelIDFlashLite, nil, nil)
		pro := buildGenerateInner("hi", nil, "en", "req-pro", modelIDPro, nil, nil)

		for name, inner := range map[string][]interface{}{"flash": flash, "lite": lite, "pro": pro} {
			if len(inner) != 92 {
				t.Fatalf("%s: expected 92 elements, got %d", name, len(inner))
			}
			if got, ok := inner[6].([]interface{}); !ok || len(got) != 1 || got[0] != 1 {
				t.Fatalf("%s: expected inner[6]=[1], got %#v", name, inner[6])
			}
			if got, ok := inner[61].([]interface{}); !ok || len(got) != 1 || got[0] != 1 {
				t.Fatalf("%s: expected inner[61]=[1], got %#v", name, inner[61])
			}
			if inner[68] != 2 {
				t.Fatalf("%s: expected inner[68]=2, got %#v", name, inner[68])
			}
			if inner[80] != 1 {
				t.Fatalf("%s: expected inner[80]=1, got %#v", name, inner[80])
			}
			if inner[91] != 0 {
				t.Fatalf("%s: expected inner[91]=0, got %#v", name, inner[91])
			}
		}

		if flash[79] != 1 {
			t.Fatalf("expected flash mode 1 at inner[79], got %#v", flash[79])
		}
		if lite[79] != 6 {
			t.Fatalf("expected lite mode 6 at inner[79], got %#v", lite[79])
		}
		if pro[79] != 3 {
			t.Fatalf("expected pro mode 3 at inner[79], got %#v", pro[79])
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

func TestGenerateContentStreamForOpenAIReturnsErrorForEmptyParsedStream(t *testing.T) {
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(")]}'\n\n")),
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

	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool {
		t.Fatalf("expected no stream events, got %#v", event)
		return false
	}, WithModel("gemini-3.5-flash"))
	if err == nil || !strings.Contains(err.Error(), "completed without parsed content") {
		t.Fatalf("expected empty parsed stream error, got %v", err)
	}
}

func TestResolveModelsExposesOnlyUIModelAliases(t *testing.T) {
	_, aliases, canonical := resolveModels([]ModelInfo{
		{ID: modelIDFlashLite, Created: time.Now().Unix(), OwnedBy: "google", Provider: "gemini"},
		{ID: modelIDFlash, Created: time.Now().Unix(), OwnedBy: "google", Provider: "gemini"},
		{ID: modelIDPro, Created: time.Now().Unix(), OwnedBy: "google", Provider: "gemini"},
	})

	want := map[string]string{
		"gemini-3.5-flash-lite": "8c46e95b1a07cecc",
		"gemini-3.1-flash-lite": "8c46e95b1a07cecc", // backward-compatible
		"gemini-3.6-flash":      "56fdd199312815e2",
		"gemini-3.5-flash":      "56fdd199312815e2", // backward-compatible
		"gemini-3.1-pro":        "e6fa609c3fa255c0",
	}
	for alias, id := range want {
		if aliases[alias] != id {
			t.Fatalf("expected %q to resolve to %q, got %q", alias, id, aliases[alias])
		}
	}
	if len(aliases) != len(want) {
		t.Fatalf("expected only UI aliases, got %#v", aliases)
	}

	canonicalWant := map[string]bool{
		"gemini-3.5-flash-lite": true,
		"gemini-3.6-flash":      true,
		"gemini-3.1-pro":        true,
	}
	for name := range canonicalWant {
		if !canonical[name] {
			t.Fatalf("expected %q to be canonical", name)
		}
	}
	for name := range canonical {
		if !canonicalWant[name] {
			t.Fatalf("unexpected canonical model: %q", name)
		}
	}
}

func TestSetGenerationHeadersUsesModelSpecificMode(t *testing.T) {
	flashReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(flashReq, modelIDFlash, "flash-req", "")

	liteReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(liteReq, modelIDFlashLite, "lite-req", "")

	proReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(proReq, modelIDPro, "pro-req", "")

	var flashHeader []interface{}
	if err := json.Unmarshal([]byte(flashReq.Header.Get("x-goog-ext-525001261-jspb")), &flashHeader); err != nil {
		t.Fatalf("unmarshal flash header: %v", err)
	}
	var liteHeader []interface{}
	if err := json.Unmarshal([]byte(liteReq.Header.Get("x-goog-ext-525001261-jspb")), &liteHeader); err != nil {
		t.Fatalf("unmarshal lite header: %v", err)
	}
	var proHeader []interface{}
	if err := json.Unmarshal([]byte(proReq.Header.Get("x-goog-ext-525001261-jspb")), &proHeader); err != nil {
		t.Fatalf("unmarshal pro header: %v", err)
	}

	if got := int(flashHeader[7].(float64)); got != 0 {
		t.Fatalf("expected header index 7 = 0, got %d", got)
	}
	if got := int(flashHeader[11].(float64)); got != 2 {
		t.Fatalf("expected header index 11 = 2, got %d", got)
	}
	if got := int(flashHeader[14].(float64)); got != 1 {
		t.Fatalf("expected flash mode 1, got %d", got)
	}
	if got := int(liteHeader[14].(float64)); got != 6 {
		t.Fatalf("expected lite mode 6, got %d", got)
	}
	if got := int(proHeader[14].(float64)); got != 3 {
		t.Fatalf("expected pro mode 3, got %d", got)
	}
	if got := proHeader[4].(string); got != modelIDPro {
		t.Fatalf("expected pro model id in header, got %q", got)
	}
}

func TestSetGenerationHeadersUsesThinkingLevel(t *testing.T) {
	standardReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(standardReq, modelIDFlash, "standard-req", "standard")

	extendedReq, err := http.NewRequest(http.MethodPost, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	setGenerationHeaders(extendedReq, modelIDFlash, "extended-req", "extended")

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

func TestRedactProxyURL(t *testing.T) {
	got := redactProxyURL("http://user:pass@127.0.0.1:8038")
	if got != "http://***@127.0.0.1:8038" {
		t.Fatalf("unexpected redacted proxy: %q", got)
	}
	if got := redactProxyURL("http://127.0.0.1:8038"); got != "http://127.0.0.1:8038" {
		t.Fatalf("unexpected plain proxy: %q", got)
	}
	if got := redactProxyURL(""); got != "" {
		t.Fatalf("expected empty proxy, got %q", got)
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

func TestExtractStreamStateReadsCurrentThinkingSummaryWithoutStructuredMetadata(t *testing.T) {
	currentSummary := "**Analyzing the Goal**\n\nI've clarified the task and selected a proof strategy."
	candidate := buildCandidateWithThinking("rc_1", "", currentSummary)
	candidate[37] = []interface{}{
		[]interface{}{currentSummary},
		[]interface{}{
			[]interface{}{"Analyzing the Goal", "Structured UI description must not be emitted as reasoning."},
		},
	}
	line := buildStreamLine(t,
		[]interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{candidate}},
	)

	state := ExtractStreamState([]byte(line))
	if !containsString(state.ThinkingTexts, currentSummary) {
		t.Fatalf("expected current thinking summary in %#v", state.ThinkingTexts)
	}
	for _, unexpected := range []string{"Analyzing the Goal", "Structured UI description must not be emitted as reasoning."} {
		if containsString(state.ThinkingTexts, unexpected) {
			t.Fatalf("structured metadata %q leaked into thinking: %#v", unexpected, state.ThinkingTexts)
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
	if defaultStreamFinishIdleTimeout != 0 {
		t.Fatalf("expected idle timeout disabled by default, got %v", defaultStreamFinishIdleTimeout)
	}

	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "0")
	if got := streamFinishIdleTimeout(); got != 0 {
		t.Fatalf("expected disabled idle timeout, got %v", got)
	}

	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "2500")
	if got := streamFinishIdleTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("expected configured idle timeout without an implicit clamp, got %v", got)
	}
	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "16000")
	if got := streamFinishIdleTimeout(); got != 16*time.Second {
		t.Fatalf("expected configured idle timeout, got %v", got)
	}
}

func TestStreamFirstActivityTimeoutConfig(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "")
	t.Setenv("GEMINI_STREAM_FIRST_CONTENT_TIMEOUT_MS", "")
	if got := streamFirstActivityTimeout(); got != defaultStreamFirstActivityTimeout {
		t.Fatalf("expected default first activity timeout %v, got %v", defaultStreamFirstActivityTimeout, got)
	}
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "0")
	if got := streamFirstActivityTimeout(); got != 0 {
		t.Fatalf("expected disabled first activity timeout, got %v", got)
	}
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "2500")
	if got := streamFirstActivityTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("expected configured first activity timeout, got %v", got)
	}
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "")
	t.Setenv("GEMINI_STREAM_FIRST_CONTENT_TIMEOUT_MS", "3000")
	if got := streamFirstActivityTimeout(); got != 3*time.Second {
		t.Fatalf("expected legacy first content timeout fallback, got %v", got)
	}
}

func TestStreamProgressIdleTimeoutConfig(t *testing.T) {
	t.Setenv("GEMINI_STREAM_PROGRESS_IDLE_TIMEOUT_MS", "")
	if got := streamProgressIdleTimeout(); got != defaultStreamProgressIdleTimeout {
		t.Fatalf("expected default progress idle timeout %v, got %v", defaultStreamProgressIdleTimeout, got)
	}
	t.Setenv("GEMINI_STREAM_PROGRESS_IDLE_TIMEOUT_MS", "0")
	if got := streamProgressIdleTimeout(); got != 0 {
		t.Fatalf("expected disabled progress idle timeout, got %v", got)
	}
	t.Setenv("GEMINI_STREAM_PROGRESS_IDLE_TIMEOUT_MS", "2500")
	if got := streamProgressIdleTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("expected configured progress idle timeout, got %v", got)
	}
}

func TestGenerateContentStreamForOpenAITimesOutBeforeFirstActivity(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "1")
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(&slowEOFReader{delay: 50 * time.Millisecond}),
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

	start := time.Now()
	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool {
		t.Fatalf("expected no stream events, got %#v", event)
		return false
	}, WithModel("gemini-3.5-flash"))
	if err == nil || !strings.Contains(err.Error(), "upstream result unknown") || !strings.Contains(err.Error(), "first activity timeout") {
		t.Fatalf("expected first activity timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected fast timeout, took %v", elapsed)
	}
}

func TestGenerateContentStreamForOpenAIReasoningCancelsFirstActivityTimeout(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "10")
	reader, writer := io.Pipe()
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
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
		maxRetries:    1,
	}
	thinkingLine := buildStreamLine(t, []interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
		buildCandidateWithThinking("rc_1", "", "正在分析问题"),
	}})
	contentLine := buildStreamLine(t, []interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
		buildCandidateWithThinking("rc_1", "完成", "正在分析问题"),
	}})
	go func() {
		_, _ = io.WriteString(writer, thinkingLine+"\n")
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(writer, contentLine+"\n")
		_ = writer.Close()
	}()

	var reasoning, content strings.Builder
	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool {
		switch event.Kind {
		case "thinking_text":
			reasoning.WriteString(event.Delta)
		case "content_delta":
			content.WriteString(event.Delta)
		}
		return true
	}, WithModel("gemini-3.5-flash"))
	if err != nil {
		t.Fatalf("reasoning should keep the stream alive: %v", err)
	}
	if reasoning.String() == "" || content.String() != "完成" {
		t.Fatalf("unexpected events: reasoning=%q content=%q", reasoning.String(), content.String())
	}
}

func TestGenerateContentStreamForOpenAIDoesNotRetryAfterReasoning(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "100")
	thinkingLine := buildStreamLine(t, []interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
		buildCandidateWithThinking("rc_1", "", "已经向客户端输出的思考"),
	}})
	calls := 0
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(&errAfterReader{data: []byte(thinkingLine + "\n"), err: io.ErrUnexpectedEOF}),
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
		maxRetries:    3,
	}
	reasoningEvents := 0
	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool {
		if event.Kind == "thinking_text" {
			reasoningEvents++
		}
		return true
	}, WithModel("gemini-3.5-flash"))
	if err == nil {
		t.Fatal("expected interrupted stream error")
	}
	if calls != 1 || reasoningEvents != 1 {
		t.Fatalf("expected no transparent retry after reasoning, calls=%d reasoning_events=%d", calls, reasoningEvents)
	}
}

func TestGenerateContentStreamForOpenAITimesOutWhenReasoningStalls(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "100")
	t.Setenv("GEMINI_STREAM_PROGRESS_IDLE_TIMEOUT_MS", "5")
	reader, writer := io.Pipe()
	defer writer.Close()
	calls := 0
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
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
		maxRetries:    3,
	}
	thinkingLine := buildStreamLine(t, []interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
		buildCandidateWithThinking("rc_1", "", "开始思考后上游停住"),
	}})
	go func() {
		_, _ = io.WriteString(writer, thinkingLine+"\n")
	}()

	reasoningEvents := 0
	start := time.Now()
	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(event StreamEvent) bool {
		if event.Kind == "thinking_text" {
			reasoningEvents++
		}
		return true
	}, WithModel("gemini-3.5-flash"))
	if err == nil || !strings.Contains(err.Error(), "upstream result unknown") || !strings.Contains(err.Error(), "progress idle timeout") {
		t.Fatalf("expected progress idle timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected fast progress idle timeout, took %v", elapsed)
	}
	if calls != 1 || reasoningEvents != 1 {
		t.Fatalf("expected one request and one reasoning event, calls=%d reasoning_events=%d", calls, reasoningEvents)
	}
}

func TestGenerateContentStreamForOpenAIRetriesDefinitelyUnsentRequestOnce(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "0")
	contentLine := buildStreamLine(t, []interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
		buildCandidateWithThinking("rc_1", "重试成功", ""),
	}})
	terminalLine := `[["e",17,null,null,1234]]`
	calls := 0
	client := newStreamTestClient(3, func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("dial tcp: connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(contentLine + "\n" + terminalLine + "\n")), Request: req}, nil
	})
	ctx, timings := ContextWithStreamTimings(context.Background())
	var content strings.Builder
	err := client.GenerateContentStreamForOpenAI(ctx, "你好", func(event StreamEvent) bool {
		if event.Kind == "content_delta" {
			content.WriteString(event.Delta)
		}
		return true
	}, WithModel("gemini-3.5-flash"))
	if err != nil {
		t.Fatalf("expected retry to recover the stream: %v", err)
	}
	if calls != 2 || content.String() != "重试成功" {
		t.Fatalf("unexpected retry result: calls=%d content=%q", calls, content.String())
	}
	snapshot := timings.Snapshot()
	if snapshot.RetryCount != 1 || snapshot.CompletionSource != "terminal_entry" {
		t.Fatalf("unexpected timing snapshot: %#v", snapshot)
	}
}

func TestGenerateContentStreamForOpenAIDoesNotRetryTransportErrorAfterWrite(t *testing.T) {
	calls := 0
	client := newStreamTestClient(3, func(req *http.Request) (*http.Response, error) {
		calls++
		if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteRequest != nil {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		return nil, errors.New("connection reset after request write")
	})
	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(StreamEvent) bool {
		return true
	}, WithModel("gemini-3.5-flash"))
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("expected transport error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("request written to upstream must not retry, got %d calls", calls)
	}
}

func TestGenerateContentStreamForOpenAIDoesNotRetryConversationTimeout(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "1")
	calls := 0
	client := newStreamTestClient(3, func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(&slowEOFReader{delay: 50 * time.Millisecond}),
			Request:    req,
		}, nil
	})
	err := client.GenerateContentStreamForOpenAI(context.Background(), "继续", func(StreamEvent) bool {
		return true
	}, WithModel("gemini-3.5-flash"), WithConversationID("existing-thread"))
	if err == nil || !strings.Contains(err.Error(), "upstream result unknown") || !strings.Contains(err.Error(), "first activity timeout") {
		t.Fatalf("expected first activity timeout, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected no transparent retry for a conversation, got %d calls", calls)
	}
}

func TestGenerateContentStreamForOpenAIDoesNotRetryNewConversationTimeout(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "1")
	calls := 0
	client := newStreamTestClient(3, func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(&slowEOFReader{delay: 50 * time.Millisecond}),
			Request:    req,
		}, nil
	})
	err := client.GenerateContentStreamForOpenAI(context.Background(), "你好", func(StreamEvent) bool {
		return true
	}, WithModel("gemini-3.5-flash"))
	if err == nil || !strings.Contains(err.Error(), "first activity timeout") {
		t.Fatalf("expected first activity timeout, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("timeout after dispatch must not retry, got %d calls", calls)
	}
}

func TestGenerateContentStreamForOpenAIStopsAtTerminalEntry(t *testing.T) {
	t.Setenv("GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS", "100")
	t.Setenv("GEMINI_STREAM_FINISH_IDLE_MS", "0")
	reader, writer := io.Pipe()
	defer writer.Close()
	client := newStreamTestClient(1, func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
	})
	contentLine := buildStreamLine(t, []interface{}{nil, []interface{}{"c_1", "r_1"}, nil, nil, []interface{}{
		buildCandidateWithThinking("rc_1", "立即结束", ""),
	}})
	go func() {
		_, _ = io.WriteString(writer, contentLine+"\n"+`[["e",17,null,null,1234]]`+"\n")
	}()
	ctx, timings := ContextWithStreamTimings(context.Background())
	start := time.Now()
	err := client.GenerateContentStreamForOpenAI(ctx, "你好", func(StreamEvent) bool { return true }, WithModel("gemini-3.5-flash"))
	if err != nil {
		t.Fatalf("expected terminal entry success: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expected terminal entry to close promptly, took %v", elapsed)
	}
	if snapshot := timings.Snapshot(); snapshot.CompletionSource != "terminal_entry" {
		t.Fatalf("expected terminal completion source, got %#v", snapshot)
	}
}

func TestHasStreamTerminalEntry(t *testing.T) {
	raw := "137\n" + buildStreamLine(t, []interface{}{nil, []interface{}{"c", "r"}, map[string]interface{}{"44": true}}) + "\n28\n" + `[["e",17,null,null,1234]]`
	if !hasStreamTerminalEntry([]byte(raw)) {
		t.Fatal("expected transport terminal entry to be detected")
	}
	if hasStreamTerminalEntry([]byte(buildStreamLine(t, []interface{}{nil, []interface{}{"c", "r"}, map[string]interface{}{"44": true}}))) {
		t.Fatal("ordinary wrb.fr metadata must not be treated as terminal")
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

func newStreamTestClient(maxRetries int, transport roundTripFunc) *Client {
	return &Client{
		rawHTTPClient: &http.Client{Transport: transport},
		at:            "test-at",
		cookieHeader:  "SID=test",
		buildLabel:    "test-build",
		sessionID:     "test-session",
		language:      "zh-CN",
		log:           zap.NewNop(),
		cachedModels:  []ModelInfo{{ID: "fbb127bbb056c959"}},
		cachedAliases: map[string]string{"gemini-3.5-flash": "fbb127bbb056c959"},
		conversations: make(map[string]*SessionMetadata),
		maxRetries:    maxRetries,
	}
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

type slowEOFReader struct {
	delay time.Duration
	done  bool
}

func (r *slowEOFReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	time.Sleep(r.delay)
	return 0, io.EOF
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

func TestCheckConversationContinuityMarksUntrustedOnMismatch(t *testing.T) {
	client := &Client{
		log:                   zap.NewNop(),
		conversations:         make(map[string]*SessionMetadata),
		conversationSeen:      make(map[string]time.Time),
		conversationUntrusted: make(map[string]bool),
	}
	metadata := map[string]any{"cid": "c_actual_different"}

	// contentAlreadyEmitted == true is the dangerous "silent mismatch" path:
	// content was already streamed to the client, but the Gemini-side record
	// diverged. The call returns nil (cannot undo emitted bytes) but must still
	// flag the conversation as untrusted so the next turn does not trust it.
	err := client.checkConversationContinuity("thread-x", "c_expected", metadata, true)
	if err != nil {
		t.Fatalf("expected nil error when content already emitted, got %v", err)
	}
	if !client.IsConversationUntrusted("thread-x") {
		t.Fatal("expected conversation flagged untrusted after silent continuity mismatch")
	}

	// When content was NOT emitted, the error is returned AND it is flagged.
	other := map[string]any{"cid": "c_other"}
	err = client.checkConversationContinuity("thread-y", "c_expected_y", other, false)
	if err == nil {
		t.Fatal("expected continuity mismatch error when content not yet emitted")
	}
	if !client.IsConversationUntrusted("thread-y") {
		t.Fatal("expected thread-y flagged untrusted after mismatch")
	}
}

func TestCheckConversationContinuityDoesNotFlagMatchingCid(t *testing.T) {
	client := &Client{
		log:                   zap.NewNop(),
		conversations:         make(map[string]*SessionMetadata),
		conversationSeen:      make(map[string]time.Time),
		conversationUntrusted: make(map[string]bool),
	}
	metadata := map[string]any{"cid": "c_same"}
	if err := client.checkConversationContinuity("thread-z", "c_same", metadata, false); err != nil {
		t.Fatalf("expected nil error for matching cid, got %v", err)
	}
	if client.IsConversationUntrusted("thread-z") {
		t.Fatal("expected conversation NOT flagged untrusted when cid matches")
	}
}

type trackingReadCloser struct {
	io.Reader
	mu     sync.Mutex
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	return nil
}

func (r *trackingReadCloser) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func TestGenerateContentStreamClosesBodyWhenConsumerStops(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader("chunk")}
	client := &Client{
		rawHTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
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
		maxRetries:    1,
	}

	err := client.generateContentStreamInternal(context.Background(), "hello", func([]byte, string) streamVisitResult {
		return streamVisitResult{}
	}, WithModel("gemini-3.5-flash"))
	if err != nil {
		t.Fatal(err)
	}
	if !body.isClosed() {
		t.Fatal("expected upstream response body to be closed when consumer stops")
	}
}

func TestStreamIncrementalParserHandlesSplitLines(t *testing.T) {
	first := buildStreamLine(t, []interface{}{nil, []interface{}{"c", "r"}, nil, nil, []interface{}{[]interface{}{"rc", []interface{}{"Hello"}, nil, nil, nil, nil, true}}})
	second := buildStreamLine(t, []interface{}{nil, []interface{}{"c", "r"}, nil, nil, []interface{}{[]interface{}{"rc", []interface{}{"Hello world"}, nil, nil, nil, nil, true}}})
	data := []byte(first + "\n" + second + "\n")
	parser := &streamIncrementalParser{}
	var deltas []string
	for _, chunk := range [][]byte{data[:7], data[7:31], data[31:]} {
		if !parser.Feed(chunk, false, func(_ []byte, delta string) bool {
			if delta != "" {
				deltas = append(deltas, delta)
			}
			return true
		}) {
			t.Fatal("unexpected stop")
		}
	}
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Fatalf("expected cumulative text, got %q (%#v)", got, deltas)
	}
}

func TestRotateCookiesContextStopsDuringBackoff(t *testing.T) {
	client := &Client{
		accountID:  "test",
		proxyURL:   "http://127.0.0.1:1",
		cookies:    &CookieStore{Secure1PSID: "bad"},
		httpClient: req.NewClient(),
		log:        zap.NewNop(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := client.RotateCookiesContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("canceled rotation took too long: %v", time.Since(start))
	}
}

func TestRotateCookiesRequiresReplacementPSIDTS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousEndpoint := endpointRotateCookies
	endpointRotateCookies = server.URL
	defer func() { endpointRotateCookies = previousEndpoint }()

	client := &Client{
		accountID: "test",
		cookies: &CookieStore{
			Secure1PSID:   "existing-psid",
			Secure1PSIDTS: "stale-ts",
		},
		httpClient: req.NewClient(),
		log:        zap.NewNop(),
	}
	err := client.rotateCookiesOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "without issuing a replacement") {
		t.Fatalf("expected missing replacement error, got %v", err)
	}
}

func TestIncrementalParserReplaysSanitizedRealStreamFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/stream_fixture.raw.txt")
	if err != nil {
		t.Fatal(err)
	}
	wantText := extractStreamTextFromBuffer(data)
	wantMetadata := extractConversationMetadataFromBuffer(data)
	if wantText != "你好，世界" || wantMetadata["cid"] != "c_fixture" {
		t.Fatalf("invalid fixture: text=%q metadata=%#v", wantText, wantMetadata)
	}

	for _, chunkSize := range []int{1, 7, 31, 1024} {
		parser := &streamIncrementalParser{}
		var text strings.Builder
		metadata := map[string]any{}
		for start := 0; start < len(data); start += chunkSize {
			end := start + chunkSize
			if end > len(data) {
				end = len(data)
			}
			ok := parser.Feed(data[start:end], end == len(data), func(entry []byte, delta string) bool {
				text.WriteString(delta)
				metadata = mergeConversationMetadata(metadata, extractConversationMetadataFromBuffer(entry))
				return true
			})
			if !ok {
				t.Fatalf("chunk size %d stopped unexpectedly", chunkSize)
			}
		}
		if text.String() != wantText {
			t.Fatalf("chunk size %d: got text %q, want %q", chunkSize, text.String(), wantText)
		}
		if metadata["cid"] != wantMetadata["cid"] || metadata["context_token"] != wantMetadata["context_token"] {
			t.Fatalf("chunk size %d: metadata=%#v want=%#v", chunkSize, metadata, wantMetadata)
		}
	}
}

func TestAppendStreamTailCapsMemory(t *testing.T) {
	var buf bytes.Buffer
	appendStreamTail(&buf, bytes.Repeat([]byte("a"), maxStreamTailBytes))
	appendStreamTail(&buf, []byte("tail"))
	if buf.Len() != maxStreamTailBytes {
		t.Fatalf("got %d", buf.Len())
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("tail")) {
		t.Fatal("tail missing")
	}
}

func TestUpdateCookiesRejectsReplacementPSIDWithoutMatchingPSIDTS(t *testing.T) {
	client := &Client{cookies: &CookieStore{Secure1PSID: "old-psid", Secure1PSIDTS: "old-ts"}}
	err := client.UpdateCookies(context.Background(), "new-psid", "")
	if err == nil || !strings.Contains(err.Error(), "must be updated together") {
		t.Fatalf("expected mismatched pair rejection, got %v", err)
	}
	got := client.GetCookies()
	if got.Secure1PSID != "old-psid" || got.Secure1PSIDTS != "old-ts" {
		t.Fatalf("failed update changed active pair: %#v", got)
	}
}

func TestParseResponseKeepsMostCompleteCandidate(t *testing.T) {
	complete := "complete answer with nonce E2E-MULTI-TEST"
	partial := "complete answer with nonce"
	client := &Client{log: zap.NewNop()}
	resp, err := client.parseResponse(buildGenerateResponse(t, complete, "cid") + "\n" + buildGenerateResponse(t, partial, "cid"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != complete {
		t.Fatalf("expected complete candidate %q, got %q", complete, resp.Text)
	}
}

func TestGoogleChallengeErrorClassifiesSorryRedirect(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "https://gemini.google.com/app?hl=en", nil)
	resp := &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://www.google.com/sorry/index?continue=x"}}, Request: request}
	err := googleChallengeError(resp)
	if err == nil || !strings.Contains(err.Error(), "proxy egress IP") {
		t.Fatalf("expected actionable Google challenge error, got %v", err)
	}
}

func TestGeminiHTTPClientsStopAtRedirectResponse(t *testing.T) {
	client := &Client{}
	httpClient := client.newHTTPClient(time.Second)
	request, _ := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err := httpClient.CheckRedirect(request, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("expected redirect policy to stop, got %v", err)
	}
}

func TestUpdateCookiesStopsOnGoogleChallengeAndRollsBack(t *testing.T) {
	calls := 0
	client := &Client{
		accountID:  "test",
		cookies:    &CookieStore{Secure1PSID: "old-psid", Secure1PSIDTS: "old-ts"},
		httpClient: req.NewClient(),
		rawHTTPClient: &http.Client{
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://www.google.com/sorry/index?continue=x"}}, Body: io.NopCloser(strings.NewReader("challenge")), Request: request}, nil
			}),
			CheckRedirect: stopGoogleRedirects,
		},
		log: zap.NewNop(),
	}
	err := client.UpdateCookies(context.Background(), "new-psid", "new-ts")
	if err == nil || !strings.Contains(err.Error(), "proxy egress IP") {
		t.Fatalf("expected actionable proxy challenge error, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected one request per validation step, not a redirect loop; got %d requests", calls)
	}
	if got := client.GetCookies(); got.Secure1PSID != "old-psid" || got.Secure1PSIDTS != "old-ts" {
		t.Fatalf("expected failed update to roll back cookies, got %#v", got)
	}
}
