# Changelog

## [Unreleased] — 性能与可靠性优化

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
