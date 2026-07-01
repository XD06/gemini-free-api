# Gemini Web to API <img src="https://th.bing.com/th/id/ODF.nAWZa6qAQb-ILV5Rp8qOrw?w=32&amp;h=32&amp;qlt=90&amp;pcl=fffffa&amp;o=6&amp;pid=1.2" height="32" width="32" alt="logo" align="left" hspace="8" vspace="4">

把 Gemini 网页端封装成 OpenAI / Claude / Gemini 兼容接口的本地代理服务。

基于网页端逆向实现，依赖 Google Gemini Web 的私有请求结构。适合个人研究和本地客户端接入。

## 能力概览

| 能力 | OpenAI | Claude | Gemini 原生 |
|:---|:---:|:---:|:---:|
| 文本对话 | ✅ | ✅ | ✅ |
| 流式输出 | **实时流** | 模拟流 | 模拟流 |
| Thinking Level | ✅ | — | — |
| 多轮上下文 | 实验性 | — | — |
| 图片/文件输入 | ✅ | ✅ | ✅ |
| 工具调用 | 实验性桥接 | 桥接 | 桥接 |

## 快速开始

```bash
cp .env.example .env
# 编辑 .env，至少填写 COOKIE_SYNC_TOKEN
go run cmd/server/main.go
```

服务默认监听 `http://localhost:8787`，API 文档 `http://localhost:8787/docs`。

### 基础配置

**获取cookie**：

1. 请访问[gemini.google.com](https://gemini.google.com/)并登录
2. 按`F12` →**Application**→**Storage**→ **Cookies**
3. 复制`__Secure-1PSID`和`__Secure-1PSIDTS`的值

```env
PORT=8787
COOKIE_SYNC_TOKEN=你的管理密钥
PROXY_URL=http://127.0.0.1:10808
```

单账号模式直接填写 Cookie：

```env
GEMINI_1PSID=你的 __Secure-1PSID
GEMINI_1PSIDTS=可选，留空自动轮换
```

多账号模式：

```env
GEMINI_ACCOUNTS=main,backup1
GEMINI_ACCOUNT_MAIN_1PSID=主号 __Secure-1PSID
GEMINI_ACCOUNT_MAIN_PROXY=socks5h://127.0.0.1:10808
GEMINI_ACCOUNT_MAIN_PRIORITY=3

GEMINI_ACCOUNT_BACKUP1_1PSID=备用号 __Secure-1PSID
GEMINI_ACCOUNT_BACKUP1_PROXY=http://127.0.0.1:10809
```

> Docker 环境下代理地址用 `http://host.docker.internal:10808`，`docker-compose.yml` 已内置映射。

## Web 控制台

访问 `http://localhost:8787/console`，使用 `COOKIE_SYNC_TOKEN` 登录。

![Console](./asset/console.png)

控制台功能：

- 账号列表：状态、代理、同步时间、健康度
- 添加 / 编辑 / 删除账号
- 账号测试：发送真实对话消息验证可用性
- 代理测试：验证代理连通性

### Playground

控制台内置 Playground 聊天界面，支持模型切换、Thinking Level 调节和流式对话测试。

![Playground](./asset/playground.gif)

## 调用示例

### 流式对话

```bash
curl http://localhost:8787/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-3.5-flash",
    "stream": true,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

### Thinking Level

```json
{
  "model": "gemini-3.5-flash",
  "reasoning_effort": "high",
  "stream": true,
  "messages": [{"role": "user", "content": "详细分析这个问题"}]
}
```

| 参数 | 值 | 对应 |
|:---|:---|:---|
| `reasoning_effort` | `low` / `medium` | Standard |
| `reasoning_effort` | `high` | Extended |
| `thinking_level` | `standard` / `extended` | 直接映射 |

### 文件上传

```bash
curl http://localhost:8787/openai/v1/files \
  -F purpose=assistants \
  -F file=@./note.txt
```

返回 `file_id` 后在 messages 中引用：

```json
{
  "role": "user",
  "content": [
    {"type": "input_text", "text": "总结这个文件"},
    {"type": "input_file", "file_id": "file-...", "filename": "note.txt"}
  ]
}
```

### Python SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8787/openai/v1", api_key="not-needed")

stream = client.chat.completions.create(
    model="gemini-3.5-flash",
    stream=True,
    messages=[{"role": "user", "content": "你好"}],
)

for chunk in stream:
    if chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="", flush=True)
```

## 模型列表

| 模型名 | 说明 |
|:---|:---|
| `gemini-3.5-flash` | UI 可见模型 |
| `gemini-3.1-flash-lite` | UI 可见模型 |
| `gemini-3.1-pro` | UI 可见模型 |

需要深度思考时使用 Thinking Level 参数，而非依赖模型名后缀。

## Docker 部署

```bash
docker compose up -d --build
```

- 多阶段构建，BuildKit 缓存挂载，`GOPROXY=goproxy.cn`
- `.env` 只读挂载，修改后 `docker compose restart` 即可生效
- `console.html` 通过 `go:embed` 编入二进制

| 挂载 | 说明 |
|:---|:---|
| `./data` | Cookie 缓存和账号状态 |
| `./.env` (ro) | 配置文件 |
| `./.cookies` | Cookie jar |

## Admin API

所有 `/admin/*` 请求需带 `Authorization: Bearer <COOKIE_SYNC_TOKEN>`。

```bash
# 列出账号
curl http://localhost:8787/admin/accounts -H "Authorization: Bearer $TOKEN"

# 添加账号
curl -X POST http://localhost:8787/admin/accounts \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"account_id":"acc3","secure_1psid":"...","proxy_url":"..."}'

# 测试账号
curl -X POST http://localhost:8787/admin/accounts/acc1/test -H "Authorization: Bearer $TOKEN"

# 测试代理
curl -X POST http://localhost:8787/admin/proxy-test \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"proxy_url":"http://host.docker.internal:10808"}'
```

## 开发

```bash
go test ./...
```

维护者文档：

- [技术细节](docs/technical-details.md) — 多轮上下文、工具桥接、Cookie 快速同步与 Worker、容错策略、排错开关、环境变量完整列表
- [Core Bridge Handoff](docs/core-bridge-handoff.md) — 核心边界和改动入口
- [Stream Pipeline](docs/openai-gemini-stream-pipeline.md) — 完整流程说明

## 说明

基于 [ntthanh2603/gemini-web-to-api](https://github.com/ntthanh2603/gemini-web-to-api) 修改。Gemini 网页端结构可能变化，涉及 `f.req`、`x-goog-ext-*`、`c/r/rc/context token` 的行为以抓包和回归测试为准。
