# M365-Copilot2API 性能审计报告

> 审计时间：2026-08-07
> 审计方式：只读代码审查。修复状态截至提交 b428075（第一批快赢修复）。

## Top1. 每请求 5-7 次同步整文件写盘且持全局锁（部分修复）

- **位置**：`internal/auth/keys.go:117`、`internal/web/session_resolver.go:234/268/428`、`internal/web/conversation_manager.go:114`、`internal/web/usage.go:83`、`internal/web/sessions.go:144/158`
- **问题**：每次请求都会多次对 JSON 存储文件做全量 marshal + 写盘，且全部发生在全局锁临界区内。磁盘延迟直接串行进请求路径，吞吐被钉死；多 key 相互拖慢。
- **修复状态**：`internal/auth/cache.go`、`session_resolver.go`、`conversation_manager.go`、`sessions.go`、`keys.go`、`settings.go`、`deployments.go`、`admin_security.go` 已改为临时文件 + rename 原子落盘（`internal/web/atomicfile.go`），避免写坏半截文件；但"锁内写盘"本身（性能问题）仍未解决，留待后续：`LastUsedAt` 改为仅内存更新、异步批量落盘。

## Top2. debug 中间件每 chunk 拷贝 + 全量 unmarshal

- **位置**：`internal/web/debug.go:169-190`（captureWriter 每 chunk 拷贝）、`debug.go:162-168`、`redactBody` 全量 `json.Unmarshal`
- **问题**：开启 debug 后每个 `/v1/*` 请求缓冲全文并全量 JSON 解析，叠加流式每 chunk 拷贝，CPU 与内存开销大。
- **修复状态**：未修复（第一批范围外）。建议：debug 默认关闭、限定捕获字节、深度递归脱敏、日志轮转。

## Top3. 流式拼接 O(n²)（已修复）

- **位置**：`internal/chathub/client.go` 原 `streamedText += d` 逐次字符串拼接
- **问题**：每次 delta 都重新分配整串，长回复下 O(n²) 拷贝。
- **修复状态**：已改为 `strings.Builder`（`streamed`），`emitDelta`/`emitSnapshot` 均走 Builder，`streamed.String()` 仅在快照比对时调用。

## Top4. WS 读取循环阻塞窗口

- **位置**：`internal/chathub/client.go` 顶层循环 `ReadMessage` 阻塞达 90s，ctx 取消只在循环顶部检查
- **问题**：`ReadMessage` 阻塞期间不响应 ctx 取消，最多延迟 90s（读 deadline）才退出。
- **修复状态**：未修复。建议读 goroutine + select 或 `SetReadDeadline` 与 ctx 联动。

## Top5. sessionResolver miss 时全量 Jaccard 对比

- **位置**：`internal/web/session_resolver.go` 兜底相似度扫描（锁内逐条 tokenize 整个历史）
- **问题**：上下文未精确命中时对全部 session 逐一算 Jaccard，锁内 O(会话数 × 历史消息量)。
- **修复状态**：部分缓解——已加 `maxSessions` 上限（默认 1000，LRU 淘汰，`evictLocked` 在 Resolve/Bind 时触发），兜底扫描规模受限；但按 IP 指纹分桶索引的建议未实施。

## 修复优先级（第一批已勾选项打勾）

1. [x] 原子落盘（M2，Top1 子项）
2. [x] 流式拼接 O(n²)（Top3）
3. [x] sessionResolver 条数上限（Top5 缓解）
4. [ ] 锁内写盘移出临界区（Top1 主体）
5. [ ] debug 中间件开销（Top2）
6. [ ] WS 读取 ctx 联动（Top4）
