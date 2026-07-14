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

When context reuse works, only the latest user text is sent to Gemini Web and `providers.WithConversationID` carries the internal provider conversation ID. A generic timeout or empty response does not rebuild the conversation because Google may already have appended the turn. Only an explicit conversation rejection such as Bard `1097` or a conversation continuity mismatch migrates to a fresh provider conversation built from the full OpenAI history.

The service keeps an in-process recovery transcript for each active provider conversation. This allows explicit `conversation_id` clients that send only the newest message to rebuild the known history during an approved migration. The recovery transcript has the same 12-hour/1000-entry memory bounds as context matching and is not persisted across process restarts.

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

When tool bridge is active, `thinking_text` is still forwarded as OpenAI `delta.reasoning_content`. Tool JSON candidates remain buffered until the complete planner output can be parsed into OpenAI `tool_calls`.

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

Header redaction is not sufficient by itself: the request URL and trace logs can still contain the Gemini Web `at` query token. Treat every capture under `GEMINI_DEBUG_STREAM_DIR` as sensitive, sanitize URL query values as well as headers and bodies, and never commit raw captures.

### Thinking Protocol Baseline

As verified on 2026-07-13, `x-goog-ext-525001261-jspb[15]` is `1` for Standard and `2` for Extended. The cumulative thinking summary is currently read from `payload[4][0][37][0][0]`. A sibling at `[37][1]` may contain structured web UI metadata and must not be recursively flattened into reasoning text.

The current Gemini Web `f.req` inner array has been observed at 92 elements, while this project can still produce thinking with its older 81-element shape. Array length alone is therefore not evidence of a breaking change. Use single-variable browser captures and the [upstream protocol drift runbook](upstream-protocol-drift-runbook.md) before changing the request template or parser.

### Upstream Parsing

The provider reads the HTTP response body in `readStreamChunks`, appending all bytes to a buffer. On each chunk it:

- captures debug records if enabled
- extracts conversation metadata with `extractConversationMetadataFromBuffer`
- checks continuity with `checkConversationContinuity` when this was a continuation request
- updates in-memory conversation metadata with `updateConversation` as soon as `cid` appears
- parses recent bytes with `extractStreamTextFromBuffer` or `extractTextFromBuffer`
- computes the delta with `streamTextDelta`
- calls the service callback with the delta

`GenerateContentStreamForOpenAI` also calls `ExtractStreamState(buffer)` before content starts to emit `thinking_text` deltas. Once normal content has appeared, thinking extraction stops. A reasoning delta counts as visible stream progress, so Extended thinking is not mistaken for an empty stream while it is still active.

### Finish and Error Detection

Gemini Web normally emits a transport `e` entry after its final `wrb.fr` payload. The provider treats that entry as authoritative completion and closes immediately instead of waiting for HTTP EOF. `GEMINI_STREAM_FINISH_IDLE_MS` is an exact-value fallback only when the terminal entry is missing; it has no implicit minimum.

`GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS` separately limits a stream that has emitted neither reasoning nor answer content; its default is 15 seconds. Reaching this timeout means the upstream result is unknown: the request is ended without appending the same turn again or migrating the conversation. Once reasoning starts, `GEMINI_STREAM_PROGRESS_IDLE_TIMEOUT_MS` applies as a rolling 30-second guard until answer content appears, and no retry is allowed after visible output. The legacy `GEMINI_STREAM_FIRST_CONTENT_TIMEOUT_MS` name is still accepted.

The provider retries at most once only when Go's HTTP trace confirms that the request was not written to the upstream connection. HTTP responses, response-read failures, empty streams, parse failures, and activity timeouts all happen after dispatch and are not retried transparently.

Request timing records distinguish upstream TTFB, first reasoning, first content, tail close latency, completion source (`terminal_entry`, `idle_fallback`, or `eof`), and retry count. These fields are available in the console request detail and CSV export.

If the upstream stream ends with no parsed text, `extractBardErrorCode` checks the raw buffer for `BardErrorInfo [code]`. The provider returns errors like:

```text
gemini bard error 1097
gemini bard error 1060
```

The OpenAI service uses explicit conversation errors to decide whether a provider conversation must be migrated. Generic transport and timeout errors are returned without changing the conversation binding.

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

Tool planning intentionally runs in the main Gemini conversation. The first tool-enabled turn writes the bridge protocol and available tool definitions into that record; later turns reuse the same provider conversation and normally send only the latest user request. This keeps the tool decision, tool result, and final answer in one Gemini topic.

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
4. After provider completion, the service remembers the request context with `rememberRequestContext(contextPlan)`. Tool-call turns also retain the request-prefix mapping so the following `role: tool` request can return to the same provider conversation.
5. The final chunk sets `finish_reason` to `stop` or `tool_calls`.
6. If `stream_options.include_usage` is set, an extra usage chunk is emitted after the finish chunk.

## Failure and Fallback Rules

The recovery rules inside `CreateChatCompletionStream` are intentionally narrow:

- a request confirmed not written to Google may be retried once by the provider
- first-activity timeout, progress-idle timeout, empty stream, read failure, or generic upstream error is returned without retry or migration
- once reasoning or content is observed, transparent retry and migration are disabled
- Bard `1097` or a conversation continuity mismatch migrates once to a fresh provider conversation using the full OpenAI history
- tool planning follows the same rules and remains attached to the main provider conversation

These rules prioritize one continuous Gemini Web record and avoid appending the same user turn or tool result twice when the upstream outcome is uncertain.

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
  - tool bridge planning and tool results stay on the main provider conversation
  - Markdown-wrapped URL arguments are sanitized
- `internal/modules/providers/gemini_service_test.go`
  - Gemini stream request shape
  - context metadata extraction
  - continuity checks
  - early state does not block content
  - Bard error extraction
  - debug header redaction

When adding a new parser or tool-bridge behavior, prefer a small captured fixture and a unit test over only testing through a live Gemini account.
