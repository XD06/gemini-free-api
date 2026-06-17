# Gemini Web to API<img src="https://th.bing.com/th/id/ODF.nAWZa6qAQb-ILV5Rp8qOrw?w=32&amp;h=32&amp;qlt=90&amp;pcl=fffffa&amp;o=6&amp;pid=1.2" height="32" width="32" alt="全球 Web 图标" class="rms_img" data-bm="32">

把 Gemini 网页端封装成 OpenAI / Claude / Gemini 兼容接口的本地代理服务。

本项目是网页端逆向实现，行为依赖 Google Gemini Web 的私有请求结构。适合个人研究、调试和本地客户端接入，不建议当作稳定生产服务使用。

## 当前能力

| 能力 | OpenAI 兼容 | Claude 兼容 | Gemini 原生兼容 | 说明 |
|:---|:---:|:---:|:---:|:---|
| 普通文本对话 | 支持 | 支持 | 支持 | 三种协议都会转发到 Gemini  |
| 流式输出 | 实时流式 | 模拟流式 | 模拟流式 | 只有 OpenAI 兼容接口接入 provider 实时流 |
| Thinking Level | **支持** | 未接入 | 未接入 | OpenAI 支持 `reasoning_effort` / `reasoning.effort` / `thinking_level` |
| 思考内容输出 | 支持 | 未接入 | 未接入 | OpenAI 流式通过 `delta.reasoning_content` 输出 |
| 服务端多轮上下文 | **实验性支持** | 未接入 | 未接入 | 复用 Gemini Web 的 `c/r/rc/context token`，多轮对话只有一条消息记录(不稳定，有时候似乎会丢失记录) |
| 图片/文件输入 | 支持 | 支持 | 支持 | base64 内联文件会先上传到 Google `content-push` |
| 图片生成 | **暂不支持** | 不支持 | 不支持 | OpenAI `/images/generations` |
| 工具调用 | **暂不支持** | 桥接支持 | 桥接支持 | 通过 prompt 约束输出工具调用 JSON，非 Gemini 原生 tool calling |

## 快速启动

复制配置：

```bash
cp .env.example .env
```

至少填写：

```env
PORT=8787
PROXY_URL=http://127.0.0.1:10808（视环境可选）
```

单账号 legacy 模式可直接填写：

```env
GEMINI_1PSID=你的 __Secure-1PSID
GEMINI_1PSIDTS=可选，留空时程序会尝试自动轮换
```

多账号模式推荐只把账号、代理、优先级和 profile 写进 `.env`，Cookie 由 worker 同步到 `data/cookies/accounts.json`。开启 `GEMINI_COOKIE_CACHE_ENABLED=true` 后，启动时 cache 中的账号 Cookie 会优先于 `.env` 里的旧 Cookie，避免失效配置挡住已刷新的 Cookie。

多账号可选。设置 `GEMINI_ACCOUNTS` 后，程序会为每个账号创建独立 Gemini Web 客户端、Cookie 状态和代理出口：

```env
GEMINI_ACCOUNTS=main,backup1

GEMINI_ACCOUNT_MAIN_1PSID=主号 __Secure-1PSID
GEMINI_ACCOUNT_MAIN_1PSIDTS=主号 __Secure-1PSIDTS
GEMINI_ACCOUNT_MAIN_PROXY=socks5h://127.0.0.1:10808
GEMINI_ACCOUNT_MAIN_PRIORITY=3
GEMINI_ACCOUNT_MAIN_STAY_MINUTES=60
GEMINI_ACCOUNT_MAIN_PROFILE_DIR=profiles/main

GEMINI_ACCOUNT_BACKUP1_1PSID=备用号 __Secure-1PSID
GEMINI_ACCOUNT_BACKUP1_1PSIDTS=备用号 __Secure-1PSIDTS
GEMINI_ACCOUNT_BACKUP1_PROXY=http://127.0.0.1:10809
GEMINI_ACCOUNT_BACKUP1_PRIORITY=1
GEMINI_ACCOUNT_BACKUP1_STAY_MINUTES=60
GEMINI_ACCOUNT_BACKUP1_PROFILE_DIR=profiles/backup1
```

账号轮换只影响新话题。已经接入 Gemini Web 服务端上下文的话题会一直绑定首次使用的账号，避免同一客户端话题在 Gemini 网页端断成多个记录。账号实际停留时间 = `GEMINI_ACCOUNT_<ID>_STAY_MINUTES * GEMINI_ACCOUNT_<ID>_PRIORITY`；priority 为空或小于等于 0 时按 1 计算。active 账号状态会写入 `GEMINI_ACCOUNT_STATE_PATH`，重启后如果窗口未过期，会继续使用原 active 账号到原到期时间。

启动：

```bash
go run cmd/server/main.go
```

服务默认监听：

```text
http://localhost:8787
```

API 文档：

```text
http://localhost:8787/docs
```

## OpenAI 兼容调用

流式对话：

```bash
curl http://localhost:8787/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.5-flash",
    "stream": true,
    "messages": [
      {"role": "user", "content": "用三句话介绍 Mermaid"}
    ]
  }'
```

Thinking Level：

```json
{
  "model": "gemini-3.5-flash",
  "reasoning_effort": "high",
  "stream": true,
  "messages": [{"role": "user", "content": "详细分析这个问题"}]
}
```

映射规则：

| OpenAI 字段 | 支持值 | Gemini Web |
|:---|:---|:---|
| `reasoning_effort` | `low` / `medium` | Standard |
| `reasoning_effort` | `high` | Extended |
| `reasoning.effort` | `low` / `medium` / `high` | 同上 |
| `thinking_level` | `standard` / `extended` | 直接映射 |
| 模型后缀 | `gemini-3.5-flash:thinking=extended` | Extended |

不传 thinking 字段时使用 Gemini Web 默认档位。

## 模型列表

当前项目按 Gemini Web 界面实际使用的内部模型 ID 发送请求，并对 OpenAI 客户端暴露更容易理解的别名。`/openai/v1/models` 返回的是可传入 `model` 字段的名字。

| 可传模型名 | 传参方式 | 说明 |
|:---|:---|:---|
| `gemini-3.5-flash` | 映射到 UI 内部 ID | UI 可见模型 |
| `gemini-3.1-flash-lite` | 映射到 UI 内部 ID | UI 可见模型 |
| `gemini-3.1-pro` | 映射到 UI 内部 ID | UI 可见模型 |

原项目会从 Gemini 初始化页面里扫描 `gemini-*` 字符串并暴露出来。本项目没有照搬这个策略，因为当前实时流式路径的模型选择依赖 Gemini Web 的 `x-goog-ext-525001261-jspb` header，直接暴露页面里所有字符串容易让客户端误以为存在更多独立可选模型。

抓包里出现的 `3.5 Flash 扩展` 更接近 Gemini Web 的扩展/深思档位表现，不作为独立模型暴露。客户端需要深度思考时应使用 Thinking Level，例如 `reasoning_effort: "high"` 或 `thinking_level: "extended"`。

## 多轮上下文

OpenAI Chat Completions 本身通常是无状态协议。这里有两种方式接入 Gemini Web 服务端上下文。

### 方式一：显式 `conversation_id`

客户端能传自定义字段时，推荐传同一个 `conversation_id`：

```json
{
  "model": "gemini-3.5-flash",
  "conversation_id": "thread-1",
  "messages": [{"role": "user", "content": "记住一个词：海棠"}]
}
```

第二轮继续传：

```json
{
  "model": "gemini-3.5-flash",
  "conversation_id": "thread-1",
  "messages": [{"role": "user", "content": "刚才让你记住的词是什么？"}]
}
```

### 方式二：自动从 OpenAI `messages` 历史推断

很多客户端不会暴露 `conversation_id`，但会在第二轮回传完整历史：

```json
[
  {"role": "user", "content": "记住一个词：海棠"},
  {"role": "assistant", "content": "已记住"},
  {"role": "user", "content": "刚才让你记住的词是什么？"}
]
```

服务端会用“除最后一条 user 外的历史指纹”查找上一轮保存的 Gemini Web 会话。如果匹配成功，续聊时只把最后一条 user 原文发给 Gemini Web，避免把完整历史重复塞进网页输入框。

### 新话题判断规则

当前判断规则是：

- 只有单条消息且有 `conversation_id`：按 `conversation_id` 复用上下文。
- 有 `conversation_id` 且客户端回传完整历史：先检查历史指纹；如果历史被编辑或无法匹配上一轮记录，会使用完整历史 prompt 重建新的 provider 上下文，避免把修改后的话题错误接到旧 Gemini Web 话题上。
- 无 `conversation_id`，且 `messages` 是完整历史：按历史指纹找回上一轮上下文。
- 历史指纹只有在它仍是该 provider 会话的最新前沿时才会复用。客户端回撤最新 assistant 回答后重试、或从中间分支时，程序不会把新问题追加到已经前进过的 Gemini Web 话题，而会用客户端可见的完整历史重建上下文。
- 无 `conversation_id`，且只有单条新消息：分配新的随机内部会话 ID，不按内容复用旧 ID。
- 进程重启后，内存里的自动上下文映射会丢失；如果客户端回传完整历史，程序会用完整历史 prompt 重建上下文。如果客户端只发最新消息，则无法恢复重启前的服务端上下文。
- 内存上下文缓存有 12 小时 TTL 和 1000 条上限，避免长期运行时无限增长。缓存命中只是优化，不应作为唯一记忆来源。

这避免了“清空/新开话题后，同一句首轮问题错误复用旧 Gemini Web 会话”的问题。

### System Prompt 显示

Gemini Web 当前请求结构没有稳定的隐藏 system 字段。为了不丢 OpenAI `system` 指令，首轮会把 system 作为提示文本拼进输入：

```text
**Persona**: `你是善于使用工具的ai助手`

你好
```

续聊成功后，只发送最新 user 原文，不再带 `User:` 前缀。

## 容错策略

Gemini Web 可能对某些服务端上下文续聊请求返回业务错误，例如 `BardErrorInfo [1097]`。这类响应 HTTP 仍可能是 200，但没有正文。

当前处理策略：

- provider 会识别 `BardErrorInfo`，不再把它当作“正常空回复”。
- provider 会检查续聊响应里的 Gemini `cid` 是否仍是上一轮同一个网页话题；如果在正文输出前发现 `cid` 变化，会把它视为上下文断裂。
- OpenAI 流式接口如果在服务端上下文续聊阶段失败，且还没有输出正文，会自动回退到普通 OpenAI 历史 prompt 再请求一次。
- 回退后客户端能收到回答，但这次请求不再保证落在 Gemini Web 同一个网页话题中。

这类回退是为了优先保证客户端不空白、不假成功。需要排查是否同话题时，请开启调试文件。

OpenAI 实时流式请求会始终携带 Gemini Web 常规流式查询参数（`rt=c`、`hl`、`_reqid`、`bl`、`f.sid`）。续聊默认只在请求体里携带 Gemini conversation metadata 和 context token，不在 URL 上附加 `source-path=/app/<cid>`；实测 `source-path` 在部分账号/模型上容易触发 1097，导致续聊失败并回退成新话题。需要做 A/B 排查时可临时设置 `GEMINI_USE_SOURCE_PATH=true`。

## 排错开关

默认不开启详细日志，避免影响性能和刷屏。

### OpenAI 入站请求摘要

```env
OPENAI_DEBUG_REQUEST_LOG=true
```

开启后日志会打印：

- request_id（用于关联同目录下的原始请求文件和 1097 重试日志）
- model
- stream
- 是否启用 `tools`
- `tool_choice` 解析结果
- 工具数量和强制工具名
- 是否带 `conversation_id`
- message 数量
- 每条 message 的 role、内容长度、内容预览、`tool_calls` 数量、`tool_call_id`、附件数量

### 完整抓包目录

```env
GEMINI_DEBUG_STREAM_DIR=scratch/upstream_debug
```

开启后会写入：

| 文件 | 内容 |
|:---|:---|
| `*_openai_chat_request.json` | 客户端发来的原始 OpenAI 请求 JSON |
| `*.request.json` | 程序发给 Gemini Web 的 URL、headers、form body |
| `*.raw.txt` | Gemini Web 原始响应 |
| `*.chunks.jsonl` | 上游分块读取记录 |
| `*.entries.jsonl` | 解析出的 stream entry 摘要 |
| `*.entry_trace.json` | 关键 entry 到达顺序追踪 |

开启 `GEMINI_DEBUG_STREAM_DIR` 时，OpenAI 入站摘要日志也会自动开启。

### 流式时间线日志

```env
GEMINI_TRACE_STREAM=true
```

用于观察请求准备、首字节、首次解析正文、收尾 idle timeout 等事件。性能测试时应关闭。

### 流式收尾等待

```env
GEMINI_STREAM_FINISH_IDLE_MS=1500
```

OpenAI 实时流式在正文出现后，如果一段时间没有新增正文，会主动结束连接。默认 `1500`，用于避免客户端看到正文完成后长时间等待连接释放。设得太小可能截断长回答或思考模型的中途停顿；设为 `0` 可关闭主动收尾。

## 环境变量

| 变量 | 默认值 | 说明 |
|:---|:---|:---|
| `PORT` | `8787` | 服务端口 |
| `LOG_LEVEL` | `info` | 日志级别 |
| `APP_ENV` | `development` | 运行环境 |
| `COOKIE_SYNC_TOKEN` | 空 | `/admin/*` Cookie 同步接口鉴权 token；为空时 admin 接口拒绝访问 |
| `PROXY_URL` | 空 | 单账号模式的 Google 访问代理，支持 `http://` / `socks5h://` |
| `GEMINI_1PSID` | 空 | 单账号 legacy 模式的 Gemini Web `__Secure-1PSID`；设置 `GEMINI_ACCOUNTS` 后不再必填 |
| `GEMINI_1PSIDTS` | 空 | 单账号 legacy 模式的 Gemini Web `__Secure-1PSIDTS`，可自动轮换 |
| `GEMINI_REFRESH_INTERVAL` | `2` | Cookie 自动刷新间隔，单位分钟 |
| `GEMINI_MAX_RETRIES` | `3` | 非流式生成最大重试次数 |
| `GEMINI_COOKIE_CACHE_ENABLED` | `true` | 是否把 worker 同步成功的账号 Cookie 写入专用缓存文件 |
| `GEMINI_COOKIE_CACHE_PATH` | `data/cookies/accounts.json` | 账号 Cookie 缓存文件；不要提交到 Git |
| `GEMINI_ACCOUNT_STATE_PATH` | `data/state/accounts.json` | active 账号轮换状态文件；用于重启后延续未过期的停留窗口 |
| `GEMINI_ACCOUNT_AUDIT_LOG` | `true` | 简要账号审计日志；只记录账号选择、会话绑定、Cookie 同步和刷新事件，不记录 Cookie 和完整 prompt |
| `GEMINI_COOKIE_WORKER_ENABLED` | `true` | 当全部账号不可用时，主服务是否自动触发外部 Cookie Worker |
| `GEMINI_COOKIE_WORKER_COMMAND` | `npm run sync --silent` | 外部 Cookie Worker 命令；主服务会附加 `COOKIE_WORKER_ACCOUNT` / `COOKIE_WORKER_ONCE=true` 等环境变量 |
| `GEMINI_COOKIE_WORKER_DIR` | `tools/cookie-worker` | 执行外部 Cookie Worker 命令的工作目录 |
| `GEMINI_COOKIE_WORKER_TIMEOUT_SECONDS` | `120` | 单次外部 Cookie Worker 同步超时 |
| `GEMINI_ACCOUNTS` | 空 | 多账号 ID 列表，逗号分隔；为空时使用单账号配置 |
| `GEMINI_ACCOUNT_<ID>_1PSID` | 空 | 指定账号的 `__Secure-1PSID`，`<ID>` 会转大写，非字母数字转 `_` |
| `GEMINI_ACCOUNT_<ID>_1PSIDTS` | 空 | 指定账号的 `__Secure-1PSIDTS` |
| `GEMINI_ACCOUNT_<ID>_PROXY` | 空 | 指定账号的独立代理 |
| `GEMINI_ACCOUNT_<ID>_PRIORITY` | `0` | 账号优先级，数值越大越先作为新话题账号；同时作为停留时间倍率，`<=0` 按 1 计算 |
| `GEMINI_ACCOUNT_<ID>_STAY_MINUTES` | `180` | 指定账号基础停留时间；实际停留时间 = 基础停留时间 × priority 倍率 |
| `GEMINI_ACCOUNT_<ID>_PROFILE_DIR` | 空 | Playwright Cookie Worker 使用的持久浏览器 profile 目录 |
| `RATE_LIMIT_ENABLED` | `true` | 是否开启限流 |
| `RATE_LIMIT_WINDOW_MS` | `60000` | 限流窗口 |
| `RATE_LIMIT_MAX_REQUESTS` | `30` | 限流窗口内最大请求数 |
| `OPENAI_DEBUG_REQUEST_LOG` | `false` | OpenAI 入站请求摘要日志 |
| `GEMINI_DEBUG_STREAM_DIR` | 空 | 上下游请求和响应抓包目录 |
| `GEMINI_TRACE_STREAM` | `false` | 流式时间线日志 |
| `GEMINI_STREAM_FINISH_IDLE_MS` | `1500` | 流式正文 idle 收尾等待；太小可能截断长输出，太大客户端会在正文结束后等待 |
| `GEMINI_WEB_STREAM_QUERY` | `false` | 强制带 Gemini Web 流式查询参数，排查用 |

## Cookie 热更新与 Worker

主服务提供内部 admin 接口，用于 Playwright Cookie Worker 同步账号 Cookie。所有 `/admin/*` 请求都必须带 `COOKIE_SYNC_TOKEN`：

```bash
curl http://localhost:8787/admin/accounts \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN"
```

更新某个账号 Cookie：

```bash
curl -X POST http://localhost:8787/admin/accounts/acc1/cookies \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN" \
  -d '{"secure_1psid":"...","secure_1psidts":"...","source":"playwright-cookie-worker"}'
```

主服务会先用新 Cookie 获取 Gemini Web `SNlM0e` 验证；验证成功才替换当前账号，失败不会覆盖旧 Cookie。请求中发现认证类错误时，该账号会被标记为不可用，新话题自动跳过，后台异步尝试一次 `RotateCookies + refreshSessionToken`。如果启动或请求选择账号时发现没有任何健康账号，主服务会触发外部 Cookie Worker：启动阶段后台重试；运行中请求会优先同步刷新最高优先级账号并等待结果，避免主对话长期不可用。

同步成功后，主服务默认还会写入专用 Cookie cache：`data/cookies/accounts.json`。启动时会先读取 `.env` 得到账户列表、代理、优先级和 profile，再用 cache 中的账号 Cookie 覆盖 `.env` 里的旧 Cookie。`.env` 不会被 worker 改写；运行时状态以 `/admin/accounts` 的 `last_cookie_sync`、`last_validated` 和 `cookie_source` 为准。

Playwright Worker 在 `tools/cookie-worker`，它是独立进程/容器，建议每个账号一个持久 profile 和固定代理：

```bash
cd tools/cookie-worker
npm install
API_BASE=http://127.0.0.1:8787 \
COOKIE_SYNC_TOKEN=你的同步token \
GEMINI_ACCOUNTS=acc1,acc2 \
GEMINI_ACCOUNT_ACC1_PROFILE_DIR=profiles/acc1 \
GEMINI_ACCOUNT_ACC1_PROXY=http://127.0.0.1:8014/ \
GEMINI_ACCOUNT_ACC2_PROFILE_DIR=profiles/acc2 \
GEMINI_ACCOUNT_ACC2_PROXY=http://127.0.0.1:8019/ \
npm start
```

第一次登录、二次验证、风控验证建议人工在对应 profile 中完成。Worker 只负责复用已登录 profile、打开 Gemini 官网、读取 `__Secure-1PSID` / `__Secure-1PSIDTS` 并同步到主服务。

真实测试建议先按单账号执行：

```bash
# 1. 打开 acc1 的持久 profile，手动登录 Gemini；关闭浏览器窗口后退出。
COOKIE_WORKER_ACCOUNT=acc1 npm run login

# 2. 对 acc1 执行一次 Cookie 读取和同步，然后退出。
COOKIE_WORKER_ACCOUNT=acc1 npm run sync

# 3. 全账号同步一次。
npm run sync
```

Windows `cmd.exe` 写法：

```bat
set COOKIE_WORKER_ACCOUNT=acc1
npm run login

set COOKIE_WORKER_ACCOUNT=acc1
npm run sync
```

Worker 会自动读取项目根目录 `.env`。命令行环境变量优先级更高，可临时覆盖 `.env`。

Worker 测试开关：

| 变量 | 说明 |
|:---|:---|
| `COOKIE_WORKER_ACCOUNT` | 只处理指定账号，如 `acc1` |
| `COOKIE_WORKER_OPEN_ONLY` | 只打开浏览器 profile 供人工登录，不同步 Cookie |
| `COOKIE_WORKER_ONCE` | 同步一轮后退出 |
| `COOKIE_WORKER_HEADLESS` | 是否无头运行；第一次登录建议 `false` |
| `COOKIE_WORKER_MIN_INTERVAL_MINUTES` / `COOKIE_WORKER_MAX_INTERVAL_MINUTES` | 常驻模式下随机同步间隔 |

## 端到端测试

`tools/e2e` 用固定程序模拟 OpenAI 兼容客户端，不依赖临时 shell 拼接请求。它会自动构造 UTF-8 JSON、保存每轮 `messages`，并把结果写入 `scratch/e2e-reports/*.json`：

```bash
go run ./tools/e2e \
  -base-url http://127.0.0.1:8787 \
  -scenarios status,multiturn,stream,bom,negative-cookie
```

可选场景：

| 场景 | 说明 |
|:---|:---|
| `status` | 检查 `/admin/accounts` 中所有账号是否 healthy |
| `multiturn` | 按真实客户端方式逐轮追加 `messages`，验证服务端上下文没有丢 |
| `truncated-history` | 模拟客户端第 5 轮开始裁剪早期历史，验证仍复用同一 Gemini 服务端话题 |
| `stream` | 验证 OpenAI 实时流式、Mermaid 代码块、`finish_reason` 和 usage |
| `bom` | 验证带 UTF-8 BOM 的 JSON 请求可以被兼容 |
| `negative-cookie` | 向指定账号提交无效 Cookie，验证失败不会覆盖旧 Cookie 或污染账号状态 |
| `rotation` | 等待轮换窗口后发新话题，再继续旧话题，验证新话题轮换与旧话题连续性 |
| `audit-explicit` | 使用显式 `conversation_id` 发一轮请求，方便在审计日志中按 ID 查账号绑定 |

轮换测试需要把账号 `STAY_MINUTES` 临时设短，或启动一个独立测试端口：

```bash
go run ./tools/e2e \
  -base-url http://127.0.0.1:8799 \
  -scenarios status,rotation,audit-explicit \
  -rotation-wait 75s
```

账号审计日志统一输出为 `Gemini account audit`，常见事件包括 `active_account_selected`、`conversation_bound`、`conversation_reused`、`account_marked_unhealthy`、`background_refresh_started`、`background_refresh_succeeded`、`cookie_sync_ok`、`cookie_sync_failed`。如需降低日志量，可设置：

```env
GEMINI_ACCOUNT_AUDIT_LOG=false
```

## 客户端接入

OpenAI Python SDK：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8787/openai/v1",
    api_key="not-needed",
)

stream = client.chat.completions.create(
    model="gemini-3.5-flash",
    stream=True,
    messages=[{"role": "user", "content": "你好"}],
)

for chunk in stream:
    delta = chunk.choices[0].delta
    if delta.content:
        print(delta.content, end="", flush=True)
```

## 开发验证

```bash
go test ./...
```

## 说明

本项目基于开源项目 [ntthanh2603/gemini-web-to-api: ✨Reverse-engineered API for Gemini web app. It can be used as a genuine API key from OpenAI, Gemini, and Claude.](https://github.com/ntthanh2603/gemini-web-to-api) 修改。由于 Gemini网页端结构也许会变化，任何涉及 `f.req`、`x-goog-ext-*`、`c/r/rc/context token` 的行为都应以抓包和回归测试为准。
