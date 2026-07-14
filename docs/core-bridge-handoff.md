# Core Bridge Handoff

Read this first when changing the OpenAI-compatible Gemini Web bridge.

## Best Entry Points

| Need | Start Here |
|:---|:---|
| Understand the whole stream path | `docs/openai-gemini-stream-pipeline.md` |
| Change OpenAI SSE chunk behavior | `internal/modules/openai/openai_service.go`, `CreateChatCompletionStream` |
| Change tool-call bridge behavior | `internal/modules/openai/openai_service.go`, `shouldUseToolBridge`, `buildToolBridgePrompt`, `parseToolBridgeOutput` |
| Change Google upstream parsing | `internal/modules/providers/gemini_service.go`, `GenerateContentStreamForOpenAI`, `generateContentStreamInternal` |
| Change account selection/failover | `internal/modules/providers/gemini_client_pool.go` |

## Mental Model

The bridge has three layers:

1. `OpenAIController` receives OpenAI JSON and writes OpenAI SSE.
2. `OpenAIService` owns OpenAI semantics: prompts, tool bridge, context fallback, chunk conversion.
3. `providers.Client` owns Gemini Web semantics: cookies, `f.req`, upstream stream parsing, conversation metadata.

Keep these boundaries intact. The provider should not emit OpenAI DTOs. The OpenAI service should not parse raw Google stream bytes.

## Do Not Break

- No `tools` field means no tool bridge. Pure chat should stay the fastest path.
- Existing provider conversations must stay bound to their original account.
- Tool planning intentionally uses the main Gemini conversation so tool decisions, tool results, and final answers stay in one upstream topic.
- Debug captures must redact cookies and auth headers.
- Retry once only when the HTTP request was confirmed not written upstream. Timeouts, empty streams, read failures, and visible reasoning/content must not be transparently retried.
- Rebuild from full OpenAI history only for explicit conversation rejection such as Bard `1097` or a conversation continuity mismatch; record that migration because it creates a new Gemini Web topic.

## Before Editing

Run the focused tests first:

```bash
go test ./internal/modules/openai ./internal/modules/providers
```

Then run all tests before committing:

```bash
go test ./...
```

For parser changes, prefer a small captured raw-stream fixture and a unit test over only testing against a live Gemini account.
