# OpenAI Tool Calling Structured Planner Design

Date: 2026-06-22

## Goal

Improve OpenAI-compatible `tools` / `tool_choice` behavior while keeping normal chat fast and unchanged. The bridge should convert Gemini Web text output into OpenAI-standard `tool_calls` more reliably, without hard-coding any specific MCP tool or argument shape.

This design only targets the OpenAI compatibility layer. Gemini-compatible and Claude-compatible endpoints are out of scope for this pass.

## Current Behavior

The OpenAI service currently enables a tool bridge when the request includes usable `tools`. It builds a prompt asking Gemini to output:

```json
{"tool_calls":[{"name":"tool_name","arguments":{}}]}
```

The service then strips code fences, extracts JSON when possible, validates tool names loosely, and maps the result to OpenAI `tool_calls`. This works for simple cases but is unstable because the model may add prose, emit malformed JSON, invent tool names, or produce arguments that do not match the client-provided tool schema.

## Proposed Approach

Add an OpenAI Tool Structured Planner inside `internal/modules/openai`. The planner is a small pipeline:

1. Build a tool-planning prompt from the OpenAI request.
2. Ask Gemini Web for a structured planning result.
3. Parse the first valid JSON object from plain text or a code fence.
4. Validate the payload against the current request's dynamic tool definitions.
5. Convert valid calls into OpenAI `ChatCompletionToolCall` values.
6. If parsing or validation fails, run one repair request that only asks Gemini to repair the previous output into the required JSON protocol.
7. If repair still fails, fall back or return an explicit error according to `tool_choice`.

The planner never hard-codes MCP-specific argument fields. It derives constraints from `tools[].function.parameters` for every request.

## Planner Output Protocol

The planner accepts exactly two semantic outcomes:

```json
{"status":"tool_calls","tool_calls":[{"name":"tool_name","arguments":{}}]}
```

```json
{"status":"message","content":"assistant text"}
```

Accepted syntax forms:

- raw JSON object
- fenced JSON code block
- prose containing one complete JSON object

Rejected or repaired forms:

- missing `status`
- invalid JSON
- unknown tool name
- non-object `arguments`
- `tool_choice` conflicts
- required parameters missing when the schema declares them
- primitive argument types that clearly conflict with the provided JSON schema

## Dynamic Argument Validation

Validation uses the OpenAI tool definition from the same request:

- `tools[].function.name` is the allowlist.
- `tools[].function.parameters` is interpreted as a JSON Schema-like object.
- `required` is enforced when present.
- basic `type` is checked for `object`, `array`, `string`, `number`, `integer`, and `boolean`.
- nested object validation is best-effort and bounded so deeply complex MCP schemas do not slow down requests.
- unknown properties are allowed unless the schema explicitly sets `additionalProperties: false`.

This keeps conversion fast while catching the mistakes that most often break clients.

## Prompt Strategy

The prompt should be strict but compact:

- state that the assistant is producing a tool-planning result for an OpenAI-compatible bridge
- require one JSON object and no surrounding text
- include the two output shapes
- list available tools with name, description, and raw parameters schema
- include only compact recent conversation context needed for planning
- include the current user request separately
- explain `tool_choice` rules:
  - `none`: planner is bypassed before prompt construction
  - `auto`: tool calls only when needed
  - `required`: at least one valid tool call
  - named function: exactly that tool name

The repair prompt should be even smaller. It should include:

- the invalid output
- the validation error
- the allowed tool names and schemas
- the required output protocol

The repair request should not ask the model to rethink the user's task. It only fixes format and arguments.

## Streaming Behavior

When tools are enabled, stream mode should buffer the planning response until the planner decides whether it is a tool call or a normal message.

If valid tool calls are produced:

- emit one OpenAI chunk with `delta.tool_calls`
- finish with `finish_reason: "tool_calls"`

If the result is a normal message:

- emit buffered text as `delta.content`
- finish with `finish_reason: "stop"`

This slightly delays first output for tool-enabled requests, but avoids leaking malformed planning text to clients.

Requests without tools remain on the direct streaming path and keep the current low-latency behavior.

## Failure Policy

For `tool_choice: "auto"`:

- parse or validation failure triggers one repair request
- if repair fails, return the best normal text content when available
- if no useful content exists, return a normal assistant message explaining that no valid tool call could be formed

For `tool_choice: "required"` or a named function:

- parse or validation failure triggers one repair request
- if repair fails, return an OpenAI-compatible error instead of fabricating empty `{}` arguments

This avoids sending fake tool calls to clients, which is worse than an explicit failure for automation.

## Conversation ID Design

Replace the current auto provider conversation ID format:

```text
openai-auto-<unix_nano>-<random>
```

with a Gemini-Web-like local key format:

```text
c_<lowercase hex/random token>
```

Constraints:

- the ID is only a local provider-conversation key before Gemini returns real metadata
- once Gemini returns `cid`, `rid`, `rcid`, or `context_token`, those real upstream values remain authoritative
- the generated key must not overwrite or masquerade as an upstream response `cid`
- tests should assert that generated IDs no longer contain `openai-auto`

This improves protocol shape without changing the existing continuity model.

## Test Plan

Unit tests in `internal/modules/openai`:

- no `tools` bypasses the planner
- `tool_choice: none` bypasses the planner
- `auto` with valid JSON returns standard OpenAI `tool_calls`
- `auto` with normal message returns normal assistant content
- `required` requires at least one valid tool call
- named function rejects other tool names
- fenced JSON is parsed
- prose-wrapped JSON is parsed
- unknown tool names are rejected
- non-object arguments are rejected
- required schema fields are enforced
- basic schema type mismatches are rejected
- one repair request can recover malformed planner output
- repair failure in `required` mode returns an error
- stream mode emits `delta.tool_calls` and `finish_reason: "tool_calls"`
- generated auto provider conversation IDs use the new local format

Integration or e2e test:

- add a tool-calling scenario to `tools/e2e` using a simple dynamic tool schema
- verify standard OpenAI non-stream and stream responses
- keep existing multiturn and stream scenarios passing

## Non-Goals

- Do not implement Gemini Web Canvas or browser feature capture in this pass.
- Do not change Gemini-compatible or Claude-compatible tool bridges.
- Do not add real native Gemini API structured-output fields to the Gemini Web private request unless packet capture proves they are supported.
- Do not hard-code MCP tool argument names.
