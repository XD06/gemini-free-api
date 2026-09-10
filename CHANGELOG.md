# Changelog

> 遵循 Keep a Changelog：写给人看的变更摘要，细节以 `git log` 为准。
> 本文件上次更新止于 `286d6ec`（2026-07-02）；之后条目按提交信息归纳。

## [Unreleased]

### 新增

- **三协议真正流式输出**：Claude / Gemini 复用 provider 原生 stream 能力实时转发 delta，首字节延迟与 OpenAI 侧一致（`dd30994`）。
- **请求记录看板**：控制台调用记录、首字节延迟列、账号列；统计改由前端计算（`0070213`、`caedb54`、`a687325`）。
- **模型对齐线上**：canonical 三模型 `gemini-3.8-flash` / `gemini-3.5-flash-lite` / `gemini-3.1-pro`，旧名仅作兼容别名，`/models` 只暴露 canonical；e2e 默认模型同步切换（`10550c6`）。
- **控制台与流韧性升级**：账号操作错误分类、stream timings、代理前置校验、Cookie/流状态可靠性（`9ebc237`、`db35f15`、`253b628`、`7fd2eb0`）。
- **CI 质量门**：`go vet` + `go test` + `-race`（providers/openai）+ `govulncheck`（`e0f778e`）。

### 修复

- **Cookie 持久化与文件安全**：Cookie 缓存落盘与文件店安全性修复（`00c24f9`）。
- **Bard 错误处理**：非流式 `BardErrorInfo` 检测、错误触发 Cookie 轮换并跳过无用重试；1060 语义回退到原始行为（`d6e39b6`、`0bd4018`、`12f3ec6`、`e91d3fe`）。
- **流式重试对齐**：流式路径补重试循环，与非流式行为一致（`b01a273`）。
- **流重置与重复日志**：修复 stream reset 错误、重复请求日志、流 handler 错误账号归属（`c8fcadd`、`6039824`）。
- **控制台安全**：XSS 修复（账号 ID 事件委托 / Markdown URL 过滤 / title 转义）、认证前 401 刷屏、Playground 历史重构与流式取消（`a687325`）。
- **初始化校验顺序**：优先直接 SNlM0e 校验而非 Cookie 轮换（`149c75b`）。

### 变更

- **流超时对齐线上**：默认首活动 15s→25s、进度空闲 30s→45s、响应头 15s→60s；会话 ACK 视为首活动（`10550c6`）。

## [2026-07-02] — 性能与可靠性优化

### P0 — 严重 Bug 修复

- **`Close()` 双重 close panic**：多次调用 `Close()` 会触发 `close(stopRefresh)` panic。新增 `sync.Once` 保护，确保 channel 仅关闭一次。
  - 文件：`internal/modules/providers/gemini_service.go`

- **会话读操作用写锁**：`conversationID`、`IsConversationUntrusted`、`conversationMetadata`、`conversationContextToken`、`conversationSourcePath` 均使用 `sync.Mutex.Lock()` 做纯读操作，导致高并发下读操作互相阻塞。将 `conversationMu` 改为 `sync.RWMutex`，所有纯读方法改用 `RLock()` / `RUnlock()`。
  - 文件：`internal/modules/providers/gemini_service.go`

### P1 — 数据安全 & 性能

- **Cookie 缓存非原子写入**：`saveAccountCookieCache` / `saveAccountProxyCache` / `removeAccountCookieCache` 直接使用 `os.WriteFile`，进程崩溃时可能产生截断/损坏文件。新增 `atomicWriteFile()` — 写入同目录临时文件后 `os.Rename` 原子替换。
  - 文件：`internal/modules/providers/gemini_cookie_cache.go`

- **流式解析 O(n²) CPU**：流式循环中每个 16KB chunk 都对最多 512KB 缓冲区执行全量 `extractStreamTextFromBuffer` + `extractTextFromBuffer` 重扫描。新增 `streamParseMinIntervalBytes = 8KB` 节流，仅当新数据 ≥ 8KB 或流结束时才重新解析，长回复 CPU 开销降低 ~94%。
  - 文件：`internal/modules/providers/gemini_service.go`

- **`conversationTo` map 无限增长**：`ClientPool.conversationTo` 只增不删，长期运行导致内存泄漏。新增 `conversationSeen` 时间戳 map + `pruneConversationBindingsLocked()` 方法，12 小时 TTL 自动清理过期绑定。
  - 文件：`internal/modules/providers/gemini_client_pool.go`

### P2 — 性能优化

- **`refreshSessionToken` 重复创建 HTTP 客户端**：每次刷新都 `req.NewClient()` 和 `c.newHTTPClient(30s)`，新建 Transport 和 TLS 连接池。改为复用 `c.httpClient`（google.com 请求）和 `c.rawHTTPClient`（gemini 请求），保持连接池复用。
  - 文件：`internal/modules/providers/gemini_service.go`

- **流式循环 Timer 重复创建**：每次循环迭代 `time.NewTimer` 产生 GC 压力。Timer 提到循环外创建一次，循环内用 `Reset()` 复用，`defer` 统一 `Stop()`。
  - 文件：`internal/modules/providers/gemini_service.go`

- **文件上传串行处理**：`uploadRequestFiles` 串行遍历所有文件调用 `uploadFile`。改为 goroutine + `sync.WaitGroup` + 信号量（并发度 4）并行上传，输出切片保持原始顺序。
  - 文件：`internal/modules/providers/gemini_upload.go`

### P3 — 健壮性

- **`pruneTranscriptContextsLocked` 遗漏清理**：TTL 清理循环未覆盖 `toolBridgeContexts`、`toolPlannerContexts`、`explicitProviderContexts` 三组 map。补全这三组 map 的 TTL 清理逻辑。
  - 文件：`internal/modules/openai/openai_service.go`

- **`generateChatID` 碰撞风险**：使用 `math/rand` + 时间戳生成 ID，高并发下可能碰撞。改为 `crypto/rand` 生成 12 字节随机数（24 hex 字符），彻底消除碰撞。
  - 文件：`internal/modules/openai/openai_service.go`

### Benchmark

```
BenchmarkExtractStreamTextFromBuffer-12    256KB buffer    ~4.5ms/op   36-40 MB/s
BenchmarkHasConversationStateRLock-12      1000 entries    20.87 ns/op  0 allocs
BenchmarkPruneConversationsLocked-12       1000 entries    220μs/op
BenchmarkGenerateChatID-12                 crypto/rand     240 ns/op    88 B/op
```

### 测试

```
go test -count=1 ./...
ok  gemini-free-api/cmd/server               4.091s
ok  gemini-free-api/internal/commons/configs  0.613s
ok  gemini-free-api/internal/modules/admin    3.789s
ok  gemini-free-api/internal/modules/openai   3.447s
ok  gemini-free-api/internal/modules/providers 2.400s
```
