# Gemini Web to API<img src="https://th.bing.com/th/id/ODF.nAWZa6qAQb-ILV5Rp8qOrw?w=32&amp;h=32&amp;qlt=90&amp;pcl=fffffa&amp;o=6&amp;pid=1.2" height="32" width="32" alt="全球 Web 图标" class="rms_img" data-bm="32">

把 Gemini 网页端封装成 OpenAI / Claude / Gemini 兼容接口的本地代理服务。

本项目是网页端逆向实现，行为依赖 Google Gemini Web 的私有请求结构。适合个人研究、调试和本地客户端接入，不建议当作稳定生产服务使用。

## 当前能力

| 能力 | OpenAI 兼容 | Claude 兼容 | Gemini 原生兼容 | 说明 |
|:---|:---:|:---:|:---:|:---|
| 普通文本对话 | 支持 | 支持 | 支持 | 三种协议都会转发到 Gemini Web |
| 流式输出 | 实时流式 | 模拟流式 | 模拟流式 | 只有 OpenAI 兼容接口接入 provider 实时流 |
| Thinking Level | **支持** | 未接入 | 未接入 | OpenAI 支持 `reasoning_effort` / `reasoning.effort` / `thinking_level` |
| 思考内容输出 | 支持 | 未接入 | 未接入 | OpenAI 流式通过 `delta.reasoning_content` 输出 |
| 服务端多轮上下文 | **实验性支持** | 未接入 | 未接入 | 复用 Gemini Web 的 `c/r/rc/context token`，一轮对话只有一条消息记录 |
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
GEMINI_1PSID=你的 __Secure-1PSID
GEMINI_1PSIDTS=可选，留空时程序会尝试自动轮换
PROXY_URL=http://127.0.0.1:10808
```

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

OpenAI 实时流式请求会始终携带 Gemini Web 常规流式查询参数（`rt=c`、`hl`、`_reqid`、`bl`、`f.sid`）。续聊时额外携带 `source-path=/app/<cid>`；新话题或重建上下文时不带 `source-path`。这样可以避免新话题请求退化成只带 `at` 的非标准路径，降低“客户端收到回答但网页端不落话题”的风险。

## 排错开关

默认不开启详细日志，避免影响性能和刷屏。

### OpenAI 入站请求摘要

```env
OPENAI_DEBUG_REQUEST_LOG=true
```

开启后日志会打印：

- model
- stream
- 是否带 `conversation_id`
- message 数量
- 每条 message 的 role、内容长度、内容预览、附件数量

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
GEMINI_STREAM_FINISH_IDLE_MS=15000
```

OpenAI 实时流式在正文出现后，如果一段时间没有新增正文，会主动结束连接。默认 `15000`，设得太小可能截断长回答或思考模型的中途停顿；设为 `0` 可关闭主动收尾。

## 环境变量

| 变量 | 默认值 | 说明 |
|:---|:---|:---|
| `PORT` | `8787` | 服务端口 |
| `LOG_LEVEL` | `info` | 日志级别 |
| `APP_ENV` | `development` | 运行环境 |
| `PROXY_URL` | 空 | Google 访问代理，支持 `http://` / `socks5h://` |
| `GEMINI_1PSID` | 必填 | Gemini Web `__Secure-1PSID` |
| `GEMINI_1PSIDTS` | 空 | Gemini Web `__Secure-1PSIDTS`，可自动轮换 |
| `GEMINI_REFRESH_INTERVAL` | `2` | Cookie 自动刷新间隔，单位分钟 |
| `GEMINI_MAX_RETRIES` | `3` | 非流式生成最大重试次数 |
| `RATE_LIMIT_ENABLED` | `true` | 是否开启限流 |
| `RATE_LIMIT_WINDOW_MS` | `60000` | 限流窗口 |
| `RATE_LIMIT_MAX_REQUESTS` | `30` | 限流窗口内最大请求数 |
| `OPENAI_DEBUG_REQUEST_LOG` | `false` | OpenAI 入站请求摘要日志 |
| `GEMINI_DEBUG_STREAM_DIR` | 空 | 上下游请求和响应抓包目录 |
| `GEMINI_TRACE_STREAM` | `false` | 流式时间线日志 |
| `GEMINI_STREAM_FINISH_IDLE_MS` | `15000` | 流式正文 idle 收尾等待；太小可能截断长输出 |
| `GEMINI_WEB_STREAM_QUERY` | `false` | 强制带 Gemini Web 流式查询参数，排查用 |

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
