# -*- coding: utf-8 -*-
"""本地控制面板 —— 注册 / 授权导入 / 状态。

所有路径与凭据来自 config.json,代码里不留硬编码。
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from collections import deque
from datetime import datetime, timezone, timedelta
from pathlib import Path
from typing import Any

from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, JSONResponse

from gateway.config import Settings

BASE_DIR = Path(__file__).resolve().parent.parent
REG_DIR = BASE_DIR / "Register"
OAUTH_DIR = BASE_DIR / "oauth"
STATIC_DIR = BASE_DIR / "panel" / "web"

TZ = timezone(timedelta(hours=8))
LOG_CAP = 800
ALLOWED_HOSTS = {"127.0.0.1", "localhost", "[::1]", "::1"}

JOB: dict[str, Any] = {
    "running": False, "kind": "", "cmd": [], "proc": None,
    "log": deque(maxlen=LOG_CAP), "started": 0, "exit": None,
}
JOB_LOCK = threading.Lock()


def now() -> str:
    return datetime.now(TZ).strftime("%H:%M:%S")


def _log(line: str) -> None:
    JOB["log"].append(f"[{now()}] {line}")


def create_panel(settings: Settings) -> FastAPI:
    app = FastAPI(title="M365 面板", docs_url=None, redoc_url=None)
    reg = settings.register or {}
    cred_file = Path(str(reg.get("cred_file") or BASE_DIR / "data" / "credentials.txt")).expanduser()
    gw = {"cookie": "", "ts": 0.0}

    # ── 只接受本机来源 ────────────────────────────────
    @app.middleware("http")
    async def local_only(request: Request, call_next):
        """面板没有登录态。任意网页都能跨站 POST /api/oauth/batch(无 body,
        不触发预检)在用户不知情时触发批量授权,所以按 Host + Origin 双重校验,
        顺带防 DNS rebinding。"""
        host = (request.headers.get("host") or "").rsplit(":", 1)[0]
        if host not in ALLOWED_HOSTS:
            return JSONResponse({"ok": False, "error": f"拒绝的 Host: {host}"}, status_code=403)
        origin = request.headers.get("origin")
        if origin:  # 无 Origin(curl / 地址栏直开)放行
            try:
                oh = urllib.parse.urlsplit(origin).hostname or ""
            except Exception:
                oh = ""
            if oh not in ALLOWED_HOSTS:
                return JSONResponse({"ok": False, "error": "跨站请求已拒绝"}, status_code=403)
        return await call_next(request)

    # ── 网关会话 ──────────────────────────────────────
    def gw_cookie(force: bool = False) -> str:
        if not force and gw["cookie"] and time.time() - gw["ts"] < 60:
            return gw["cookie"]
        if not settings.admin_password:
            raise RuntimeError("未配置管理员密码,先运行 python run.py --bootstrap")
        from http.cookiejar import CookieJar
        cj = CookieJar()
        op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
        req = urllib.request.Request(
            settings.gateway_url + "/api/admin/login",
            data=json.dumps({"password": settings.admin_password}).encode(),
            headers={"Content-Type": "application/json"})
        with op.open(req, timeout=8) as r:
            login = json.loads(r.read().decode())
        if login.get("status") != "authenticated":
            raise RuntimeError(f"网关管理员登录失败: {login}")
        gw["cookie"] = "; ".join(f"{c.name}={c.value}" for c in cj)
        gw["ts"] = time.time()
        return gw["cookie"]

    def gw_get(path: str) -> Any:
        def once(force: bool) -> Any:
            req = urllib.request.Request(settings.gateway_url + path,
                                         headers={"Cookie": gw_cookie(force)})
            with urllib.request.build_opener().open(req, timeout=8) as r:
                return json.loads(r.read().decode())
        try:
            return once(False)
        except urllib.error.HTTPError as e:
            if e.code not in (401, 403):
                raise
            gw["cookie"] = ""  # 会话失效,重登一次
            return once(True)

    def gateway_state() -> tuple[int, str, list[str]]:
        try:
            acc = gw_get("/api/accounts")
            emails = sorted(a.get("email") for a in acc.get("accounts", []) if a.get("email"))
            return len(emails), "", emails
        except urllib.error.URLError as e:
            return 0, f"网关不可达 ({settings.gateway_url}): {e.reason}", []
        except Exception as e:
            return 0, f"{type(e).__name__}: {e}", []

    def creds_map() -> dict[str, str]:
        m: dict[str, str] = {}
        if cred_file.exists():
            for line in cred_file.read_text(encoding="utf-8", errors="replace").splitlines():
                if "----" in line:
                    e, p = line.strip().split("----", 1)
                    m[e.strip()] = p.strip()
        return m

    def jsonl(path: Path) -> list[dict[str, Any]]:
        out = []
        if path.exists():
            for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
                try:
                    out.append(json.loads(line))
                except Exception:
                    pass
        return out

    # ── 任务执行 ──────────────────────────────────────
    def reader(proc: subprocess.Popen) -> None:
        try:
            for line in proc.stdout:  # type: ignore[union-attr]
                JOB["log"].append(line.rstrip("\r\n"))
        except Exception as e:
            JOB["log"].append(f"[reader 异常] {type(e).__name__}: {e}")
        finally:
            try:
                proc.stdout.close()  # type: ignore[union-attr]
            except Exception:
                pass
            code = proc.wait()
            try:
                (OAUTH_DIR / "_pw_probe.txt").unlink(missing_ok=True)
            except Exception:
                pass
            with JOB_LOCK:
                JOB["exit"], JOB["running"] = code, False
            _log(f"任务结束 exit={code}")

    def spawn(argv: list[str], label: str) -> tuple[dict[str, Any], int]:
        with JOB_LOCK:
            if JOB["running"]:
                return {"ok": False, "error": f"已有任务在跑: {JOB['kind']}"}, 409
            JOB.update(running=True, kind=label, cmd=list(argv),
                       started=int(time.time()), exit=None)
            JOB["log"].clear()
            try:
                proc = subprocess.Popen(
                    argv, cwd=str(BASE_DIR),
                    stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT, text=True,
                    encoding="utf-8", errors="replace", bufsize=1,
                    env={**os.environ, "PYTHONUTF8": "1", "PYTHONIOENCODING": "utf-8",
                         "M365_CONFIG": str(BASE_DIR / "config.json")},
                )
            except Exception as e:
                JOB["running"] = False
                return {"ok": False, "error": f"启动失败: {e}"}, 500
            JOB["proc"] = proc
            t = threading.Thread(target=reader, args=(proc,), daemon=True)
        _log(f"启动 {label}: {Path(argv[1]).name if len(argv) > 1 else argv[0]}")
        t.start()
        return {"ok": True, "kind": label}, 200

    def reply(res: tuple[dict[str, Any], int]):
        payload, status = res
        return payload if status == 200 else JSONResponse(payload, status_code=status)

    # ── 路由 ──────────────────────────────────────────
    @app.get("/", response_class=HTMLResponse)
    def index() -> str:
        return (STATIC_DIR / "index.html").read_text(encoding="utf-8")

    @app.get("/api/state")
    def state() -> dict[str, Any]:
        creds = creds_map()
        online, error, emails = gateway_state()
        return {
            "cred_total": len(creds),
            "cred_file": str(cred_file),
            "gw_online": online,
            "gw_error": error,
            "gw_emails": emails[:200],
            "gw_url": settings.gateway_url,
            "register_ready": bool(reg.get("site_url") and reg.get("email_domain")),
            "oauth_done": jsonl(OAUTH_DIR / "_oauth_progress.jsonl"),
            "oauth_fails": jsonl(OAUTH_DIR / "_oauth_fails.jsonl"),
            "job": {"running": JOB["running"], "kind": JOB["kind"],
                    "started": JOB["started"], "exit": JOB["exit"],
                    "log": list(JOB["log"])[-200:]},
        }

    @app.post("/api/register")
    async def register(request: Request):
        if not reg.get("site_url"):
            return JSONResponse(
                {"ok": False, "error": "config.json 的 register.site_url 未配置"},
                status_code=400)
        body = await request.json()
        mode = str(body.get("mode") or "phone")
        count = int(body.get("count") or 1)
        if not 1 <= count <= 20:
            return JSONResponse({"ok": False, "error": "count 需 1-20"}, status_code=400)
        script = {"phone": "register_m365_phone.py",
                  "clash": "register_m365_clash.py",
                  "proxy": "register_accounts.py"}.get(mode)
        if script is None:
            return JSONResponse({"ok": False, "error": f"未知 mode {mode}"}, status_code=400)
        argv = [sys.executable, str(REG_DIR / script), "--limit", str(count)]
        return reply(spawn(argv, f"register:{mode}"))

    @app.post("/api/oauth")
    async def oauth(request: Request):
        body = await request.json()
        email = str(body.get("email") or "").strip()
        if not email:
            return JSONResponse({"ok": False, "error": "email 必填"}, status_code=400)
        creds = creds_map()
        if email not in creds:
            return JSONResponse({"ok": False, "error": f"账密文件里没有 {email}"},
                                status_code=400)
        # 密码经临时文件交给子进程,不进 argv(argv 对本机其他进程可见)
        (OAUTH_DIR / "_pw_probe.txt").write_text(creds[email], encoding="utf-8")
        return reply(spawn([sys.executable, str(OAUTH_DIR / "oauth_auto.py"), email],
                           f"oauth:{email}"))

    @app.post("/api/oauth/batch")
    def oauth_batch():
        return reply(spawn([sys.executable, str(OAUTH_DIR / "oauth_batch.py")], "oauth:batch"))

    @app.post("/api/job/stop")
    def job_stop() -> dict[str, Any]:
        proc = JOB.get("proc")
        if not proc or proc.poll() is not None:
            with JOB_LOCK:
                JOB["running"] = False
            return {"ok": True, "note": "没有在跑的任务"}
        # 连子孙一起杀: 脚本会拉起浏览器,只 terminate 父进程会留孤儿
        how = "taskkill"
        try:
            if os.name == "nt":
                subprocess.run(["taskkill", "/PID", str(proc.pid), "/T", "/F"],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
            else:
                raise OSError("non-windows")
        except Exception:
            how = "terminate"
            try:
                proc.terminate()
            except Exception:
                pass
        _log(f"已请求停止任务 ({how})")
        return {"ok": True}

    @app.get("/api/job/poll")
    def job_poll() -> dict[str, Any]:
        """只读快照 —— 管道由 reader 线程排空,这里不会阻塞。"""
        return {"running": JOB["running"], "kind": JOB["kind"], "exit": JOB["exit"],
                "log": list(JOB["log"])[-200:]}

    return app
