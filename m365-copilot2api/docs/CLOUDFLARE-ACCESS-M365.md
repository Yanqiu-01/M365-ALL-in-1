# M365 Cloudflare Access 运维说明

## 当前状态

`M365.yanqiudesu.kdns.fr` 由 Cloudflare Tunnel `m365`（ID `34f33ec9-71aa-4b14-ba62-00a89a3f1874`）转发到 `http://127.0.0.1:4141`。本次未执行 Cloudflare Access API 变更：本机没有找到具备 Cloudflare API 权限的 API Token。`C:\Users\ad\.cloudflared\cert.pem` 是 Tunnel 管理凭据，不是 Access 应用管理凭据，不能用于本操作。

## 路径范围

`internal/web/server.go` 的 `Routes()` 注册了以下程序化 API：

- `/v1/models`
- `/v1/chat/completions`
- `/v1/responses`
- `/v1/messages`（Anthropic 兼容）
- `/v1/images/generations`
- `/v1/images/edits`
- `/v1/images/files/`
- `/v1/sessions` 和 `/v1/sessions/`
- `/v1/mcp/tools`、`/v1/mcp/sse`、`/v1/mcp/message`（MCP；由 `internal/mcp/server.go` 挂载）

因此旁路应用必须精确覆盖 `M365.yanqiudesu.kdns.fr/v1/*`。不要把整个主机名或 `/api/*` 放入旁路应用。

控制台 SPA 的请求面在 `/api/*`；其中 `/api/auth/start`、`/api/auth/status`、`/api/auth/callback` 是本地 PKCE 流程。默认 OAuth redirect URI 是 Microsoft 的 `https://login.microsoftonline.com/common/oauth2/nativeclient`，回调结果由用户粘贴到本地应用的 `/api/auth/callback`，不是该公网主机的 OAuth redirect URI。因此没有需要公开旁路的公网 OAuth callback 路径。

## 两个 Access 应用

按以下顺序在 Cloudflare Zero Trust Dashboard 创建应用。顺序和路径优先级都很重要：Cloudflare 对更具体的路径采用 most-specific-path-wins，`/v1/*` 应先明确旁路，主机级 Allow 再保护控制台。

### 1. M365 API Bypass

入口：`https://one.dash.cloudflare.com/` → 选择对应账户 → **Access** → **Applications** → **Add an application** → **Self-hosted**。

- Application name：`M365 API Bypass`
- Session duration：`24 hours`
- Application domain：`M365.yanqiudesu.kdns.fr`
- Path：`/v1/*`
- Policy name：`API clients bypass`
- Action：`Bypass`
- Configure rules：**Include** → Selector `Everyone`
- 认证/身份提供商：旁路应用不需要交互式登录；不要添加 Allow 规则

保存应用。该应用覆盖 OpenAI 兼容、Anthropic 兼容和 MCP 的所有 `/v1/` 路径，但源站自己的 API-key 校验仍然有效。

### 2. M365 Admin Console

再次 **Add an application** → **Self-hosted**。

- Application name：`M365 Admin Console`
- Session duration：`24 hours`
- Application domain：`M365.yanqiudesu.kdns.fr`
- Path：留空（主机级，覆盖控制台和 `/api/*`）
- Policy name：`Owner email only`
- Action：`Allow`
- Configure rules：**Include** → Selector `Emails` → 填入 Cloudflare 账户所有者邮箱的完整小写邮箱地址
- Identity providers：仅选择 **One-time PIN**
- Cookie setting（如界面提供）：开启 `HttpOnly cookie attribute`

One-time PIN 是邮箱验证码，不等同于真正的 MFA。要实现真正 MFA，应接入支持 MFA 的 IdP（例如 Entra ID、Google Workspace、Okta 等），在 IdP 侧强制 MFA，再在该 Allow 策略中使用该 IdP。

保存后，主机根路径和 `/api/*` 会先经过 Access，源站自己的管理员会话/密码校验仍保留为第二层防线。

## 验证

在未完成 Access 登录的外部网络执行：

```powershell
curl.exe -s -o NUL -D - --max-time 20 https://M365.yanqiudesu.kdns.fr/
curl.exe -s -o NUL -D - --max-time 20 https://M365.yanqiudesu.kdns.fr/v1/models
curl.exe -s -o NUL -D - --max-time 20 https://M365.yanqiudesu.kdns.fr/api/health
```

期望结果：

- `/`：`302`，`Location` 指向 `*.cloudflareaccess.com` 登录页；或返回 Access challenge 页面，并出现相应 `cf-access-*` 响应头。
- `/v1/models`：仍为源站 `401 Unauthorized`，不能出现 Access 登录 `302`。这证明 API-key 客户端没有被交互式 Access 拦截。
- `/api/health`：Access challenge/登录响应，而不是直接到达源站的 `401`。

Access 变更传播后再次执行上述探测。也应确认：

```powershell
Get-Service Cloudflared | Select-Object Name,Status
curl.exe -s -o NUL -D - --max-time 20 https://M365.yanqiudesu.kdns.fr/v1/models
```

`Cloudflared` 应为 `Running`，`/v1/models` 应为 `401 Unauthorized`。不要修改 `C:\ProgramData\cloudflared\config.yml`、Windows 服务、`grok2api`、`mailhook` 或 `kiro2cc` 的配置。

## 为机器客户端增加受控访问

不要把现有 API-key 客户端改成浏览器登录。若以后某个机器客户端确实必须经过 Access，在 Zero Trust → **Access** → **Service Auth** → **Service Tokens** 创建一个专用 Service Token，然后在一个更具体的应用路径（例如 `M365.yanqiudesu.kdns.fr/v1/internal-client/*`）创建 **Allow** 策略，Include 选择 **Service Token**。机器请求发送：

```http
CF-Access-Client-Id: <client-id>
CF-Access-Client-Secret: <client-secret>
```

机密只能放在客户端的秘密存储或环境变量中，不能写入仓库。当前公开 API 的 `/v1/*` Bypass 必须保持在更广泛的范围之外，避免误伤现有手机应用。

## 回滚

在 **Access → Applications** 中删除 `M365 Admin Console` 和 `M365 API Bypass` 两个应用。删除后 Tunnel 和源站不会改变，但整个公网主机将恢复为没有 Cloudflare Access 身份门禁的状态：根页面和登录入口可被互联网访问，只有源站现有管理员/API-key 校验继续生效。回滚后立即用上面的三个 curl 探针确认行为，并记录暴露窗口。

## 已知残余风险

One-time PIN 只验证邮箱控制权，不能替代 IdP 强制 MFA。主机级 Allow 依赖 Cloudflare Access 的路径优先级正确生效；部署后必须确认 `/v1/models` 仍为 `401`，否则应立即修正旁路应用，避免手机和其他 API 客户端中断。
