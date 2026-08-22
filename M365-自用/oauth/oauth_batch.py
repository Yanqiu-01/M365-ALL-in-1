# -*- coding: utf-8 -*-
"""批量 M365 oauth: chromin 最小化(屏幕外), 每号 /api/auth/start -> ms 登录 -> callback。
健壮登录: 处理 email/password/'Use your password'/stay signed in/captcha 丢弃。
用法: python oauth_batch2.py [--start N] [--limit M]
不打印密码/token/code。"""
import json
import sys
import time
import urllib.request
import urllib.parse
from http.cookiejar import CookieJar
from pathlib import Path
from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
import common

BASE = common.gateway_url()
CRED = common.cred_file()
OUT = common.data_dir() / "_oauth_progress.jsonl"
FAILS = common.data_dir() / "_oauth_fails.jsonl"
SHORT = 12000  # 单步超时 ms

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
    with _opener().open(BASE + "/api/auth/start?forceLogin=1", timeout=20) as r:
        return json.loads(r.read().decode())


def callback(params):
    qs = urllib.parse.urlencode(params)
    with _opener().open(BASE + "/api/auth/callback?" + qs, timeout=30) as r:
        return json.loads(r.read().decode())


def first_visible(page, selectors):
    for sel in selectors:
        el = page.locator(sel).first
        try:
            if el.count() and el.is_visible():
                return el
        except Exception:
            pass
    return None


def click_submit(page):
    for sel in ['input[type="submit"]', 'button[type="submit"]', '#idSIButton9']:
        el = page.locator(sel).first
        try:
            if el.count() and el.is_visible():
                el.click(timeout=SHORT)
                return True
        except Exception:
            pass
    return False


def ms_login(page, email, password):
    """返回状态: 'done'(到回调循环) / 'need_captcha' / 'need_verify' / 'no_password_field'"""
    page.set_default_timeout(SHORT)
    deadline = time.time() + 90

    # 1) email — 轮询等待, 最多 30s
    box = None
    t0 = time.time()
    while time.time() - t0 < 30:
        box = first_visible(page, ['input[type="email"]', 'input[name="loginfmt"]', 'input[type="text"]'])
        if box is not None:
            break
        time.sleep(1)
    if box is None:
        return "no_email_field"
    box.fill(email)
    click_submit(page)

    # 2) 循环识别密码页 / 其他
    while time.time() < deadline:
        pbox = first_visible(page, ['input[type="password"]', 'input[name="passwd"]'])
        if pbox is not None:
            pbox.fill(password)
            click_submit(page)
            time.sleep(1.5)

            # 3) 可能 "保持登录?" (idSIButton9=Yes) — 已包含在 click 里不再点
            #    尝试检测 stay signed in
            st = first_visible(page, ['#KmsiCheckboxField', 'input#idSIButton9[value="Yes"]'])
            if st is not None or _has_yes_visible(page):
                _click_yes(page)
            return "done"

        use_pw = first_visible(page, ['#iShowSkipLink', 'a[id="aadPwdSwitch"]', 'a[href*="password"]'])
        if use_pw is not None and "Use your password" in (use_pw.inner_text() or "") or _pwswitch(page):
            _click_pwswitch(page)
            time.sleep(1)
            continue

        # 验证码 / 风险
        body = ""
        try:
            body = page.content().lower()
        except Exception:
            pass
        if "captcha" in body or "recaptcha" in body or "help us protect" in body:
            return "need_captcha"
        if "we need to verify" in body or "more information is required" in body:
            return "need_verify"
        if "account has been locked" in body or "been blocked" in body:
            return "locked"

        time.sleep(2)
    return "login_timeout"


def _has_yes_visible(page):
    try:
        btn = page.locator('#idSIButton9').first
        if btn.count() and btn.is_visible():
            val = (btn.get_attribute("value") or "").strip().lower()
            return val in ("yes", "是", "si", "keep me signed in")
    except Exception:
        pass
    return False


def _click_yes(page):
    try:
        btn = page.locator('#idSIButton9').first
        if btn.count() and btn.is_visible():
            btn.click(timeout=SHORT)
    except Exception:
        pass


def _pwswitch(page):
    try:
        el = page.locator('#iShowSkipLink, a[id="aadPwdSwitch"]').first
        if el.count() and el.is_visible():
            return True
    except Exception:
        pass
    return False


def _click_pwswitch(page):
    for sel in ['#iShowSkipLink', 'a[id="aadPwdSwitch"]']:
        try:
            el = page.locator(sel).first
            if el.count() and el.is_visible():
                el.click(timeout=SHORT)
                return
        except Exception:
            pass


def main():
    args = sys.argv[1:]
    start_i = 0
    limit = None
    for i, a in enumerate(args):
        if a == "--start" and i + 1 < len(args):
            start_i = int(args[i + 1])
        if a == "--limit" and i + 1 < len(args):
            limit = int(args[i + 1])

    creds = common.load_credentials()
    if not creds:
        raise SystemExit(f"账密文件为空或不存在: {CRED}")
    all_emails = list(creds.keys())
    emails = all_emails[start_i:]
    if limit is not None:
        emails = emails[:limit]

    done = set()
    if OUT.exists():
        for ln in OUT.read_text(encoding="utf-8").splitlines():
            try:
                done.add(json.loads(ln)["email"])
            except Exception:
                pass
    print(f"[i] 本批 {len(emails)} 个; 已成功 {len(done)}", flush=True)

    with sync_playwright() as p:
        browser = p.chromium.launch(
            channel="chrome",
            headless=False,
            args=["--start-minimized", "--window-position=-32000,-32000", "--window-size=1280,800"],
        )
        ctx = browser.new_context()
        fail_kinds = {}
        for i, email in enumerate(emails, 1):
            if email in done:
                print(f"[{i}/{len(emails)}] {email} 已成功, 跳过", flush=True)
                continue
            pw = creds.get(email)
            if not pw:
                print(f"[{i}/{len(emails)}] {email} 无密码", flush=True)
                continue
            print(f"[{i}/{len(emails)}] {email} ...", flush=True)
            page = ctx.new_page()
            outcome = "timeout"
            try:
                d = start()
                state = d["state"]
                page.goto(d["url"], timeout=60000)
                st = ms_login(page, email, pw)
                if st != "done":
                    outcome = st
                else:
                    # 等回调
                    cdl = time.time() + 150
                    got = None
                    while time.time() < cdl:
                        try:
                            url = page.url
                        except Exception:
                            time.sleep(1)
                            continue
                        if "code=" in url and "state=" in url:
                            params = dict(urllib.parse.parse_qsl(urllib.parse.urlparse(url).query))
                            if params.get("state") == state:
                                got = params
                                break
                            else:
                                outcome = "state_mismatch"
                        time.sleep(1)
                    if got:
                        r = callback(got)
                        outcome = r.get("account", {}).get("status", r.get("status", "ok"))
            except Exception as exc:
                outcome = "exc:" + type(exc).__name__
            finally:
                try:
                    page.close()
                except Exception:
                    pass
            rec = {"email": email, "result": outcome, "at": int(time.time())}
            if outcome in ("online", "ok", "authenticated", "present"):
                with OUT.open("a", encoding="utf-8") as f:
                    f.write(json.dumps(rec, ensure_ascii=False) + "\n")
                print(f"[{i}/{len(emails)}] {email} => {outcome}", flush=True)
                time.sleep(1)
            else:
                fail_kinds[outcome] = fail_kinds.get(outcome, 0) + 1
                with FAILS.open("a", encoding="utf-8") as f:
                    f.write(json.dumps(rec, ensure_ascii=False) + "\n")
                print(f"[{i}/{len(emails)}] {email} => {outcome} (跳)", flush=True)
            time.sleep(1.5)
        try:
            browser.close()
        except Exception:
            pass
    print("[done] 失败分类:", json.dumps(fail_kinds, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()