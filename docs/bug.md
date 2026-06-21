1. 话题匹配可能有问题，同一个话题长对话，客户端使用重试极大概率失败，返回1155，1097

---

## 诊断与修复记录（2026-06-21）

### 实测确认的根因

通过 MCP 抓真实 Gemini 网页端请求 + 本服务 `GEMINI_DEBUG_STREAM_DIR` 抓包，确认"丢失开头几轮"与"重试失败 1097/1155"是两个相关但独立的症状：

**核心机制缺陷**：续聊命中后 `plan.Prompt = rawUserPrompt(latest)` 只发最后一条 user（这是 Gemini 续聊协议的硬性要求，不能塞全量历史）。本服务把"续聊命中 = 谷歌侧记忆完整"当作必然成立，但谷歌侧会因风控、轮次截断、context_token 失效等原因**静默丢失早期轮次**，表现为续聊成功（HTTP 200、有正文）但答非所问。

实测复现：让模型记住"海棠/芭蕉/银杏"三轮后回忆，模型答成"苹果/香蕉/西瓜"——续聊 cid 一致、HTTP 200、有正文，但谷歌侧记录已断裂。这种"假成功"无法被现有 fallback（只在 `completionLen==0` 时触发）拦截。

### 已实施的修复（方案三：续聊 + 本地历史兜底）

在 `OPENAI_CONTEXT_LOCAL_FALLBACK=true`（默认开启）下：

1. **provider 层**（`gemini_service.go`）：新增 `conversationUntrusted map[string]bool`。`checkConversationContinuity` 检测到 cid 不匹配（含"正文已发出"的静默不一致）或 `extractBardErrorCode` 命中 1097 等时，`markConversationUntrusted` 标记该 provider 会话。
2. **OpenAI service 层**（`openai_service.go`）：`providerConversationReady` 在 `HasConversationState` 之上额外检查 `IsConversationUntrusted`。被标记的 provider 会话在所有续聊命中点（transcript/suffix/root）都不再被复用，下一轮自动落到"新建 + 本地完整历史 prompt"路径。
3. `rememberRequestContext` 不受 untrusted 影响（用纯 `HasConversationState`），保证即使会话中途被标记，本地 transcript 仍被记录，供后续重建使用。

### 验证

- 单元测试：3 个新测试（provider 层 continuity 标记 + openai 层 skip/legacy 切换）全通过；现有 `planRequestContext` 契约测试（含 truncated-history）不变。
- 全量 `go test ./...` 通过。
- e2e `status,multiturn,stream,bom` 全通过。
- 纯 UTF-8 多轮客户端（5 轮 + 回忆）：准确返回"海棠、芭蕉、银杏"。

### 环境变量

`OPENAI_CONTEXT_LOCAL_FALLBACK`（默认 `true`）：设 `false` 回退旧行为，用于 A/B 或紧急回滚。

### 不在本轮范围

- 持久化指纹表（进程重启丢映射）—— 需更大改动，单独评估。
- Claude / Gemini 原生协议的服务端上下文 —— README 已说明是 OpenAI 专属实验能力。
