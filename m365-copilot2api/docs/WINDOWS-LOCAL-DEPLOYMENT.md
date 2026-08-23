# Windows local deployment

This service listens on `127.0.0.1:4141` only. Do not expose the port to a LAN or the Internet directly; front it with an authenticated tunnel or reverse proxy.

## What is installed on this host

- Scheduled task: `PC-Self-MS`
- Trigger: at system startup
- Principal: `SYSTEM` (`ServiceAccount` logon, `RunLevel=Highest`)
- Action: `C:\Windows\System32\cmd.exe /c "C:\ProgramData\PC-Self-MS\run-origin.cmd"`
- Working directory: `E:\download\claude\PC-Self-MS`
- Executable: `E:\download\claude\PC-Self-MS\bin\pc-self-ms.exe`
- Wrapper: `C:\ProgramData\PC-Self-MS\run-origin.cmd`
- Data directory: `C:\ProgramData\PC-Self-MS\data`
- Admin bootstrap secret file: `C:\ProgramData\PC-Self-MS\admin-bootstrap.secret`
- Task settings: single instance (`IgnoreNew`), no execution time limit, restart on failure (99 attempts, 1 minute apart)

A Scheduled Task action cannot set environment variables, so the wrapper supplies them. It sets `M365_LISTEN=127.0.0.1:4141`, `M365_DATA_DIR`, `M365_ADMIN_PASSWORD_BOOTSTRAP_FILE`, and `M365_TRACE=false`, and clears `M365_ALLOW_NONLOOPBACK` defensively. It holds only the *path* to the secret, never the secret value, and appends stdout and stderr to `C:\ProgramData\PC-Self-MS\origin.log`.

The bootstrap secret is 48 random characters from `System.Security.Cryptography.RandomNumberGenerator`, stored as UTF-8 with no BOM and no trailing newline. The application trims surrounding whitespace when reading it, rejects the known default password, and requires at least 6 characters.

## Access control model

`C:\ProgramData\PC-Self-MS`, its `data` subdirectory, the wrapper, and the secret file all have inheritance removed (`icacls /inheritance:r`) and grant Full control only to:

- `NT AUTHORITY\SYSTEM`
- `BUILTIN\Administrators`

No `Users`, `Everyone`, or `Authenticated Users` entries are present. Restricting the wrapper matters: it is executed by SYSTEM, so write access for a non-administrator would be a privilege-escalation path.

## Privilege tradeoff

`SYSTEM` was chosen so the origin starts at boot, before any interactive logon, without storing a user password in Task Scheduler. The cost is that the process runs with full machine privileges.

The lower-privilege alternative is a dedicated local service account (or a group-managed service account) as the task principal, with ACLs granting only that account access to the ProgramData runtime directory, and read/execute on the program directory. That reduces blast radius but requires storing or managing that account's credential. A per-user logon-triggered task is lower privilege still, but the origin would be down whenever no one is signed in, which is unsuitable when a boot-started tunnel depends on it.

## Operations

From an elevated PowerShell:

```powershell
Start-ScheduledTask -TaskName 'PC-Self-MS'
Stop-ScheduledTask  -TaskName 'PC-Self-MS'
Get-ScheduledTask   -TaskName 'PC-Self-MS' | Get-ScheduledTaskInfo
```

Verify the binding and health, which does not require elevation:

```powershell
Get-NetTCPConnection -State Listen -LocalPort 4141
curl.exe -i http://127.0.0.1:4141/api/health
```

`/api/health` is behind the administrator session, so an unauthenticated probe returns `401`. A `401` therefore confirms the origin is up and enforcing authentication; a connection failure is the signal that it is down.

The task and its ProgramData directory are SYSTEM-owned, so an unelevated session cannot query the task, stop the process, or read the runtime directory. Use an elevated shell for those operations.

## Rotate the admin secret

1. Stop the task.
2. Generate a new cryptographically random secret with `System.Security.Cryptography.RandomNumberGenerator`.
3. Write it to `C:\ProgramData\PC-Self-MS\admin-bootstrap.secret` as UTF-8 without BOM and without a trailing newline.
4. Confirm the file still grants access only to SYSTEM and Administrators.
5. Delete `C:\ProgramData\PC-Self-MS\data\admin-password` if it exists; that persisted value takes precedence over the bootstrap file.
6. Start the task and authenticate with the new secret.

Changing the password through the web UI writes `data\admin-password`, which then takes precedence over the bootstrap file on subsequent starts.

## Rollback

From an elevated PowerShell:

```powershell
Stop-ScheduledTask -TaskName 'PC-Self-MS' -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName 'PC-Self-MS' -Confirm:$false
Remove-Item -LiteralPath 'C:\ProgramData\PC-Self-MS' -Recurse -Force
```

This removes only the `PC-Self-MS` task and its runtime directory. It does not touch the built executable, the repository, or any Cloudflare Tunnel configuration.

## Build

The repository carries no Go toolchain. A portable toolchain may be placed at `.toolchain\go` (git-ignored); it is not installed system-wide.

```powershell
$env:GOROOT = "$PWD\.toolchain\go"
$env:PATH = "$env:GOROOT\bin;$env:PATH"
gofmt -l cmd internal
go vet ./...
go test ./...
go build -o .\bin\pc-self-ms.exe .\cmd\server
```

Scope `gofmt` to `cmd` and `internal`; `gofmt -l .` would also walk the vendored toolchain under `.toolchain`.

Never place a live password, Microsoft token, API key, or Cloudflare token in this repository.
