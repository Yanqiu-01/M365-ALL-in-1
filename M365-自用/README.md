# M365 Copilot 自用包

一个自包含的包：Copilot → OpenAI 兼容网关 + 本地控制面板 + 注册/授权脚本。
唯一入口是 `run.py`。

## 快速开始

```bash
pip install -r requirements.txt
playwright install chromium          # 只跑网关可以不装

cp config.example.json config.json   # 按需编辑
python run.py --bootstrap            # 生成管理员密码与 API key(只显示一次)
python run.py                        # 同时启动网关和面板
```

启动后面板在 `http://127.0.0.1:8555`，网关在 `http://127.0.0.1:4141`。

```bash
python run.py --gateway-only    # 只要网关
python run.py --panel-only      # 只要面板(网关须已在跑)
```

## 网关

OpenAI 兼容，`Authorization: Bearer <API key>`。

| 端点 | 说明 |
|---|---|
| `GET /v1/models` | 模型目录 |
| `POST /v1/chat/completions` | 流式与非流式 |
| `GET /healthz` | 存活探针(无鉴权) |
| `POST /api/admin/login` | 管理员登录，置会话 cookie |
| `GET /api/auth/start` / `callback` | PKCE 授权(需 admin) |
| `GET /api/accounts` · `DELETE /api/accounts/{id}` | 账号管理(需 admin) |

模型名决定上游 tone，tone 决定上游是否发思考链。`reasoning_effort` 会把 chat tone
升级成 `_Reasoning` 变体（`none`/`minimal`/`low` 不升级，已是 reasoning 的不降级）。
思考内容走 `reasoning_content` 字段。

不支持 `tools` / function calling —— 上游没有这个通道，所以明确回 400，不假装支持。

## 面板

| 区域 | 说明 |
|---|---|
| 状态卡片 | 账密文件账号数、网关在线数、本地授权记录、当前任务 |
| 注册新账号 | 三种出口：手机 LTE / Clash 机场 / 代理池，可指定数量 |
| OAuth 授权导入 | 单账号授权，或对未成功的账号批量补授权 |
| 运行日志 | 实时输出，可随时停止（连子进程一起结束） |

同一时刻只允许一个任务，重复提交返回 409。

## 配置

所有本机相关的值都在 `config.json`（已 gitignore），代码里没有硬编码。
注册功能需要填 `register` 段的 `site_url`、`turnstile_sitekey`、`email_domain`、
`email_prefix`、`password`，缺任一项则注册不可用（面板会显示未就绪）。

凭据不进配置文件：管理员密码和 API key 由 `--bootstrap` 写入 `gateway.data_dir`
（权限 600），也可用环境变量 `M365_ADMIN_PASSWORD` / `M365_API_KEY` 覆盖。

## 目录

```
run.py              唯一入口
common.py           子脚本共享的配置读取
config.example.json 配置模板
gateway/            网关(协议常量、PKCE、加密存储、ChatHub、API)
panel/              面板后端 + web/index.html
Register/           三个注册脚本
oauth/              授权脚本(单个 / 批量)
data/               运行期数据(账密、代理、结果、日志) —— 全部 gitignore
```

## 安全

- 网关与面板都只监听 127.0.0.1。面板另按 Host + Origin 双重校验拒绝跨站请求——
  它没有登录态，否则任意网页都能静默触发批量授权。不要反代或端口转发到外网。
- refresh token 用 Fernet 加密存 `data_dir/accounts.json`，密钥是同目录的
  `store.key`。**丢了 store.key，已存的 token 就再也解不开**；把两者放在一起时，
  加密只防止误看，不防拿到目录的人。access token 不落盘，靠 refresh 重取。
- 密码经临时文件传给子进程，不进 argv（argv 对本机其他进程可见），任务结束即删。
- 日志与接口都不打印密码 / token / 授权码。

## 已知限制

- 上游连续生成到 ~210-225 秒后会静默停发帧且永不发完成帧。网关靠独立的停滞检测
  收尾，并回 `finish_reason: "length"` + `m365.truncated`，好让客户端能区分
  「被截断」和「正常结束」。调 `chat_read_timeout_seconds` 只会移动切点，治不了根。
- 思考链是否出现、出现几张，上游每次请求随机；只发一张时无法诚实地报出时长。
- 每 IP 24h 只能成功注册 1 次；注册需要有头浏览器（无头会被 Turnstile 拦）。
- `websockets` 必须 `>=14`：v13 的 legacy client 只认 `extra_headers`，
  这里用的是 `additional_headers`。
