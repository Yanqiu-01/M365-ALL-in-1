# 代理质量守护关 设计（P3 子组，本轮只出方案与独立工具，不改 internal/）

## 1. 为什么现有健康检查等于没有

实测三条，都可复现：

| 问题 | 证据（文件:行号） |
| --- | --- |
| 探测目标与真实依赖无关 | `internal/outbound/health.go:47-50`，默认目标 `http://www.msftconnecttest.com/connecttest.txt`，**明文 HTTP**。只支持明文 HTTP、不支持 CONNECT 的代理照样"reachable"，随后每次 chat 全挂。 |
| 没有后台巡检 | `CheckAll` 定义在 `internal/outbound/health.go:78`，唯一调用点是 `internal/web/proxy_pool.go:29`（`PUT /api/admin/proxy-pool?action=check`）。**只有人点才检查**，没有任何 goroutine 周期调用。 |
| 入池不校验 | `internal/web/proxy_pool.go:51` → `outbound.AddProxy`（`internal/outbound/proxy.go:134`）只做 URL 解析，不做连通性验证。20 条死代理可以一次性灌进去。 |
| 选路是纯轮询 | `internal/outbound/pool.go:62-78` `pickLocked` 只跳过 `cooldown`，不看成功率与延迟；`pickWebSocketLocked`（`pool.go:297-308`）同理。健康的和半死的等概率轮到。 |
| 失败惩罚太轻且无摘除 | `internal/outbound/pool.go:130-149` `mark`：失败仅退避 `failures*2s`，上限 2 分钟，**永不摘除**。死出口每 2 分钟回到轮询队列继续放血。 |

## 2. 探测目标：换成真实依赖

| 级别 | 目标 | 判定 |
| --- | --- | --- |
| L2 认证面 | `CONNECT`+TLS+`HEAD https://login.microsoftonline.com/common/discovery/instance` | 必须拿到 HTTP 状态码（200..499 算过；curl code 000 / 无状态行算失败） |
| L3a 业务面 | `CONNECT`+TLS `substrate.office.com:443` | TLS 握手成功 |
| L3b 业务面 | `CONNECT`+TLS `m365.cloud.microsoft:443` | TLS 握手成功 |
| L3c 传输面 | 真实 WebSocket Upgrade 到 `wss://substrate.office.com/m365Copilot/Chathub` | 收到 Upgrade 的状态行即算过：101 最理想；**400/401/403/404 也算过**（隧道确实承载了 WS 握手，只是没带凭据）。代理层拒绝 / TLS 失败 / 无响应算失败 |

L3c 的目标主机与路径取自 `internal/chathub/client.go:62`（`wsBase = "wss://substrate.office.com/m365Copilot/Chathub"`），Origin 取自 `client.go:129`。探测全程不带凭据。

## 3. 打分公式

```
score = 0.60 * successRate + 0.25 * latencyTerm + 0.15 * wsTerm

successRate = (passes + 1) / (attempts + 2)          # Laplace 平滑，单次幸运探测拿不到 1.0
latencyTerm = B / (B + medianL2Latency),  B = 1500ms  # 恰好等于预算时得 0.5
wsTerm      = 1 如果最近一轮 L3c 通过，否则 0
```

- 滑动窗口长度 20 轮（`WindowSize`），`successRate` 只统计窗口内样本。
- 权重理由：能不能通（successRate）比快不快重要，所以 0.60；延迟决定用户体验但不决定可用性，0.25；`wsTerm` 是硬门槛类信号，给 0.15 让"HTTP 通但 WS 不通"的出口无法挤进高分区（这正是当前 502 的主因）。
- 选路：在 `score` 降序里挑，而不是轮询。建议 `score < 0.60` 不参与正常选路，只做候补。

## 4. 摘除／恢复状态机

```
                      ┌──────────────── probe pass ─────────────────┐
                      │                                             │
   [candidate] --admit gate(L2+L3 全绿)--> (live) --fail x3--> (evicted) --pass x2--> (live)
                      │                      │                    │
                      │                 fail x1..2                │ 降频重探
                      │                      ↓               60s → 120s → 240s ... ≤15min
                      └──────────────── (suspect) ────────────────┘
```

| 状态 | 进入条件 | 探测间隔 | 是否参与选路 |
| --- | --- | --- | --- |
| `live` | 入池校验全绿；或 `evicted` 连续成功 2 次 | 60s（`BaseInterval`） | 是，按 score 排序 |
| `suspect` | 连续失败 1–2 次 | 30s（加密探测，快速定性） | 仅在无 live 出口时兜底 |
| `evicted` | 连续失败 3 次（`EvictAfter`） | 指数退避 2×，上限 15min（`MaxBackoff`） | 否 |

阈值取值理由：`EvictAfter=3` 容忍单次网络抖动又能在 ~3 分钟内摘掉真死的；`RestoreAfter=2` 要求两次连续成功，避免半死出口靠一次幸运探测复活来回抖动。

入池前校验：`AddProxy` 应先跑一轮 L2+L3，全绿才落库。用户 19:36 那次批量灌 20 条死代理把 502 率从 0.1% 打到 63.8%，就是缺这道门。

## 5. 挂载点（本轮不改，仅给坐标）

| 要做的改动 | 挂载位置 |
| --- | --- |
| 探测目标换成三个真实微软域名 + WS Upgrade | `internal/outbound/health.go:47-50`（替换 `target` 默认值与判定逻辑，`Check` 整体重写为四级） |
| 后台周期巡检 goroutine 启动 | `cmd/server/main.go:23` 之后（`outbound.ConfigureFromEnv()` 成功后），加 `outbound.StartHealthLoop(ctx, 60*time.Second)`；随 `main.go:45` 的 `signal.NotifyContext` 一起退出 |
| 巡检循环本体 + 状态机 | 新增 `internal/outbound/guard.go`（新文件，不动现有 `health.go` 结构） |
| `poolEntry` 增加 window/state/score 字段 | `internal/outbound/pool.go:19-29` |
| 选路改为按 score | `internal/outbound/pool.go:62-78`（`pickLocked`）与 `pool.go:297-308`（`pickWebSocketLocked`） |
| 失败惩罚改为状态机摘除 | `internal/outbound/pool.go:130-149`（`mark`） |
| 入池前校验 | `internal/outbound/proxy.go:134`（`AddProxy`）与 `internal/web/proxy_pool.go:51` 之间加校验；批量灌入走同一道门 |
| 状态可观测（把 state/score 吐给前端） | `internal/outbound/pool.go:341-349`（`List()` 增加 `state`/`score`/`successRate` 字段） |

## 6. 本工具（tools/proxy-quality-guard）

独立 `main` 包，零第三方依赖，不 import `internal/`，因此不影响网关构建与运行。

```
go build -o pqg.exe ./tools/proxy-quality-guard/
pqg.exe -in candidates.txt -out report.json -rounds 3 -interval 45s -conc 18 -timeout 12s -min-score 0.60
```

- `-in` 每行一个 `scheme://host:port`（无 scheme 默认 http），`#` 注释与空行忽略。
- `-conc` 超过 20 会被自动夹到 20，避免对微软高并发。
- 每轮只有上一轮通过的出口才继续探，省掉对死出口的重复微软请求。
- 退出码：admitted ≥ 1 → 0；否则 1。可直接当灌池前的 gate。
- `Score()` 与 `EvictAfter`/`RestoreAfter`/`WindowSize`/`LatencyBudget`/`BaseInterval`/`MaxBackoff` 是上面公式与状态机的可执行版本，落地到 `internal/` 时可直接搬。

实测（本机，2026-08-23）：对 5 条回环代理跑 2 轮，4 条 `live` 且 admitted，`7905` 因 1/2 掉到 `suspect` 未准入——说明打分确实能区分抖动出口。
