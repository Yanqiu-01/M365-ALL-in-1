# M365 源码版本核对与整合（2026-09-16）

## 结论

本整合树以 Git `origin/main` 的 `a5288e2`（2026-09-16 01:56 +08:00）为基线。该提交是检查时远端 `main` 的最新提交；新树位于独立 worktree，不修改历史快照或当前部署目录。

## 四个位置的角色

| 位置 | 判定 | 处理 |
|---|---|---|
| `E:\download\claude\_m365-git-work` | 实际 Git 仓库；`main` 与 `origin/main` 一致。其工作区包含尚未提交的 Edit/Read fidelity 和壁纸改动；2026-09-16 10:26 的构建与当前 live EXE SHA-256 相同。 | 作为补丁来源，不再作为最终整合目录。 |
| `E:\download\claude\M365-ALL-in-1` | 2026-09-16 01:32 左右的导出副本，不是 Git 工作区。含已被后续提交删除或替代的旧文件；原样 `go test ./...` 因 `internal/mcp/hide_child_windows.go` 与 `hide_child.go` 重复定义 `hideChildWindow` 而失败。 | 不做基线；不回灌重复/已废弃文件。 |
| `E:\download\claude\M365` | 部署、日志和历史构建归档。运行中的 `desktop-service-20260826\m365-gateway-pc.exe` 来自 `_m365-git-work` 的 2026-09-16 10:26 构建，两者 SHA-256 均为 `68AFA4BC452AC519E4279847CFA5E168298F4B5583554A78AFBC8FF4E2A20C37`。 | 只作为部署事实和产物校验来源；本次不覆盖、不重启。 |
| `E:\download\claude\CodeX\m365-memory-diagnosis` | Git worktree，HEAD `e65ccea`（2026-08-31），全量测试通过但明显早于当前 main。 | 保留为历史诊断快照，不向前覆盖当前实现。 |

## 未回灌的 ALL-in-1 独有文件

- `internal/web/native_panel_range.go` / `_test.go`：已在提交 `1475809` 删除，当前实现由 `native_panel_email.go` 等后续代码承担；旧副本还会与现有函数重复。
- `internal/web/hide_child_windows.go`、`internal/mcp/hide_child_windows.go` 及对应 other 文件：已在提交 `4a548f8` 删除，当前统一走 `internal/procwin`，并保留 `CREATE_NO_WINDOW` 回归测试；回灌会造成重复定义。
- `*.bak-issue2`、`_s1.py`、`_s2.py`：备份/临时脚本，不进入产品源码。

## 已整合内容

1. Read 行号 gutter 的无歧义展示，覆盖完整 prompt、增量 prompt、ledger、预算与 count_tokens。
2. Edit 参数 JSON 转义说明，覆盖 fenced/plugin/router/answer 路径。
3. 尾部 Edit 失败结果的下一轮 `[edit-recovery]` 提示，不自动重放写操作。
4. 历史 tool arguments 单层 JSON 展示，内部 wire/去重身份不变。
5. Windows 路径与控制字符组合 salvage 修复。
6. Edit 裸错误进入 ledger failure 判定。
7. 壁纸只读路由 `/wall/random`、`/wall/info` 与相应 CSP 放行。
8. Edit/Read fidelity 的专项测试和说明文档。

## 发布边界

本目录只生成候选构建。当前运行中的 `desktop-service-20260826` 不在本次整合过程中被覆盖或重启；部署应另行执行带备份、哈希核验和健康检查的切换流程。

## 验证结果

- go test ./... -count=1 -timeout=300s：通过。
- go vet ./...：通过。
- go build ./...：通过。
- git diff --check：通过。
- Windows 候选：.build-out/m365-gateway-pc-integrated.exe。
- 候选 SHA-256：$ch。
- 检查完成时 live SHA-256 仍为：$lh，未发生覆盖或重启。
