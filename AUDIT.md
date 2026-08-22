# AUDIT — 租户隔离/请求状态组收尾

- 日期：2026-08-17
- 隔离副本：`E:/download/claude/M365/repair-work/round3/tenant/`
- upstream 基线：`2ecf270654b4dd84d6b43b1caa9261cd6f1b8617`

## 实际交付结论

用户要求立即停止扩大范围后，已撤销 `internal/web/session_resolver.go` 中未完成的 `Owner`、`ExplicitID` 临时字段。当前未保留任何生产源码或测试源码改动；`patch.diff` 有意为空文件。

**本轮没有完成租户隔离修复。** 空补丁只表示没有交付半成品，不表示下述风险已经消除。

## 已审计的允许文件

- `internal/web/protocol_handlers.go`
- `internal/web/session_resolver.go`
- `internal/web/sessions.go`
- `internal/web/conversation_manager.go`
- `internal/web/conversations.go`
- `internal/web/conversation_cache.go`

未修改 `internal/web/server.go`、upstream、其他组目录、媒体 SSRF、`internal/auth`、`internal/outbound` 或 APK。

## 已确认但未修复的风险

1. **Responses 历史分桶不可靠**：`protocol_handlers.go` 使用 `extractAPIKey(r)` 作为历史桶键。X-API-Key 路径可能把完整 key 作为内存 map key；Bearer 路径只使用短前缀，存在碰撞；无 key 请求共享空桶。因此 `previous_response_id` 存在跨客户端串读风险。
2. **session resolver 没有认证 owner 边界**：显式 session ID、上游 session/conversation ID、内容前缀/后缀匹配和持久化记录均未绑定可靠认证身份。IP/UA 指纹只能作为无 key 兼容回退，不能作为租户认证边界。
3. **`/v1/sessions` 使用全局状态**：列表、读取和删除调用全局 resolver 方法，未按当前请求 owner 过滤；完整 binding 还包含上下文历史、IP 指纹和 user 字段。
4. **legacy store/cache 无法在现有签名下可靠隔离**：`sessionStore`、`userSessionStore` 和 conversation cache 的调用不传 request/owner，分别按公开 session key、请求体 user、account/model 进行全局复用。
5. **conversation manager 是全局状态**：现有 `Record` 调用不传 owner，自动清理可跨请求影响 Conversation 状态；需要明确管理员全局 inventory 语义或增加可靠 owner 传递。

## 未完成原因与阻断项

- 用户要求立即停止扩大实现范围并收尾，因此未继续编写生产代码或回归测试。
- 禁止修改的 `internal/web/server.go` 掌握认证中间件以及多处 legacy store/cache/manager 调用点。在当前写入边界内，无法可靠地把认证 owner 贯穿全部状态路径；仅修改六个允许文件会留下伪隔离或兼容性风险。
- 尚未实现统一 owner namespace，尚未新增租户隔离回归测试。
- 后续需由认证组在 `server.go` 提供经过验证的 owner context/参数，或协调授权修改相关调用接口。
- 当前 PowerShell 环境找不到 Go 可执行文件，因此 Go 测试与 vet 未运行。

## 凭据处理

未读取、使用或记录真实 API key、cookie、token 或其他敏感凭据。