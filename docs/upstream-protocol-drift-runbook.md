# Gemini Web 上游协议漂移排查手册

本文用于处理 Gemini Web 私有协议发生变化后出现的典型问题：Thinking 档位不生效、思考内容消失或混入正文、正文为空、流式顺序异常，以及请求结构与网页端不一致。

> 本项目依赖非公开协议。字段编号是观测结果，不是 Google 的稳定契约。先抓取同一时间、同一账号、同一模型的网页基线，再修改代码。

## 十分钟快速判定

| 检查点 | 证据 | 结论 |
|:---|:---|:---|
| 入站参数 | `*_openai_chat_request.json` 确认原始字段；`*_openai_upstream_trace.json` 的 `provider_config.thinking_level` 确认归一化结果 | 未进入服务或字段映射问题 |
| 上游请求 | 脱敏后的 `*.request.json`，确认 thinking header | 请求构造或网页协议变化 |
| 上游原始响应 | `*.raw.txt` 中是否存在思考文本 | Google 未返回，或响应路径变化 |
| provider 事件 | `*.entries.jsonl` / `*.entry_trace.json` 是否出现 thinking | provider 解析问题 |
| OpenAI 转发 | `OPENAI_TRACE_STREAM_FORWARD=true` 是否记录 `reasoning_content` | 协议转换问题 |
| 客户端展示 | 直接检查 SSE 是否含 `delta.reasoning_content` | 客户端不支持或未展示该字段 |

不要用“网页上看到了思考”直接推断本项目一定能收到相同字段。网页 UI 可能把摘要、结构化标题和最终正文组合显示；必须同时对照网络响应。

## 2026-07-13 已验证基线

### 请求侧

Thinking 档位由请求 header 控制：

```text
x-goog-ext-525001261-jspb[15]
Standard = 1
Extended = 2
```

当前模型族模式仍位于同一 header 的 `[14]`。项目入口是 `setGenerationHeaders`，上层参数链路是：

```text
reasoning_effort / reasoning.effort / thinking_level
  -> requestThinkingLevel
  -> providers.WithThinkingLevel
  -> setGenerationHeaders
  -> x-goog-ext-525001261-jspb[15]
```

网页端的 `f.req` inner 数组已观察到从旧样本的 81 项扩展为 92 项，但以下单变量实验均能返回思考内容：

| 实验 | 结果 |
|:---|:---|
| 网页当前 body + header `[15]=2` | 有思考 |
| 网页当前 body + header `[15]=2` + 强制 `inner[80]=1` | 有思考 |
| 项目旧 81 项 body + header `[15]=2` | 有思考 |

因此，数组长度变化本身不是故障证据。不要看到 92 项就整体复制网页请求模板；应先对单个字段做 A/B 测试。

### 响应侧

当前累积 Thinking 摘要路径仍是：

```text
payload[4][0][37][0][0]
```

新版响应还可能包含：

```text
payload[4][0][37][1]
```

`[37][1]` 可能是网页 UI 使用的结构化标题、说明或状态，不等于连续思考文本。解析器只应读取经过真实样本确认的叶子字段，不能递归展开整个 `[37]`，否则容易把未经验证的 UI 文案混入 `reasoning_content`。

当前 parser 的明确白名单是：metadata `7[5]` 的思考阶段标题、candidate `37[0][0]` 的累积摘要，以及 raw 中的 `Answer now` 阶段信号。metadata `11[0]` 是网页话题标题，必须排除；`candidate[37][1]` 的结构化 UI 标题/说明也必须排除。这里的边界是“只接受已抓包并有测试覆盖的具体路径或字面信号”，而不是排除所有状态文案，也不是递归接收整个父节点。

### 真实闭环结果

2026-07-13 使用有效账号验证 Extended SSE：

```text
HTTP 200
request header [15] = 2
reasoning_content: 4 chunks / 367 bytes
content: 3 chunks / 292 bytes
data: [DONE]
first thinking entry: 10
first content entry: 39
```

这证明当前项目的“入站字段 -> Google header -> raw response -> provider thinking event -> OpenAI `reasoning_content`”链路是生效的。若特定客户端仍不展示，应先直接检查 SSE，再判断是否为客户端展示兼容问题。

## 使用 Chrome DevTools MCP 重新校准

### 1. 准备可比较样本

在 Gemini 网页端使用同一个账号、模型、提示词，各发送一次 Standard 和 Extended。提示词应稳定触发多步推理，且两次只改变 Thinking 档位。

Chrome 操作顺序：

```text
list_pages / select_page
  -> take_snapshot
  -> 在操作前准备网络捕获
  -> click / fill / press_key
  -> list_network_requests(resourceTypes=[xhr, fetch], includePreservedRequests=true)
  -> get_network_request(目标 StreamGenerate 请求)
```

网络列表必须分页检查，避免目标请求落在下一页。优先使用被动网络捕获；只有 request body 缺失时，才在触发操作前注入 fetch/XHR hook。

### 2. 先比较请求，不先猜响应

至少记录以下脱敏后的差异：

- 请求 URL 的 path 和固定 query key 集合；
- `x-goog-ext-525001261-jspb` 各索引；
- 其他 `x-goog-ext-*` header 是否新增或消失；
- 解码后的 `f.req` inner 数组长度；
- Standard 与 Extended 唯一变化的字段；
- 模型变化与 Thinking 档位变化是否被混在同一次实验中。

任何结论至少需要一组单变量对照。不要把 Cookie 轮换、模型切换、提示词变化和 Thinking 档位变化放进同一组实验。

### 3. 再定位响应叶子字段

搜索网页响应中肉眼可识别的一小段思考文本，然后从该叶子向外记录完整数组路径。分别标注：

- 累积思考摘要；
- 增量或结构化思考节点；
- 最终正文；
- 网页标题、按钮、状态和引用信息；
- 各字段首次出现的 stream entry 序号。

如果思考文本“混在其中”，重点判断它是：

1. Google 在一个 candidate 结构中同时返回多个通道；
2. provider 对整个父节点递归提取；
3. OpenAI 转发把 `thinking_text` 写成了 `content`；
4. 客户端把 `reasoning_content` 和 `content` 合并显示。

### 4. 用本地 API 做闭环

先在 `.env` 中启用下一节的诊断开关并重启服务，然后用最小请求保存 Extended SSE：

```powershell
New-Item -ItemType Directory -Force scratch | Out-Null
curl.exe -N http://127.0.0.1:8787/openai/v1/chat/completions `
  -H "Content-Type: application/json" `
  --data-raw '{"model":"gemini-3.5-flash","reasoning_effort":"high","stream":true,"messages":[{"role":"user","content":"请逐步证明根号2是无理数，最后单独给出结论。"}]}' `
  -o scratch/extended.sse
```

不要用 SSE chunk 的 `id` 关联抓包：它与 controller request ID、Gemini upstream request UUID 是三个独立 ID。`*_openai_chat_request.json` 和 `*_openai_upstream_trace.json` 可通过文件名中的 controller ID 前缀关联；provider 的 request/raw/entries 文件只能在单请求诊断时按时间戳、模型和日志顺序关联。并发场景目前没有可靠的跨层关联键，应先降为一次只发一个请求；若要长期支持并发诊断，需要在代码中把 controller request ID 传入 provider capture。依次确认：

```text
delta.reasoning_content 先出现
delta.content 后出现
finish_reason 正常
data: [DONE] 存在
```

如果 raw 中有思考但 SSE 没有，问题在本地 parser 或转发；如果 SSE 有而界面没有，问题在客户端展示层。

## 本地诊断开关

仅在短时间诊断时启用。把这些值写进 `.env` 后重启服务；本项目加载 `.env` 时可能覆盖当前 shell 的同名变量，尤其不要只在 PowerShell 临时设置 `OPENAI_TRACE_STREAM_FORWARD`：

```env
OPENAI_DEBUG_REQUEST_LOG=true
GEMINI_DEBUG_STREAM_DIR=scratch/upstream_debug
GEMINI_TRACE_STREAM=true
OPENAI_TRACE_STREAM_FORWARD=true
```

文件用途：

| 文件 | 用途 |
|:---|:---|
| `*_openai_chat_request.json` | 确认客户端实际传入字段；包含完整对话，可能含 base64 附件或外部 URL |
| `*_openai_upstream_trace.json` | 确认 prompt、会话和回退路径；包含 prompt preview 和 conversation ID |
| `*.request.json` | 确认 Google URL、header、`f.req` |
| `*.raw.txt` | 判断 Google 是否返回目标字段 |
| `*.chunks.jsonl` | 判断分块和到达时间；preview 可能包含对话文本 |
| `*.entries.jsonl` | 判断字段路径与解析摘要 |
| `*.entry_trace.json` | 判断 thinking 与正文先后顺序 |

排查结束后关闭开关。性能测试必须关闭 dump，避免磁盘写入改变流式时序。

## 敏感数据规则

诊断产物默认全部视为敏感数据，不提交、不公开分享：

- `.env`；
- `data/cookies/accounts.json`；
- Chrome 返回的完整 Cookie header；
- `__Secure-1PSID`、`__Secure-1PSIDTS`；
- `at`、`SNlM0e` 和其他页面 token；
- 原始 `.network-request` / `.network-response`；
- `scratch/upstream_debug`。
- `scratch/extended.sse` 或其他保存的 SSE（包含完整回答和 reasoning）。

注意：即使 `*.request.json` 的 Cookie header 已被替换为 `[REDACTED]`，URL query 仍可能包含 `at`；`GEMINI_TRACE_STREAM` 也可能打印包含 `at` 的完整 URL。分享前必须同时清理 headers、URL query、form body 和响应中的账号信息。

推荐只保留最小化 fixture：删除身份信息和无关字段，用合成文本替换原始对话，确认测试仍能复现后再提交。

## `client not initialized` 与协议漂移的分界

`client not initialized` 表示账号 client 尚未完成 session/token 初始化，发生在向 Google 发送生成请求之前。它通常属于账号生命周期问题，不是 Thinking 请求或响应字段变化。

优先检查：

- Cookie 是否存在、过期或刚被替换；
- 启动初始化是否失败；
- 代理是否可达；
- 刷新/测试操作是否与初始化并发；
- 账号状态：`healthy` 可直接测试；`refreshing` 等待刷新；`expired` 先刷新；`needs_manual_login` 更新同一浏览器会话的 Cookie；`uninitialized` 先刷新初始化 session。

只有 client 已初始化、请求实际发到 Google，且网页基线与本地 raw 结构不同，才进入协议漂移排查。控制台应展示结构化错误的状态、原因和建议动作；刷新可以恢复未初始化 session，账号测试不应隐式轮换 Cookie。

推荐把三个控制台操作维持为不同语义：

| 操作 | 语义 | 边界 |
|:---|:---|:---|
| 刷新账号 | 恢复命令；已初始化时先做低成本真实探针，只有认证/session 错误才重建 session | 同账号刷新单飞；不能修复已过期或不匹配的 Cookie，也不能绕过 Google challenge；真实探针可能在 Gemini 账号中创建一条小型会话记录 |
| 测试账号 | 只读的端到端生成探针；成功时可恢复健康状态 | 不修改或轮换 Cookie，不替用户做隐式恢复；未初始化时明确提示先刷新；真实探针会在 Gemini 账号中创建一条小型会话记录 |
| 更新 Cookie | 显式凭据变更；先原子持久化并返回 `202`，再后台验证 | 两个 Cookie 必须来自同一登录会话；验证失败保留新值并将账号标为需处理；获取 Cookie 的浏览器与账号代理应使用相同出口 |

这样拆分的原因是可预测性：点击“测试”不会意外改变长期凭据，点击“刷新”可以安全恢复内存 session，而真正的凭据替换必须由用户明确发起。Cookie 保存只等待本地原子写入；账号卡片上的 `refreshing`、`healthy` 或失败状态承担后续验证反馈。结构化错误使用稳定 `code`、`state`、`retryable` 和 `action` 字段；HTTP 状态区分不存在（404）、操作冲突（409）、凭据/session 不可用（422）、上游或代理不可用（503）和超时（504）。底层错误仍应保留在服务日志中，控制台只展示可执行且不泄露凭据的信息。

## 修改与回归规则

修改请求字段前：

1. 保留 Standard/Extended 同提示词对照；
2. 一次只改一个 header 或 inner 索引；
3. 确认修改不会破坏 Standard、Extended 和默认档位；
4. 不因网页数组整体增长就替换完整模板。

修改响应 parser 前：

1. 先用脱敏 raw 建最小 fixture；
2. 断言 thinking 文本、正文和首次出现顺序；
3. 断言白名单 `7[5]`、`37[0][0]` 和 `Answer now` 按当前策略进入 reasoning，同时 `11[0]` 与 `37[1]` 不会进入；
4. 断言正文出现后不会迟发旧 thinking；
5. 同时验证 SSE 中 `reasoning_content`、`content`、finish chunk 和 `[DONE]`。

基础回归：

```powershell
go test ./internal/modules/providers -count=1
go test ./internal/modules/openai -count=1
go test ./... -count=1
```

重点测试位于 `internal/modules/providers/gemini_service_test.go` 和 `internal/modules/openai/openai_service_test.go`。

## 代码入口与历史资料

| 目标 | 入口 |
|:---|:---|
| Thinking 参数映射 | `internal/modules/openai/openai_service.go` |
| 请求 header / `f.req` | `internal/modules/providers/gemini_service.go` 中 `setGenerationHeaders`、请求构造函数 |
| Thinking 与正文解析 | 同文件的 `ExtractStreamState`、`extractStreamTextFromBuffer` |
| provider -> OpenAI SSE | `CreateChatCompletionStream` |
| 账号初始化与刷新 | `internal/modules/providers/gemini_client_pool.go` |
| 控制台错误展示 | `internal/modules/admin/admin_controller.go`、`internal/server/static/console.html` |

已跟踪的调查资料：

- `docs/openai-gemini-stream-pipeline.md`：当前完整转发链路；
- `docs/superpowers/specs/2026-07-13-thinking-and-account-recovery-design.md`：本轮设计和边界。

2026-06-16 的本地历史笔记曾记录首次 Thinking header、响应路径和流式顺序调查；其中仍有效的证据已合并到本文。后续维护不应依赖未纳入 Git 的根目录笔记。
