# Thinking Calibration and Account Recovery Design

## Scope

This change addresses two related operational problems:

1. Verify that Gemini Web Extended thinking is requested, parsed, forwarded, and displayed by the OpenAI-compatible playground.
2. Replace the ambiguous `client not initialized` response from account test/refresh operations with recoverable session initialization and actionable errors.

It does not replace the complete Gemini Web request template merely because the current web client expanded `f.req` from 81 to 92 elements. Live differential testing proved that the existing 81-element request still produces Extended thinking.

## Confirmed Protocol Behavior

- `x-goog-ext-525001261-jspb[15]` remains the thinking-level selector: `1` for Standard and `2` for Extended.
- Thinking summaries remain at candidate path `[4][0][37][0][0]`.
- Current responses may also contain structured metadata under candidate `[37][1]`; it must not be flattened into `reasoning_content` unless its semantics are separately established.
- `f.req[80]` is not a required duplicate Extended switch. A live request with header value `2` and body value `1` returned thinking.
- The existing 81-element request body returned thinking when sent through the current Gemini Web endpoint.
- OpenAI streaming exposes the available Gemini thinking summary through `choices[0].delta.reasoning_content`. Clients that ignore this non-standard field will not display it.

## Account Operation Semantics

### Test account

Testing is a generation-path probe, not a recovery operation.

- It must not rotate or update account cookies.
- If the account session token is absent, it returns the preserved initialization failure and current account state without attempting generation.
- If another refresh is in progress, it returns a conflict response instead of starting a duplicate operation.
- If initialized, it sends the existing minimal generation probe and reports its response and latency.
- The probe may create a small conversation in the upstream Gemini account; this is an unavoidable side effect of an end-to-end generation test.

### Refresh account

Refreshing is an explicitly mutating recovery operation.

1. Join an existing per-account refresh operation rather than executing concurrently.
2. If a session token already exists, run the generation probe first. A successful probe marks the account healthy without unnecessary cookie rotation.
3. If the session is absent or the probe reports an authentication/session failure, refresh the session using the initialization sequence:
   - try the current cookie pair directly;
   - if that fails, rotate cookies once;
   - retry session initialization;
   - run the generation probe only after initialization succeeds.
4. Persist updated cookies through the existing account cache behavior.
5. If an external cookie worker is configured, it remains an optional recovery path. No browser automation is started when it is disabled or unavailable.

The refresh request uses the caller's context and timeout throughout. Background initialization, manual refresh, and external refresh share one per-account single-flight guard.

## Error Model

Admin account operations return a structured error with:

- `code`: stable machine-readable reason;
- `type`: operation-level category;
- `message`: concise human-readable explanation;
- `state`: resulting account state;
- `retryable`: whether repeating without changing external state may work;
- `action`: the next operator action when one is required.

Initial reason codes:

| Code | State | Retryable | Action |
|---|---|---:|---|
| `account_not_initialized` | `uninitialized` | false | Refresh the account or update cookies |
| `cookie_pair_mismatch` | `needs_manual_login` | false | Supply `__Secure-1PSID` and `__Secure-1PSIDTS` from the same logged-in browser session |
| `cookie_expired` | `needs_manual_login` | false | Log in again and update the cookie pair |
| `refresh_in_progress` | `refreshing` | true | Wait for the current refresh |
| `proxy_unreachable` | `expired` | true | Check the account proxy |
| `upstream_challenge` | `expired` | conditionally | Resolve the Google challenge or change the proxy/session |
| `upstream_timeout` | current state | true | Retry after checking network health |
| `generation_probe_failed` | `expired` | conditionally | Inspect the nested upstream reason |

Upstream credential failures use an admin operation error rather than HTTP 401/403, because those statuses are reserved for authentication to this service's admin API. Account-not-found remains 404, concurrent conflicts use 409, invalid account credentials use 422, and temporary upstream/network failures use 503 or 504.

## Thinking Diagnostics

The existing debug pipeline remains the source of truth:

- the redacted request capture records the generated model/thinking header;
- ordered entry captures record extracted `thinking_texts` and the first content entry;
- `OPENAI_TRACE_STREAM_FORWARD` records `reasoning_content` forwarding without logging the content itself.

Regression coverage uses a minimal, synthetic, privacy-safe stream fixture representing the current `[37][0]` and `[37][1]` shape. No Chrome cookies, conversation identifiers, prompts, or raw personal responses are committed.

The playground continues to render `delta.reasoning_content` in its collapsible thinking panel. A completed panel may auto-collapse, but an empty panel is never fabricated merely because Extended was requested.

## Verification

Automated tests cover:

1. Extended maps to header index 15 value 2.
2. The current response shape emits thinking before content and ignores structured `[37][1]` metadata.
3. Test on an uninitialized account returns the saved initialization reason without calling the probe or refresh functions.
4. Refresh on an uninitialized account initializes the session and then probes.
5. Refresh on a live account probes without rotating cookies.
6. Concurrent refresh calls share one underlying recovery operation.
7. Admin controllers map typed account failures to the intended HTTP status and structured payload.

Live verification then uses the matching cookie pair already present in the user's logged-in Chrome Gemini request. Cookie values are transferred directly to the local admin API without printing them in chat, logs, shell command lines, fixtures, or commits. The live sequence is:

1. update account cookies;
2. refresh the account;
3. test the account;
4. send an OpenAI streaming request with `reasoning_effort=high`;
5. assert at least one non-empty `delta.reasoning_content`, non-empty answer content, and a clean `[DONE]` terminator;
6. confirm the request capture used Extended header value 2.

## Boundaries

- The service cannot derive a matching `__Secure-1PSIDTS` from an unrelated `__Secure-1PSID`.
- A successful rotation HTTP response does not prove Google issued a usable cookie pair.
- Automatic recovery cannot bypass login expiry, account challenges, or anti-abuse decisions.
- Gemini thinking output is a user-visible summary supplied by Google, not guaranteed raw hidden chain-of-thought.
- Google may vary the amount of thinking by prompt even when Extended is correctly selected; verification asserts presence and routing, not a minimum reasoning length.
