# 技术细节

本文档收录 Gemini Web to API 的实现细节、排错指南和内部机制说明。

## 多轮上下文

OpenAI Chat Completions 本身是无状态协议。本项目通过两种方式接入 Gemini Web 服务端上下文。

> Gemini Web 服务端上下文不是公开 API，稳定性不等同于 OpenAI 官方接口。它的优势是命中时可以让同一客户端话题落在 Gemini Web 同一条记录里，续聊只发送最新 user 文本；缺点是受账号、网页端状态、Google 风控和上游内部字段变化影响。

续聊失败时，OpenAI 流式接口会在未输出正文前自动回退到完整历史 prompt。回退优先保证客户端拿到回答，但可能创建新的 Gemini Web 记录。

### 方式一：显式 `conversation_id`

```json
{
  "model": "gemini-3.5-flash",
  "conversation_id": "thread-1",
  "messages": [{"role": "user", "content": "记住一个词：海棠"}]
}
```

第二轮继续传相同的 `conversation_id` 即可复用上下文。

### 方式二：自动从 messages 历史推断

客户端回传完整历史时，服务端用"除最后一条 user 外的历史指纹"查找上一轮保存的 Gemini Web 会话。匹配成功则只把最后一条 user 原文发给 Gemini Web。

### 新话题判断规则

- 只有单条消息且有 `conversation_id`：按 ID 复用上下文。
- 有 `conversation_id` 且客户端只发最新消息：优先复用绑定的 provider 会话；上游续聊失败回退后重绑定到新会话。
- 有 `conversation_id` 且客户端回传完整历史：先检查历史指纹；不匹配则用完整历史重建上下文。
- 无 `conversation_id` 且 messages 是完整历史：按历史指纹找回上一轮上下文。
- 无 `conversation_id` 且只保留最近若干轮历史：依次尝试完整前缀、尾部历史、最近 2-4 条 user 消息窗口、根话题指纹。
- 历史指纹仅在仍是该 provider 会话最新前沿时才复用。回撤/分支时不追加到已前进的话题。
- 无 `conversation_id` 且只有单条新消息：分配新随机会话 ID。
- 进程重启后内存映射丢失；客户端回传完整历史时可重建上下文。
- 内存缓存 12 小时 TTL，1000 条上限。
- 同一进程内会保留 provider conversation 的可恢复消息文本，因此显式 `conversation_id` 每轮只传最新消息时，遇到 `1097`/CID mismatch 仍可用已知历史迁移；该恢复历史不会跨进程重启持久化。

## 工具调用桥接

OpenAI `tools` / `tool_choice` 通过 prompt 约束让 Gemini 输出工具调用 JSON，再转换为 OpenAI 标准 `tool_calls` 返回。

设计原则：

- 无 `tools` 字段时完全跳过桥接，保持直接实时流式。
- 工具规划直接写入并复用主 Gemini 会话，保证同一话题的工具决策、工具结果和最终回答落在同一记录中。
- 命中已有主会话后通常只发送最新用户请求；客户端回传 `role: tool` 结果时继续使用同一个 provider conversation。
- 工具参数做基础防御性清洗。

已知限制：

- 工具调用阶段可能需要缓冲上游输出以判断是工具 JSON 还是正文，不保证连续真流式。
- 模型未严格输出 JSON 时尽量当普通正文处理；强制工具场景可能返回兜底或失败。
- 多轮工具对话复杂度高，更容易触发格式错乱或上下文断裂。

### System Prompt 处理

Gemini Web 没有稳定的隐藏 system 字段。首轮会把 system 作为提示文本拼进输入：

```text
**Persona**: `你是善于使用工具的ai助手`

你好
```

续聊成功后只发送最新 user 原文，不再带前缀。

## 容错策略

- provider 识别 `BardErrorInfo`（如 1097），不当作正常空回复。
- provider 检查续聊响应的 `cid` 是否与上一轮一致，变化则视为上下文断裂。
- OpenAI 流式接口在续聊失败且未输出正文时自动回退到完整历史 prompt。
- 回退后客户端能收到回答，但不保证落在同一 Gemini Web 话题。

`GEMINI_USE_SOURCE_PATH=true` 可用于 A/B 排查续聊问题。

## 排错开关

涉及 Thinking 档位、思考内容路径或 Gemini Web 请求结构变化时，先按 [上游协议漂移排查手册](upstream-protocol-drift-runbook.md) 做网页基线和本地 raw/SSE 分层对照。

### OpenAI 入站请求摘要

```env
OPENAI_DEBUG_REQUEST_LOG=true
```

打印 request_id、model、stream、tools 状态、conversation_id、message 摘要。它不打印 Thinking 入站字段；确认原始 `reasoning_effort` / `thinking_level` 需同时启用完整抓包目录并查看 `*_openai_chat_request.json`，确认归一化结果则查看 `*_openai_upstream_trace.json` 的 `provider_config.thinking_level`。

### 完整抓包目录

```env
GEMINI_DEBUG_STREAM_DIR=scratch/upstream_debug
```

写入文件：

| 文件 | 内容 |
|:---|:---|
| `*_openai_chat_request.json` | 客户端原始请求 JSON |
| `*_openai_upstream_trace.json` | 请求摘要、provider conversation、prompt 摘要、回退原因 |
| `*.request.json` | 发给 Gemini Web 的 URL、headers、form body |
| `*.raw.txt` | Gemini Web 原始响应 |
| `*.chunks.jsonl` | 上游分块读取记录 |
| `*.entries.jsonl` | 解析出的 stream entry 摘要 |
| `*.entry_trace.json` | 关键 entry 到达顺序追踪 |

### 流式时间线日志

```env
GEMINI_TRACE_STREAM=true
```

观察请求准备、上游 TTFB、首个 reasoning、首个正文、终止 entry 和尾部关闭等事件。控制台请求详情也保存这些时间点、结束方式与重试次数。

OpenAI 转发层可单独开启：

```env
OPENAI_TRACE_STREAM_FORWARD=true
```

它用于确认 provider 的 `thinking_text` / `content_delta` 是否被转为 OpenAI `reasoning_content` / `content`。若 SSE 已包含 `reasoning_content` 而界面没有展示，应转向客户端兼容性排查。

### 抓包安全

诊断文件默认视为敏感数据。Cookie header 虽会在 request dump 中脱敏，但 URL query 仍可能含 `at`，流式时间线日志也可能打印带 token 的完整 URL。不要提交 `.env`、`data/cookies/accounts.json`、`scratch/upstream_debug` 或 Chrome 原始网络请求；分享前同时清理 header、URL、form body 和响应中的账号信息。

### 流式收尾等待

```env
GEMINI_STREAM_FINISH_IDLE_MS=1500
```

正常响应检测到上游 transport `e` 终止 entry 后立即结束。仅当上游缺少终止标记时，正文 idle 才作为兜底；配置值按原值生效，不再强制抬高到 15 秒。设 `0` 可关闭该兜底。

首活动超时表示请求已经发往 Google，但服务端没有拿到 reasoning 或正文，结果状态未知。此时 API 直接返回错误，不在原会话重复追加，也不迁移到新会话。已经输出 reasoning 或正文后同样禁止透明重试。

只有 HTTP trace 明确确认请求尚未写入上游连接时，provider 才自动重试一次。Bard `1097` 或 conversation continuity mismatch 属于明确的会话拒绝，服务端会使用完整 OpenAI 历史迁移到新的 provider conversation，并更新后续绑定。HTTP 状态错误、读取失败、空流、解析失败和普通超时都不会触发透明重试。

## Docker 内代理连通性排查

```bash
docker compose exec app sh
```

检查配置：

```sh
env | grep -E '^(PROXY_URL|GEMINI_ACCOUNT_.*_PROXY)='
```

测试容器到代理端口：

```sh
wget -S -O- --timeout=5 http://host.docker.internal:10808
```

返回 400/405 说明网络路径通；`Connection refused` 或超时说明不通。

强制通过代理访问 Google：

```sh
HTTPS_PROXY=http://host.docker.internal:10808 wget -S -O- --timeout=15 https://www.google.com/generate_204
```

返回 204 说明代理可用。也可在 Web 控制台的账号编辑中直接点击「测试代理」。

## Cookie 同步与 Worker

主服务提供 `/admin/*` 接口供 Playwright Cookie Worker 同步账号 Cookie。所有请求需带 `COOKIE_SYNC_TOKEN`。

控制台或 Worker 提交 Cookie 后先原子写入 `data/cookies/accounts.json`，接口立即返回 `202 Accepted`；服务随后在后台初始化 session 并更新账号状态。`refreshing` 表示已保存、正在验证，验证失败会保留新值并显示可执行错误，不会让保存按钮一直等待上游。

启动时，来源为 `console`、`worker` 或 `runtime` 的 cache Cookie 优先于 `.env` 旧配置，因此控制台更新可跨重启生效。需要重新强制使用 `.env` 时，应删除对应 cache 条目或关闭 `GEMINI_COOKIE_CACHE_ENABLED` 后重启。

Cookie 验证仍受网络边界约束：`__Secure-1PSID` 与 `__Secure-1PSIDTS` 必须来自同一浏览器会话，账号代理也应与获取 Cookie 的浏览器保持固定出口。浏览器与服务端出口不同可能导致浏览器请求成功、服务端初始化仍被 Google 拒绝。

Playwright Worker 位于 `tools/cookie-worker`，是独立进程，建议每账号一个持久 profile 和固定代理。Worker 以主服务 `/admin/accounts` 返回的状态为准，只同步非 healthy 账号。

### Worker 使用

```bash
cd tools/cookie-worker
npm install
API_BASE=http://127.0.0.1:8787 \
COOKIE_SYNC_TOKEN=你的token \
GEMINI_ACCOUNTS=acc1,acc2 \
npm start
```

首次登录：

```bash
COOKIE_WORKER_ACCOUNT=acc1 npm run login
COOKIE_WORKER_ACCOUNT=acc1 npm run sync
```

### Worker 开关

| 变量 | 说明 |
|:---|:---|
| `COOKIE_WORKER_ACCOUNT` | 只处理指定账号 |
| `COOKIE_WORKER_OPEN_ONLY` | 只打开浏览器供人工登录 |
| `COOKIE_WORKER_ONCE` | 同步一轮后退出 |
| `COOKIE_WORKER_FORCE` | 强制同步，忽略远端 healthy 状态 |
| `COOKIE_WORKER_HEADLESS` | 无头运行；首次登录建议 `false` |

## 端到端测试

```bash
go run ./tools/e2e \
  -base-url http://127.0.0.1:8787 \
  -scenarios status,multiturn,stream,bom,negative-cookie
```

| 场景 | 说明 |
|:---|:---|
| `status` | 检查所有账号是否 healthy |
| `multiturn` | 逐轮追加 messages，验证服务端上下文 |
| `truncated-history` | 模拟客户端裁剪早期历史 |
| `stream` | 验证实时流式、代码块、finish_reason |
| `bom` | 验证 UTF-8 BOM 兼容 |
| `negative-cookie` | 验证无效 Cookie 被保存后账号进入可诊断失败状态 |
| `rotation` | 验证账号轮换与旧话题连续性 |
| `audit-explicit` | 使用显式 conversation_id 方便审计 |

## 环境变量完整列表

| 变量 | 默认值 | 说明 |
|:---|:---|:---|
| `PORT` | `8787` | 服务端口 |
| `LOG_LEVEL` | `info` | 日志级别 |
| `APP_ENV` | `development` | 运行环境 |
| `COOKIE_SYNC_TOKEN` | 空 | admin 接口鉴权 token |
| `PROXY_URL` | 空 | 单账号模式代理 |
| `GEMINI_1PSID` | 空 | 单账号 `__Secure-1PSID` |
| `GEMINI_1PSIDTS` | 空 | 单账号 `__Secure-1PSIDTS` |
| `GEMINI_REFRESH_INTERVAL` | `2` | Cookie 自动刷新间隔（分钟） |
| `GEMINI_MAX_RETRIES` | `2` | 请求未写入上游时的最大尝试次数；实际最多重试一次 |
| `GEMINI_COOKIE_CACHE_ENABLED` | `true` | Cookie 缓存开关 |
| `GEMINI_COOKIE_CACHE_PATH` | `data/cookies/accounts.json` | Cookie 缓存路径 |
| `GEMINI_STARTUP_COOKIE_ROTATE` | `true` | 启动时执行 RotateCookies |
| `GEMINI_ACCOUNT_STATE_PATH` | `data/state/accounts.json` | 账号轮换状态文件 |
| `GEMINI_ACCOUNT_AUDIT_LOG` | `true` | 账号审计日志 |
| `GEMINI_COOKIE_WORKER_ENABLED` | `true` | 自动触发外部 Cookie Worker |
| `GEMINI_COOKIE_WORKER_COMMAND` | `npm run sync --silent` | Worker 命令 |
| `GEMINI_COOKIE_WORKER_DIR` | `tools/cookie-worker` | Worker 工作目录 |
| `GEMINI_COOKIE_WORKER_TIMEOUT_SECONDS` | `120` | Worker 超时 |
| `GEMINI_ACCOUNTS` | 空 | 多账号 ID 列表 |
| `GEMINI_ACCOUNT_<ID>_1PSID` | 空 | 指定账号 `__Secure-1PSID` |
| `GEMINI_ACCOUNT_<ID>_1PSIDTS` | 空 | 指定账号 `__Secure-1PSIDTS` |
| `GEMINI_ACCOUNT_<ID>_PROXY` | 空 | 指定账号代理 |
| `GEMINI_ACCOUNT_<ID>_PRIORITY` | `0` | 账号优先级 |
| `GEMINI_ACCOUNT_<ID>_STAY_MINUTES` | `180` | 基础停留时间 |
| `GEMINI_ACCOUNT_<ID>_PROFILE_DIR` | 空 | Playwright profile 目录 |
| `RATE_LIMIT_ENABLED` | `true` | 限流开关 |
| `RATE_LIMIT_WINDOW_MS` | `60000` | 限流窗口 |
| `RATE_LIMIT_MAX_REQUESTS` | `30` | 窗口内最大请求数 |
| `OPENAI_DEBUG_REQUEST_LOG` | `false` | 入站请求摘要日志 |
| `GEMINI_DEBUG_STREAM_DIR` | 空 | 抓包目录 |
| `GEMINI_TRACE_STREAM` | `false` | 流式时间线日志 |
| `OPENAI_TRACE_STREAM_FORWARD` | `false` | provider 事件到 OpenAI SSE 的转发日志 |
| `GEMINI_STREAM_FINISH_IDLE_MS` | `1500` | 流式 idle 收尾等待 |
| `GEMINI_STREAM_FIRST_ACTIVITY_TIMEOUT_MS` | `15000` | 首个可见流活动（思考或正文）最大等待；旧的 `GEMINI_STREAM_FIRST_CONTENT_TIMEOUT_MS` 仍兼容 |
| `GEMINI_STREAM_PROGRESS_IDLE_TIMEOUT_MS` | `30000` | 思考已开始、正文未出现时的连续无进度最大等待 |
| `GEMINI_WEB_STREAM_QUERY` | `false` | 强制带 Gemini Web 流式查询参数 |
| `OPENAI_CONTEXT_LOCAL_FALLBACK` | `true` | 服务端会话不可信时从本地历史重建 |
