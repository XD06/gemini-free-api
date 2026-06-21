package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Stream         bool          `json:"stream"`
	Messages       []chatMessage `json:"messages"`
	ConversationID string        `json:"conversation_id,omitempty"`
	StreamOptions  *streamOpts   `json:"stream_options,omitempty"`
	Tools          []toolDef     `json:"tools,omitempty"`
	ToolChoice     interface{}   `json:"tool_choice,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type toolDef struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type adminAccountsResponse struct {
	Accounts []accountStatus `json:"accounts"`
}

type accountStatus struct {
	ID             string    `json:"id"`
	Healthy        bool      `json:"healthy"`
	State          string    `json:"state"`
	CookieSource   string    `json:"cookie_source,omitempty"`
	Active         bool      `json:"active,omitempty"`
	ActiveUntil    time.Time `json:"active_until,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastCookieSync time.Time `json:"last_cookie_sync,omitempty"`
}

type report struct {
	StartedAt time.Time        `json:"started_at"`
	BaseURL   string           `json:"base_url"`
	Model     string           `json:"model"`
	Scenarios []scenarioResult `json:"scenarios"`
	Passed    bool             `json:"passed"`
}

type scenarioResult struct {
	Name     string                 `json:"name"`
	Passed   bool                   `json:"passed"`
	Duration string                 `json:"duration"`
	Error    string                 `json:"error,omitempty"`
	Details  map[string]interface{} `json:"details,omitempty"`
}

type runner struct {
	baseURL string
	token   string
	model   string
	client  *http.Client
}

func main() {
	var scenariosCSV string
	var reportDir string
	var baseURL string
	var token string
	var model string
	var invalidAccount string
	var rotationWait time.Duration
	var timeout time.Duration

	flag.StringVar(&baseURL, "base-url", "http://127.0.0.1:8787", "API base URL")
	flag.StringVar(&token, "admin-token", os.Getenv("COOKIE_SYNC_TOKEN"), "admin token for /admin/*")
	flag.StringVar(&model, "model", "gemini-3.5-flash", "model name")
	flag.StringVar(&scenariosCSV, "scenarios", "status,multiturn,stream,tool,bom,negative-cookie", "comma-separated scenarios: status,multiturn,truncated-history,stream,tool,bom,negative-cookie,rotation,audit-explicit")
	flag.StringVar(&reportDir, "report-dir", "scratch/e2e-reports", "directory for JSON reports")
	flag.StringVar(&invalidAccount, "invalid-account", "acc2", "account used by negative-cookie scenario")
	flag.DurationVar(&rotationWait, "rotation-wait", 75*time.Second, "wait duration for rotation scenario")
	flag.DurationVar(&timeout, "timeout", 3*time.Minute, "HTTP timeout per request")
	flag.Parse()
	if strings.TrimSpace(token) == "" {
		token = readDotEnvValue("COOKIE_SYNC_TOKEN")
	}

	r := &runner{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   strings.TrimSpace(token),
		model:   model,
		client:  &http.Client{Timeout: timeout},
	}
	rep := report{
		StartedAt: time.Now(),
		BaseURL:   r.baseURL,
		Model:     model,
		Passed:    true,
	}

	for _, name := range splitCSV(scenariosCSV) {
		start := time.Now()
		result := scenarioResult{Name: name, Passed: true, Details: map[string]interface{}{}}
		err := runScenario(context.Background(), r, name, invalidAccount, rotationWait, result.Details)
		result.Duration = time.Since(start).String()
		if err != nil {
			result.Passed = false
			result.Error = err.Error()
			rep.Passed = false
		}
		rep.Scenarios = append(rep.Scenarios, result)
	}

	if err := os.MkdirAll(reportDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "create report dir: %v\n", err)
		os.Exit(2)
	}
	reportPath := filepath.Join(reportDir, time.Now().Format("20060102_150405")+"_e2e_report.json")
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(reportPath, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "write report: %v\n", err)
		os.Exit(2)
	}
	fmt.Println(string(data))
	fmt.Println("report:", reportPath)
	if !rep.Passed {
		os.Exit(1)
	}
}

func runScenario(ctx context.Context, r *runner, name, invalidAccount string, rotationWait time.Duration, details map[string]interface{}) error {
	switch name {
	case "status":
		return r.scenarioStatus(ctx, details)
	case "multiturn":
		return r.scenarioMultiTurn(ctx, details)
	case "truncated-history":
		return r.scenarioTruncatedHistory(ctx, details)
	case "stream":
		return r.scenarioStream(ctx, details)
	case "tool":
		return r.scenarioToolCalling(ctx, details)
	case "bom":
		return r.scenarioBOM(ctx, details)
	case "negative-cookie":
		return r.scenarioNegativeCookie(ctx, invalidAccount, details)
	case "rotation":
		return r.scenarioRotation(ctx, rotationWait, details)
	case "audit-explicit":
		return r.scenarioAuditExplicit(ctx, details)
	default:
		return fmt.Errorf("unknown scenario %q", name)
	}
}

func (r *runner) scenarioStatus(ctx context.Context, details map[string]interface{}) error {
	accounts, err := r.listAccounts(ctx)
	if err != nil {
		return err
	}
	details["accounts"] = accounts
	for _, account := range accounts {
		if !account.Healthy || account.State != "healthy" {
			return fmt.Errorf("account %s is not healthy: state=%s error=%s", account.ID, account.State, account.LastError)
		}
	}
	return nil
}

func (r *runner) scenarioMultiTurn(ctx context.Context, details map[string]interface{}) error {
	nonce := "E2E-MULTI-" + stamp()
	messages := []chatMessage{{
		Role:    "user",
		Content: "多轮真实客户端测试。请记住唯一标记 " + nonce + "，并用约180字说明代理连接池的作用，最后原样写标记。",
	}}
	a1, err := r.chat(ctx, messages, false, false, "")
	if err != nil {
		return err
	}
	messages = append(messages, chatMessage{Role: "assistant", Content: a1})
	messages = append(messages, chatMessage{
		Role:    "user",
		Content: "第二轮：根据你记住的内容回答唯一标记是什么，再用约160字说明 keep-alive 为什么能降低延迟。",
	})
	a2, err := r.chat(ctx, messages, false, false, "")
	if err != nil {
		return err
	}
	messages = append(messages, chatMessage{Role: "assistant", Content: a2})
	messages = append(messages, chatMessage{
		Role:    "user",
		Content: "第三轮：列出3个连接复用失败的原因，并再次写出唯一标记。",
	})
	a3, err := r.chat(ctx, messages, false, false, "")
	if err != nil {
		return err
	}
	details["nonce"] = nonce
	details["turn_lengths"] = []int{len(a1), len(a2), len(a3)}
	details["contains_nonce"] = []bool{strings.Contains(a1, nonce), strings.Contains(a2, nonce), strings.Contains(a3, nonce)}
	details["turn3_sample"] = preview(a3, 500)
	if !strings.Contains(a1, nonce) || !strings.Contains(a2, nonce) || !strings.Contains(a3, nonce) {
		return errors.New("multi-turn conversation lost nonce")
	}
	return nil
}

func (r *runner) scenarioTruncatedHistory(ctx context.Context, details map[string]interface{}) error {
	nonce := "E2E-TRUNC-" + stamp()
	messages := []chatMessage{{
		Role:    "user",
		Content: "裁剪历史测试第一轮。请记住唯一标记 " + nonce + "，回复时必须写出这个标记。",
	}}
	for turn := 1; turn <= 4; turn++ {
		answer, err := r.chat(ctx, messages, false, false, "")
		if err != nil {
			return err
		}
		messages = append(messages, chatMessage{Role: "assistant", Content: answer})
		if turn < 4 {
			messages = append(messages, chatMessage{
				Role:    "user",
				Content: fmt.Sprintf("第%d轮继续：围绕连接池、keep-alive、超时、代理账号绑定各说80字，并再次写出唯一标记。", turn+1),
			})
		}
	}

	if len(messages) < 4 {
		return errors.New("internal test setup produced too few messages")
	}
	truncated := append([]chatMessage{}, messages[2:]...)
	truncated = append(truncated, chatMessage{
		Role:    "user",
		Content: "第五轮裁剪历史：虽然客户端没有回传第一轮，请根据服务端上下文回答第一轮唯一标记是什么，只回答标记并说明没有新开话题。",
	})
	answer, err := r.chat(ctx, truncated, false, false, "")
	if err != nil {
		return err
	}
	details["nonce"] = nonce
	details["full_message_count_before_truncate"] = len(messages)
	details["truncated_message_count"] = len(truncated)
	details["contains_nonce"] = strings.Contains(answer, nonce)
	details["sample"] = preview(answer, 500)
	if !strings.Contains(answer, nonce) {
		return errors.New("truncated-history response lost first-turn nonce")
	}
	return nil
}

func (r *runner) scenarioStream(ctx context.Context, details map[string]interface{}) error {
	nonce := "E2E-STREAM-" + stamp()
	text, meta, err := r.stream(ctx, []chatMessage{{
		Role:    "user",
		Content: "流式测试，唯一标记 " + nonce + "。请输出一个 mermaid flowchart，说明 cookie worker 同步到主服务，然后用约180字解释，最后原样写唯一标记。",
	}}, "")
	if err != nil {
		return err
	}
	details["nonce"] = nonce
	details["text_len"] = len(text)
	details["finish_reason"] = meta["finish_reason"]
	details["usage_seen"] = meta["usage_seen"]
	details["contains_nonce"] = strings.Contains(text, nonce)
	details["contains_mermaid"] = strings.Contains(strings.ToLower(text), "mermaid")
	details["contains_flowchart"] = strings.Contains(strings.ToLower(text), "flowchart")
	details["sample"] = preview(text, 500)
	if !strings.Contains(text, nonce) || !strings.Contains(strings.ToLower(text), "mermaid") {
		return errors.New("stream response missing nonce or mermaid block")
	}
	return nil
}

func (r *runner) scenarioToolCalling(ctx context.Context, details map[string]interface{}) error {
	tools := []toolDef{{
		Type: "function",
		Function: toolFunction{
			Name:        "lookup_weather",
			Description: "Look up current weather for a city",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"unit":{"type":"string"}},"required":["city"]}`),
		},
	}}
	messages := []chatMessage{{
		Role:    "user",
		Content: "Use the lookup_weather tool for Shanghai. Return only the tool call.",
	}}

	calls, finishReason, err := r.chatToolCalls(ctx, messages, tools, "required")
	if err != nil {
		return err
	}
	details["non_stream_finish_reason"] = finishReason
	details["non_stream_tool_calls"] = calls
	if len(calls) == 0 {
		return errors.New("non-stream tool scenario returned no tool calls")
	}
	if calls[0].Function.Name != "lookup_weather" {
		return fmt.Errorf("non-stream tool scenario returned unexpected tool %q", calls[0].Function.Name)
	}
	if !strings.Contains(calls[0].Function.Arguments, "Shanghai") && !strings.Contains(calls[0].Function.Arguments, "上海") {
		return fmt.Errorf("non-stream tool arguments do not mention Shanghai: %s", calls[0].Function.Arguments)
	}

	streamCalls, streamMeta, err := r.streamToolCalls(ctx, messages, tools, "required")
	if err != nil {
		return err
	}
	details["stream_meta"] = streamMeta
	details["stream_tool_calls"] = streamCalls
	if len(streamCalls) == 0 {
		return errors.New("stream tool scenario returned no tool calls")
	}
	if streamCalls[0].Function.Name != "lookup_weather" {
		return fmt.Errorf("stream tool scenario returned unexpected tool %q", streamCalls[0].Function.Name)
	}
	if streamMeta["finish_reason"] != "tool_calls" {
		return fmt.Errorf("stream tool scenario expected finish_reason tool_calls, got %v", streamMeta["finish_reason"])
	}
	return nil
}

func (r *runner) scenarioBOM(ctx context.Context, details map[string]interface{}) error {
	nonce := "E2E-BOM-" + stamp()
	text, err := r.chat(ctx, []chatMessage{{
		Role:    "user",
		Content: "BOM 兼容测试 " + nonce + "。请只用一句话回答并原样写标记。",
	}}, false, true, "")
	if err != nil {
		return err
	}
	details["nonce"] = nonce
	details["contains_nonce"] = strings.Contains(text, nonce)
	details["sample"] = text
	if !strings.Contains(text, nonce) {
		return errors.New("BOM request response missing nonce")
	}
	return nil
}

func (r *runner) scenarioNegativeCookie(ctx context.Context, accountID string, details map[string]interface{}) error {
	before, err := r.accountByID(ctx, accountID)
	if err != nil {
		return err
	}
	status, body, err := r.postInvalidCookie(ctx, accountID)
	details["target_account"] = accountID
	details["http_status"] = status
	details["response"] = preview(body, 400)
	if err == nil {
		return errors.New("invalid cookie update unexpectedly succeeded")
	}
	after, err := r.accountByID(ctx, accountID)
	if err != nil {
		return err
	}
	details["before"] = before
	details["after"] = after
	if before.Healthy != after.Healthy || before.State != after.State || before.CookieSource != after.CookieSource {
		return fmt.Errorf("invalid cookie update polluted account state: before=%+v after=%+v", before, after)
	}
	return nil
}

func (r *runner) scenarioRotation(ctx context.Context, wait time.Duration, details map[string]interface{}) error {
	before, err := r.listAccounts(ctx)
	if err != nil {
		return err
	}
	aNonce := "E2E-ROT-A-" + stamp()
	aMessages := []chatMessage{{
		Role:    "user",
		Content: "轮换测试 A 第一轮。请记住唯一标记 " + aNonce + "，并用80字说明同一话题为什么不应换账号。",
	}}
	a1, err := r.chat(ctx, aMessages, false, false, "")
	if err != nil {
		return err
	}
	aMessages = append(aMessages, chatMessage{Role: "assistant", Content: a1})
	time.Sleep(wait)
	bNonce := "E2E-ROT-B-" + stamp()
	b1, err := r.chat(ctx, []chatMessage{{
		Role:    "user",
		Content: "轮换测试 B 新话题。唯一标记 " + bNonce + "。请用80字说明账号轮换只影响新话题。",
	}}, false, false, "")
	if err != nil {
		return err
	}
	aMessages = append(aMessages, chatMessage{
		Role:    "user",
		Content: "轮换测试 A 第二轮：刚才 A 话题的唯一标记是什么？只回答标记并说明这是同一 A 话题。",
	})
	a2, err := r.chat(ctx, aMessages, false, false, "")
	if err != nil {
		return err
	}
	after, err := r.listAccounts(ctx)
	if err != nil {
		return err
	}
	details["wait"] = wait.String()
	details["before_accounts"] = before
	details["after_accounts"] = after
	details["a_nonce"] = aNonce
	details["b_nonce"] = bNonce
	details["a1_contains"] = strings.Contains(a1, aNonce)
	details["b1_contains"] = strings.Contains(b1, bNonce)
	details["a2_contains"] = strings.Contains(a2, aNonce)
	details["a2_sample"] = preview(a2, 300)
	if !strings.Contains(a1, aNonce) || !strings.Contains(b1, bNonce) || !strings.Contains(a2, aNonce) {
		return errors.New("rotation scenario lost one of the nonce markers")
	}
	return nil
}

func (r *runner) scenarioAuditExplicit(ctx context.Context, details map[string]interface{}) error {
	nonce := "E2E-AUDIT-" + stamp()
	conversationID := "e2e-audit-" + stamp() + fmt.Sprintf("-%04d", rand.Intn(10000))
	text, err := r.chat(ctx, []chatMessage{{
		Role:    "user",
		Content: "审计日志测试 " + nonce + "。请只用一句话回答并原样写标记。",
	}}, false, false, conversationID)
	if err != nil {
		return err
	}
	details["conversation_id"] = conversationID
	details["nonce"] = nonce
	details["contains_nonce"] = strings.Contains(text, nonce)
	details["sample"] = text
	if !strings.Contains(text, nonce) {
		return errors.New("audit explicit response missing nonce")
	}
	return nil
}

func (r *runner) chat(ctx context.Context, messages []chatMessage, stream, bom bool, conversationID string) (string, error) {
	req := chatRequest{Model: r.model, Stream: stream, Messages: messages, ConversationID: conversationID}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if bom {
		body = append([]byte{0xEF, 0xBB, 0xBF}, body...)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/openai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("chat status %d: %s", resp.StatusCode, preview(string(respBody), 500))
	}
	var out chatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("chat response has no choices")
	}
	return out.Choices[0].Message.Content, nil
}

func (r *runner) chatToolCalls(ctx context.Context, messages []chatMessage, tools []toolDef, toolChoice interface{}) ([]toolCall, string, error) {
	req := chatRequest{Model: r.model, Messages: messages, Tools: tools, ToolChoice: toolChoice}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/openai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("tool chat status %d: %s", resp.StatusCode, preview(string(respBody), 500))
	}
	var out chatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, "", err
	}
	if len(out.Choices) == 0 {
		return nil, "", errors.New("tool chat response has no choices")
	}
	return out.Choices[0].Message.ToolCalls, out.Choices[0].FinishReason, nil
}

func (r *runner) stream(ctx context.Context, messages []chatMessage, conversationID string) (string, map[string]interface{}, error) {
	req := chatRequest{
		Model:          r.model,
		Stream:         true,
		Messages:       messages,
		ConversationID: conversationID,
		StreamOptions:  &streamOpts{IncludeUsage: true},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/openai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("stream status %d: %s", resp.StatusCode, preview(string(respBody), 500))
	}
	var text strings.Builder
	meta := map[string]interface{}{"usage_seen": false, "finish_reason": ""}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage interface{} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			meta["usage_seen"] = true
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
			if choice.FinishReason != "" {
				meta["finish_reason"] = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, err
	}
	return text.String(), meta, nil
}

func (r *runner) streamToolCalls(ctx context.Context, messages []chatMessage, tools []toolDef, toolChoice interface{}) ([]toolCall, map[string]interface{}, error) {
	req := chatRequest{
		Model:         r.model,
		Stream:        true,
		Messages:      messages,
		Tools:         tools,
		ToolChoice:    toolChoice,
		StreamOptions: &streamOpts{IncludeUsage: true},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/openai/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("tool stream status %d: %s", resp.StatusCode, preview(string(respBody), 500))
	}
	var calls []toolCall
	meta := map[string]interface{}{"usage_seen": false, "finish_reason": ""}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []toolCall `json:"tool_calls,omitempty"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage interface{} `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			meta["usage_seen"] = true
		}
		for _, choice := range chunk.Choices {
			calls = append(calls, choice.Delta.ToolCalls...)
			if choice.FinishReason != "" {
				meta["finish_reason"] = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	return calls, meta, nil
}

func (r *runner) listAccounts(ctx context.Context) ([]accountStatus, error) {
	if r.token == "" {
		return nil, errors.New("admin token is required for account scenarios")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+"/admin/accounts", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("accounts status %d: %s", resp.StatusCode, preview(string(body), 500))
	}
	var out adminAccountsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Accounts, nil
}

func (r *runner) accountByID(ctx context.Context, accountID string) (accountStatus, error) {
	accounts, err := r.listAccounts(ctx)
	if err != nil {
		return accountStatus{}, err
	}
	for _, account := range accounts {
		if account.ID == accountID {
			return account, nil
		}
	}
	return accountStatus{}, fmt.Errorf("account %q not found", accountID)
}

func (r *runner) postInvalidCookie(ctx context.Context, accountID string) (int, string, error) {
	if r.token == "" {
		return 0, "", errors.New("admin token is required for negative-cookie scenario")
	}
	body := []byte(`{"secure_1psid":"invalid-e2e-cookie","secure_1psidts":"invalid-e2e-cookie-ts","source":"e2e-negative-cookie"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/admin/accounts/"+accountID+"/cookies", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, string(respBody), nil
	}
	return resp.StatusCode, string(respBody), fmt.Errorf("invalid cookie update rejected with status %d", resp.StatusCode)
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func stamp() string {
	return time.Now().Format("150405")
}

func preview(text string, limit int) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func readDotEnvValue(key string) string {
	data, err := os.ReadFile(".env")
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, prefix)), `"'`)
	}
	return ""
}
