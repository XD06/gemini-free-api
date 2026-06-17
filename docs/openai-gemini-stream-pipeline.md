# OpenAI to Gemini Web Stream Pipeline

This document describes the main bridge path that accepts OpenAI-compatible chat requests, sends them to Gemini Web, parses the upstream Google stream, and returns OpenAI-compatible chunks, including the experimental tool-call bridge.

## Main Files

| Area | File | Responsibility |
|:---|:---|:---|
| HTTP / SSE entrypoint | `internal/modules/openai/openai_controller.go` | Parse OpenAI requests, write SSE responses, capture inbound debug payloads |
| OpenAI conversion layer | `internal/modules/openai/openai_service.go` | Build prompts, plan context reuse, handle tool bridge, convert provider events into OpenAI chunks |
| OpenAI DTOs | `internal/modules/openai/dto/openai_dto.go` | Request/response/chunk types, tools, attachments |
| Account routing | `internal/modules/providers/gemini_client_pool.go` | Select Gemini accounts, bind conversations, mark unhealthy accounts, refresh cookies |
| Gemini provider | `internal/modules/providers/gemini_service.go` | Build Google `f.req`, call Gemini Web stream endpoint, parse raw stream into text and thinking events |
| Upload support | `internal/modules/providers/gemini_upload.go` | Upload attachments to Google `content-push` before generation |

## High-Level Flow

1. `OpenAIController.HandleChatCompletions` receives the request.
2. The controller binds `dto.ChatCompletionRequest`, optionally logs a raw request capture, and sets SSE headers.
3. `OpenAIService.CreateChatCompletionStream` turns the OpenAI request into a Gemini Web prompt and provider options.
4. `ClientPool` or a single `Client` selects the account and invokes Gemini Web.
5. `generateContentStreamInternal` sends the Google request and parses the upstream stream.
6. Provider events are converted into OpenAI streaming chunks.
7. The controller forwards the chunks to the client and ends with `data: [DONE]`.

## Controller Layer

Entry point: `OpenAIController.HandleChatCompletions` in `internal/modules/openai/openai_controller.go`.

Responsibilities:

- trim a UTF-8 BOM if the request has one
- bind JSON into `dto.ChatCompletionRequest`
- write inbound request debug data when enabled
- for `stream: true`, set SSE headers and call `OpenAIService.CreateChatCompletionStream`
- serialize every `dto.ChatCompletionChunk` with `utils.SendSSEEvent`
- write the final `data: [DONE]` marker

The controller does not know anything about Gemini Web stream format. It only manages OpenAI DTOs and SSE framing.

## OpenAI Service Layer

Main streaming entrypoint: `OpenAIService.CreateChatCompletionStream` in `internal/modules/openai/openai_service.go`.

This is the central conversion layer. It owns:

- OpenAI message validation
- prompt construction
- service-side context planning and fallback
- tool-bridge planning and parsing
- conversion from provider `StreamEvent` to OpenAI `ChatCompletionChunk`
- final `finish_reason` and optional usage chunk

### Prompt and Context Planning

`planRequestContext(req)` decides whether the request should reuse an existing Gemini Web conversation or build a full prompt from OpenAI `messages`.

Important helpers:

| Helper | Purpose |
|:---|:---|
| `buildGeminiWebPromptFromOpenAIMessages` | Convert OpenAI messages into the text prompt sent to Gemini Web |
| `buildFirstTurnPrompt` | Preserve first-turn `system` messages as a visible `**Persona**` block |
| `latestUserMessage` | Validate that the last message is a usable user turn |
| `transcriptFingerprint` | Hash normalized OpenAI history for automatic context matching |
| `fallbackOptionsWithFreshConversation` | Allocate a new provider conversation when server-side context fails |

When context reuse works, only the latest user text is sent to Gemini Web and `providers.WithConversationID` carries the internal provider conversation ID. When reuse fails before any content is emitted, the service falls back to a stateless prompt built from the full OpenAI history.

### Provider Options

`CreateChatCompletionStream` maps OpenAI fields into provider options:

| OpenAI input | Provider behavior |
|:---|:---|
| `model` | `modelAndThinkingLevel` maps aliases / suffixes before provider dispatch |
| `reasoning_effort`, `reasoning.effort`, `thinking_level` | `providers.WithThinkingLevel` sets Gemini Web thinking header behavior |
| inferred or explicit conversation | `providers.WithConversationID` enables Gemini Web continuation metadata |
| attachments | `providers.InputFilesFromAttachments` then `providers.WithInputFiles` |
| `GEMINI_USE_SOURCE_PATH=true` | `providers.WithSourcePath(true)` adds Gemini Web `source-path` query parameter |

`GEMINI_USE_SOURCE_PATH` is disabled by default because it has triggered `BardErrorInfo [1097]` in some continuation requests.

## Provider Stream Events

The OpenAI service does not parse raw Google chunks directly. It receives simplified provider events from `GenerateContentStreamForOpenAI`:

```go
type StreamEvent struct {
    Kind  string
    Delta string
}
```

Current event kinds:

| Kind | Meaning | OpenAI output |
|:---|:---|:---|
| `content_delta` | Newly parsed assistant text | `choices[0].delta.content` |
| `thinking_text` | Newly parsed pre-answer thinking text | `choices[0].delta.reasoning_content` |

If tool bridge is active, `thinking_text` is suppressed because the planner prompt should only return tool JSON or normal text, not reasoning content.

## Gemini Web Provider Layer

Main streaming entrypoint: `Client.GenerateContentStreamForOpenAI` in `internal/modules/providers/gemini_service.go`.

This wraps `generateContentStreamInternal`, which performs the actual Google call.

### Request Construction

`generateContentStreamInternal` does the following:

1. Applies `GenerateOption` values into `GenerateConfig`.
2. Resolves model aliases against `cachedAliases` / `cachedModels`.
3. Uploads input files with `uploadRequestFiles` if any attachments exist.
4. Reads current Gemini web tokens from the client: `at`, cookie header, build label, session ID, language.
5. Builds Gemini Web inner payload with prompt text, uploaded file references, language, request ID, conversation metadata, and context token.
6. Encodes that payload as the Google `f.req` form value.
7. Calls `EndpointGenerate` with Gemini Web stream query parameters: `at`, `hl`, `_reqid`, `rt=c`, optional `bl`, optional `f.sid`, optional `source-path`.
8. Sets Gemini Web headers, model/thinking headers, and the cookie header.

Debug capture hooks live here:

| Hook | Output |
|:---|:---|
| `newStreamDebugCapture` | per-request `.request.json`, `.raw.txt`, `.chunks.jsonl`, `.entries.jsonl`, `.entry_trace.json` under `GEMINI_DEBUG_STREAM_DIR` |
| `maybeDumpStreamRequest` | single request dump to `GEMINI_DEBUG_REQUEST_PATH` |
| `GEMINI_TRACE_STREAM=true` | timing logs for request prepared, first byte, first parsed text, idle finish |

Request debug headers are redacted before being written. Do not remove that redaction; raw cookies must not be committed or shared.

### Upstream Parsing

The provider reads the HTTP response body in `readStreamChunks`, appending all bytes to a buffer. On each chunk it:

- captures debug records if enabled
- extracts conversation metadata with `extractConversationMetadataFromBuffer`
- checks continuity with `checkConversationContinuity` when this was a continuation request
- updates in-memory conversation metadata with `updateConversation` as soon as `cid` appears
- parses recent bytes with `extractStreamTextFromBuffer` or `extractTextFromBuffer`
- computes the delta with `streamTextDelta`
- calls the service callback with the delta

`GenerateContentStreamForOpenAI` also calls `ExtractStreamState(buffer)` before content starts to emit `thinking_text` deltas. Once normal content has appeared, thinking extraction stops.

### Finish and Error Detection

Gemini Web can leave the HTTP stream open after visible text is done. The provider uses `GEMINI_STREAM_FINISH_IDLE_MS` to close after a period with no new content. Default is currently configured in `.env.example` as `1000` ms for daily use.

If the upstream stream ends with no parsed text, `extractBardErrorCode` checks the raw buffer for `BardErrorInfo [code]`. The provider returns errors like:

```text
gemini bard error 1097
gemini bard error 1060
```

The OpenAI service can use these errors to decide whether to retry or fall back before any content has been sent to the client.

## Account Pool Layer

`ClientPool.GenerateContentStreamForOpenAI` in `internal/modules/providers/gemini_client_pool.go` chooses the concrete Gemini account client and delegates to `Client.GenerateContentStreamForOpenAI`.

Key behavior:

- explicit or inferred provider conversation IDs stay bound to the account that first created them
- new stateless requests use the active account window and priority order
- errors mark accounts unhealthy when appropriate and can trigger background cookie refresh
- if all accounts are unhealthy, the configured external cookie worker can be invoked

The OpenAI service only sees the `GeminiClient` interface. Account selection, cookie cache, worker refresh, and account audit logs are provider concerns.

## Tool-Call Bridge

OpenAI tool calling is not Gemini Web native tool calling. It is a prompt-level bridge implemented mostly in `internal/modules/openai/openai_service.go`.

### Activation

`shouldUseToolBridge(req)` returns true only when the request has OpenAI `tools`, the latest message is a valid user message, and `tool_choice` is `auto`, `required`, or a specific function.

If the client does not send `tools`, this entire path is skipped and the request remains normal streaming.

### Planning Prompt

When active, the service replaces the normal user prompt with `buildToolBridgePrompt(req, buildToolPlanningPrompt(req), requireToolCall)`.

The prompt contains bridge instructions, the required JSON schema `{"tool_calls":[{"name":"<tool_name>","arguments":{}}]}`, tool-choice constraints, all allowed tool definitions from `req.Tools`, and compact recent conversation context from `buildToolPlanningPrompt`.

The tool planner normally runs in a temporary Gemini call without `providers.WithConversationID`. This avoids appending tool schema to the main Gemini Web conversation record.

### Streaming Decision

Tool bridge can operate in two modes:

| Mode | Trigger | Behavior |
|:---|:---|:---|
| fully buffered | `tool_choice=required` or specific function | Buffer all content until upstream finishes, then parse tool JSON |
| prefix-classified | `tool_choice=auto` | Buffer only until `classifyToolBridgeStreamPrefix` can distinguish JSON from normal text |

If the prefix is normal text, the buffered text is flushed as OpenAI `delta.content` and later deltas stream normally. If the prefix looks like JSON, the output remains buffered and is parsed after upstream completion.

### Parsing and OpenAI Tool Chunks

After upstream completion, `parseToolBridgeOutput` performs:

1. `utils.StripCodeFence` to remove JSON fences.
2. `decodeToolBridgePayload` to parse the full JSON or first JSON object inside noisy text.
3. Validation that tool names are in the OpenAI request's allowed tool list.
4. Forced tool-name filtering if `tool_choice` specifies one function.
5. `normalizeArguments` to compact JSON and sanitize obvious string issues such as Markdown-wrapped URL values.
6. Construction of `dto.ChatCompletionToolCall` values.

The service emits tool calls as a single streaming delta with `choices[0].delta.tool_calls`, then finishes with `finish_reason: "tool_calls"`.

If no valid tool call is parsed, normal content is emitted if available. `tool_choice=required` or forced function can use `buildFallbackToolCalls`; otherwise the request finishes as normal text.

### Tool Result Turn

When the client sends a later request containing `role: "tool"` messages, `shouldUseToolBridge` only sees the latest user message. If the latest message is not a user message, the bridge is skipped.

The final answer after tool execution is generated by Gemini from the OpenAI message history. This can use the main provider conversation path, but it is still subject to server-side context fallback if Gemini Web rejects the continuation.

## Normal Streaming Chunk Conversion

For non-tool requests, or tool-auto requests classified as normal text, provider events are converted as follows:

1. The service immediately emits a role chunk: `{"choices":[{"delta":{"role":"assistant"}}]}`.
2. Provider `thinking_text` becomes OpenAI `delta.reasoning_content`.
3. Provider `content_delta` becomes OpenAI `delta.content`.
4. After provider completion, the service remembers the request context with `rememberRequestContext(contextPlan)` unless it emitted tool calls.
5. The final chunk sets `finish_reason` to `stop` or `tool_calls`.
6. If `stream_options.include_usage` is set, an extra usage chunk is emitted after the finish chunk.

## Failure and Fallback Rules

The most important fallback is inside `CreateChatCompletionStream`:

- if a server-side context continuation fails before any content was emitted and `shouldRetrySameProviderContext(err)` allows it, the service retries once against the same provider context
- if it still fails before content, the service forgets the provider conversation and retries with a full-history stateless prompt and a fresh provider conversation ID
- this fallback is disabled for active tool-bridge planning because tool planning intentionally uses temporary context

This protects the client from blank responses, but it weakens the guarantee that a Gemini Web page shows one continuous record.

## Where to Change Things

| Goal | Start here | Notes |
|:---|:---|:---|
| Improve raw Gemini parsing | `extractStreamTextFromBuffer`, `extractTextFromBuffer`, `ExtractStreamState` in `gemini_service.go` | Use debug `.raw.txt` and `.entries.jsonl` fixtures before changing parser behavior |
| Improve OpenAI streaming behavior | `CreateChatCompletionStream` in `openai_service.go` | Keep no-tool path fast and direct |
| Improve tool-call detection | `classifyToolBridgeStreamPrefix`, `parseToolBridgeOutput`, `decodeToolBridgePayload` | Avoid matching one specific tool name; operate on generic OpenAI tool schema |
| Improve tool argument cleanup | `normalizeArguments`, `sanitizeToolArgumentValue` | Must not rewrite ordinary natural-language queries |
| Improve context reuse | `planRequestContext`, `rememberRequestContext`, transcript fingerprint helpers | Preserve branch/retry safety; do not append edited history to an old Gemini record |
| Improve account failover | `ClientPool.clientForOptions`, `markClientError`, refresh helpers | Do not move an existing provider conversation to a different account |
| Add diagnostics | `dumpOpenAIRawRequest`, `newStreamDebugCapture`, trace logging | Keep sensitive header redaction intact |

## Regression Tests to Read First

Before changing this pipeline, read the tests around these cases:

- `internal/modules/openai/openai_service_test.go`
  - no-tool requests bypass the tool bridge
  - auto tools can stream normal text
  - bridge JSON emits OpenAI `tool_calls`
  - tool-result answers skip re-planning
  - temporary tool bridge does not append schema to provider conversation
  - Markdown-wrapped URL arguments are sanitized
- `internal/modules/providers/gemini_service_test.go`
  - Gemini stream request shape
  - context metadata extraction
  - continuity checks
  - early state does not block content
  - Bard error extraction
  - debug header redaction

When adding a new parser or tool-bridge behavior, prefer a small captured fixture and a unit test over only testing through a live Gemini account.