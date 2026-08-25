# M365 Cloudflare Tunnel 运维说明

## 固定信息

- Tunnel 名称：`m365`
- Tunnel ID：`34f33ec9-71aa-4b14-ba62-00a89a3f1874`
- 公网主机名：`M365.yanqiudesu.kdns.fr`
- 本地配置：`C:\ProgramData\cloudflared\m365-config.yml`
- 凭据文件：`C:\ProgramData\cloudflared\34f33ec9-71aa-4b14-ba62-00a89a3f1874.json`
- 日志文件：`C:\ProgramData\cloudflared\m365.log`
- 监督任务：`Cloudflared-M365`
- 预期 loopback 源站：`http://127.0.0.1:4141`

`Cloudflared` Windows 服务及其配置 `C:\ProgramData\cloudflared\config.yml` 属于 `grok2api`，不要修改或停止它。

## 启停与查看

以下 PowerShell 命令需要管理员 PowerShell：

```powershell
Start-ScheduledTask -TaskName Cloudflared-M365
Stop-ScheduledTask -TaskName Cloudflared-M365
Get-ScheduledTask -TaskName Cloudflared-M365
Get-ScheduledTaskInfo -TaskName Cloudflared-M365
Get-Content C:\ProgramData\cloudflared\m365.log -Tail 100
& 'C:\Program Files (x86)\cloudflared\cloudflared.exe' tunnel info m365
```

任务设置为开机启动、SYSTEM、无限执行、已有实例时不启动新实例，并在失败后自动重启。

## 配置验证

```powershell
& 'C:\Program Files (x86)\cloudflared\cloudflared.exe' tunnel --config C:\ProgramData\cloudflared\m365-config.yml ingress validate
& 'C:\Program Files (x86)\cloudflared\cloudflared.exe' tunnel --config C:\ProgramData\cloudflared\m365-config.yml ingress rule 'https://M365.yanqiudesu.kdns.fr/v1/chat/completions'
```

第二条应显示 `Matched rule #0`，并指向 `http://127.0.0.1:4141`。

## 回滚

以下命令会移除 M365 监督任务、本地配置和凭据文件，不影响 grok2api 的 `Cloudflared` 服务：

```powershell
Stop-ScheduledTask -TaskName Cloudflared-M365 -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName Cloudflared-M365 -Confirm:$false
Remove-Item -LiteralPath C:\ProgramData\cloudflared\m365-config.yml -Force
Remove-Item -LiteralPath C:\ProgramData\cloudflared\34f33ec9-71aa-4b14-ba62-00a89a3f1874.json -Force
```

回滚后确认 grok2api 服务仍在运行：

```powershell
Get-Service Cloudflared
(Get-CimInstance Win32_Service -Filter "Name='Cloudflared'").PathName
```

不要执行 `cloudflared service install` 或 `cloudflared service uninstall`。

## 远程配置注意事项

本机配置验证通过，但日志若出现 `Updated to new configuration` 并显示不同的 `originService`，说明 Cloudflare Zero Trust 的远程 tunnel configuration 覆盖了本地 ingress。当前探测曾显示远程源站为 `http://localhost:1520`，应在 Cloudflare 控制台将 m365 tunnel 的 hostname 规则改为 `M365.yanqiudesu.kdns.fr` -> `http://127.0.0.1:4141`，或停用该 tunnel 的远程配置，然后重新探测公网地址。
