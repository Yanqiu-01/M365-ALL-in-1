# -*- coding: utf-8 -*-
"""Macro: 用 playwright + 系统 Chrome, 全自动走完 M365 授权:
/api/auth/start -> 微软登录(填 email+password) -> 等 nativeclient 回调 -> /api/auth/callback
用法: python oauth_auto.py <email> <password> [--headless]
不打印密码/token/回调 code。"""
import json, sys, time, urllib.request, urllib.parse
from http.cookiejar import CookieJar
from pathlib import Path
from playwright.sync_api import sync_playwright

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import common

BASE = common.gateway_url()
PW_PROBE = Path(__file__).parent / "_pw_probe.txt"

_OPENER = None


def _opener():
    """带管理员会话的 opener —— 网关的授权端点需要 admin cookie。"""
    global _OPENER
    if _OPENER is not None:
        return _OPENER
    cj = CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
    req = urllib.request.Request(
        BASE + "/api/admin/login",
        data=json.dumps({"password": common.admin_password()}).encode(),
        headers={"Content-Type": "application/json"})
    with op.open(req, timeout=10) as r:
        if json.loads(r.read().decode()).get("status") != "authenticated":
            raise SystemExit("网关管理员登录失败")
    _OPENER = op
    return op


def start():
    with _opener().open(BASE + "/api/auth/start?forceLogin=1", timeout=15) as r:
        return json.loads(r.read().decode())


def callback(params):
    qs = urllib.parse.urlencode(params)
    with _opener().open(BASE + "/api/auth/callback?" + qs, timeout=30) as r:
        return json.loads(r.read().decode())


def fill_ms_login(page, email, password):
    """微软登录: 填账号页 -> next -> 密码页 -> sign in. 返回最终地址. 容忍验证/其他页面."""
    # 等 email 输入框
    for _ in range(30):
        try:
            box = page.locator('input[type="email"],input[name="loginfmt"]')
            if box.count() and box.first.is_visible():
                box.first.fill(email)
                break
        except Exception:
            pass
        time.sleep(1)
    # 点下一步
    _click_next(page)
    time.sleep(1.5)
    # 密码框
    for _ in range(30):
        try:
            pbox = page.locator('input[type="password"],input[name="passwd"]')
            if pbox.count() and pbox.first.is_visible():
                pbox.first.fill(password)
                break
        except Exception:
            pass
        time.sleep(1)
    # 提交 (Sign in / 登录)
    subs = page.locator('input[type="submit"],button[type="submit"]')
    n = subs.count()
    for i in range(n):
        try:
            if subs.nth(i).is_visible():
                subs.nth(i).click(timeout=3000)
                break
        except Exception:
            pass
    time.sleep(2)


def _click_next(page):
    for sel in ['input[type="submit"]', 'button[type="submit"]', '#idSIButton9']:
        try:
            el = page.locator(sel).first
            if el.is_visible():
                el.click(timeout=3000)
                return
        except Exception:
            pass


def main():
    email = sys.argv[1]
    password = sys.argv[2] if len(sys.argv) > 2 else PW_PROBE.read_text(encoding="utf-8").strip()
    headless = "--headless" in sys.argv
    d = start()
    state = d["state"]
    auth_url = d["url"]
    print(f"[i] {email} 开始授权 (state={state[:8]}...)", flush=True)
    ok = False
    outcome = ""
    with sync_playwright() as p:
        browser = p.chromium.launch(channel="chrome", headless=headless)
        ctx = browser.new_context()
        page = ctx.new_page()
        page.goto(auth_url, timeout=60000)
        fill_ms_login(page, email, password)
        # 等回调 (nativeclient + code)
        for _ in range(150):
            try:
                url = page.url
            except Exception:
                time.sleep(1)
                continue
            if "code=" in url and "state=" in url:
                qs = urllib.parse.urlparse(url).query
                params = dict(urllib.parse.parse_qsl(qs))
                if params.get("state") == state:
                    r = callback(params)
                    st = r.get("account", {}).get("status", r.get("status", "?"))
                    outcome = st
                    print(f"[i] {email} 回调完成 status={st}", flush=True)
                    ok = True
                    break
                else:
                    print(f"[!] state mismatch", flush=True)
            time.sleep(1)
        if not ok:
            outcome = "timeout"
            # 页面状态诊断(不含密码)
            try:
                print(f"[!] 最终URL主机: {urllib.parse.urlparse(page.url).netloc}", flush=True)
            except Exception:
                pass
        browser.close()
    print(f"{email} => {outcome}", flush=True)
    return 0 if ok else 2


if __name__ == "__main__":
    sys.exit(main())