# Gemini Web to API<img src="https://th.bing.com/th/id/ODF.nAWZa6qAQb-ILV5Rp8qOrw?w=32&amp;h=32&amp;qlt=90&amp;pcl=fffffa&amp;o=6&amp;pid=1.2" height="32" width="32" alt="全球 Web 图标" class="rms_img" data-bm="32">

把 Gemini 网页端封装成 OpenAI / Claude / Gemini 兼容接口的本地代理服务。

本项目是网页端逆向实现，行为依赖 Google Gemini Web 的私有请求结构。适合个人研究、调试和本地客户端接入，不建议当作稳定生产服务使用。

## 当前能力

| 能力 | OpenAI 兼容 | Claude 兼容 | Gemini 原生兼容 | 说明 |
|:---|:---:|:---:|:---:|:---|
| 普通文本对话 | 支持 | 支持 | 支持 | 三种协议都会转发到 Gemini  |
| 流式输出 | **实时流式** | 模拟流式 | 模拟流式 | 只有 OpenAI 兼容接口接入 provider 实时流 |
| Thinking Level | **支持** | 未接入 | 未接入 | OpenAI 支持 `reasoning_effort` / `reasoning.effort` / `thinking_level` |
| 思考内容输出 | 支持 | 未接入 | 未接入 | OpenAI 流式通过 `delta.reasoning_content` 输出 |
| 服务端多轮上下文 | **实验性支持** | 未接入 | 未接入 | 复用 Gemini Web 的 `c/r/rc/context token`，可让多轮对话落在同一 Gemini Web 记录；该能力依赖网页端私有状态，偶发 1097/1060 或记录断裂时会回退 |
| 图片/文件输入 | 支持 | 支持 | 支持 | OpenAI 支持 data URL、远程 URL、`file_id` 和 `/files` 上传；文件会先上传到 Google `content-push` |
| 图片生成 | **暂不支持** | 不支持 | 不支持 | OpenAI `/images/generations` |
| 工具调用 | **实验性桥接** | 桥接支持 | 桥接支持 | 通过 prompt 约束输出工具调用 JSON，非 Gemini Web 原生 tool calling；复杂多轮和格式稳定性弱于纯聊天 |

## 快速启动

复制配置：

```bash
cp .env.example .env
```

至少填写：

```env
PORT=8787
COOKIE_SYNC_TOKEN=你的管理密钥
PROXY_URL=http://127.0.0.1:10808（视环境可选）
```

如果服务运行在 Docker 容器里，`127.0.0.1` 指的是容器自身，不是宿主机。宿主机上的代理建议写成：

```env
PROXY_URL=http://host.docker.internal:10808
GEMINI_ACCOUNT_MAIN_PROXY=http://host.docker.internal:10808
```

`docker-compose.yml` 已内置 `host.docker.internal:host-gateway` 映射，Linux Docker 也可以用这个地址。宿主机代理软件本身还需要允许 Docker 网桥访问，不能只监听宿主机 `127.0.0.1`。

单账号 legacy 模式可直接配置文件中填写：

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

### Web 控制台

服务内置一个 Web 控制台，用于在浏览器中可视化管理账号、测试代理和验证模型可用性：

```text
http://localhost:8787/console
```

控制台功能：

| 功能 | 说明 |
|:---|:---|
| 账号列表 | 展示所有账号的状态（healthy / refreshing / expired）、代理、最后同步时间和健康度 |
| 添加账号 | 填写 Account ID、`__Secure-1PSID`、`__Secure-1PSIDTS` 和代理地址，在线添加新账号 |
| 编辑账号 | 更新 Cookie 和代理地址 |
| 删除账号 | 从运行池中移除指定账号 |
| 刷新账号 | 触发 `RotateCookies + refreshSessionToken`，重新获取会话令牌 |
| 账号测试 | 向指定账号发送真实对话消息（"Hi, please reply with only the word: OK"），端到端验证模型是否真正可用，返回回复内容和延迟 |
| 代理测试 | 在添加/编辑账号时，点击「测试代理」按钮验证代理地址是否可达，通过该代理请求 `https://gemini.google.com` 并返回 HTTP 状态码和延迟 |

控制台使用 `COOKIE_SYNC_TOKEN` 鉴权，在登录页输入该 token 即可进入。

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

文件上传后引用：

```bash
curl http://localhost:8787/openai/v1/files \
  -F purpose=assistants \
  -F file=@./note.txt
```

然后在 Chat Completions 中使用返回的 `file_id`：

```json
{
  "model": "gemini-3.5-flash",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "input_text", "text": "请总结这个文件"},
        {"type": "input_file", "file_id": "file-...", "filename": "note.txt", "mime_type": "text/plain"}
      ]
    }
  ]
}
```

也支持直接内联文件：

```json
{"type": "input_file", "filename": "note.txt", "file_data": "data:text/plain;base64,..."}
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

| 可传模型名 | 传参方式 | 说明 |
|:---|:---|:---|
| `gemini-3.5-flash` | 映射到 UI 内部 ID | UI 可见模型 |
| `gemini-3.1-flash-lite` | 映射到 UI 内部 ID | UI 可见模型 |
| `gemini-3.1-pro` | 映射到 UI 内部 ID | UI 可见模型 |

原项目会从 Gemini 初始化页面里扫描 `gemini-*` 字符串并暴露出来。本项目没有照搬这个策略，因为当前实时流式路径的模型选择依赖 Gemini Web 的 `x-goog-ext-525001261-jspb` header，直接暴露页面里所有字符串容易让客户端误以为存在更多独立可选模型。

抓包里出现的 `3.5 Flash 扩展` 更接近 Gemini Web 的扩展/深思档位表现，不作为独立模型暴露。客户端需要深度思考时应使用 Thinking Level，例如 `reasoning_effort: "high"` 或 `thinking_level: "extended"`。

## 多轮上下文

OpenAI Chat Completions 本身通常是无状态协议。这里有两种方式接入 Gemini Web 服务端上下文。

先明确限制：Gemini Web 服务端上下文不是公开 API，稳定性不等同于 OpenAI 官方 Chat Completions。它的优势是“命中时”可以让同一客户端话题落在 Gemini Web 同一条记录里，并且续聊只发送最新 user 文本，速度和上下文一致性都更好；缺点是它会受账号、网页端状态、Google 风控和上游内部字段变化影响。

如果服务端上下文续聊失败，OpenAI 流式接口会在还没输出正文时自动回退到完整历史 prompt。这个回退优先保证客户端拿到回答，但它可能创建新的 Gemini Web 记录，因此不要把“网页端始终同一条记录”当作强保证。

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
- 有 `conversation_id` 且客户端只发最新消息：优先复用该客户端 ID 当前绑定的 provider 会话；如果上游续聊失败并回退到新 provider 会话，成功后会把同一个客户端 ID 重绑定到新 provider 会话，避免后续请求反复打到旧坏记录。
- 有 `conversation_id` 且客户端回传完整历史：先检查历史指纹；如果历史被编辑或无法匹配上一轮记录，会使用完整历史 prompt 重建新的 provider 上下文，避免把修改后的话题错误接到旧 Gemini Web 话题上。
- 无 `conversation_id`，且 `messages` 是完整历史：按历史指纹找回上一轮上下文。
- 无 `conversation_id`，且客户端只保留最近若干轮历史：会依次尝试完整前缀、尾部历史、最近 2-4 条 user 消息窗口、根话题指纹。最近 user 窗口会忽略 assistant 文本的轻微差异，适配 Cherry Studio 这类默认只发送最近五轮的客户端；单条 user 消息不会生成该宽松窗口，降低新话题串线风险。
- 历史指纹只有在它仍是该 provider 会话的最新前沿时才会复用。客户端回撤最新 assistant 回答后重试、或从中间分支时，程序不会把新问题追加到已经前进过的 Gemini Web 话题，而会用客户端可见的完整历史重建上下文。
- 无 `conversation_id`，且只有单条新消息：分配新的随机内部会话 ID，不按内容复用旧 ID。
- 进程重启后，内存里的自动上下文映射会丢失；如果客户端回传完整历史，程序会用完整历史 prompt 重建上下文。如果客户端只发最新消息，则无法恢复重启前的服务端上下文。
- 内存上下文缓存有 12 小时 TTL 和 1000 条上限，避免长期运行时无限增长。缓存命中只是优化，不应作为唯一记忆来源。

这避免了“清空/新开话题后，同一句首轮问题错误复用旧 Gemini Web 会话”的问题。

## 工具调用桥接

OpenAI `tools` / `tool_choice` 在本项目中不是 Gemini Web 原生工具能力，而是桥接能力。程序会把客户端传入的工具 schema 转成一段临时 Gemini 规划提示，让 Gemini 决定是否输出：

```json
{"tool_calls":[{"name":"tool_name","arguments":{}}]}
```

如果输出匹配，程序再把它转换成 OpenAI 标准 `tool_calls` 返回给客户端。客户端执行工具后，再把 `role: "tool"` 结果发回，程序会让 Gemini 基于工具结果生成最终回答。

当前设计原则：

- 无 `tools` 字段时完全跳过工具桥接，普通聊天保持直接实时流式。
- 工具规划走临时 Gemini 会话，不绑定主 Gemini Web 对话，避免工具 schema 长期污染正文会话记录。
- 工具执行结果回来后的最终总结会走主对话路径，因此可以继续利用服务端多轮上下文，但仍受上面“不稳定时回退”的限制。
- 工具参数会做基础防御性清洗，例如把模型误输出的 Markdown URL 链接还原成裸 URL；但复杂 JSON 格式仍可能被上游模型写错。

已知限制：

- 工具调用阶段可能需要先缓冲一小段上游输出，用来判断它是在输出工具 JSON 还是正文，因此工具场景不一定像纯聊天一样连续真流式。
- 如果模型没有严格输出 JSON，程序会尽量当作普通正文处理；强制工具场景可能返回兜底工具调用或失败。
- 多轮工具对话会同时引入工具 schema、工具结果、客户端历史和 Gemini Web 服务端上下文，复杂度明显高于纯聊天，更容易触发格式错乱、上下文断裂或上游空回复。

因此，推荐只在确实需要搜索、MCP 或外部系统数据时传 `tools`。如果目标是稳定、快速的聊天体验，关闭工具字段效果最好。

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
| `*_openai_upstream_trace.json` | OpenAI 请求摘要、程序选择的 provider conversation、prompt 摘要、附件摘要、回退原因 |
| `*.request.json` | 程序发给 Gemini Web 的 URL、headers、form body |
| `*.raw.txt` | Gemini Web 原始响应 |
| `*.chunks.jsonl` | 上游分块读取记录 |
| `*.entries.jsonl` | 解析出的 stream entry 摘要 |
| `*.entry_trace.json` | 关键 entry 到达顺序追踪 |

开启 `GEMINI_DEBUG_STREAM_DIR` 时，OpenAI 入站摘要日志也会自动开启。抓包会写磁盘并保存 prompt 预览，只用于定位问题；长对话压测和高并发日常使用应关闭。

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
| `GEMINI_STARTUP_COOKIE_ROTATE` | `true` | 启动时是否串行执行 `RotateCookies`；Docker/远端服务已有缓存时建议设 `false` 加速启动 |
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
| `GEMINI_STREAM_FIRST_CONTENT_TIMEOUT_MS` | `45000` | OpenAI 流式首个可解析正文最大等待时间；避免上游空流导致客户端长时间无输出；设 `0` 可关闭 |
| `GEMINI_WEB_STREAM_QUERY` | `false` | 强制带 Gemini Web 流式查询参数，排查用 |
| `OPENAI_CONTEXT_LOCAL_FALLBACK` | `true` | 当 Gemini 服务端会话被标记为不可信（连续性不一致或 bard error 1097/1060）时，下一轮从本地完整历史重建而非续聊坏记录；修复"丢失开头几轮"；设 `false` 回退旧行为做 A/B |

### Docker 内代理连通性排查

容器内判断代理是否可用，核心是确认容器能连到代理监听地址，而不是只看宿主机能不能访问。

```bash
docker compose exec app sh
```

进入容器后先看配置是否进入容器：

```sh
env | grep -E '^(PROXY_URL|GEMINI_ACCOUNT_.*_PROXY)='
```

如果代理在宿主机 `10808` 端口，先测容器到代理端口：

```sh
wget -S -O- --timeout=5 http://host.docker.internal:10808
```

这一步只要能连上端口即可，返回 `400`、`405` 或代理自己的错误页都说明网络路径通了；如果是 `Connection refused` 或超时，说明容器到宿主机代理不通，通常是代理只监听了宿主机 `127.0.0.1`，需要改成监听 `0.0.0.0` 或允许 Docker 网桥地址。

再强制通过代理访问 Google：

```sh
HTTPS_PROXY=http://host.docker.internal:10808 wget -S -O- --timeout=15 https://www.google.com/generate_204
```

返回 `204 No Content` 或能看到 Google 响应头，说明容器内代理可用。主服务日志里的 `proxy_enabled=true` 和脱敏 `proxy=http://host.docker.internal:10808` 只能证明程序使用了代理配置；真正能否出网要以上面容器内测试为准。

也可以在 Web 控制台（`/console`）的账号编辑抽屉中直接点击「测试代理」按钮，无需进入容器即可验证代理连通性。

## Docker 部署

### 构建与启动

```bash
docker compose up -d --build
```

`Dockerfile` 使用多阶段构建，已内置以下优化：

- **BuildKit 缓存挂载**（`--mount=type=cache`）：Go 模块和编译缓存跨构建复用，增量构建更快。
- **GOPROXY**：默认使用 `goproxy.cn` 加速模块下载，可通过 `--build-arg GOPROXY=off` 关闭。
- **静态文件嵌入**：`console.html` 通过 `go:embed` 编译进二进制，无需额外 COPY。

### 配置热更新

`docker-compose.yml` 已将 `.env` 以只读方式挂载到容器内（`./.env:/home/appuser/.env:ro`）。修改 `.env` 后只需重启容器即可生效，**无需重新构建镜像**：

```bash
# 修改 .env 后执行
docker compose restart
```

服务启动时会通过 `godotenv.Overload()` 读取 `.env` 并覆盖 `env_file` 中的环境变量。

### 数据持久化

| 挂载路径 | 容器路径 | 说明 |
|:---|:---|:---|
| `./data` | `/home/appuser/data` | Cookie 缓存（`data/cookies/accounts.json`）和账号轮换状态（`data/state/accounts.json`） |
| `./.cookies` | `/home/appuser/.cookies` | 硬编码的 Cookie jar 目录 |
| `./.env` | `/home/appuser/.env` (ro) | 配置文件，只读挂载，重启即生效 |

### 健康检查

`docker-compose.yml` 内置健康检查，每 30 秒请求 `http://localhost:8787/health`。注意使用的是 `wget -O /dev/null` 而非 `--spider`，因为 `/health` 是 GET-only 路由。

### 容器内注意事项

- 镜像不包含 Node.js / Chromium，因此 `GEMINI_COOKIE_WORKER_ENABLED` 默认在 `docker-compose.yml` 中设为 `false`。Cookie 同步应在宿主机或独立容器中运行 Worker。
- `GEMINI_STARTUP_COOKIE_ROTATE` 默认设为 `false`，避免多个账号启动时串行 `RotateCookies` 拖慢容器启动。确保 `./data` 已挂载且 Cookie 缓存已同步。

## Cookie 热更新与 Worker

主服务提供内部 admin 接口，用于 Playwright Cookie Worker 同步账号 Cookie。所有 `/admin/*` 请求都必须带 `COOKIE_SYNC_TOKEN`（通过 `X-Cookie-Sync-Token` header 或 `Authorization: Bearer <token>`）：

### 账号管理 API

```bash
# 列出所有账号状态
curl http://localhost:8787/admin/accounts \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN"

# 更新某个账号 Cookie
curl -X POST http://localhost:8787/admin/accounts/acc1/cookies \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN" \
  -d '{"secure_1psid":"...","secure_1psidts":"...","source":"playwright-cookie-worker"}'

# 添加新账号（运行时动态添加，不需重启）
curl -X POST http://localhost:8787/admin/accounts \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN" \
  -d '{"account_id":"acc3","secure_1psid":"...","secure_1psidts":"...","proxy_url":"http://host.docker.internal:10808"}'

# 删除账号
curl -X DELETE http://localhost:8787/admin/accounts/acc3 \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN"

# 更新账号代理
curl -X POST http://localhost:8787/admin/accounts/acc1/proxy \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN" \
  -d '{"proxy_url":"http://host.docker.internal:10809"}'

# 刷新账号（RotateCookies + refreshSessionToken）
curl -X POST http://localhost:8787/admin/accounts/acc1/refresh \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN"
```

### 账号测试 API

向指定账号发送真实对话消息，端到端验证模型是否真正可用（不仅仅是模型列表）：

```bash
curl -X POST http://localhost:8787/admin/accounts/acc1/test \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN"
```

响应：

```json
{
  "status": "ok",
  "account": "acc1",
  "latency": 2345,
  "reply": "OK"
}
```

该接口会通过指定账号的 Gemini Web 客户端发送一条测试消息（"Hi, please reply with only the word: OK"），返回模型回复文本和延迟。如果账号不健康或模型不可用，返回 400 和错误信息。

### 代理测试 API

测试代理地址是否可达。通过指定代理请求 `https://gemini.google.com`，返回 HTTP 状态码和延迟：

```bash
curl -X POST http://localhost:8787/admin/proxy-test \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $COOKIE_SYNC_TOKEN" \
  -d '{"proxy_url":"http://host.docker.internal:10808"}'
```

响应：

```json
{
  "status": "ok",
  "latency": 523,
  "http_code": 200,
  "proxy_url": "http://host.docker.internal:10808"
}
```

任何 HTTP 响应（包括 302/403）都说明代理网络路径可达。只有网络不可达或超时时才返回 `status: "fail"`。

主服务会先用新 Cookie 获取 Gemini Web `SNlM0e` 验证；验证成功才替换当前账号，失败不会覆盖旧 Cookie。请求中发现认证类错误时，该账号会被标记为不可用，新话题自动跳过，后台异步尝试一次 `RotateCookies + refreshSessionToken`。如果启动或请求选择账号时发现没有任何健康账号，主服务会触发外部 Cookie Worker：启动阶段后台重试；运行中请求会优先同步刷新最高优先级账号并等待结果，避免主对话长期不可用。

同步成功后，主服务默认还会写入专用 Cookie cache：`data/cookies/accounts.json`。启动时会先读取 `.env` 得到账户列表、代理、优先级和 profile，再用 cache 中的账号 Cookie 覆盖 `.env` 里的旧 Cookie。`.env` 不会被 worker 改写；运行时状态以 `/admin/accounts` 的 `last_cookie_sync`、`last_validated` 和 `cookie_source` 为准。

Playwright Worker 在 `tools/cookie-worker`，它是独立进程/容器，建议每个账号一个持久 profile 和固定代理：

Worker 还可以单独放一份本地配置文件 `tools/cookie-worker/.env`，优先读取它，再回退到仓库根目录 `.env`。这样 worker 的账号、profile、代理和 token 就不用每次手工输入。

普通 `sync` 模式下，Worker 会先请求 `API_BASE/admin/accounts`，以主服务返回的账号状态为准。远端返回 `healthy` 或 `refreshing` 的账号会跳过，不会打开本地浏览器 profile；只有远端状态缺失、过期或未初始化的账号才会打开 profile 抓取 Cookie。`API_BASE` 指向 VPS 时，VPS 是状态权威，本地状态文件不会参与判断。远端状态接口不可达时，Worker 默认失败退出，避免网络短暂异常导致本地批量打开所有 profile；需要手动强制刷新时设置 `COOKIE_WORKER_FORCE=true`。

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
如果存在 `tools/cookie-worker/.env`，worker 会先读取它，再读取项目根目录 `.env` 作为兜底。

Worker 测试开关：

| 变量 | 说明 |
|:---|:---|
| `COOKIE_WORKER_ACCOUNT` | 只处理指定账号，如 `acc1` |
| `COOKIE_WORKER_OPEN_ONLY` | 只打开浏览器 profile 供人工登录，不同步 Cookie |
| `COOKIE_WORKER_ONCE` | 同步一轮后退出 |
| `COOKIE_WORKER_FORCE` | 强制打开并同步目标账号；忽略远端 `/admin/accounts` 的 healthy 状态 |
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

维护者入口：

- [Core Bridge Handoff](docs/core-bridge-handoff.md)：接手前先读，列出核心边界和改动入口。
- [OpenAI to Gemini Web Stream Pipeline](docs/openai-gemini-stream-pipeline.md)：完整流程说明，覆盖 OpenAI SSE、Gemini Web 上游解析、工具桥接和多轮上下文回退。

## 说明

本项目基于开源项目 [ntthanh2603/gemini-web-to-api: ✨Reverse-engineered API for Gemini web app. It can be used as a genuine API key from OpenAI, Gemini, and Claude.](https://github.com/ntthanh2603/gemini-web-to-api) 修改。由于 Gemini网页端结构也许会变化，任何涉及 `f.req`、`x-goog-ext-*`、`c/r/rc/context token` 的行为都应以抓包和回归测试为准。
