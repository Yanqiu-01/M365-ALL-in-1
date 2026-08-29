# register-headed

独立验证脚本：用有头 Chromium 打开 `https://office.965007.xyz`，按真实操作顺序跑一遍。

1. 打开注册站
2. 藏掉左侧说明栏
3. 填显示名 / 用户名 / 密码
4. 把 `#turnstileBox` 滚进视口
5. 点 Turnstile 方框左侧
6. 等 `input[name=cf-turnstile-response]` 出现 token

```bash
NODE_PATH=/usr/share/nodejs node tools/register-headed/run.js
```

产物在 `/tmp/register-headed/`：`01-filled.png`、`02-token.png` 或 `02-stuck.png`、`report.json`。

这个环境里的 Chromium 过不了 Cloudflare（GPU 进程会崩，Turnstile 停在 Verifying）。手机系统 WebView 才是正式后端。脚本只负责把步骤钉死，App 里的 FlareSolver 按同一顺序执行。
