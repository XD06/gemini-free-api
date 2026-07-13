# Web Console Refinement and Observability Design

## Scope

Upgrade the existing embedded Web console as one cohesive product surface while preserving its deployment simplicity and all current account, Playground, and request-log behavior.

The approved direction is **Refined Minimal**: retain the bright neutral palette, top navigation, restrained typography, and familiar page structure; improve hierarchy, spacing, component states, responsive behavior, and perceived quality. Add lightweight visualization and operational features without turning the console into a dark monitoring dashboard.

The first functional priority is:

1. runtime insight from the recent in-memory request records;
2. request diagnosis and inspection;
3. consistent refinement of accounts, Playground, navigation, drawers, notifications, loading states, and mobile behavior.

Metrics remain in memory and reset when the service restarts. No database or history file is introduced.

## Product Principles

- **Quiet, not empty:** neutral surfaces and restrained color, with enough grouping and contrast to scan quickly.
- **Operational truth first:** account health, success rate, latency, and errors should be visible before decorative content.
- **Details on demand:** summaries remain compact; request and account diagnostics open in drawers instead of expanding every card.
- **One action, one meaning:** test is read-only, refresh is recovery, Cookie update is explicit mutation, and destructive actions remain clearly separated.
- **No framework tax:** keep the console as an embedded HTML artifact with vanilla CSS and JavaScript. Use inline SVG/CSS visualization; add no chart library.
- **Desktop-first, mobile-complete:** desktop uses higher information density; mobile retains every action through stacked cards and responsive drawers.

## Information Architecture

The top-level navigation remains:

1. **Overview / Accounts** — account pool status plus recent runtime insight;
2. **Playground** — real streaming test surface;
3. **Requests** — searchable diagnostic history.

The first tab becomes a true operational overview rather than only an account-card list:

- compact page header with primary action;
- four summary metrics;
- a small request-health visualization row;
- account search, state filters, selection, and account cards;
- contextual batch action bar when accounts are selected.

Navigation state is reflected in the URL hash so refresh/back navigation returns to the active tab. Existing authentication remains local-storage based and is not exposed in the URL.

## Visual System

### Direction

- Background: warm near-white, with white raised surfaces and subtle neutral separators.
- Text: near-black primary, cool gray secondary, monospace only for IDs, metrics, and protocol values.
- Accent: restrained green for healthy/success, amber for recovery/pending, red for failure/destructive actions, blue only for neutral informational emphasis.
- Shape: 8–12 px radii, one-pixel borders, minimal shadows limited to overlays and hover elevation.
- Typography: keep the current Geist / Instrument Serif pairing, but reserve the serif face for page-level editorial headings rather than routine labels.
- Motion: 120–220 ms transitions; respect `prefers-reduced-motion`.

### Component Quality

Every interactive component receives explicit default, hover, focus-visible, active, disabled, loading, success, and error states. Repeated inline styles are replaced by semantic classes. Buttons use consistent sizes and icon alignment. Tooltips or visible labels explain ambiguous icons.

The login view, empty states, failure states, skeleton/loading states, toast notifications, confirmation dialogs, and drawers share the same visual language.

## Runtime Insight

The dashboard derives all metrics from the existing recent request records returned by `/admin/requests`.

### Summary Metrics

- recent request count;
- success rate;
- median first-byte latency;
- median total duration.

Each metric includes a compact supporting label. Zero-data states display an explanatory placeholder rather than a misleading `0%` success rate.

### Visualizations

Use dependency-free SVG or CSS:

- **Request activity sparkline:** requests grouped into recent time buckets;
- **Latency mini-chart:** first-byte and total-duration trend, with extreme values capped visually but preserved in tooltips/details;
- **Status distribution:** compact success/error/pending segmented bar;
- **Account usage:** small proportional bar or ranked list based on `account_id`.

Charts are summaries, not analytical dashboards. They include text equivalents and do not rely on color alone.

## Account Operations

### Account Cards

Cards keep the current actions but improve their hierarchy:

- identity and status in the header;
- proxy, Cookie source, last validation, and active-account state as aligned definition rows;
- last error/action displayed as an actionable inline notice when present;
- primary `Test` action, secondary `Refresh` and `Edit`, destructive delete inside the edit drawer;
- operation result and latency remain visible after the toast disappears.

### Selection and Batch Actions

Account cards gain keyboard-accessible selection. When one or more accounts are selected, a sticky contextual bar offers:

- batch test;
- batch refresh;
- clear selection.

Operations run with a small concurrency limit, report per-account results, and never batch-delete or batch-update credentials. Existing per-account refresh single-flight behavior remains authoritative.

### Error Handling

Structured admin errors render `message`, `state`, and `action`. Retryable errors expose an explicit retry button. Credential/session errors direct the user to the edit drawer. Proxy errors can prefill the proxy-test section. Raw nested errors are not rendered when they may contain sensitive data.

## Request Diagnostics

### Table and Filters

The request table gains:

- search across model, account, path, status, and request ID;
- status, protocol/path, account, model, and stream filters;
- sortable timestamp, first-byte latency, and total duration;
- compact active-filter chips and a reset action;
- CSV export of the currently filtered records;
- responsive card rows on narrow screens.

### Request Detail Drawer

Selecting a request opens a read-only drawer containing:

- request ID and timestamp;
- path/protocol, model, account, stream mode, and status;
- first-byte latency, remaining generation time, and total duration visualized as a simple timing bar;
- token counts when available;
- user-agent summary;
- complete recorded error message with copy action;
- copy-as-JSON action for the individual sanitized record.

The drawer uses the already-returned record. No endpoint exposes request prompts, Cookie values, authorization headers, raw SSE, or upstream tokens.

### Small Backend Additions

Extend `RequestRecord` only with non-sensitive fields that controllers already know and can populate consistently, such as protocol and normalized thinking level, when feasible. The UI must remain compatible when those fields are absent in older records.

No raw request-body logging is added to the admin API. Existing debug capture remains an opt-in filesystem diagnostic mechanism.

## Playground Refinement

The Playground retains real streaming and current Thinking rendering, with interaction polish:

- clearer model and Thinking controls in a compact settings row;
- stable message layout during streaming;
- visible elapsed time and streaming state;
- copy response and copy thinking actions;
- regenerate the last user turn;
- stop generation with an explicit destructive/stop state;
- improved code blocks, long-line wrapping, empty replies, cancellation, and error presentation;
- conversation clear remains confirmed when content exists.

The first release does not add persisted conversations, file uploads, arbitrary generation parameters, or raw SSE display. Those features would create additional privacy and protocol complexity outside this scope.

## State and Data Flow

The browser maintains small isolated state objects:

- authentication/session UI state;
- account list, filters, selection, and per-account operation state;
- recent request records, derived metrics, filters, sorting, and selected request;
- Playground messages and active stream state.

Rendering helpers are separated by surface even though deployment remains one HTML file. DOM event delegation is preferred for generated account and request rows. Derived metrics are pure functions over request records so they can be unit-tested independently from DOM rendering.

Polling behavior remains conservative:

- accounts refresh at the existing interval only while connected;
- requests refresh only while Overview or Requests is visible;
- active drawers and user selection are not reset by background refresh;
- a visible “last updated” indicator and manual refresh remain available.

## Accessibility and Responsive Behavior

- Use semantic buttons, tables, headings, labels, and form controls.
- Provide `aria-live` regions for operation status and toasts.
- Drawers trap focus, close on Escape, restore focus to the opener, and prevent background scroll.
- Tabs implement appropriate selected state and keyboard navigation.
- All focusable controls have visible `:focus-visible` styling.
- Status never depends only on color.
- At widths below 768 px, metric cards become a two-column grid, charts stack, account cards become single-column, request rows become labeled cards, and drawers fill the viewport.
- Touch targets are at least 40 px where practical.

## Verification

### Automated

- Go tests for any added request-record fields and admin serialization.
- Existing account-operation controller and provider tests remain green.
- Static console script smoke checks for syntax and required element IDs.
- Pure JavaScript metric/filter helpers tested with zero, success-only, mixed, missing-field, and outlier datasets where practical.

### Browser

Use the running local service with Chrome DevTools:

1. verify login, logout, and invalid-token states;
2. verify all three tabs and URL-hash restoration;
3. test empty, loading, populated, and API-error states;
4. test account selection, batch test, batch refresh, drawer focus, and structured errors;
5. generate Standard and Extended Playground responses and confirm thinking/content rendering;
6. open request details, filters, sorting, export, and clearing history;
7. test desktop and mobile widths, keyboard-only navigation, Escape behavior, and reduced motion;
8. confirm no Cookie, `at`, prompt body, or authorization value appears in request diagnostics.

## Boundaries

- No persistent metrics or request history.
- No frontend framework, build pipeline, or third-party chart library.
- No credential reveal, raw Cookie display, prompt/body history, or upstream debug-file browser.
- No bulk credential mutation or destructive bulk actions.
- No replacement of the existing admin authentication model in this iteration.
- No change to Gemini generation semantics, Thinking protocol calibration, or account recovery rules.
